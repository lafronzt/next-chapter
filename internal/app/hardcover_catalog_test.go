package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type catMember struct {
	ID    int
	Pos   string // JSON: a number, a string, or null
	Title string
}

type catSeries struct {
	ID      int
	Name    string
	Total   int
	Pos     string
	Members []catMember
}

func catBook(id int, title, desc, pages, rating, image string, series ...catSeries) string {
	ss := []string{}
	for _, s := range series {
		ms := []string{}
		for _, m := range s.Members {
			ms = append(ms, fmt.Sprintf(`{"position":%s,"book":%s}`, m.Pos, hcBook(m.ID, m.Title, "Series Author", 2000)))
		}
		total := s.Total
		if total == 0 {
			total = len(s.Members)
		}
		ss = append(ss, fmt.Sprintf(`{"position":%s,"series":{"id":%d,"name":%q,"books_count":%d,"book_series":[%s]}}`, s.Pos, s.ID, s.Name, total, strings.Join(ms, ",")))
	}
	img := `{}`
	if image != "" {
		img = fmt.Sprintf(`{"url":%q}`, image)
	}
	return fmt.Sprintf(`{"id":%d,"title":%q,"slug":"s","description":%q,"pages":%s,"release_year":1999,"rating":%s,"ratings_count":12431,"cached_image":%s,"contributions":[{"author":{"name":"Series Author"}}],"book_series":[%s]}`, id, title, desc, pages, rating, img, strings.Join(ss, ","))
}

type catFake struct {
	mu       sync.Mutex
	queries  []string
	vars     []map[string]any
	byID     map[int]string // books_by_pk answers
	byISBN   map[string]string
	search   []string // search hit documents
	override func(query string) (int, string, bool)
}

func (f *catFake) handle(query string, vars map[string]any, n int) (int, map[string]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries, f.vars = append(f.queries, query), append(f.vars, vars)
	if f.override != nil {
		if s, b, ok := f.override(query); ok {
			return s, nil, b
		}
	}
	switch {
	case strings.Contains(query, "NextChapterCatalogISBN"):
		if b, ok := f.byISBN[fmt.Sprint(vars["isbn"])]; ok {
			return 200, nil, `{"data":{"editions":[{"book":` + b + `}]}}`
		}
		return 200, nil, `{"data":{"editions":[]}}`
	case strings.Contains(query, "NextChapterCatalog"):
		if b, ok := f.byID[int(vars["id"].(float64))]; ok {
			return 200, nil, `{"data":{"books_by_pk":` + b + `}}`
		}
		return 200, nil, `{"data":{"books_by_pk":null}}`
	case strings.Contains(query, "NextChapterSearch"):
		return 200, nil, searchReply(f.search...)
	}
	return 500, nil, `{}`
}

func (f *catFake) reqs(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.queries {
		if strings.Contains(q, name) {
			n++
		}
	}
	return n
}

// catApp makes an app with lookup and Hardcover on, no other book database,
// and one linked book per spec (a Hardcover id, or 0 for unlinked).
func catApp(t *testing.T, f *catFake, books ...Book) (*App, *browser, []int64) {
	t.Helper()
	a, b, _ := newHCApp(t, f.handle)
	linkAccount(t, a)
	a.finder = &Finder{}
	if err := a.store.Write(func(tx *sql.Tx) error {
		p := defaultPreferences
		p.Hardcover, p.BookLookup = true, true
		return setMeta(tx, "preferences", p)
	}); err != nil {
		t.Fatal(err)
	}
	ids := addBooks(t, a, books...)
	b.request("GET", "/hardcover", nil)
	return a, b, ids
}

func link(t *testing.T, a *App, id int64, hc int) {
	t.Helper()
	if _, err := a.store.DB.Exec("INSERT INTO book_hardcover(book_id,hardcover_book_id,linked_at) VALUES(?,?,'2026-01-01T00:00:00Z')", id, hc); err != nil {
		t.Fatal(err)
	}
}

func startLookup(t *testing.T, b *browser, a *App, id int64) string {
	t.Helper()
	b.request("GET", bookPath(id), nil)
	refill(a)
	return locationOf(t, b.request("POST", fmt.Sprintf("/books/%d/lookup", id), nil))
}

func TestCatalogLookupByLinkedIDStripsMarkupAndFillsOnlyEmpty(t *testing.T) {
	t.Parallel()
	f := &catFake{byID: map[int]string{601: catBook(601, "Linked", `<p>A <b>bold</b> tale.</p><script>alert(1)</script>`, "320", "4.1", "", catSeries{ID: 5, Name: "The Saga", Pos: "2", Members: []catMember{{601, "2", "Linked"}}})}}
	a, b, ids := catApp(t, f, Book{Title: "Linked", Author: "Series Author", Description: "My own words."})
	link(t, a, ids[0], 601)
	loc := startLookup(t, b, a, ids[0])
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "Hardcover") || strings.Contains(body, "<script>") || strings.Contains(body, "<b>") {
		t.Fatalf("source not shown, or markup survived:\n%s", body)
	}
	// An exact id lookup: the request carries the id, not the title or author.
	raw, _ := json.Marshal(f.vars)
	if f.reqs("NextChapterCatalog(") != 1 || strings.Contains(string(raw), "Linked") || strings.Contains(string(raw), "Series Author") {
		t.Fatalf("lookup by id: %d requests, vars %s", f.reqs("NextChapterCatalog("), raw)
	}
	expect(t, b.request("POST", loc, nil), 303)
	got := rowsOf(t, a, "SELECT description,pages,published_year,series,series_number FROM books WHERE id=?", ids[0])
	if fmt.Sprint(got) != "[[My own words. 320 1999 The Saga 2]]" {
		t.Fatalf("a typed description was replaced, or empty fields were not filled: %v", got)
	}
}

func TestCatalogLookupISBNThenSearchFallbacks(t *testing.T) {
	t.Parallel()
	f := &catFake{
		byID:   map[int]string{611: catBook(611, "By Isbn", "From the ISBN.", "100", "null", ""), 612: catBook(612, "By Search", "From the search.", "200", "null", "")},
		byISBN: map[string]string{"9780306406157": catBook(611, "By Isbn", "From the ISBN.", "100", "null", "")},
	}
	f.search = []string{searchDoc(612, "By Search")}
	a, b, ids := catApp(t, f,
		Book{Title: "By Isbn", Author: "Series Author", ISBN: "9780306406157"},
		Book{Title: "By Search", Author: "Series Author"},
		Book{Title: "ISBN misses", Author: "Series Author", ISBN: "9781111111111"})
	// ISBN hit: no search.
	body := b.request("GET", startLookup(t, b, a, ids[0]), nil).Body.String()
	if !strings.Contains(body, "From the ISBN.") || f.reqs("NextChapterSearch") != 0 {
		t.Fatalf("ISBN lookup:\n%s", body)
	}
	// No ISBN: one search, then the id.
	body = b.request("GET", startLookup(t, b, a, ids[1]), nil).Body.String()
	if !strings.Contains(body, "From the search.") || f.reqs("NextChapterSearch") != 1 {
		t.Fatalf("search fallback:\n%s", body)
	}
	// An ISBN Hardcover lacks falls back to the title search, which finds nothing for this title.
	b.request("GET", bookPath(ids[2]), nil)
	refill(a)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/lookup", ids[2]), nil), 404)
	// Two different books matching one title are ambiguous, never guessed.
	f.search = []string{searchDoc(612, "By Search"), searchDoc(613, "By Search")}
	b.request("GET", bookPath(ids[1]), nil)
	refill(a)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/lookup", ids[1]), nil), 409)
}

func TestCatalogLookupFailures(t *testing.T) {
	t.Parallel()
	f := &catFake{byID: map[int]string{}}
	a, b, ids := catApp(t, f, Book{Title: "Gone", Author: "A"})
	link(t, a, ids[0], 621)
	post := func() int {
		b.request("GET", bookPath(ids[0]), nil)
		refill(a)
		return b.request("POST", fmt.Sprintf("/books/%d/lookup", ids[0]), nil).Code
	}
	b.gateway = true
	if code := post(); code != 404 { // Hardcover has no such book
		t.Fatalf("not found: %d", code)
	}
	f.override = func(string) (int, string, bool) { return 429, `{}`, true }
	if code := post(); code != 502 {
		t.Fatalf("429: %d", code)
	}
	a.hardcover.mu.Lock()
	a.hardcover.blockedUntil = a.hardcover.blockedUntil.Add(-1e13)
	a.hardcover.mu.Unlock()
	f.override = func(string) (int, string, bool) {
		return 200, `{"data":{"books_by_pk":{"description":"` + strings.Repeat("x", 1<<21) + `"}}}`, true
	}
	if code := post(); code != 502 {
		t.Fatalf("oversized body: %d", code)
	}
	f.override = func(string) (int, string, bool) {
		return 403, `{"errors":[{"extensions":{"code":"insufficient_scope","required_scope":"read:catalog"}}]}`, true
	}
	b.request("GET", bookPath(ids[0]), nil)
	refill(a)
	rr := b.request("POST", fmt.Sprintf("/books/%d/lookup", ids[0]), nil)
	if rr.Code != 502 || !strings.Contains(rr.Body.String(), "read:catalog") {
		t.Fatalf("a missing scope is not named: %d %s", rr.Code, rr.Body)
	}
	// Hardcover failing does not fail a lookup another source answered.
	a.finder = &Finder{sources: []BookSource{stubSource{BookDetails{Description: "From elsewhere.", Pages: positive(10)}}}}
	body := b.request("GET", startLookup(t, b, a, ids[0]), nil).Body.String()
	if !strings.Contains(body, "From elsewhere.") {
		t.Fatalf("a Hardcover failure sank the lookup:\n%s", body)
	}
}

type stubSource struct{ d BookDetails }

func (stubSource) Name() string { return "Stub" }
func (s stubSource) Lookup(_ context.Context, _ LookupQuery) (BookDetails, error) {
	return s.d, nil
}

func TestCatalogLookupIsOffWithoutBothSwitches(t *testing.T) {
	t.Parallel()
	f := &catFake{byID: map[int]string{631: catBook(631, "T", "D.", "10", "null", "")}}
	a, b, ids := catApp(t, f, Book{Title: "T", Author: "A"})
	link(t, a, ids[0], 631)
	if err := a.store.Write(func(tx *sql.Tx) error {
		p := defaultPreferences
		p.BookLookup = true // Hardcover off
		if err := setMeta(tx, "preferences", p); err != nil {
			return err
		}
		return bump(tx)
	}); err != nil {
		t.Fatal(err)
	}
	b.request("GET", bookPath(ids[0]), nil)
	b.gateway = true
	refill(a)
	b.request("POST", fmt.Sprintf("/books/%d/lookup", ids[0]), nil)
	if len(f.queries) != 0 {
		t.Fatal("Hardcover was asked while it is off")
	}
}

func TestRefreshCachesSeriesWithOddPositions(t *testing.T) {
	t.Parallel()
	saga := catSeries{ID: 7, Name: "The Saga", Pos: "2", Total: 9, Members: []catMember{
		{701, "1", "First"}, {702, `"2"`, "Second"}, {703, "2.5", "Novella Between"}, {704, "null", "Omnibus"}, {706, "6", "Sixth After A Gap"}}}
	other := catSeries{ID: 8, Name: "Side Stories", Pos: "null", Members: []catMember{{702, "1", "Second"}, {709, "2", "Side Two"}}}
	f := &catFake{byID: map[int]string{702: catBook(702, "Second", "d", "300", "4.14", "https://assets.hardcover.app/books/702/cover.jpg", saga, other)}}
	a, b, ids := catApp(t, f, Book{Title: "Second", Author: "Series Author", Series: "The Saga"}, Book{Title: "First", Author: "Series Author", Series: "The Saga"})
	link(t, a, ids[0], 702)
	if _, err := a.store.DB.Exec("UPDATE books SET series_number=2 WHERE id=?", ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE books SET series_number=1 WHERE id=?", ids[1]); err != nil {
		t.Fatal(err)
	}
	link(t, a, ids[1], 701)
	order, rev := queueOrderOf(t, a), currentRevision(t, a)
	b.request("GET", bookPath(ids[0]), nil)
	refill(a)
	expect(t, b.request("POST", bookPath(ids[0])+"/hardcover/refresh", nil), 303)
	if f.reqs("NextChapterCatalog(") != 1 {
		t.Fatal("a refresh is more than one request")
	}
	if currentRevision(t, a) != rev || queueOrderOf(t, a) != order {
		t.Fatal("a refresh changed the library")
	}
	got := rowsOf(t, a, "SELECT series_id,hardcover_book_id,position FROM series_cache ORDER BY series_id,position IS NULL,position,hardcover_book_id")
	if fmt.Sprint(got) != "[[7 701 1] [7 702 2] [7 703 2.5] [7 706 6] [7 704 <nil>] [8 702 1] [8 709 2]]" && fmt.Sprint(got) != "[[7 701 1] [7 702 2] [7 703 2.5] [7 706 6] [7 704 ] [8 702 1] [8 709 2]]" {
		t.Fatalf("cache: %v", got)
	}
	// The dashboard shows what the reader lacks, from the cache, without a request and without creating a book.
	before := f.reqs("NextChapter")
	books := dbValue[int](t, a, "SELECT count(*) FROM books")
	body := b.request("GET", "/series", nil).Body.String()
	for _, want := range []string{"Not in your library", "#2.5", "Novella Between", "no number", "Omnibus", "#6", "Next volume you do not have"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	if strings.Contains(body, "Side Two") {
		t.Error("a different series of the same book was shown under this one")
	}
	if strings.Contains(body, ">Second<") && strings.Contains(body, "Sixth After A Gap</strong>") == false {
		t.Error("next missing volume is not the first after the highest owned")
	}
	if f.reqs("NextChapter") != before || dbValue[int](t, a, "SELECT count(*) FROM books") != books {
		t.Fatal("viewing the dashboard made a request or created a book")
	}
	if !strings.Contains(body, "Hardcover lists 9 books") {
		t.Error("a truncated series is not called out")
	}
	cat, err := loadCatalog(a.store.DB, ids[0])
	if err != nil || cat == nil || cat.Rating == nil || *cat.Rating != 4.14 || cat.RatingsCount != 12431 {
		t.Fatalf("catalog: %+v %v", cat, err)
	}
}

func TestSeriesAddIsPreviewedAndPlacedNeedingPlacement(t *testing.T) {
	t.Parallel()
	saga := catSeries{ID: 7, Name: "The Saga", Pos: "1", Members: []catMember{{701, "1", "First"}, {702, "2", "Second"}, {703, "3", "Third"}, {704, "4", "Deleted One"}}}
	f := &catFake{byID: map[int]string{701: catBook(701, "First", "d", "300", "4", "", saga)}}
	a, b, ids := catApp(t, f, Book{Title: "First", Author: "Series Author", Series: "The Saga"}, Book{Title: "Other Queued", Author: "X"}, Book{Title: "Second", Author: "Series Author"})
	link(t, a, ids[0], 701)
	if _, err := a.store.DB.Exec("UPDATE books SET series_number=1 WHERE id=?", ids[0]); err != nil {
		t.Fatal(err)
	}
	// "Deleted One" was deleted before: it must not come back.
	dead := addBooks(t, a, Book{Title: "Deleted One", Author: "Series Author"})[0]
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, dead) }); err != nil {
		t.Fatal(err)
	}
	b.request("GET", bookPath(ids[0]), nil)
	refill(a)
	expect(t, b.request("POST", bookPath(ids[0])+"/hardcover/refresh", nil), 303)
	body := b.request("GET", "/series", nil).Body.String()
	// "Second" is owned under the same title and author, so only Third is offered.
	if strings.Contains(body, "Deleted One") || strings.Contains(body, ">Second<") && strings.Contains(body, `value="702"`) || !strings.Contains(body, `value="703"`) {
		t.Fatalf("the unowned list is wrong:\n%s", body)
	}
	order := queueOrderOf(t, a)
	n := dbValue[int](t, a, "SELECT count(*) FROM books")
	src := strconv.FormatInt(ids[0], 10)
	expect(t, b.request("POST", "/series/add", url.Values{"source": {src}}), 400) // nothing chosen
	loc := locationOf(t, b.request("POST", "/series/add", url.Values{"source": {src}, "member": {"703", "702"}}))
	page := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(page, "Third") || strings.Contains(page, "Second") || dbValue[int](t, a, "SELECT count(*) FROM books") != n {
		t.Fatalf("the preview is wrong, or added a book:\n%s", page)
	}
	expect(t, b.request("POST", loc, url.Values{"csrf": {"bad"}}), 400)
	expect(t, b.request("POST", loc, nil), 303)
	got := rowsOf(t, a, "SELECT title,source,needs_placement,reading_status,series,series_number FROM books WHERE title='Third'")
	if fmt.Sprint(got) != "[[Third hardcover-series 1 to-read The Saga 3]]" {
		t.Fatalf("added book: %v", got)
	}
	if dbValue[int](t, a, "SELECT hardcover_book_id FROM book_hardcover WHERE book_id=(SELECT id FROM books WHERE title='Third')") != 703 {
		t.Fatal("the added book is not linked")
	}
	if !strings.HasPrefix(queueOrderOf(t, a), order) {
		t.Fatalf("the queue was reordered: %s -> %s", order, queueOrderOf(t, a))
	}
	// Applying again, or a stale preview, adds nothing.
	expect(t, b.request("POST", loc, nil), 409)
	if dbValue[int](t, a, "SELECT count(*) FROM books WHERE title='Third'") != 1 {
		t.Fatal("a duplicate was added")
	}
	// A deleted volume is not offered, so asking for it directly adds nothing.
	b.request("GET", "/series", nil)
	expect(t, b.request("POST", "/series/add", url.Values{"source": {src}, "member": {"704"}}), 400)
}

func TestCoverAndRatingSwitches(t *testing.T) {
	t.Parallel()
	f := &catFake{byID: map[int]string{
		801: catBook(801, "Covered", "d", "300", "4.1", "https://assets.hardcover.app/books/801/a.jpg"),
		802: catBook(802, "Elsewhere", "d", "300", "4.1", "https://evil.example/a.jpg"),
	}}
	a, b, ids := catApp(t, f, Book{Title: "Covered", Author: "A"}, Book{Title: "Elsewhere", Author: "A"})
	link(t, a, ids[0], 801)
	link(t, a, ids[1], 802)
	for _, id := range ids {
		b.request("GET", bookPath(id), nil)
		refill(a)
		expect(t, b.request("POST", bookPath(id)+"/hardcover/refresh", nil), 303)
	}
	if dbValue[string](t, a, "SELECT image_url FROM hardcover_catalog WHERE book_id=?", ids[1]) != "" {
		t.Fatal("a cover URL on another host was stored")
	}
	csp := func() string {
		rr := b.request("GET", bookPath(ids[0]), nil)
		return rr.Header().Get("Content-Security-Policy")
	}
	const base = "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'self'"
	page := b.request("GET", bookPath(ids[0]), nil).Body.String()
	if csp() != base || strings.Contains(page, "hc-cover") || strings.Contains(page, "Hardcover average") {
		t.Fatal("covers or the rating show while switched off, or the policy changed")
	}
	set := func(covers, rating bool) {
		if err := a.store.Write(func(tx *sql.Tx) error {
			p := defaultPreferences
			p.Hardcover, p.HardcoverCovers, p.HardcoverRating = true, covers, rating
			if err := setMeta(tx, "preferences", p); err != nil {
				return err
			}
			return bump(tx)
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(true, true)
	page = b.request("GET", bookPath(ids[0]), nil).Body.String()
	for _, want := range []string{`class="hc-cover"`, `src="https://assets.hardcover.app/books/801/a.jpg"`, `alt="Cover of Covered"`, `referrerpolicy="no-referrer"`, `loading="lazy"`, "Hardcover average 4.1 (12,431 ratings)"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if csp() != strings.Replace(base, "img-src 'self' data:", "img-src 'self' data: https://assets.hardcover.app", 1) {
		t.Fatalf("policy with covers on: %s", csp())
	}
	if strings.Contains(b.request("GET", bookPath(ids[1]), nil).Body.String(), "hc-cover") {
		t.Fatal("a book without a cover address shows an image")
	}
	// A stored address on another host is never rendered, even if it got into the table.
	if _, err := a.store.DB.Exec("UPDATE hardcover_catalog SET image_url='https://evil.example/x.jpg' WHERE book_id=?", ids[0]); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.request("GET", bookPath(ids[0]), nil).Body.String(), "evil.example") {
		t.Fatal("an address off the allowed host was rendered")
	}
	// Covers off with Hardcover on: no image, and the policy is the default again.
	set(false, true)
	if csp() != base {
		t.Fatal("the policy stayed wide with covers off")
	}
	// Hardcover off wins over everything.
	if err := a.store.Write(func(tx *sql.Tx) error {
		p := defaultPreferences
		p.HardcoverCovers, p.HardcoverRating = true, true
		return setMeta(tx, "preferences", p)
	}); err != nil {
		t.Fatal(err)
	}
	if csp() != base {
		t.Fatal("covers load while Hardcover is off")
	}
}

func TestCatalogDataNeverReachesRankingOrEstimates(t *testing.T) {
	t.Parallel()
	f := &catFake{byID: map[int]string{901: catBook(901, "Quiet", "HC-DESCRIPTION-SECRET", "300", "4.9", "", catSeries{ID: 3, Name: "HC-SERIES-SECRET", Pos: "1", Members: []catMember{{901, "1", "Quiet"}}})}}
	a, b, ids := catApp(t, f, Book{Title: "Quiet", Author: "A"}, Book{Title: "Other", Author: "B"})
	link(t, a, ids[0], 901)
	order := queueOrderOf(t, a)
	b.request("GET", bookPath(ids[0]), nil)
	refill(a)
	expect(t, b.request("POST", bookPath(ids[0])+"/hardcover/refresh", nil), 303)
	if queueOrderOf(t, a) != order {
		t.Fatal("a refresh reordered the queue")
	}
	bk, err := getBook(a.store.DB, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(buildEstimatePayload(bk, nil, ratingBaseline{}, false))
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "4.9") {
		t.Fatalf("Hardcover data reached an estimate payload: %s", raw)
	}
	// A lookup the reader then applies is the book's own description, and only then.
	lk := startLookup(t, b, a, ids[0])
	b.request("GET", lk, nil)
	expect(t, b.request("POST", lk, nil), 303)
	bk, _ = getBook(a.store.DB, ids[0])
	raw, _ = json.Marshal(buildEstimatePayload(bk, nil, ratingBaseline{}, false))
	if !strings.Contains(string(raw), "HC-DESCRIPTION-SECRET") {
		t.Fatal("a description the reader applied is not the book's own")
	}
	if strings.Contains(string(raw), "4.9") {
		t.Fatal("the community rating reached the payload")
	}
}

func TestCatalogMigrationCleanupAndPosts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:21]); err != nil {
		t.Fatal(err)
	}
	if err := migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[int](t, path, "SELECT count(*) FROM hardcover_catalog")+scalar[int](t, path, "SELECT count(*) FROM series_cache") != 0 {
		t.Fatal("existing books have cached Hardcover data")
	}
	f := &catFake{byID: map[int]string{950: catBook(950, "Cleanup", "d", "10", "3", "", catSeries{ID: 1, Name: "S", Pos: "1", Members: []catMember{{950, "1", "Cleanup"}, {951, "2", "Next"}}})}}
	a, b, ids := catApp(t, f, Book{Title: "Cleanup", Author: "Series Author", Series: "S"})
	link(t, a, ids[0], 950)
	b.request("GET", bookPath(ids[0]), nil)
	refill(a)
	expect(t, b.request("POST", bookPath(ids[0])+"/hardcover/refresh", nil), 303)
	if dbValue[int](t, a, "SELECT count(*)+(SELECT count(*) FROM series_cache) FROM hardcover_catalog") != 3 {
		t.Fatal("nothing cached")
	}
	// The cache is not exported: it is rebuilt on request.
	var data []byte
	if err := a.store.Write(func(tx *sql.Tx) error {
		x, e := exportLibrary(tx)
		if e != nil {
			return e
		}
		data, e = json.Marshal(x)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "series_cache") || strings.Contains(string(data), "Cleanup\",\"author\":\"Series Author\",\"year\"") {
		t.Fatal("the cache was exported")
	}
	// CSRF and a stale form on the new POSTs.
	for _, p := range []string{bookPath(ids[0]) + "/hardcover/refresh", "/series/add"} {
		expect(t, b.request("POST", p, url.Values{"csrf": {"bad"}}), 400)
		expect(t, b.request("POST", p, url.Values{"revision": {"999999"}, "source": {strconv.FormatInt(ids[0], 10)}, "member": {"951"}}), 409)
	}
	// Unlinking clears the cache; so does deleting the book.
	if err := a.store.Write(func(tx *sql.Tx) error { return resetSync(tx, ids[0]) }); err != nil {
		t.Fatal(err)
	}
	if dbValue[int](t, a, "SELECT count(*)+(SELECT count(*) FROM series_cache) FROM hardcover_catalog") != 0 {
		t.Fatal("the cache outlived the link")
	}
	if err := a.store.Write(func(tx *sql.Tx) error { return storeCatalog(tx, ids[0], hcCatalogBook{ID: 950}, "t") }); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, ids[0]) }); err != nil {
		t.Fatal(err)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM hardcover_catalog") != 0 {
		t.Fatal("deleting a book left its cache")
	}
}

func searchDoc(id int, title string) string {
	return fmt.Sprintf(`{"id":%d,"title":%q,"author_names":["Series Author"],"release_year":1999}`, id, title)
}
