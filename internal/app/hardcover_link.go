package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// hcBookLink is a local book's link to a Hardcover book.
type hcBookLink struct {
	BookID          int64  `json:"book_id"`
	HardcoverBookID int64  `json:"hardcover_book_id"`
	EditionID       *int64 `json:"hardcover_edition_id,omitempty"`
	Slug            string `json:"slug,omitempty"`
	LinkedAt        string `json:"linked_at"`
	// ProgressSync is the reader's opt-in to send progress to Hardcover.
	ProgressSync bool `json:"progress_sync,omitempty"`
}

// hcAlias remembers the Hardcover id of a deleted or merged book under its
// identity, so a later pull cannot recreate or relink it wrongly.
type hcAlias struct {
	Identity        string `json:"identity"`
	HardcoverBookID int64  `json:"hardcover_book_id"`
}

func getBookLink(q queryer, bookID int64) (*hcBookLink, error) {
	var l hcBookLink
	err := q.QueryRow("SELECT book_id,hardcover_book_id,hardcover_edition_id,slug,linked_at,progress_sync FROM book_hardcover WHERE book_id=?", bookID).Scan(&l.BookID, &l.HardcoverBookID, &l.EditionID, &l.Slug, &l.LinkedAt, &l.ProgressSync)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &l, err
}

func loadBookLinks(q queryer) ([]hcBookLink, error) {
	rows, err := q.Query("SELECT book_id,hardcover_book_id,hardcover_edition_id,slug,linked_at,progress_sync FROM book_hardcover ORDER BY book_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hcBookLink{}
	for rows.Next() {
		var l hcBookLink
		if err = rows.Scan(&l.BookID, &l.HardcoverBookID, &l.EditionID, &l.Slug, &l.LinkedAt, &l.ProgressSync); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func loadHardcoverAliases(q queryer) ([]hcAlias, error) {
	rows, err := q.Query("SELECT identity,hardcover_book_id FROM hardcover_alias ORDER BY identity")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hcAlias{}
	for rows.Next() {
		var a hcAlias
		if err = rows.Scan(&a.Identity, &a.HardcoverBookID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// linkOwner names the local book that already holds a Hardcover book, other
// than except, or 0.
func linkOwner(q queryer, hardcoverID, except int64) (int64, string, error) {
	var id int64
	var title string
	err := q.QueryRow("SELECT b.id,b.title FROM book_hardcover h JOIN books b ON b.id=h.book_id WHERE h.hardcover_book_id=? AND h.book_id!=?", hardcoverID, except).Scan(&id, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, title, err
}

// saveBookLink links a book, replacing any earlier link. It reports a
// reader-facing conflict, never a constraint error, when another book holds
// the Hardcover book.
func saveBookLink(q queryer, bookID int64, c hcCandidate, at string) error {
	owner, title, err := linkOwner(q, c.BookID, bookID)
	if err != nil {
		return err
	}
	if owner != 0 {
		return conflict(fmt.Sprintf("That Hardcover book is already linked to “%s”. Unlink that book first, or merge the two books.", title))
	}
	var edition *int64
	if c.EditionID > 0 {
		edition = &c.EditionID
	}
	// A different link means a different Hardcover entry: the old history says
	// nothing about it.
	if err = resetSync(q, bookID); err != nil {
		return err
	}
	_, err = q.Exec(`INSERT INTO book_hardcover(book_id,hardcover_book_id,hardcover_edition_id,slug,linked_at) VALUES(?,?,?,?,?)
 ON CONFLICT(book_id) DO UPDATE SET hardcover_book_id=excluded.hardcover_book_id,hardcover_edition_id=excluded.hardcover_edition_id,slug=excluded.slug,linked_at=excluded.linked_at,progress_sync=0`, bookID, c.BookID, edition, truncate(c.Slug, 200), at)
	return err
}

// rememberHardcover moves a book's link into hardcover_alias when the book is
// deleted or merged away. A deleted book keeps its Hardcover id as a tombstone.
func rememberHardcover(q queryer, b Book) error {
	l, err := getBookLink(q, b.ID)
	if err != nil || l == nil {
		return err
	}
	if _, err = q.Exec("INSERT OR REPLACE INTO hardcover_alias(identity,hardcover_book_id) VALUES(?,?)", b.Identity, l.HardcoverBookID); err != nil {
		return err
	}
	_, err = q.Exec("DELETE FROM book_hardcover WHERE book_id=?", b.ID)
	return err
}

// hardcoverMergeConflict says why a merge cannot proceed: both books are
// linked to different Hardcover books. It is shown in the preview and never
// resolved silently.
func hardcoverMergeConflict(q queryer, sourceID, targetID int64) (string, error) {
	s, err := getBookLink(q, sourceID)
	if err != nil || s == nil {
		return "", err
	}
	t, err := getBookLink(q, targetID)
	if err != nil || t == nil || t.HardcoverBookID == s.HardcoverBookID {
		return "", err
	}
	return "These books are linked to different Hardcover books. Unlink one of them first, so the merge does not choose a link for you.", nil
}

// mergeHardcover keeps the target's link, else takes the source's.
func mergeHardcover(q queryer, source Book, targetID int64) error {
	l, err := getBookLink(q, source.ID)
	if err != nil || l == nil {
		return err
	}
	t, err := getBookLink(q, targetID)
	if err != nil {
		return err
	}
	if t == nil {
		_, err = q.Exec("UPDATE book_hardcover SET book_id=? WHERE book_id=?", targetID, source.ID)
		return err
	}
	return rememberHardcover(q, source)
}

// ---- lookups: ISBN through editions, text through search ----

type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		if x, e := strconv.ParseFloat(s, 64); e == nil {
			*f = flexInt(x)
			return nil
		}
		return err
	}
	*f = flexInt(n)
	return nil
}

type hcBookJSON struct {
	ID            flexInt `json:"id"`
	Title         string  `json:"title"`
	Slug          string  `json:"slug"`
	ReleaseYear   flexInt `json:"release_year"`
	Pages         flexInt `json:"pages"`
	Contributions []struct {
		Author struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"contributions"`
}

func (b hcBookJSON) candidate(edition int64) hcCandidate {
	names := []string{}
	for _, c := range b.Contributions {
		if c.Author.Name != "" {
			names = append(names, c.Author.Name)
		}
	}
	return hcCandidate{BookID: int64(b.ID), EditionID: edition, Title: truncate(b.Title, 300), Author: truncate(strings.Join(names, ", "), 300), Slug: truncate(b.Slug, 200), Year: int(b.ReleaseYear), Pages: int(b.Pages)}
}

type hcEditionJSON struct {
	ID   flexInt     `json:"id"`
	Book *hcBookJSON `json:"book"`
}

const hcBookFields = `book { id title slug release_year pages contributions { author { name } } }`

// hardcoverISBNQuery looks up several ISBNs in one request: each alias is one
// top-level field, so n ISBNs cost n requests of the budget. n is at most the
// burst of 10.
func hardcoverISBNQuery(isbns []string) string {
	var decl, body strings.Builder
	for i, isbn := range isbns {
		field := "isbn_13"
		if len(isbn) == 10 {
			field = "isbn_10"
		}
		if i > 0 {
			decl.WriteString(", ")
		}
		fmt.Fprintf(&decl, "$i%d: String!", i)
		fmt.Fprintf(&body, " e%d: editions(where: {%s: {_eq: $i%d}}, limit: 3) { id %s }\n", i, field, i, hcBookFields)
	}
	return "query NextChapterISBN(" + decl.String() + ") {\n" + body.String() + "}"
}

// hardcoverByISBN returns the candidates for each ISBN, keyed by ISBN.
func (c *hardcoverClient) byISBN(ctx context.Context, isbns []string) (map[string][]hcCandidate, error) {
	vars := map[string]any{}
	for i, isbn := range isbns {
		vars[fmt.Sprintf("i%d", i)] = isbn
	}
	var out map[string][]hcEditionJSON
	if err := c.do(ctx, hardcoverISBNQuery(isbns), vars, &out); err != nil {
		return nil, err
	}
	result := map[string][]hcCandidate{}
	for i, isbn := range isbns {
		for _, e := range out[fmt.Sprintf("e%d", i)] {
			if e.Book != nil && e.Book.ID > 0 {
				result[isbn] = append(result[isbn], e.Book.candidate(int64(e.ID)))
			}
		}
	}
	return result, nil
}

const hardcoverSearchQuery = `query NextChapterSearch($q: String!) {
 search(query: $q, query_type: "Book", per_page: 5) { results }
}`

type hcSearchResponse struct {
	Search struct {
		Results json.RawMessage `json:"results"`
	} `json:"search"`
}

// searchHits decodes the Typesense blob in search.results. It may arrive as an
// object or as a JSON string holding one, and ids may be numbers or strings.
func searchHits(raw json.RawMessage) ([]hcCandidate, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errHardcoverResponse
		}
		raw = json.RawMessage(s)
	}
	var blob struct {
		Hits []struct {
			Document struct {
				ID          flexInt  `json:"id"`
				Title       string   `json:"title"`
				Slug        string   `json:"slug"`
				AuthorNames []string `json:"author_names"`
				ReleaseYear flexInt  `json:"release_year"`
				Pages       flexInt  `json:"pages"`
			} `json:"document"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		return nil, errHardcoverResponse
	}
	out := []hcCandidate{}
	for _, h := range blob.Hits {
		d := h.Document
		if d.ID > 0 && len(out) < 10 {
			out = append(out, hcCandidate{BookID: int64(d.ID), Title: truncate(d.Title, 300), Author: truncate(strings.Join(d.AuthorNames, ", "), 300), Slug: truncate(d.Slug, 200), Year: int(d.ReleaseYear), Pages: int(d.Pages)})
		}
	}
	return out, nil
}

func (c *hardcoverClient) search(ctx context.Context, query string) ([]hcCandidate, error) {
	var out hcSearchResponse
	if err := c.do(ctx, hardcoverSearchQuery, map[string]any{"q": truncate(query, 300)}, &out); err != nil {
		return nil, err
	}
	return searchHits(out.Search.Results)
}

// ---- plans ----

// hcLinkItem is one proposed change to a book's link.
type hcLinkItem struct {
	BookID int64  `json:"book_id"`
	Title  string `json:"title"`
	Author string `json:"author"`
	ISBN   string `json:"isbn,omitempty"`
	// Action is "link" (a first link, or a relink when Current is set) or "unlink".
	Action  string       `json:"action"`
	Match   hcMatch      `json:"match"`
	Current *hcBookLink  `json:"current,omitempty"`
	Note    string       `json:"note,omitempty"`
	Chosen  *hcCandidate `json:"-"`
}

// hcLinkPlan is a previewed set of link changes. NextAfter and Remaining let
// the reader continue a batch that stopped at the request budget.
type hcLinkPlan struct {
	Items     []hcLinkItem `json:"items"`
	NextAfter int64        `json:"next_after,omitempty"`
	Remaining int          `json:"remaining,omitempty"`
	Stopped   string       `json:"stopped,omitempty"`
}

// hardcoverRunLimit bounds one batch: at most one ISBN request of this many
// fields plus one search each stays inside the burst of 10.
const hardcoverRunLimit = 5

// recoverable errors end a batch early but keep what was found.
func recoverableHardcover(err error) bool {
	var rate hardcoverRateError
	return errors.As(err, &rate) || errors.Is(err, errHardcoverTimeout) || errors.Is(err, errHardcoverUnavailable)
}

// hardcoverHTTPError turns a client error into a reader-facing response. The
// messages are fixed text; provider text never reaches them.
func hardcoverHTTPError(err error) error {
	var h httpError
	if errors.As(err, &h) {
		return err
	}
	var rate hardcoverRateError
	var scope hardcoverScopeError
	switch {
	case errors.As(err, &rate):
		return httpError{Status: http.StatusTooManyRequests, Message: err.Error(), Header: http.Header{"Retry-After": {strconv.Itoa(int(rate.RetryAfter.Seconds()) + 1)}}}
	case errors.Is(err, errHardcoverAuth), errors.As(err, &scope), errors.Is(err, errHardcoverCapacity):
		return bad(err.Error())
	}
	var he hardcoverError
	if errors.As(err, &he) {
		return httpError{Status: http.StatusBadGateway, Message: err.Error()}
	}
	return err
}

// findMatches asks Hardcover about each book: ISBN first (one request for all
// of them), then text search for the books the ISBN did not settle. It stops
// at the first recoverable error and returns what it has; processed counts
// the leading books that are fully handled. It runs no database transaction.
func (c *hardcoverClient) findMatches(ctx context.Context, books []Book) (items []hcLinkItem, stopped error, err error) {
	isbns := []string{}
	seen := map[string]bool{}
	for _, b := range books {
		if b.ISBN != "" && !seen[b.ISBN] {
			seen[b.ISBN] = true
			isbns = append(isbns, b.ISBN)
		}
	}
	byISBN := map[string][]hcCandidate{}
	if len(isbns) > 0 {
		var e error
		if byISBN, e = c.byISBN(ctx, isbns); e != nil {
			if recoverable := recoverableHardcover(e); !recoverable {
				return nil, nil, e
			}
			return nil, e, nil
		}
	}
	for _, b := range books {
		item := hcLinkItem{BookID: b.ID, Title: b.Title, Author: b.Author, ISBN: b.ISBN, Action: "link"}
		if b.ISBN != "" {
			item.Match = classifyMatch(b, "ISBN", byISBN[b.ISBN])
		}
		if item.Match.Class == "" || item.Match.Class == matchNone {
			if strings.TrimSpace(hardcoverSearchText(b)) == "" {
				item.Match = hcMatch{Class: matchNone, Key: "title and author"}
			} else {
				cands, e := c.search(ctx, hardcoverSearchText(b))
				if e != nil {
					if !recoverableHardcover(e) {
						return items, nil, e
					}
					return items, e, nil
				}
				item.Match = classifyMatch(b, "title and author", cands)
			}
		}
		items = append(items, item)
	}
	return items, nil, nil
}
