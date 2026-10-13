package app

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// bookFilter selects the books of one list: a reading status ("" for every
// book) and a search query already normalized by searchQuery.
type bookFilter struct{ Status, Query string }

// likeEscaper makes %, _, and the escape character itself match literally in
// LIKE ... ESCAPE '\'.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// where matches a lowercase substring of "title author tags". nc_lower is
// strings.ToLower, so non-ASCII letters fold the same way the query does.
func (f bookFilter) where() (string, []any) {
	conds, args := []string{}, []any{}
	if f.Status != "" {
		conds, args = append(conds, "b.reading_status=?"), append(args, f.Status)
	}
	if f.Query != "" {
		conds, args = append(conds, `nc_lower(b.title||' '||b.author||' '||b.tags) LIKE ? ESCAPE '\'`), append(args, "%"+likeEscaper.Replace(f.Query)+"%")
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

func countBooks(q queryer, f bookFilter) (int, error) {
	where, args := f.where()
	var n int
	err := q.QueryRow("SELECT count(*) FROM books b "+where, args...).Scan(&n)
	return n, err
}

// filteredIDs returns the IDs of every book the filter matches, in list order.
func filteredIDs(q queryer, f bookFilter) ([]int64, error) {
	where, args := f.where()
	rows, err := q.Query("SELECT b.id FROM books b "+where+" ORDER BY b.position,b.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// listedBook is a book as the list shows it. QueuePos is its 1-based place in
// the whole queue (0 outside it). A move is blocked by a pin on the book or
// on its neighbor, or by the end of the queue; the neighbor may be on
// another page.
type listedBook struct {
	Book
	QueuePos           int
	CanUp, CanDown     bool
	UpBlock, DownBlock string
}

// listBooks returns one page of the filter's books in list order.
func listBooks(q queryer, f bookFilter, limit, offset int) ([]listedBook, error) {
	where, args := f.where()
	books, err := queryBooks(q, where+" ORDER BY b.position,b.id LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	items := make([]listedBook, len(books))
	queued := []*listedBook{}
	for i, b := range books {
		items[i].Book = b
		if b.Status == "to-read" {
			queued = append(queued, &items[i])
		}
	}
	return items, placeInQueue(q, queued, f.Query != "")
}

// placeInQueue numbers the page's queued books within the whole queue and
// works out which moves are allowed. They arrive in queue order, so one count
// places the first; each later one follows the previous directly unless a
// search hid the books between them, which are then counted. A neighbor that
// is not on the page is looked up.
func placeInQueue(q queryer, queued []*listedBook, search bool) error {
	for i, l := range queued {
		var err error
		switch {
		case i == 0:
			l.QueuePos, err = queueCount(q, "(position,id)<=(?,?)", l.Position, l.ID)
		case search:
			prev := queued[i-1]
			var between int
			between, err = queueCount(q, "(position,id)>(?,?) AND (position,id)<=(?,?)", prev.Position, prev.ID, l.Position, l.ID)
			l.QueuePos = prev.QueuePos + between
		default:
			l.QueuePos = queued[i-1].QueuePos + 1
		}
		if err != nil {
			return err
		}
	}
	for i, l := range queued {
		var above, below sql.NullBool
		var err error
		switch {
		case i > 0 && queued[i-1].QueuePos == l.QueuePos-1:
			above = sql.NullBool{Bool: queued[i-1].Pinned, Valid: true}
		case l.QueuePos > 1:
			above, err = queueNeighborPinned(q, "(position,id)<(?,?) ORDER BY position DESC,id DESC", l)
		}
		if err != nil {
			return err
		}
		if i+1 < len(queued) && queued[i+1].QueuePos == l.QueuePos+1 {
			below = sql.NullBool{Bool: queued[i+1].Pinned, Valid: true}
		} else if below, err = queueNeighborPinned(q, "(position,id)>(?,?) ORDER BY position,id", l); err != nil {
			return err
		}
		l.CanUp, l.UpBlock = canMove(l.Book, above, "first", "above")
		l.CanDown, l.DownBlock = canMove(l.Book, below, "last", "below")
	}
	return nil
}
func queueCount(q queryer, cond string, args ...any) (int, error) {
	var n int
	err := q.QueryRow("SELECT count(*) FROM books WHERE reading_status='to-read' AND "+cond, args...).Scan(&n)
	return n, err
}

// queueNeighborPinned reports whether the queue book next to l in the given
// direction is pinned; it is not Valid when l is at that end of the queue.
func queueNeighborPinned(q queryer, cond string, l *listedBook) (sql.NullBool, error) {
	var pinned bool
	err := q.QueryRow("SELECT pinned FROM books WHERE reading_status='to-read' AND "+cond+" LIMIT 1", l.Position, l.ID).Scan(&pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullBool{}, nil
	}
	return sql.NullBool{Bool: pinned, Valid: err == nil}, err
}

// canMove mirrors bookAction's rule for swapping with a queue neighbor.
func canMove(b Book, neighborPinned sql.NullBool, end, side string) (bool, string) {
	switch {
	case b.Status != "to-read":
		return false, ""
	case b.Pinned:
		return false, "Pinned books stay in place. Unpin it to move it."
	case !neighborPinned.Valid:
		return false, "Already " + end + " in your queue."
	case neighborPinned.Bool:
		return false, "The book " + side + " is pinned."
	}
	return true, ""
}

// libraryStats is the overview row above the list.
type libraryStats struct{ Books, Queue, Pending, ActiveLoans int }

func loadStats(q queryer) (s libraryStats, err error) {
	err = q.QueryRow(`SELECT (SELECT count(*) FROM books),
 (SELECT count(*) FROM books WHERE reading_status='to-read'),
 (SELECT count(*) FROM books WHERE reading_status='to-read' AND needs_placement),
 (SELECT count(DISTINCT l.book_id) FROM loans l JOIN books b ON b.id=l.book_id WHERE l.status IN ('borrowed','ready'))`).Scan(&s.Books, &s.Queue, &s.Pending, &s.ActiveLoans)
	return s, err
}

const defaultPerPage, maxPerPage = 50, 200

// pager is one page of a list. A page past the end shows the last page, so a
// link that outlived some deletions still lands on books.
type pager struct {
	Page, PerPage, Pages, Total, From, To int
	// Self links to this page; Prev and Next to its neighbors, empty at
	// either end.
	Self, Prev, Next string
}

// newPager reads page and per_page from the request. Its links go to path
// with the non-empty keep values.
func newPager(r *http.Request, total int, path string, keep url.Values) pager {
	p := pager{Total: total, PerPage: defaultPerPage}
	if n, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && n > 0 {
		p.PerPage = min(n, maxPerPage)
	}
	p.Pages = max(1, (total+p.PerPage-1)/p.PerPage)
	p.Page = 1
	if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && n > 1 {
		p.Page = min(n, p.Pages)
	}
	if total > 0 {
		p.From, p.To = p.offset()+1, min(p.Page*p.PerPage, total)
	}
	link := func(page int) string {
		v := url.Values{}
		for key, values := range keep {
			if len(values) > 0 && values[0] != "" {
				v.Set(key, values[0])
			}
		}
		if p.PerPage != defaultPerPage {
			v.Set("per_page", strconv.Itoa(p.PerPage))
		}
		if page > 1 {
			v.Set("page", strconv.Itoa(page))
		}
		if len(v) == 0 {
			return path
		}
		return path + "?" + v.Encode()
	}
	p.Self = link(p.Page)
	if p.Page > 1 {
		p.Prev = link(p.Page - 1)
	}
	if p.Page < p.Pages {
		p.Next = link(p.Page + 1)
	}
	return p
}
func (p pager) offset() int { return (p.Page - 1) * p.PerPage }
