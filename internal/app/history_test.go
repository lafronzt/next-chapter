package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// finished is a Goodreads read-shelf record.
func finished(title, date string, rating int) Record {
	r := shelf(title, "read")
	r.FinishedAt, r.Rating = date, rating
	if rating > 0 {
		r.Interest = rating
	}
	return r
}

func readingsOf(t *testing.T, a *App, title string) []readingEntry {
	t.Helper()
	rs, err := bookReadings(a.store.DB, bookNamed(t, a, title).ID)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func currentRevision(t *testing.T, a *App) int64 {
	t.Helper()
	rev, err := revision(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestParseGoodreadsHistory(t *testing.T) {
	t.Parallel()
	data, err := assets.ReadFile("web/examples/goodreads.csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, problems, err := ParseCSV(data, "goodreads")
	if err != nil || len(problems) != 0 || len(rows) != 3 {
		t.Fatal(rows, problems, err)
	}
	read := rows[2]
	if read.Status != "read" || read.FinishedAt != "2024-03-09" || read.Rating != 5 || *read.PublishedYear != 1969 || *read.AvgRating != 4.09 || read.ReviewChars != 63 || !strings.HasPrefix(read.Review, "A slow") {
		t.Fatalf("%+v", read)
	}
	if queued := rows[0]; queued.FinishedAt != "" || queued.Rating != 0 || *queued.PublishedYear != 2021 || queued.ReviewChars != 0 {
		t.Fatalf("%+v", queued)
	}
	cases := []struct{ date, want string }{{"2024/03/09", "2024-03-09"}, {"2024-03-09", "2024-03-09"}, {"", ""}, {"03/09/2024", "2024-03-09"}}
	for _, c := range cases {
		rows, problems, err := ParseCSV([]byte("Title,Author,Exclusive Shelf,Date Read,Year Published,Average Rating\nBook,Writer,read,"+c.date+",abc,0.00\n"), "goodreads")
		if err != nil || len(problems) != 0 || rows[0].FinishedAt != c.want || rows[0].PublishedYear != nil || rows[0].AvgRating != nil {
			t.Fatalf("%q: %+v %v %v", c.date, rows, problems, err)
		}
	}
	// An unreadable Date Read is a row error with the row number.
	_, problems, err = ParseCSV([]byte("Title,Author,Exclusive Shelf,Date Read\nGood,Writer,read,2024/03/09\nBad,Writer,read,last spring\n"), "goodreads")
	if err != nil || len(problems) != 1 || !strings.HasPrefix(problems[0], "Row 3: Date Read: unrecognized date") {
		t.Fatalf("%q %v", problems, err)
	}
	// Ancient works have negative years.
	if y := parseYear("-800"); y == nil || *y != -800 {
		t.Fatal(y)
	}
}

func TestParseRSSHistory(t *testing.T) {
	t.Parallel()
	item := func(title, readAt string) string {
		return "<item><book_title>" + title + "</book_title><author_name>W</author_name><user_rating>4</user_rating><user_read_at>" + readAt +
			"</user_read_at><user_review>Loved it.</user_review><average_rating>3.95</average_rating><book_published>1999</book_published></item>"
	}
	rows, err := ParseRSS([]byte("<rss><channel>" + item("Evening", "Sat, 09 Mar 2024 20:00:00 -0800") + item("Blank", "") + item("Garbled", "someday") + "</channel></rss>"))
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	// The date is the reader's calendar date, not the UTC one.
	if r := rows[0]; r.FinishedAt != "2024-03-09" || r.Rating != 4 || r.ReviewChars != 9 || *r.PublishedYear != 1999 || *r.AvgRating != 3.95 || r.DateIgnored {
		t.Fatalf("%+v", r)
	}
	if rows[1].FinishedAt != "" || rows[1].DateIgnored || rows[2].FinishedAt != "" || !rows[2].DateIgnored {
		t.Fatalf("%+v %+v", rows[1], rows[2])
	}
}

func TestReadingLogImportRules(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("Queued"))
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1, needs_placement=0, interest=2"); err != nil {
		t.Fatal(err)
	}
	queued := bookNamed(t, a, "Queued")
	// A read-shelf record for a queued book logs a reading and changes
	// nothing about the book.
	r := finished("Queued", "2024-03-09", 5)
	r.ReviewChars, r.Review = 9, "Loved it."
	if result := importResultFor(t, a, []Record{r}); result.Logged != 1 || result.Added != 0 || !result.Changed {
		t.Fatalf("%+v", result)
	}
	if after := bookNamed(t, a, "Queued"); !reflect.DeepEqual(queued, after) {
		t.Fatalf("history changed the book:\n%+v\n%+v", queued, after)
	}
	rs := readingsOf(t, a, "Queued")
	if len(rs) != 1 || *rs[0].FinishedAt != "2024-03-09" || *rs[0].Rating != 5 || rs[0].Source != "goodreads" || rs[0].ReviewChars != 9 || rs[0].Review != nil {
		t.Fatalf("%+v", rs)
	}
	// The same record again is a no-op.
	if result := importResultFor(t, a, []Record{r}); result.Changed || result.Logged != 0 {
		t.Fatalf("%+v", result)
	}
	// Re-rating on Goodreads updates the same reading.
	r.Rating = 4
	if result := importResultFor(t, a, []Record{r}); !result.Changed || result.Logged != 0 {
		t.Fatalf("%+v", result)
	}
	if rs = readingsOf(t, a, "Queued"); len(rs) != 1 || *rs[0].Rating != 4 {
		t.Fatalf("%+v", rs)
	}
	// An undated record adds nothing to a book that has a reading.
	if result := importResultFor(t, a, []Record{finished("Queued", "", 0)}); result.Changed {
		t.Fatalf("%+v", result)
	}
	// A later Date Read is a re-read.
	importResultFor(t, a, []Record{finished("Queued", "2025-11-30", 5)})
	if rs = readingsOf(t, a, "Queued"); len(rs) != 2 || *rs[0].FinishedAt != "2025-11-30" {
		t.Fatalf("%+v", rs)
	}
	// An undated reading is filled in when Goodreads later has the date.
	importResultFor(t, a, []Record{finished("Undated", "", 3)})
	if rs = readingsOf(t, a, "Undated"); len(rs) != 1 || rs[0].FinishedAt != nil {
		t.Fatalf("%+v", rs)
	}
	importResultFor(t, a, []Record{finished("Undated", "2024-01-02", 3)})
	if rs = readingsOf(t, a, "Undated"); len(rs) != 1 || *rs[0].FinishedAt != "2024-01-02" {
		t.Fatalf("%+v", rs)
	}
	// Other shelves and library events never log readings.
	importResultFor(t, a, []Record{shelf("Wanted", "to-read"), libbyLoan("Queued")})
	if n := len(readingsOf(t, a, "Wanted")); n != 0 {
		t.Fatal(n)
	}
	// Status, interest, pins, and positions are untouched throughout.
	if after := bookNamed(t, a, "Queued"); after.Status != "to-read" || after.Interest != 2 || !after.Pinned || after.Position != queued.Position {
		t.Fatalf("%+v", after)
	}
	if b := bookNamed(t, a, "Undated"); b.Status != "read" || b.Interest != 3 {
		t.Fatalf("%+v", b)
	}
}

func TestReviewTextNeedsOptIn(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	r := finished("Reviewed", "2024-03-09", 5)
	r.ReviewChars, r.Review = 9, "Loved it."
	importResultFor(t, a, []Record{r})
	if rs := readingsOf(t, a, "Reviewed"); rs[0].Review != nil || rs[0].ReviewChars != 9 {
		t.Fatalf("%+v", rs[0])
	}
	b := &browser{t: t, handler: a.Handler()}
	prefs := url.Values{"library_weight": {"1"}, "length": {"any"}, "shelf": {"to-read"}, "keep_reviews": {"on"}}
	expect(t, b.form("/settings", prefs), 303)
	importResultFor(t, a, []Record{r})
	if rs := readingsOf(t, a, "Reviewed"); rs[0].Review == nil || *rs[0].Review != "Loved it." {
		t.Fatalf("%+v", rs[0])
	}
	rr := b.request("GET", fmt.Sprintf("/books/%d", bookNamed(t, a, "Reviewed").ID), nil)
	if !strings.Contains(rr.Body.String(), "Loved it.") {
		t.Fatal("review not shown")
	}
	// Opting out deletes the kept text, but not the length.
	prefs.Del("keep_reviews")
	expect(t, b.form("/settings", prefs), 303)
	if rs := readingsOf(t, a, "Reviewed"); rs[0].Review != nil || rs[0].ReviewChars != 9 {
		t.Fatalf("%+v", rs[0])
	}
	if !strings.Contains(b.request("GET", fmt.Sprintf("/books/%d", bookNamed(t, a, "Reviewed").ID), nil).Body.String(), "9-character review") {
		t.Fatal("review length not shown")
	}
	// Without the opt-in, review text is not even kept in a stored preview.
	expect(t, b.request("GET", "/imports", nil), 200)
	expect(t, uploadCSV(b, "goodreads", "Title,Author,Exclusive Shelf,My Review\nOther,Writer,read,Secret thoughts\n"), 303)
	var raw []byte
	if err := a.store.DB.QueryRow("SELECT payload FROM plans").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	p, err := decodePlan(a.store.DB, "import", raw)
	if err != nil || p.Records[0].Review != "" || p.Records[0].ReviewChars != 15 {
		t.Fatalf("%+v %v", p.Records, err)
	}
}

func TestManualReadingsWinOverSync(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	if err := setMeta(a.store.DB, "sync", Connection{UserID: "42", Shelf: "to-read", ReadShelf: "read"}); err != nil {
		t.Fatal(err)
	}
	feed := map[string][]Record{"to-read": records("Queued"), "read": {finished("Finished", "2024-03-09", 4)}}
	shelves := []string{}
	a.fetch = func(_ context.Context, c Connection) ([]Record, error) {
		shelves = append(shelves, c.Shelf)
		out := []Record{}
		for _, r := range feed[c.Shelf] {
			r.Status = "to-read" // what ParseRSS returns; sync marks read-shelf entries
			out = append(out, r)
		}
		return out, nil
	}
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	if _, err := a.syncOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if strings.Join(shelves, ",") != "to-read,read" || bookNamed(t, a, "Finished").Status != "read" || bookNamed(t, a, "Finished").Interest != 4 {
		t.Fatalf("%v %+v", shelves, bookNamed(t, a, "Finished"))
	}
	last, err := readMeta(a.store.DB, "last_read_sync", SyncResult{})
	if err != nil || !last.OK || !strings.Contains(last.Message, "logged 1 finished readings") {
		t.Fatalf("%+v %v", last, err)
	}
	// A re-poll with the same feeds changes nothing, not even the revision.
	rev := currentRevision(t, a)
	if _, err := a.syncOnce(context.Background(), now.Add(time.Hour)); err != nil || currentRevision(t, a) != rev {
		t.Fatalf("re-poll bumped the revision: %v", err)
	}
	// The reader edits the rating and date in the app.
	reading := readingsOf(t, a, "Finished")[0]
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.form(fmt.Sprintf("/readings/%d", reading.ID), url.Values{"action": {"save"}, "rating": {"2"}, "finished_at": {"2024-03-10"}}), 303)
	// Goodreads still says 4 on March 9. The edit survives, with no duplicate.
	if _, err := a.syncOnce(context.Background(), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rs := readingsOf(t, a, "Finished")
	if len(rs) != 1 || *rs[0].Rating != 2 || *rs[0].FinishedAt != "2024-03-10" || rs[0].Source != "manual" || rs[0].AddedByHand() {
		t.Fatalf("%+v", rs)
	}
	// A Goodreads reading cannot be deleted; one logged by hand can.
	rr := b.form(fmt.Sprintf("/readings/%d", reading.ID), url.Values{"action": {"delete"}})
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "next sync would add them again") {
		t.Fatal(rr.Body)
	}
	id := bookNamed(t, a, "Finished").ID
	expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), url.Values{"finished_at": {"2026-01-05"}, "rating": {"5"}}), 303)
	rs = readingsOf(t, a, "Finished")
	if len(rs) != 2 || !rs[0].AddedByHand() || *rs[0].FinishedAt != "2026-01-05" {
		t.Fatalf("%+v", rs)
	}
	expect(t, b.form(fmt.Sprintf("/readings/%d", rs[0].ID), url.Values{"action": {"delete"}}), 303)
	if n := len(readingsOf(t, a, "Finished")); n != 1 {
		t.Fatal(n)
	}
	// Validation.
	for _, values := range []url.Values{{"finished_at": {"2024-02-30"}}, {"finished_at": {"0999-01-01"}}, {"rating": {"6"}}, {"rating": {"x"}}} {
		expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), values), 400)
	}
	expect(t, b.form("/books/999/readings", nil), 404)
	expect(t, b.form("/readings/999", url.Values{"action": {"save"}}), 404)
	expect(t, b.form(fmt.Sprintf("/readings/%d", reading.ID), url.Values{"action": {"burn"}}), 400)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/readings", id), url.Values{"revision": {"0"}}), 409)
	if b := bookNamed(t, a, "Finished"); b.Status != "read" {
		t.Fatalf("%+v", b)
	}
}

func TestReadShelfSettings(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/settings", nil)
	if !strings.Contains(rr.Body.String(), `name="read_shelf" value="read"`) {
		t.Fatal("read shelf default not shown")
	}
	base := url.Values{"library_weight": {"1"}, "length": {"any"}, "shelf": {"to-read"}, "user_id": {"42"}}
	bad := url.Values{}
	for k, v := range base {
		bad[k] = v
	}
	bad.Set("read_shelf", "my read shelf")
	expect(t, b.form("/settings", bad), 400)
	base.Set("read_shelf", "")
	expect(t, b.form("/settings", base), 303)
	calls := 0
	a.fetch = func(context.Context, Connection) ([]Record, error) { calls++; return nil, nil }
	if _, err := a.syncOnce(context.Background(), time.Now()); err != nil || calls != 1 {
		t.Fatalf("blank read shelf still fetched: %d %v", calls, err)
	}
	// Equal shelves are fetched once, as reading history.
	base.Set("shelf", "read")
	base.Set("read_shelf", "read")
	expect(t, b.form("/settings", base), 303)
	calls = 0
	a.fetch = func(context.Context, Connection) ([]Record, error) {
		calls++
		r := shelf("Done", "to-read")
		r.FinishedAt = "2024-05-01"
		return []Record{r}, nil
	}
	if _, err := a.syncOnce(context.Background(), time.Now()); err != nil || calls != 1 || len(readingsOf(t, a, "Done")) != 1 {
		t.Fatalf("%d %v", calls, err)
	}
}

func TestReadingHistoryPageAndStats(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	pages := func(n int) *int { return &n }
	rs := []Record{}
	for i := range 55 {
		r := finished(fmt.Sprintf("Book %02d", i), fmt.Sprintf("%d-06-%02d", 2023+i%3, 1+i%28), 1+i%5)
		r.Tags = []string{"Fiction, hopeful", "fiction", "mystery, Hopeful"}[i%3]
		if i%2 == 0 {
			r.Pages = pages(100 + i)
		}
		rs = append(rs, r)
	}
	rs = append(rs, finished("Undated", "", 0))
	addRecords(t, a, rs)
	stats, err := historyStats(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	// 55 dated readings over 2023-2025 (19, 18, 18) and one undated.
	// Ratings cycle 1-5: 11 of each, average 3. Pages are 100+i for even i.
	wantPages := 0.0
	for i := 0; i < 55; i += 2 {
		wantPages += float64(100 + i)
	}
	wantPages /= 28
	if stats.Readings != 56 || stats.Books != 56 || stats.Undated != 1 || stats.Rated != 55 || stats.AverageRating != 3 || stats.AveragePages != wantPages {
		t.Fatalf("%+v", stats)
	}
	if !reflect.DeepEqual(stats.PerYear, []yearCount{{"2025", 18}, {"2024", 18}, {"2023", 19}}) || stats.MostPerYear != 19 {
		t.Fatalf("%+v", stats.PerYear)
	}
	// Tags count books, case-insensitively: fiction 37, hopeful 37, mystery 18.
	if len(stats.TopTags) != 3 || stats.TopTags[0].Books != 37 || stats.TopTags[1].Books != 37 || stats.TopTags[2] != (tagCount{"mystery", 18}) || !strings.EqualFold(stats.TopTags[0].Tag, "fiction") {
		t.Fatalf("%+v", stats.TopTags)
	}
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/reading", nil)
	expect(t, rr, 200)
	body := rr.Body.String()
	if strings.Count(body, `class="history-row"`) != 50 || !strings.Contains(body, `<meter min="0" max="19" value="18">`) || !strings.Contains(body, "Showing 1–50 of 56") {
		t.Fatal("first page")
	}
	// Newest first; the undated reading comes last.
	if first := strings.Index(body, "2025-"); first < 0 || first > strings.Index(body, "2024-06") {
		t.Fatal("order")
	}
	body = b.request("GET", "/reading?page=2", nil).Body.String()
	if strings.Count(body, `class="history-row"`) != 6 || !strings.Contains(body, "Date unknown") || !strings.Contains(body, "Reading history, page 2 of 2") {
		t.Fatal("second page")
	}
	// An empty history explains where it comes from.
	empty := testApp(t, true)
	eb := &browser{t: t, handler: empty.Handler()}
	if body := eb.request("GET", "/reading", nil).Body.String(); !strings.Contains(body, "Every finished book tells you something") || strings.Contains(body, "Books per year") {
		t.Fatal("empty page")
	}
}

func TestReadingHistoryOnDeleteAndMerge(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{finished("Kept", "2024-01-01", 4), finished("Duplicate", "2024-01-01", 2), finished("Gone", "", 3)})
	importResultFor(t, a, []Record{finished("Duplicate", "2025-02-02", 5)})
	ids := bookIDs(t, a)
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", fmt.Sprintf("/books/%d/merge?target=%d", ids["Duplicate"], ids["Kept"]), nil)
	if !strings.Contains(rr.Body.String(), "Finished readings moved: 1; 1 already recorded on the kept book and dropped") {
		t.Fatal(rr.Body)
	}
	if !strings.Contains(b.request("GET", fmt.Sprintf("/books/%d/delete", ids["Gone"]), nil).Body.String(), "and 1 finished readings") {
		t.Fatal("delete preview")
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		if _, err := mergeBooks(tx, ids["Duplicate"], ids["Kept"], true); err != nil {
			return err
		}
		return deleteBook(tx, ids["Gone"])
	})
	if err != nil {
		t.Fatal(err)
	}
	rs := readingsOf(t, a, "Kept")
	if len(rs) != 2 || *rs[0].FinishedAt != "2025-02-02" || *rs[1].Rating != 4 {
		t.Fatalf("%+v", rs)
	}
	var n int
	if err = a.store.DB.QueryRow("SELECT count(*) FROM reading_log").Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	// Re-importing the merged book's readings is a no-op on the kept book.
	if result := importResultFor(t, a, []Record{finished("Duplicate", "2025-02-02", 5), finished("Duplicate", "2024-01-01", 2)}); result.Changed {
		t.Fatalf("%+v", result)
	}
}

// Migration 6 adds the history table and columns, and points existing
// Goodreads connections at the read shelf.
func TestMigrateReadingLogFromVersionFive(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:5]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO meta VALUES('sync','{"user_id":"42","shelf":"favorites","enabled":true}');
INSERT INTO books(title,author,identity,reading_status,position,added_at,source) VALUES('Kept','Writer','kept|writer','read',0,'2026-10-01T00:00:00.000000000+00:00','goodreads')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := readMeta(s.DB, "sync", Connection{})
	if err != nil || conn != (Connection{UserID: "42", Shelf: "favorites", Enabled: true, ReadShelf: "read"}) {
		t.Fatalf("%+v %v", conn, err)
	}
	books, err := loadBooks(s.DB)
	if err != nil || len(books) != 1 || books[0].PublishedYear != nil || books[0].AvgRating != nil {
		t.Fatalf("%+v %v", books, err)
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v6.bak" {
		t.Fatalf("backups %v", b)
	}
	// A cleared read shelf stays cleared on later starts.
	if err = setMeta(s.DB, "sync", Connection{UserID: "42", Shelf: "favorites"}); err != nil {
		t.Fatal(err)
	}
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if conn, _ = readMeta(s.DB, "sync", Connection{}); conn.ReadShelf != "" {
		t.Fatalf("%+v", conn)
	}
}

func TestRestoreReadingHistory(t *testing.T) {
	t.Parallel()
	source := testApp(t, true)
	r := finished("Finished", "2024-03-09", 5)
	r.ReviewChars, r.Review = 9, "Loved it."
	if err := setMeta(source.store.DB, "preferences", Preferences{LibraryWeight: 1, Length: "any", KeepReviews: true}); err != nil {
		t.Fatal(err)
	}
	addRecords(t, source, []Record{r, finished("Undated", "", 0)})
	original := exportOf(t, source)
	if !strings.Contains(string(original), `"review": "Loved it."`) {
		t.Fatal("export lacks the kept review")
	}
	restore := func(data []byte) *App {
		t.Helper()
		target := testApp(t, true)
		b := &browser{t: t, handler: target.Handler()}
		expect(t, b.request("GET", "/imports", nil), 200)
		rr := uploadRestore(b, data)
		expect(t, rr, 303)
		expect(t, b.request("GET", rr.Header().Get("Location"), nil), 200)
		expect(t, b.request("POST", rr.Header().Get("Location"), nil), 303)
		return target
	}
	target := restore(original)
	rs := readingsOf(t, target, "Finished")
	if len(rs) != 1 || *rs[0].Rating != 5 || rs[0].Review == nil || len(readingsOf(t, target, "Undated")) != 1 {
		t.Fatalf("%+v", rs)
	}
	// Re-importing after a restore recognizes the restored readings.
	if result := importResultFor(t, target, []Record{r}); result.Changed {
		t.Fatalf("%+v", result)
	}
	// Without the opt-in in the file, review text is dropped.
	optedOut := editExport(t, original, func(m map[string]any) { m["preferences"].(map[string]any)["keep_reviews"] = false })
	if rs = readingsOf(t, restore(optedOut), "Finished"); rs[0].Review != nil || rs[0].ReviewChars != 9 {
		t.Fatalf("%+v", rs[0])
	}
	// A version 5 export has no history and no read shelf; it gets the default.
	old := editExport(t, original, func(m map[string]any) {
		m["schema_version"] = 5
		delete(m, "reading_log")
		delete(m["sync"].(map[string]any), "read_shelf")
		for _, b := range m["books"].([]any) {
			delete(b.(map[string]any), "published_year")
			delete(b.(map[string]any), "avg_rating")
		}
	})
	if conn, _ := readMeta(restore(old).store.DB, "sync", Connection{}); conn.ReadShelf != "read" {
		t.Fatalf("%+v", conn)
	}
	for name, change := range map[string]func(map[string]any){
		"bad rating": func(m map[string]any) { m["reading_log"].([]any)[0].(map[string]any)["rating"] = 6 },
		"bad date":   func(m map[string]any) { m["reading_log"].([]any)[0].(map[string]any)["finished_at"] = "2024/03/09" },
		"bad source": func(m map[string]any) { m["reading_log"].([]any)[0].(map[string]any)["source"] = "rss" },
		"bad book":   func(m map[string]any) { m["reading_log"].([]any)[0].(map[string]any)["book_id"] = 999 },
	} {
		target := testApp(t, true)
		b := &browser{t: t, handler: target.Handler()}
		expect(t, b.request("GET", "/imports", nil), 200)
		if rr := uploadRestore(b, editExport(t, original, change)); rr.Code != 400 {
			t.Errorf("%s: status %d", name, rr.Code)
		}
	}
}
