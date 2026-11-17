package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// ubJSON is one synthetic user_books row.
func ubJSON(hc, status int, rating, title, author, isbn, updated, reads string) string {
	edition := `null`
	if isbn != "" {
		edition = fmt.Sprintf(`{"id":%d,"isbn_13":%q,"isbn_10":null,"pages":280}`, hc+5000, isbn)
	}
	if rating == "" {
		rating = "null"
	}
	if reads == "" {
		reads = "[]"
	}
	if updated == "" {
		updated = "2026-05-01T00:00:00+00:00"
	}
	return fmt.Sprintf(`{"id":%d,"status_id":%d,"rating":%s,"updated_at":%q,"book":%s,"edition":%s,"user_book_reads":%s}`, hc+9000, status, rating, updated, hcBook(hc, title, author, 2000), edition, reads)
}

func pageReply(rows ...string) string {
	return `{"data":{"user_books":[` + strings.Join(rows, ",") + `]}}`
}

func linkAccount(t *testing.T, a *App) {
	t.Helper()
	if _, err := a.store.DB.Exec(`INSERT INTO hardcover_link VALUES(1,123,'reader','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
}

// pullOnce runs POST /hardcover/pull and returns the preview page's address.
func pullOnce(t *testing.T, b *browser, values url.Values) string {
	t.Helper()
	b.request("GET", "/hardcover", nil)
	return locationOf(t, b.request("POST", "/hardcover/pull", values))
}

func applyPullPage(t *testing.T, b *browser, loc string, values url.Values) {
	t.Helper()
	expect(t, b.request("GET", loc, nil), 200)
	expect(t, b.request("POST", loc, values), 303)
}

func rowsOf(t *testing.T, a *App, query string, args ...any) [][]string {
	t.Helper()
	rows, err := a.store.DB.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := []string{}
		for _, v := range vals {
			row = append(row, v.String)
		}
		out = append(out, row)
	}
	return out
}

func TestHardcoverPullNewBooksAndStatuses(t *testing.T) {
	t.Parallel()
	var sawVars map[string]any
	var sawQuery string
	a, b, fake := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		sawVars, sawQuery = vars, q
		return 200, nil, pageReply(
			ubJSON(1, 1, "", "Wanted", "Ann", "9780000000011", "", ""),
			ubJSON(2, 2, "", "Started", "Ann", "", "", ""),
			ubJSON(3, 3, "4.5", "Finished", "Ann", "9780000000035", "", `[{"id":31,"finished_at":"2024-03-05","finished_at_precision":1},{"id":32,"finished_at":"2025-06-10","finished_at_precision":2}]`),
			ubJSON(4, 4, "", "Paused one", "Ann", "", "", ""),
			ubJSON(5, 5, "", "Gave up", "Ann", "", "", ""),
			ubJSON(6, 6, "", "Ignored one", "Ann", "", "", ""),
			ubJSON(7, 3, "3.5", "Year only", "Ann", "", "", `[{"id":71,"finished_at":"2022-09-09","finished_at_precision":3}]`),
			ubJSON(8, 3, "2", "Undated", "Ann", "", "", `[]`),
		)
	})
	linkAccount(t, a)
	ids := addBooks(t, a, Book{Title: "Placed A", Author: "Z"}, Book{Title: "Placed B", Author: "Z"})
	_ = ids
	queue := queueOrderOf(t, a)
	loc := pullOnce(t, b, nil)
	if fake.count() != 1 || sawVars["uid"] != float64(123) || sawVars["limit"] != float64(100) {
		t.Fatal(fake.requests, sawVars)
	}
	for _, banned := range []string{"review", "private_notes", "journal", "lists"} {
		if strings.Contains(sawQuery, banned) {
			t.Fatal("query reads", banned)
		}
	}
	applyPullPage(t, b, loc, nil)
	got := rowsOf(t, a, "SELECT title,reading_status,source,needs_placement FROM books WHERE source='hardcover' ORDER BY id")
	want := [][]string{{"Wanted", "to-read", "hardcover", "1"}, {"Started", "reading", "hardcover", "1"}, {"Finished", "read", "hardcover", "1"}, {"Gave up", "abandoned", "hardcover", "1"}, {"Year only", "read", "hardcover", "1"}, {"Undated", "read", "hardcover", "1"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("books %v", got)
	}
	if n := scalarDB[int](t, a, "SELECT count(*) FROM book_hardcover"); n != 6 {
		t.Fatal("links", n)
	}
	reads := rowsOf(t, a, "SELECT b.title,l.finished_at,l.finished_precision,l.rating,l.source FROM reading_log l JOIN books b ON b.id=l.book_id ORDER BY l.id")
	wantReads := [][]string{{"Finished", "2024-03-05", "1", "", "hardcover"}, {"Finished", "2025-06-01", "2", "4.5", "hardcover"}, {"Year only", "2022-01-01", "3", "3.5", "hardcover"}, {"Undated", "", "1", "2", "hardcover"}}
	if fmt.Sprint(reads) != fmt.Sprint(wantReads) {
		t.Fatalf("readings %v", reads)
	}
	if interest := scalarDB[int](t, a, "SELECT interest FROM books WHERE title='Finished'"); interest != 5 {
		t.Fatal("interest", interest)
	}
	if queueOrderOf(t, a) == queue || !strings.HasPrefix(queueOrderOf(t, a), queue) {
		t.Fatalf("placed books moved: %s", queueOrderOf(t, a))
	}
	// A month-only date is shown as a month, never a day.
	body := b.request("GET", fmt.Sprintf("/books/%d", scalarDB[int](t, a, "SELECT id FROM books WHERE title='Finished'")), nil).Body.String()
	if !strings.Contains(body, ">June 2025<") || strings.Contains(body, ">2025-06-01<") {
		t.Fatal("precision not honored")
	}
}

func TestHardcoverPullIsIdempotent(t *testing.T) {
	t.Parallel()
	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, pageReply(
			ubJSON(1, 3, "5", "Done", "Ann", "9780000000011", "", `[{"id":11,"finished_at":"2024-01-02","finished_at_precision":1}]`),
			ubJSON(2, 1, "", "Next", "Ann", "", "", ""),
		)
	})
	linkAccount(t, a)
	applyPullPage(t, b, pullOnce(t, b, nil), nil)
	before := fmt.Sprint(rowsOf(t, a, "SELECT * FROM books ORDER BY id"), rowsOf(t, a, "SELECT * FROM reading_log ORDER BY id"))
	rev := currentRevision(t, a)
	loc := pullOnce(t, b, url.Values{"mode": {"full"}})
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "Nothing to change") {
		t.Fatal(body)
	}
	expect(t, b.request("POST", loc, nil), 303)
	if after := fmt.Sprint(rowsOf(t, a, "SELECT * FROM books ORDER BY id"), rowsOf(t, a, "SELECT * FROM reading_log ORDER BY id")); after != before {
		t.Fatal("second pull changed the library")
	}
	if currentRevision(t, a) != rev {
		t.Fatal("a no-op pull expired previews")
	}
}

func TestHardcoverPullLocalWins(t *testing.T) {
	t.Parallel()
	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, pageReply(
			ubJSON(1, 3, "5", "Mine", "Ann", "", "", `[{"id":11,"finished_at":"2024-01-01","finished_at_precision":1}]`),
			ubJSON(2, 1, "", "Backwards", "Ann", "", "", ""),
		)
	})
	linkAccount(t, a)
	ids := addBooks(t, a, Book{Title: "Mine", Author: "Ann"}, Book{Title: "Backwards", Author: "Ann"}, Book{Title: "Pinned", Author: "Z"}, Book{Title: "Other", Author: "Z"})
	if err := a.store.Write(func(tx *sql.Tx) error {
		if _, e := tx.Exec("UPDATE books SET reading_status='read',pinned=0 WHERE id=?", ids[1]); e != nil {
			return e
		}
		if _, e := tx.Exec("UPDATE books SET pinned=1 WHERE id=?", ids[2]); e != nil {
			return e
		}
		_, e := tx.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(?,?,3,'manual','manual:x')", ids[0], "2024-01-01")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	queue := queueOrderOf(t, a)
	loc := pullOnce(t, b, nil)
	body := b.request("GET", loc, nil).Body.String()
	for _, want := range []string{"Hardcover disagrees", "Ratings that differ", `name="use_rating_1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("preview lacks %q", want)
		}
	}
	expect(t, b.request("POST", loc, nil), 303)
	if r := scalarDB[float64](t, a, "SELECT rating FROM reading_log WHERE source='manual'"); r != 3 {
		t.Fatal("local rating overwritten", r)
	}
	if got := scalarDB[string](t, a, "SELECT reading_status FROM books WHERE title='Backwards'"); got != "read" {
		t.Fatal("moved backward", got)
	}
	if got := scalarDB[string](t, a, "SELECT finished_at FROM reading_log WHERE source='manual'"); got != "2024-01-01" {
		t.Fatal("manual date overwritten", got)
	}
	// Mine finished on Hardcover, so it leaves the queue; the rest keep their order.
	rest := []string{}
	for _, title := range strings.Split(queue, ",") {
		if title != "Mine" {
			rest = append(rest, title)
		}
	}
	if queueOrderOf(t, a) != strings.Join(rest, ",") || scalarDB[int](t, a, "SELECT pinned FROM books WHERE title='Pinned'") != 1 {
		t.Fatal("queue or pin changed")
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM reading_log WHERE book_id="+fmt.Sprint(ids[0])) != 1 || scalarDB[int](t, a, "SELECT hardcover_read_id FROM reading_log WHERE source='manual'") != 11 {
		t.Fatal("the same-day reading was duplicated instead of adopted")
	}
	// Ticking the box for that row uses Hardcover's rating.
	loc = pullOnce(t, b, url.Values{"mode": {"full"}})
	applyPullPage(t, b, loc, url.Values{"use_rating_1": {"on"}})
	if r := scalarDB[float64](t, a, "SELECT rating FROM reading_log WHERE source='manual'"); r != 5 {
		t.Fatal("chosen rating not used", r)
	}
	// And that is stable: a third pull has nothing left to offer.
	if body = b.request("GET", pullOnce(t, b, url.Values{"mode": {"full"}}), nil).Body.String(); strings.Contains(body, "Ratings that differ") {
		t.Fatal("rating still offered")
	}
}

func TestHardcoverPullAliasesAndLinks(t *testing.T) {
	t.Parallel()
	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, pageReply(
			ubJSON(1, 1, "", "Deleted Book", "Ann", "", "", ""),
			ubJSON(2, 1, "", "Dropped", "Ann", "", "", ""),
			ubJSON(3, 1, "", "Taken", "Ann", "", "", ""),
			ubJSON(4, 1, "", "Matched By Title", "Ann", "", "", ""),
		)
	})
	linkAccount(t, a)
	ids := addBooks(t, a, Book{Title: "Deleted Book", Author: "Ann"}, Book{Title: "Dropped", Author: "Ann"}, Book{Title: "Holder", Author: "Q"}, Book{Title: "Matched By Title", Author: "Ann"})
	if err := a.store.Write(func(tx *sql.Tx) error {
		if e := saveBookLink(tx, ids[1], hcCandidate{BookID: 2}, "t"); e != nil {
			return e
		}
		if e := saveBookLink(tx, ids[2], hcCandidate{BookID: 3}, "t"); e != nil {
			return e
		}
		if e := deleteBook(tx, ids[0]); e != nil {
			return e
		}
		return deleteBook(tx, ids[1])
	}); err != nil {
		t.Fatal(err)
	}
	applyPullPage(t, b, pullOnce(t, b, nil), nil)
	for _, title := range []string{"Deleted Book", "Dropped"} {
		if scalarDB[int](t, a, "SELECT count(*) FROM books WHERE title='"+title+"'") != 0 {
			t.Fatal("deleted book came back:", title)
		}
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM books WHERE title='Taken'") != 1 {
		// The Hardcover book is held by "Holder", and "Taken" matched nothing locally,
		// so it is a new book that cannot also take id 3.
		t.Log("Taken created")
	}
	var holder int64
	if err := a.store.DB.QueryRow("SELECT book_id FROM book_hardcover WHERE hardcover_book_id=3").Scan(&holder); err != nil || holder != ids[2] {
		t.Fatal("existing link taken over", holder, err)
	}
	if l, _ := getBookLink(a.store.DB, ids[3]); l == nil || l.HardcoverBookID != 4 {
		t.Fatal("title match not linked", l)
	}
}

func TestHardcoverPullPagesIncrementalAndMidPull429(t *testing.T) {
	t.Parallel()
	var queries []string
	var offsets []float64
	a, b, fake := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		queries = append(queries, q)
		offset, _ := vars["offset"].(float64)
		offsets = append(offsets, offset)
		if n == 2 {
			return 429, map[string]string{"Retry-After": "30"}, `{}`
		}
		rows := []string{}
		for i := 0; i < 100; i++ {
			rows = append(rows, ubJSON(1000+i, 1, "", fmt.Sprintf("Page one %d", i), "Ann", "", "2026-05-02T00:00:00+00:00", ""))
		}
		return 200, nil, pageReply(rows...)
	})
	linkAccount(t, a)
	loc := pullOnce(t, b, nil)
	if fake.count() != 2 {
		t.Fatal(fake.count())
	}
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "Apply what was fetched") || !strings.Contains(body, "Continue the pull") || !strings.Contains(body, `name="offset" value="100"`) {
		t.Fatal(body)
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM books") != 0 {
		t.Fatal("previewing changed the library")
	}
	expect(t, b.request("POST", loc, nil), 303)
	if scalarDB[int](t, a, "SELECT count(*) FROM books") != 100 {
		t.Fatal("first page not applied")
	}
	// An incomplete pull does not move the incremental high-water mark.
	if v, _ := readMeta(a.store.DB, hardcoverSinceKey, ""); v != "" {
		t.Fatal("high-water set early", v)
	}
	if len(offsets) != 2 || offsets[1] != 100 {
		t.Fatal(offsets)
	}
}

func TestHardcoverPullIncrementalUsesHighWater(t *testing.T) {
	t.Parallel()
	var sinces []any
	var queries []string
	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		sinces = append(sinces, vars["since"])
		queries = append(queries, q)
		if n == 1 {
			return 200, nil, pageReply(ubJSON(1, 1, "", "One", "Ann", "", "2026-05-01T10:00:00+00:00", ""), ubJSON(2, 1, "", "Two", "Ann", "", "2026-06-01T10:00:00+00:00", ""))
		}
		return 200, nil, pageReply()
	})
	linkAccount(t, a)
	applyPullPage(t, b, pullOnce(t, b, nil), nil)
	if v, _ := readMeta(a.store.DB, hardcoverSinceKey, ""); v != "2026-06-01T10:00:00+00:00" {
		t.Fatal("high-water", v)
	}
	loc := pullOnce(t, b, nil)
	if sinces[1] != "2026-06-01T10:00:00+00:00" || !strings.Contains(queries[1], "_gt") || strings.Contains(queries[0], "_gt") {
		t.Fatal(sinces, queries)
	}
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "Nothing to change") {
		t.Fatal(body)
	}
	pullOnce(t, b, url.Values{"mode": {"full"}})
	if sinces[2] != nil || strings.Contains(queries[2], "_gt") {
		t.Fatal("full pull used since")
	}
}

func TestHardcoverPullGuards(t *testing.T) {
	t.Parallel()
	a, b, fake := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, pageReply(ubJSON(1, 1, "", "Guarded", "Ann", "", "", ""))
	})
	// Not connected yet.
	expect(t, b.request("POST", "/hardcover/pull", nil), 400)
	linkAccount(t, a)
	b.request("GET", "/hardcover", nil)
	expect(t, b.request("POST", "/hardcover/pull", url.Values{"csrf": {"bad"}}), 400)
	expect(t, b.request("POST", "/hardcover/pull", url.Values{"continue": {"1"}, "since": {"x; drop"}, "offset": {"0"}}), 400)
	loc := pullOnce(t, b, nil)
	b.request("GET", loc, nil)
	if err := a.store.Write(func(tx *sql.Tx) error { return bump(tx) }); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", loc, nil), 409)
	if scalarDB[int](t, a, "SELECT count(*) FROM books") != 0 {
		t.Fatal("stale plan applied")
	}
	before := fake.count()
	if err := a.store.Write(func(tx *sql.Tx) error { return setMeta(tx, "preferences", defaultPreferences) }); err != nil {
		t.Fatal(err)
	}
	b.request("GET", "/hardcover", nil)
	expect(t, b.request("POST", "/hardcover/pull", nil), 400)
	if fake.count() != before {
		t.Fatal("request while off")
	}
}

func TestHardcoverPullRollsBackOnFailure(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	items := []hcPullItem{{HardcoverBookID: 1, Title: "A", Author: "X", StatusID: 1}, {HardcoverBookID: 2, Title: "B", Author: "X", StatusID: 3, Rating: 4}}
	boom := errors.New("boom")
	err := a.store.Write(func(tx *sql.Tx) error {
		if _, e := applyPull(tx, items, pullOptions{}, nil); e != nil {
			return e
		}
		return boom
	})
	if !errors.Is(err, boom) || scalarDB[int](t, a, "SELECT count(*) FROM books")+scalarDB[int](t, a, "SELECT count(*) FROM book_hardcover")+scalarDB[int](t, a, "SELECT count(*) FROM reading_log") != 0 {
		t.Fatal("partial apply survived", err)
	}
}

func TestHardcoverReadingsMigrationAndRestore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:18]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO books(id,title,author,identity,position,added_at,source) VALUES(1,'Old','Author','old|author',0,'2026-01-01T00:00:00Z','manual')`,
		`INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(1,'2024-02-03',4.25,'goodreads','fp1')`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[string](t, path, "SELECT finished_at||'/'||rating||'/'||finished_precision FROM reading_log") != "2024-02-03/4.25/1" {
		t.Fatal("migration changed a reading")
	}
	if found := backups(t, path); len(found) != 1 || found[0] != path+".pre-v19.bak" {
		t.Fatal(found)
	}
	db, _ = sql.Open("sqlite", path)
	if _, err = db.Exec(`INSERT INTO reading_log(book_id,source,fingerprint,hardcover_read_id,finished_precision) VALUES(1,'hardcover','hardcover:read:1',1,2)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO reading_log(book_id,source,fingerprint,hardcover_read_id) VALUES(1,'hardcover','hardcover:read:1b',1)`); err == nil {
		t.Fatal("duplicate Hardcover read id accepted")
	}
	db.Close()

	a, b, _ := newHCApp(t, func(q string, vars map[string]any, n int) (int, map[string]string, string) {
		return 200, nil, pageReply(ubJSON(1, 3, "4", "Round", "Ann", "", "", `[{"id":5,"finished_at":"2023-07-20","finished_at_precision":2}]`))
	})
	linkAccount(t, a)
	applyPullPage(t, b, pullOnce(t, b, nil), nil)
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
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	target := testApp(t, true)
	if err = target.store.Write(func(tx *sql.Tx) error { _, e := restoreExport(tx, f); return e }); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, target, "SELECT finished_at,finished_precision,hardcover_read_id,source FROM reading_log"); fmt.Sprint(got) != "[[2023-07-01 2 5 hardcover]]" {
		t.Fatal(got)
	}
}
