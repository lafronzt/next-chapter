package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// queueApp has n to-read books "Book 000".. in queue order, none waiting for
// placement.
func queueApp(t *testing.T, n int) (*App, *browser) {
	t.Helper()
	a := testApp(t, true)
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("Book %03d", i)
	}
	addRecords(t, a, records(names...))
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	return a, b
}

var positionPattern = regexp.MustCompile(`<div class="position">(?:<span class="sr-only">Queue position </span>)?([^<]+)</div>`)

// listed returns the queue position labels of the rows on a list page.
func listed(t *testing.T, b *browser, path string) (string, []string) {
	t.Helper()
	rr := b.request("GET", path, nil)
	expect(t, rr, 200)
	out := []string{}
	for _, m := range positionPattern.FindAllStringSubmatch(rr.Body.String(), -1) {
		out = append(out, m[1])
	}
	return rr.Body.String(), out
}

func TestBookListPagination(t *testing.T) {
	t.Parallel()
	_, b := queueApp(t, 500)
	body, rows := listed(t, b, "/")
	if len(rows) != 50 || rows[0] != "01" || rows[49] != "50" {
		t.Fatalf("page 1: %v", rows)
	}
	if !strings.Contains(body, "Showing 1–50 of 500") || !strings.Contains(body, `href="/?page=2&amp;view=queue" rel="next"`) || strings.Contains(body, `rel="prev"`) {
		t.Fatal("page 1 pager")
	}
	if !strings.Contains(body, `<span class="stat-number">500</span><span>books in your queue</span>`) {
		t.Fatal("stats count the whole queue")
	}
	body, rows = listed(t, b, "/?page=2")
	if len(rows) != 50 || rows[0] != "51" || rows[49] != "100" || !strings.Contains(body, "Showing 51–100 of 500") {
		t.Fatalf("page 2: %v", rows)
	}
	if !strings.Contains(body, `href="/?view=queue" rel="prev"`) || !strings.Contains(body, `href="/?page=3&amp;view=queue" rel="next"`) {
		t.Fatal("page 2 links")
	}
	// Absolute positions hold in the All books view too.
	if _, rows = listed(t, b, "/?view=all&page=4"); len(rows) != 50 || rows[0] != "151" {
		t.Fatalf("all view: %v", rows)
	}
	for perPage, want := range map[string]int{"200": 200, "1000": 200, "0": 50, "abc": 50, "7": 7} {
		if _, rows = listed(t, b, "/?per_page="+perPage); len(rows) != want {
			t.Fatalf("per_page=%s: %d rows", perPage, len(rows))
		}
	}
	if body, _ = listed(t, b, "/?per_page=7&page=2"); !strings.Contains(body, `href="/?per_page=7&amp;view=queue" rel="prev"`) {
		t.Fatal("per_page not preserved")
	}
	// A page past the end shows the last page rather than an empty list.
	for _, path := range []string{"/?page=999", "/?page=10"} {
		body, rows = listed(t, b, path)
		if len(rows) != 50 || rows[49] != "500" || !strings.Contains(body, "Showing 451–500 of 500") || strings.Contains(body, `rel="next"`) {
			t.Fatalf("%s: %v", path, rows)
		}
	}
	if _, rows = listed(t, b, "/?page=-3"); rows[0] != "01" {
		t.Fatal("negative page")
	}
	// A short list shows no pager.
	if body, _ = listed(t, b, "/?view=read"); strings.Contains(body, `class="pager"`) {
		t.Fatal("pager on an empty list")
	}
}

func TestSearchAcrossPages(t *testing.T) {
	t.Parallel()
	a, b := queueApp(t, 0)
	rs := []Record{}
	for i := range 500 {
		title := fmt.Sprintf("Plain %03d", i)
		if i%4 == 3 {
			title = fmt.Sprintf("Needle %03d", i)
		}
		rs = append(rs, Record{Book: Book{Title: title, Author: "Author", Status: "to-read", Source: "manual"}})
	}
	addRecords(t, a, rs)
	body, rows := listed(t, b, "/?q=NEEDLE&page=2")
	// The 51st match is book 203, queue position 204.
	if len(rows) != 50 || rows[0] != "204" || rows[49] != "400" || !strings.Contains(body, "Showing 51–100 of 125") {
		t.Fatalf("%v", rows)
	}
	if !strings.Contains(body, `href="/?page=3&amp;q=needle&amp;view=queue" rel="next"`) || !strings.Contains(body, `href="/?q=needle&amp;view=queue" rel="prev"`) {
		t.Fatal("search not preserved in page links")
	}
	if body, rows = listed(t, b, "/?view=all&q=needle&page=3"); len(rows) != 25 || rows[24] != "500" || strings.Contains(body, `rel="next"`) {
		t.Fatalf("last search page: %v", rows)
	}
	if body, _ = listed(t, b, "/?q=needle"); !strings.Contains(body, "125 books · Your order") {
		t.Fatal("footnote counts every match")
	}
}

// moveState finds the listed book with the given title.
func moveState(t *testing.T, items []listedBook, title string) listedBook {
	t.Helper()
	for _, l := range items {
		if l.Title == title {
			return l
		}
	}
	t.Fatalf("%s not listed", title)
	return listedBook{}
}

func TestQueueMoveStateAcrossPages(t *testing.T) {
	t.Parallel()
	a, b := queueApp(t, 120)
	// Pin the first book of page 2 and the last of page 2.
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1 WHERE title IN ('Book 050','Book 099')"); err != nil {
		t.Fatal(err)
	}
	queue := bookFilter{Status: "to-read"}
	page1 := noErr(listBooks(a.store.DB, queue, 50, 0))
	page2 := noErr(listBooks(a.store.DB, queue, 50, 50))
	page3 := noErr(listBooks(a.store.DB, queue, 50, 100))
	cases := []struct {
		items        []listedBook
		title        string
		pos          int
		up, down     bool
		upWhy, dnWhy string
	}{
		{page1, "Book 000", 1, false, true, "Already first in your queue.", ""},
		{page1, "Book 001", 2, true, true, "", ""},
		{page1, "Book 049", 50, true, false, "", "The book below is pinned."},
		{page2, "Book 050", 51, false, false, "Pinned books stay in place. Unpin it to move it.", "Pinned books stay in place. Unpin it to move it."},
		{page2, "Book 051", 52, false, true, "The book above is pinned.", ""},
		{page2, "Book 098", 99, true, false, "", "The book below is pinned."},
		{page3, "Book 100", 101, false, true, "The book above is pinned.", ""},
		{page3, "Book 119", 120, true, false, "", "Already last in your queue."},
	}
	for _, c := range cases {
		l := moveState(t, c.items, c.title)
		if l.QueuePos != c.pos || l.CanUp != c.up || l.CanDown != c.down || l.UpBlock != c.upWhy || l.DownBlock != c.dnWhy {
			t.Errorf("%s: %+v", c.title, l)
		}
	}
	// A search can hide a pinned neighbor; it still blocks the move.
	found := noErr(listBooks(a.store.DB, bookFilter{Status: "to-read", Query: "book 051"}, 50, 0))
	if l := moveState(t, found, "Book 051"); l.QueuePos != 52 || l.CanUp || !l.CanDown {
		t.Fatalf("search: %+v", l)
	}
	found = noErr(listBooks(a.store.DB, bookFilter{Query: "book 05"}, 50, 0))
	if l := moveState(t, found, "Book 052"); l.QueuePos != 53 || !l.CanUp {
		t.Fatalf("search gap: %+v", l)
	}
	// The page renders the same state, and the server agrees.
	body, _ := listed(t, b, "/")
	if !strings.Contains(body, `aria-label="Move Book 049 down" disabled`) || strings.Contains(body, `aria-label="Move Book 049 up" disabled`) || !strings.Contains(body, `aria-label="Move Book 000 up" disabled`) {
		t.Fatal("disabled buttons on page 1")
	}
	ids := bookIDs(t, a)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/action", ids["Book 049"]), url.Values{"action": {"down"}}), 400)
	// A move returns to the page it was made from.
	b.request("GET", "/?page=2", nil)
	rr := b.request("POST", fmt.Sprintf("/books/%d/action", ids["Book 060"]), url.Values{"action": {"up"}, "return": {"/?page=2&view=queue&notice=saved"}})
	expect(t, rr, 303)
	if got := rr.Header().Get("Location"); got != "/?page=2&view=queue" {
		t.Fatal(got)
	}
	for _, hostile := range []string{"//evil.example/?page=2", "https://evil.example/", "/books/1", `/\evil`, ""} {
		b.request("GET", "/", nil)
		rr = b.request("POST", fmt.Sprintf("/books/%d/action", ids["Book 060"]), url.Values{"action": {"pin"}, "return": {hostile}})
		if got := rr.Header().Get("Location"); got != "/" {
			t.Fatalf("%q redirected to %q", hostile, got)
		}
	}
}

func TestSearchMatchesSpecialCharactersLiterally(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	titles := []string{"100% Wolf", "1000 Wolves", "Snake_Case Stories", "SnakeXCase Stories", "It's a Book", `Back\slash Tales`, "Backslash Tales", "Ébène", `Say "Hello"`}
	rs := []Record{}
	for _, title := range titles {
		rs = append(rs, Record{Book: Book{Title: title, Author: "Writer", Status: "to-read", Source: "manual"}})
	}
	addRecords(t, a, rs)
	for raw, want := range map[string][]string{
		"100%":    {"100% Wolf"},
		"%":       {"100% Wolf"},
		"_case":   {"Snake_Case Stories"},
		"_":       {"Snake_Case Stories"},
		"it's":    {"It's a Book"},
		`k\s`:     {`Back\slash Tales`},
		`\`:       {`Back\slash Tales`},
		"ÉBÈNE":   {"Ébène"},
		`"hello"`: {`Say "Hello"`},
		// The search spans title, author, and tags with a space between.
		"wolf writer": {"100% Wolf"},
	} {
		ids := noErr(filteredIDs(a.store.DB, bookFilter{Query: searchQuery(raw)}))
		got := []string{}
		for _, id := range ids {
			got = append(got, noErr(getBook(a.store.DB, id)).Title)
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%q matched %q, want %q", raw, got, want)
		}
	}
	b := browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/?q="+url.QueryEscape(`"Hello"`), nil)
	if !strings.Contains(rr.Body.String(), "Say &#34;Hello&#34;</a></h2>") || strings.Count(rr.Body.String(), `class="book-row"`) != 1 {
		t.Fatal("quoted search")
	}
}

func TestReorderWritesOnlyChangedRows(t *testing.T) {
	t.Parallel()
	a, _ := queueApp(t, 40)
	changes := func(ids []int64) int {
		var n int
		err := a.store.Write(func(tx *sql.Tx) error {
			var before, after int
			if err := tx.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
				return err
			}
			if err := reorder(tx, ids); err != nil {
				return err
			}
			err := tx.QueryRow("SELECT total_changes()").Scan(&after)
			n = after - before
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	queue := noErr(queueOrder(a.store.DB))
	ids := []int64{}
	for _, e := range queue {
		ids = append(ids, e.ID)
	}
	changes(ids) // numbers the queue 0..39
	if n := changes(ids); n != 0 {
		t.Fatalf("unchanged order wrote %d rows", n)
	}
	ids[10], ids[11] = ids[11], ids[10]
	if n := changes(ids); n != 2 {
		t.Fatalf("one move wrote %d rows", n)
	}
	got := noErr(queueOrder(a.store.DB))
	if got[10].ID != ids[10] || got[11].ID != ids[11] {
		t.Fatal("order not applied")
	}
}

func TestHistoryPagination(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	rs := []Record{}
	statuses := []string{"borrowed", "returned", "hold"}
	for i := range 620 {
		source := "libby"
		if i%5 == 0 {
			source = "nypl"
		}
		status := statuses[i%3]
		rs = append(rs, Record{Book: Book{Title: fmt.Sprintf("Loaned %03d", i%40), Author: "Author", Status: "unreviewed", Source: source}, Event: &Event{Format: "ebook", Status: status, Activity: status, At: canonicalTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour))}})
	}
	addRecords(t, a, rs)
	b := browser{t: t, handler: a.Handler()}
	rowsOn := func(path string) (string, int) {
		rr := b.request("GET", path, nil)
		expect(t, rr, 200)
		return rr.Body.String(), strings.Count(rr.Body.String(), `class="history-row"`)
	}
	body, n := rowsOn("/history")
	if n != 50 || !strings.Contains(body, "Showing 1–50 of 620") || !strings.Contains(body, "620 events.") {
		t.Fatalf("first page: %d", n)
	}
	// Every event is reachable, newest first, with no repeats.
	seen := map[string]bool{}
	for page := 1; page <= 4; page++ {
		_, n = rowsOn(fmt.Sprintf("/history?per_page=200&page=%d", page))
		events := noErr(pageEvents(a.store.DB, eventFilter{}, 200, (page-1)*200))
		for _, e := range events {
			if seen[e.Fingerprint] {
				t.Fatal("repeated event")
			}
			seen[e.Fingerprint] = true
		}
		if want := min(200, 620-(page-1)*200); n != want {
			t.Fatalf("page %d: %d rows, want %d", page, n, want)
		}
	}
	if len(seen) != 620 {
		t.Fatal(len(seen))
	}
	if body, n = rowsOn("/history?page=99"); n != 20 || !strings.Contains(body, "Showing 601–620 of 620") {
		t.Fatal("past the end")
	}
	body, n = rowsOn("/history?source=nypl")
	if !strings.Contains(body, "124 events.") || n != 50 || strings.Contains(body, "LIBBY · ebook") {
		t.Fatal("source filter")
	}
	if !strings.Contains(body, `href="/history?page=2&amp;source=nypl" rel="next"`) || !strings.Contains(body, `<option value="nypl" selected>NYPL</option>`) {
		t.Fatal("source filter not kept")
	}
	if body, _ = rowsOn("/history?status=returned&source=libby"); !strings.Contains(body, "166 events.") {
		t.Fatal("combined filters")
	}
	if body, _ = rowsOn("/history?status=finished&source=elsewhere"); !strings.Contains(body, "620 events.") {
		t.Fatal("unknown filters should be ignored")
	}
	if body, n = rowsOn("/history?status=cancelled"); n != 0 || !strings.Contains(body, "No events match these filters.") {
		t.Fatal("empty filter result")
	}
}

func TestBookPageCapsEvents(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	rs := []Record{}
	for i := range bookEventLimit + 5 {
		rs = append(rs, Record{Book: Book{Title: "Often Borrowed", Author: "Author", Status: "unreviewed", Source: "libby"}, Event: &Event{Format: "ebook", Status: "borrowed", Activity: "Borrowed", At: canonicalTime(time.Date(2025, 1, 1, i, 0, 0, 0, time.UTC))}})
	}
	addRecords(t, a, rs)
	b := browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/books/1", nil)
	if n := strings.Count(rr.Body.String(), `class="event"`); n != bookEventLimit || !strings.Contains(rr.Body.String(), "Showing the latest 200 of 205 events.") {
		t.Fatal(n)
	}
}

func TestLargeImportEndToEnd(t *testing.T) {
	t.Parallel()
	if _, _, err := ParseCSV([]byte(syntheticGoodreadsCSV(maxRows)), "goodreads"); err != nil {
		t.Fatal(err)
	}
	a := testApp(t, true)
	b := browser{t: t, handler: a.Handler()}
	b.request("GET", "/imports", nil)
	rr := uploadCSV(&b, "goodreads", syntheticGoodreadsCSV(maxRows+1))
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "import at most 10,000 rows") {
		t.Fatal("10,001 rows accepted")
	}
	n := maxRows
	if raceEnabled {
		n = 1000
	}
	b.request("GET", "/imports", nil)
	rr = uploadCSV(&b, "goodreads", syntheticGoodreadsCSV(n))
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	var raw []byte
	if err := a.store.DB.QueryRow("SELECT payload FROM plans").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, gzipMagic) || len(raw) > n*40 {
		t.Fatalf("stored plan: %d bytes", len(raw))
	}
	rr = b.request("GET", planURL, nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), fmt.Sprintf("%d rows from goodreads", n)) || strings.Count(rr.Body.String(), `class="preview-row"`) != 100 {
		t.Fatal("preview")
	}
	expect(t, b.request("POST", planURL, nil), 303)
	stats := noErr(loadStats(a.store.DB))
	if stats.Books != n || stats.Queue != (n+2)/3 {
		t.Fatalf("%+v", stats)
	}
}

// Plans saved before the compact format still preview and apply.
func TestLegacyPlanPayloadStillDecodes(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	gr := Record{Book: Book{Title: "Legacy", Author: "Writer", Status: "to-read", Source: "goodreads", Interest: 3, NeedsPlacement: true, Loans: []Loan{}}}
	raw := noErr(json.Marshal(Plan{Records: []Record{gr}, Source: "goodreads"}))
	if _, err := a.store.DB.Exec("INSERT INTO plans VALUES('legacy','import',?,0,?)", string(raw), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	b := browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/plans/legacy", nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "1 rows from goodreads") {
		t.Fatal("legacy preview")
	}
	expect(t, b.request("POST", "/plans/legacy", nil), 303)
	if _, ok := bookIDs(t, a)["Legacy"]; !ok {
		t.Fatal("legacy plan not applied")
	}
	// The compact form round-trips every field importing uses.
	pages, number := 320, 2.5
	full := Record{Book: Book{Title: "T", Author: "A", ISBN: "9780000000001", GoodreadsID: "7", Pages: &pages, Tags: "x", Status: "unreviewed", Series: "S", SeriesNumber: &number, Source: "libby", Interest: 3, NeedsPlacement: true}, Event: &Event{Format: "ebook", Status: "borrowed", Activity: "Borrowed", At: "2026-01-01T00:00:00.000000000+00:00", DueDate: "2026-01-15"}}
	p := noErr(decodePlan(a.store.DB, "import", noErr(encodePlan(Plan{Records: []Record{full}}))))
	got, _ := json.Marshal(p.Records[0])
	want, _ := json.Marshal(full)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s\n%s", got, want)
	}
}

// noErr unwraps a (value, error) result; an error fails the test by panicking.
func noErr[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
