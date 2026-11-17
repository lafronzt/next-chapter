package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hcFake is a synthetic Hardcover server. handle sees the GraphQL query text,
// its variables, and the 1-based request number, and returns a status, extra
// headers, and a JSON body.
type hcFake struct {
	mu       sync.Mutex
	requests []string
	costs    []int
	handle   func(query string, vars map[string]any, n int) (int, map[string]string, string)
}

func newHCApp(t *testing.T, handle func(query string, vars map[string]any, n int) (int, map[string]string, string)) (*App, *browser, *hcFake) {
	t.Helper()
	f := &hcFake{handle: handle}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, body.Query)
		cost, _ := hardcoverCost(body.Query)
		f.costs = append(f.costs, cost)
		n := len(f.requests)
		f.mu.Unlock()
		status, header, out := f.handle(body.Query, body.Variables, n)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		w.Write([]byte(out))
	}))
	t.Cleanup(s.Close)
	a := testApp(t, true)
	a.hardcover = newHardcoverClient(HardcoverConfig{Token: "secret"}, nil, s.URL)
	// A loaded machine must not turn a slow fake answer into a search timeout.
	a.hardcover.searchTimeout = time.Minute
	if err := a.store.Write(func(tx *sql.Tx) error {
		p := defaultPreferences
		p.Hardcover = true
		return setMeta(tx, "preferences", p)
	}); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/hardcover", nil), 200)
	return a, b, f
}

func (f *hcFake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.requests) }

func addBooks(t *testing.T, a *App, books ...Book) []int64 {
	t.Helper()
	recs := []Record{}
	for _, b := range books {
		b.Status, b.Source, b.Interest = "to-read", "manual", 3
		recs = append(recs, Record{Book: b})
	}
	addRecords(t, a, recs)
	out := []int64{}
	for _, b := range books {
		var id int64
		if err := a.store.DB.QueryRow("SELECT id FROM books WHERE title=?", b.Title).Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func hcBook(id int, title, author string, year int) string {
	return fmt.Sprintf(`{"id":%d,"title":%q,"slug":"book-%d","release_year":%d,"pages":300,"contributions":[{"author":{"name":%q}}]}`, id, title, id, year, author)
}

// isbnReply answers a NextChapterISBN query from a map of ISBN to editions.
func isbnReply(vars map[string]any, known map[string][]string) string {
	parts := []string{}
	for i := 0; ; i++ {
		v, ok := vars[fmt.Sprintf("i%d", i)]
		if !ok {
			break
		}
		editions := []string{}
		for j, book := range known[v.(string)] {
			editions = append(editions, fmt.Sprintf(`{"id":%d,"book":%s}`, 900+j, book))
		}
		parts = append(parts, fmt.Sprintf(`"e%d":[%s]`, i, strings.Join(editions, ",")))
	}
	return `{"data":{` + strings.Join(parts, ",") + `}}`
}

func searchReply(hits ...string) string {
	docs := []string{}
	for _, h := range hits {
		docs = append(docs, `{"document":`+h+`}`)
	}
	return `{"data":{"search":{"results":{"hits":[` + strings.Join(docs, ",") + `]}}}}`
}

func locationOf(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	expect(t, rr, 303)
	return rr.Header().Get("Location")
}

func TestClassifyMatch(t *testing.T) {
	t.Parallel()
	year := 1999
	local := Book{Title: "The Test Book", Author: "A. Writer", PublishedYear: &year}
	same := hcCandidate{BookID: 1, Title: "The Test Book", Author: "A. Writer", Year: 1999}
	other := hcCandidate{BookID: 2, Title: "Another", Author: "B. Writer"}
	for _, c := range []struct {
		name  string
		key   string
		cands []hcCandidate
		class string
		first int64
	}{
		{"isbn one book", "ISBN", []hcCandidate{same}, matchConfident, 1},
		{"isbn two editions of one book", "ISBN", []hcCandidate{same, same}, matchConfident, 1},
		{"isbn two books", "ISBN", []hcCandidate{other, same}, matchAmbiguous, 1},
		{"search never confident", "title and author", []hcCandidate{same}, matchAmbiguous, 1},
		{"none", "ISBN", nil, matchNone, 0},
	} {
		got := classifyMatch(local, c.key, append([]hcCandidate(nil), c.cands...))
		if got.Class != c.class || (c.first != 0 && got.Candidates[0].BookID != c.first) {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

func TestHardcoverISBNQueryCost(t *testing.T) {
	t.Parallel()
	q := hardcoverISBNQuery([]string{"9780000000002", "0306406152", "9780000000019"})
	if cost, search := hardcoverCost(q); cost != 3 || search {
		t.Fatal(cost, search)
	}
	if !strings.Contains(q, "isbn_10") || !strings.Contains(q, "isbn_13") {
		t.Fatal(q)
	}
}

func TestHardcoverLinkByISBNThenApply(t *testing.T) {
	t.Parallel()
	a, b, fake := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		if !strings.Contains(q, "NextChapterISBN") {
			t.Errorf("unexpected query %s", q)
		}
		return 200, nil, isbnReply(vars, map[string][]string{"9780000000002": {hcBook(77, "Exact", "Author", 2001)}})
	})
	ids := addBooks(t, a, Book{Title: "Exact", Author: "Author", ISBN: "9780000000002"}, Book{Title: "Other", Author: "Author"})
	queue := queueOrderOf(t, a)
	expect(t, b.request("GET", fmt.Sprintf("/books/%d", ids[0]), nil), 200)
	loc := locationOf(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover", ids[0]), nil))
	if fake.count() != 1 {
		t.Fatal(fake.count())
	}
	rr := b.request("GET", loc, nil)
	expect(t, rr, 200)
	if body := rr.Body.String(); !strings.Contains(body, "Exact") || !strings.Contains(body, "ISBN") || !strings.Contains(body, `value="77" `) || !strings.Contains(body, "checked") {
		t.Fatal(body)
	}
	if n := scalarDB[int](t, a, "SELECT count(*) FROM book_hardcover"); n != 0 {
		t.Fatal("linked before confirming")
	}
	expect(t, b.request("POST", loc, url.Values{fmt.Sprintf("choice_%d", ids[0]): {"77"}}), 303)
	l, err := getBookLink(a.store.DB, ids[0])
	if err != nil || l == nil || l.HardcoverBookID != 77 || l.EditionID == nil || *l.EditionID != 900 {
		t.Fatal(l, err)
	}
	if queueOrderOf(t, a) != queue {
		t.Fatal("linking changed the queue")
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM plans WHERE kind='hc-link'") != 0 {
		t.Fatal("applied plan was kept")
	}
}

func TestHardcoverSearchFallbackAndAmbiguous(t *testing.T) {
	t.Parallel()
	a, b, fake := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		if strings.Contains(q, "NextChapterISBN") {
			return 200, nil, isbnReply(vars, nil)
		}
		if got := vars["q"]; got != "Fallback Author" {
			t.Errorf("search sent %v", got)
		}
		return 200, nil, searchReply(`{"id":"5","title":"Fallback","author_names":["Author"],"release_year":2010,"pages":"250","slug":"f"}`, `{"id":6,"title":"Fallback II","author_names":["Author"]}`)
	})
	ids := addBooks(t, a, Book{Title: "Fallback", Author: "Author", ISBN: "9780000000002", Reason: "private reason never sent", Tags: "secret-tag"})
	loc := locationOf(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover", ids[0]), nil))
	if fake.count() != 2 {
		t.Fatal(fake.requests)
	}
	for _, q := range fake.requests {
		if strings.Contains(q, "private reason") || strings.Contains(q, "secret-tag") {
			t.Fatal("private data sent")
		}
	}
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "Pick the right book") || strings.Contains(body, `value="5" checked`) || strings.Count(body, `type="radio"`) != 3 {
		t.Fatal(body)
	}
	// Skipping saves nothing.
	expect(t, b.request("POST", loc, nil), 303)
	if scalarDB[int](t, a, "SELECT count(*) FROM book_hardcover") != 0 {
		t.Fatal("skip linked")
	}
}

func TestHardcoverNoResults(t *testing.T) {
	t.Parallel()
	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, searchReply()
	})
	ids := addBooks(t, a, Book{Title: "Nothing", Author: "Nobody"})
	loc := locationOf(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover", ids[0]), nil))
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "No Hardcover match") {
		t.Fatal(body)
	}
}

func TestHardcoverBatchStopsOnRateLimitAndResumes(t *testing.T) {
	t.Parallel()
	a, b, fake := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		if n == 3 {
			return 429, map[string]string{"Retry-After": "30"}, `{}`
		}
		return 200, nil, searchReply(`{"id":` + fmt.Sprint(100+n) + `,"title":"X","author_names":["A"]}`)
	})
	addBooks(t, a, Book{Title: "B1", Author: "A"}, Book{Title: "B2", Author: "A"}, Book{Title: "B3", Author: "A"}, Book{Title: "B4", Author: "A"}, Book{Title: "B5", Author: "A"}, Book{Title: "B6", Author: "A"}, Book{Title: "B7", Author: "A"})
	loc := locationOf(t, b.request("POST", "/hardcover/match", nil))
	body := b.request("GET", loc, nil).Body.String()
	if fake.count() != 3 {
		t.Fatalf("requests after 429: %d", fake.count())
	}
	if !strings.Contains(body, "rate limit") || !strings.Contains(body, "B1") || !strings.Contains(body, "B2") || strings.Contains(body, "B3") {
		t.Fatal(body)
	}
	for _, c := range fake.costs {
		if c > 10 {
			t.Fatal("request over burst", c)
		}
	}
	// The cursor sits after B2, and a later run reaches the rest. The client
	// stays blocked until Retry-After passes, so no request is made yet.
	rr := b.request("POST", "/hardcover/match", url.Values{"after": {"2"}})
	expect(t, rr, 429)
	if fake.count() != 3 {
		t.Fatal("request while blocked")
	}
}

func TestHardcoverBatchBounded(t *testing.T) {
	t.Parallel()
	a, b, fake := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		if strings.Contains(q, "NextChapterISBN") {
			return 200, nil, isbnReply(vars, nil)
		}
		return 200, nil, searchReply()
	})
	books := []Book{}
	for i := 1; i <= 8; i++ {
		books = append(books, Book{Title: fmt.Sprintf("Bounded %d", i), Author: "A", ISBN: fmt.Sprintf("97800000000%02d", i)})
	}
	addBooks(t, a, books...)
	loc := locationOf(t, b.request("POST", "/hardcover/match", nil))
	if fake.count() > hardcoverRunLimit+1 {
		t.Fatal("too many requests", fake.count())
	}
	total := 0
	for _, c := range fake.costs {
		total += c
	}
	if total > 2*hardcoverRunLimit {
		t.Fatal("cost over run limit", total)
	}
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "3 more books") {
		t.Fatal(body)
	}
}

func TestHardcoverLinkConflictIsShownNotFailed(t *testing.T) {
	t.Parallel()
	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, searchReply(`{"id":42,"title":"Shared","author_names":["Author"]}`)
	})
	ids := addBooks(t, a, Book{Title: "Holder", Author: "Author"}, Book{Title: "Claimant", Author: "Author"})
	if err := a.store.Write(func(tx *sql.Tx) error {
		return saveBookLink(tx, ids[0], hcCandidate{BookID: 42}, "2026-01-01T00:00:00Z")
	}); err != nil {
		t.Fatal(err)
	}
	loc := locationOf(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover", ids[1]), nil))
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "Already linked to “Holder”") || !strings.Contains(body, "disabled") {
		t.Fatal(body)
	}
	// Forcing the choice is a clear conflict, not a 500.
	rr := b.request("POST", loc, url.Values{fmt.Sprintf("choice_%d", ids[1]): {"42"}})
	expect(t, rr, 409)
	if !strings.Contains(rr.Body.String(), "already linked") {
		t.Fatal(rr.Body)
	}
	if err := a.store.Write(func(tx *sql.Tx) error {
		return saveBookLink(tx, ids[1], hcCandidate{BookID: 42}, "x")
	}); err == nil {
		t.Fatal("expected a conflict")
	}
}

func TestHardcoverLinkRejectsForgedChoiceAndStalePlan(t *testing.T) {
	t.Parallel()
	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, searchReply(`{"id":8,"title":"T","author_names":["A"]}`)
	})
	ids := addBooks(t, a, Book{Title: "T", Author: "A"})
	loc := locationOf(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover", ids[0]), nil))
	b.request("GET", loc, nil)
	expect(t, b.request("POST", loc, url.Values{fmt.Sprintf("choice_%d", ids[0]): {"999"}}), 400)
	expect(t, b.request("POST", loc, url.Values{"csrf": {"bad"}}), 400)
	if err := a.store.Write(func(tx *sql.Tx) error { return bump(tx) }); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", loc, url.Values{fmt.Sprintf("choice_%d", ids[0]): {"8"}}), 409)
	if scalarDB[int](t, a, "SELECT count(*) FROM book_hardcover") != 0 {
		t.Fatal("stale plan linked")
	}
	expect(t, b.request("POST", "/books/"+fmt.Sprint(ids[0])+"/hardcover", url.Values{"revision": {"-1"}}), 409)
}

func TestHardcoverUnlinkAndRelink(t *testing.T) {
	t.Parallel()
	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, searchReply(`{"id":9,"title":"R","author_names":["A"]}`)
	})
	ids := addBooks(t, a, Book{Title: "R", Author: "A"})
	if err := a.store.Write(func(tx *sql.Tx) error { return saveBookLink(tx, ids[0], hcCandidate{BookID: 5}, "t") }); err != nil {
		t.Fatal(err)
	}
	b.request("GET", fmt.Sprintf("/books/%d", ids[0]), nil)
	loc := locationOf(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover", ids[0]), nil))
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "resets the sync history") {
		t.Fatal(body)
	}
	expect(t, b.request("POST", loc, url.Values{fmt.Sprintf("choice_%d", ids[0]): {"9"}}), 303)
	if l, _ := getBookLink(a.store.DB, ids[0]); l == nil || l.HardcoverBookID != 9 {
		t.Fatal(l)
	}
	b.request("GET", fmt.Sprintf("/books/%d", ids[0]), nil)
	loc = locationOf(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover/unlink", ids[0]), nil))
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "Remove the link") {
		t.Fatal(body)
	}
	expect(t, b.request("POST", loc, nil), 303)
	if l, _ := getBookLink(a.store.DB, ids[0]); l != nil {
		t.Fatal("still linked")
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover/unlink", ids[0]), nil), 400)
}

func TestHardcoverLinkingOffMakesNoRequests(t *testing.T) {
	t.Parallel()
	a, b, fake := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		t.Error("request while off")
		return 500, nil, ``
	})
	ids := addBooks(t, a, Book{Title: "Off", Author: "A"})
	if err := a.store.Write(func(tx *sql.Tx) error { return setMeta(tx, "preferences", defaultPreferences) }); err != nil {
		t.Fatal(err)
	}
	b.request("GET", "/hardcover", nil)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/hardcover", ids[0]), nil), 400)
	expect(t, b.request("POST", "/hardcover/match", nil), 400)
	if fake.count() != 0 {
		t.Fatal("requests made")
	}
}

func TestHardcoverMergeAndDeleteKeepLinks(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	ids := addBooks(t, a, Book{Title: "Keep", Author: "A"}, Book{Title: "Drop", Author: "A"}, Book{Title: "Other", Author: "A"}, Book{Title: "Gone", Author: "A"})
	link := func(id, hc int64) {
		if err := a.store.Write(func(tx *sql.Tx) error { return saveBookLink(tx, id, hcCandidate{BookID: hc}, "t") }); err != nil {
			t.Fatal(err)
		}
	}
	// Source's link moves to an unlinked target.
	link(ids[1], 11)
	if err := a.store.Write(func(tx *sql.Tx) error { _, e := mergeBooks(tx, ids[1], ids[0], true); return e }); err != nil {
		t.Fatal(err)
	}
	if l, _ := getBookLink(a.store.DB, ids[0]); l == nil || l.HardcoverBookID != 11 {
		t.Fatal("merge lost the link", l)
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM book_hardcover") != 1 {
		t.Fatal("source link kept")
	}
	// Two different links conflict in the preview and on apply.
	link(ids[2], 12)
	var p mergePreview
	if err := a.store.Write(func(tx *sql.Tx) (e error) { p, e = mergeBooks(tx, ids[2], ids[0], false); return }); err != nil || !strings.Contains(p.Refusal, "different Hardcover books") {
		t.Fatal(p.Refusal, err)
	}
	if err := a.store.Write(func(tx *sql.Tx) error { _, e := mergeBooks(tx, ids[2], ids[0], true); return e }); err == nil {
		t.Fatal("merge should be refused")
	}
	// Delete keeps the Hardcover id with the tombstone.
	link(ids[3], 13)
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, ids[3]) }); err != nil {
		t.Fatal(err)
	}
	var hc int64
	if err := a.store.DB.QueryRow("SELECT hardcover_book_id FROM hardcover_alias WHERE identity=?", identity("Gone", "A")).Scan(&hc); err != nil || hc != 13 {
		t.Fatal(hc, err)
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM book_hardcover WHERE hardcover_book_id=13") != 0 {
		t.Fatal("deleted book still linked")
	}
}

func TestHardcoverLinksMigrationAndRestore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:17]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source) VALUES(1,'Old','Author','old|author',0,'2026-01-01T00:00:00Z','manual')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[int](t, path, "SELECT count(*) FROM books") != 1 || scalar[int](t, path, "SELECT count(*) FROM book_hardcover") != 0 {
		t.Fatal("migration changed books")
	}
	if found := backups(t, path); len(found) != 1 || found[0] != path+".pre-v18.bak" {
		t.Fatal(found)
	}

	a := testApp(t, true)
	ids := addBooks(t, a, Book{Title: "Linked", Author: "A"}, Book{Title: "Deleted", Author: "A"})
	for i, hc := range []int64{31, 32} {
		hc := hc
		if err = a.store.Write(func(tx *sql.Tx) error {
			return saveBookLink(tx, ids[i], hcCandidate{BookID: hc, EditionID: 7, Slug: "s"}, "t")
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, ids[1]) }); err != nil {
		t.Fatal(err)
	}
	var data []byte
	if err = a.store.Write(func(tx *sql.Tx) error {
		f, e := exportLibrary(tx)
		if e != nil {
			return e
		}
		data, e = json.Marshal(f)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	f, problems, err := decodeExport(data)
	if err != nil || len(problems) > 0 || len(f.HardcoverBooks) != 1 || len(f.HardcoverAliases) != 1 {
		t.Fatal(err, problems, string(data))
	}
	target := testApp(t, true)
	if err = target.store.Write(func(tx *sql.Tx) error { _, e := restoreExport(tx, f); return e }); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err = target.store.DB.QueryRow("SELECT book_id FROM book_hardcover WHERE hardcover_book_id=31").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if scalarDB[int](t, target, "SELECT count(*) FROM hardcover_alias WHERE hardcover_book_id=32") != 1 {
		t.Fatal("alias not restored")
	}
	// An export from before links existed still restores.
	old := f
	old.SchemaVersion, old.HardcoverBooks, old.HardcoverAliases = 17, nil, nil
	raw, _ := json.Marshal(old)
	if _, problems, err = decodeExport(raw); err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	// A duplicate Hardcover id in a file is a problem, not a database error.
	bad := f
	bad.HardcoverBooks = append(append([]hcBookLink(nil), f.HardcoverBooks...), f.HardcoverBooks[0])
	raw, _ = json.Marshal(bad)
	if _, problems, _ = decodeExport(raw); len(problems) == 0 {
		t.Fatal("duplicate link accepted")
	}
}
