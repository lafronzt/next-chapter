package app

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// bookColumns are the books columns scanBook reads, in order. Queries alias
// the table as b.
const bookColumns = "b.id,b.title,b.author,b.identity,b.isbn,b.goodreads_id,b.pages,b.tags,b.interest,b.reading_status,b.position,b.pinned,b.needs_placement,b.series,b.series_number,b.published_year,b.avg_rating,b.added_at,b.source"

func scanBook(rows *sql.Rows, extra ...any) (Book, error) {
	b := Book{Loans: []Loan{}}
	dest := append([]any{&b.ID, &b.Title, &b.Author, &b.Identity, &b.ISBN, &b.GoodreadsID, &b.Pages, &b.Tags, &b.Interest, &b.Status, &b.Position, &b.Pinned, &b.NeedsPlacement, &b.Series, &b.SeriesNumber, &b.PublishedYear, &b.AvgRating, &b.AddedAt, &b.Source}, extra...)
	return b, rows.Scan(dest...)
}

// queryBooks runs SELECT bookColumns FROM books b <tail> and attaches each
// book's loans.
func queryBooks(q queryer, tail string, args ...any) ([]Book, error) {
	rows, err := q.Query("SELECT "+bookColumns+" FROM books b "+tail, args...)
	if err != nil {
		return nil, err
	}
	result := []Book{}
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, b)
	}
	err = rows.Err()
	// The store has one connection: close these rows before the loan query.
	rows.Close()
	if err != nil {
		return nil, err
	}
	return result, attachLoans(q, result)
}

// jsonIDs encodes ids for json_each(?), which avoids SQLite's limit on bound
// parameters for long IN lists.
func jsonIDs(ids []int64) string {
	raw, _ := json.Marshal(ids)
	return string(raw)
}

// attachLoans loads the loans of the given books, newest first.
func attachLoans(q queryer, books []Book) error {
	if len(books) == 0 {
		return nil
	}
	ids := make([]int64, len(books))
	lookup := map[int64]int{}
	for i, b := range books {
		ids[i] = b.ID
		lookup[b.ID] = i
	}
	rows, err := q.Query("SELECT book_id,source,format,status,due_date,observed_at FROM loans WHERE book_id IN (SELECT value FROM json_each(?)) ORDER BY observed_at DESC", jsonIDs(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l Loan
		if err = rows.Scan(&l.BookID, &l.Source, &l.Format, &l.Status, &l.DueDate, &l.ObservedAt); err != nil {
			return err
		}
		i := lookup[l.BookID]
		books[i].Loans = append(books[i].Loans, l)
	}
	return rows.Err()
}

// loadBooks returns every book in list order. Use it only when the whole
// library is needed, such as for an export.
func loadBooks(q queryer) ([]Book, error) {
	return queryBooks(q, "ORDER BY b.position,b.id")
}

// loadQueue returns the to-read books in queue order.
func loadQueue(q queryer) ([]Book, error) {
	return queryBooks(q, "WHERE b.reading_status='to-read' ORDER BY b.position,b.id")
}

// getBook returns one book with its loans, or sql.ErrNoRows.
func getBook(q queryer, id int64) (Book, error) {
	books, err := queryBooks(q, "WHERE b.id=?", id)
	if err != nil {
		return Book{}, err
	}
	if len(books) == 0 {
		return Book{}, sql.ErrNoRows
	}
	return books[0], nil
}

// getBooks returns the books with the given IDs that still exist, by ID.
func getBooks(q queryer, ids []int64) (map[int64]Book, error) {
	books, err := queryBooks(q, "WHERE b.id IN (SELECT value FROM json_each(?))", jsonIDs(ids))
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]Book, len(books))
	for _, b := range books {
		byID[b.ID] = b
	}
	return byID, nil
}

// queueEntry is the part of a queued book that reordering needs.
type queueEntry struct {
	ID             int64
	Pinned         bool
	NeedsPlacement bool
}

// queueOrder lists the queue without loading whole books.
func queueOrder(q queryer) ([]queueEntry, error) {
	rows, err := q.Query("SELECT id,pinned,needs_placement FROM books WHERE reading_status='to-read' ORDER BY position,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []queueEntry{}
	for rows.Next() {
		var e queueEntry
		if err = rows.Scan(&e.ID, &e.Pinned, &e.NeedsPlacement); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// reorder gives the books in ids the positions 0..n-1, in order. It writes only
// rows whose position changes, so once the queue is numbered compactly,
// moving a book one place updates two rows.
func reorder(q queryer, ids []int64) error {
	rows, err := q.Query("SELECT b.id,b.position FROM books b JOIN json_each(?) j ON j.value=b.id", jsonIDs(ids))
	if err != nil {
		return err
	}
	current := make(map[int64]int, len(ids))
	for rows.Next() {
		var id int64
		var pos int
		if err = rows.Scan(&id, &pos); err != nil {
			rows.Close()
			return err
		}
		current[id] = pos
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	update, err := q.Prepare("UPDATE books SET position=? WHERE id=?")
	if err != nil {
		return err
	}
	defer update.Close()
	for pos, id := range ids {
		if old, ok := current[id]; ok && old == pos {
			continue
		}
		if _, err := update.Exec(pos, id); err != nil {
			return err
		}
	}
	return nil
}

// eventFilter narrows library events to one book, source, or status; empty
// fields match everything.
type eventFilter struct {
	BookID         int64
	Source, Status string
}

func (f eventFilter) where() (string, []any) {
	conds, args := []string{}, []any{}
	if f.BookID != 0 {
		conds, args = append(conds, "e.book_id=?"), append(args, f.BookID)
	}
	if f.Source != "" {
		conds, args = append(conds, "e.source=?"), append(args, f.Source)
	}
	if f.Status != "" {
		conds, args = append(conds, "e.status=?"), append(args, f.Status)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// loadEvents returns a book's events (every event when bid is 0), newest
// first, at most limit of them when limit is positive.
func loadEvents(q queryer, bid int64, limit int) ([]Event, error) {
	return pageEvents(q, eventFilter{BookID: bid}, limit, 0)
}
func pageEvents(q queryer, f eventFilter, limit, offset int) ([]Event, error) {
	where, args := f.where()
	statement := `SELECT e.id,e.book_id,e.source,e.format,e.status,e.activity,e.at,e.due_date,e.fingerprint,b.title,b.author FROM events e JOIN books b ON b.id=e.book_id` + where + " ORDER BY e.at DESC,e.id DESC"
	if limit > 0 {
		statement += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	}
	rows, err := q.Query(statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.ID, &e.BookID, &e.Source, &e.Format, &e.Status, &e.Activity, &e.At, &e.DueDate, &e.Fingerprint, &e.Title, &e.Author); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}
func countEvents(q queryer, f eventFilter) (int, error) {
	where, args := f.where()
	var n int
	err := q.QueryRow("SELECT count(*) FROM events e"+where, args...).Scan(&n)
	return n, err
}

// eventSources lists the sources that have recorded events, for the history
// filter.
func eventSources(q queryer) ([]string, error) {
	rows, err := q.Query("SELECT DISTINCT source FROM events ORDER BY source")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}
