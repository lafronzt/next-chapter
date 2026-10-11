package app

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func libbyLoan(title string) Record {
	return Record{Book: Book{Title: title, Author: "Author", Status: "unreviewed", Source: "libby", Interest: 3}, Event: &Event{Format: "ebook", Status: "borrowed", Activity: "Borrowed", At: "2026-10-01T00:00:00.000000000+00:00"}}
}
func shelf(title, status string) Record {
	return Record{Book: Book{Title: title, Author: "Author", Status: status, Source: "goodreads", Interest: 3}}
}
func bookNamed(t *testing.T, a *App, title string) Book {
	t.Helper()
	all, err := loadBooks(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.Title == title {
			return b
		}
	}
	t.Fatalf("no book %q", title)
	return Book{}
}
func importResultFor(t *testing.T, a *App, rs []Record) importResult {
	t.Helper()
	var result importResult
	if err := a.store.Write(func(tx *sql.Tx) (err error) { result, err = importRecords(tx, rs); return err }); err != nil {
		t.Fatal(err)
	}
	return result
}
func maxPosition(t *testing.T, a *App, except int64) int {
	t.Helper()
	var n int
	if err := a.store.DB.QueryRow("SELECT COALESCE(MAX(position),-1) FROM books WHERE id!=?", except).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func eventCount(t *testing.T, a *App) int {
	t.Helper()
	var n int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// unchangedExceptStatus fails if anything other than status, pin, placement,
// or position differs.
func unchangedExceptStatus(t *testing.T, before, after Book) {
	t.Helper()
	for _, b := range []*Book{&before, &after} {
		b.Status, b.Pinned, b.NeedsPlacement, b.Position = "", false, false, 0
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("other fields changed:\n%+v\n%+v", before, after)
	}
}

func TestGoodreadsImportUpgradesUnreviewed(t *testing.T) {
	a := testApp(t, true)
	addRecords(t, a, records("Queued one", "Queued two"))
	addRecords(t, a, []Record{libbyLoan("Wanted"), libbyLoan("Finished"), libbyLoan("Started"), libbyLoan("Dropped"), libbyLoan("Untouched")})
	before := bookNamed(t, a, "Wanted")
	events := eventCount(t, a)
	result := importResultFor(t, a, []Record{shelf("Wanted", "to-read"), shelf("Finished", "read"), shelf("Started", "reading"), shelf("Dropped", "archived")})
	if !reflect.DeepEqual(result.Upgraded, map[string]int{"to-read": 1, "read": 1, "reading": 1, "archived": 1}) || result.Added != 0 || result.Events != 0 || !result.Changed {
		t.Fatalf("%+v", result)
	}
	wanted := bookNamed(t, a, "Wanted")
	if wanted.Status != "to-read" || !wanted.NeedsPlacement || wanted.Pinned || wanted.Position <= maxPosition(t, a, wanted.ID) {
		t.Fatalf("%+v", wanted)
	}
	unchangedExceptStatus(t, before, wanted)
	if queue := queueOnly(mustBooks(t, a)); queue[len(queue)-1].ID != wanted.ID {
		t.Fatal("upgraded book not at the end of the queue")
	}
	for title, status := range map[string]string{"Finished": "read", "Started": "reading", "Dropped": "archived", "Untouched": "unreviewed"} {
		if b := bookNamed(t, a, title); b.Status != status || (status != "unreviewed" && b.NeedsPlacement) {
			t.Fatalf("%s: %+v", title, b)
		}
	}
	if eventCount(t, a) != events {
		t.Fatal("library events changed")
	}
	// Re-running the same import is a no-op.
	again := importResultFor(t, a, []Record{shelf("Wanted", "read"), shelf("Finished", "to-read"), shelf("Started", "reading"), shelf("Dropped", "archived")})
	if again.upgradedTotal() != 0 || again.Events != 0 || again.Changed {
		t.Fatalf("%+v", again)
	}
	if b := bookNamed(t, a, "Wanted"); b.Status != "to-read" || b.Position != wanted.Position {
		t.Fatalf("second import moved the book: %+v", b)
	}
}

func TestGoodreadsNeverOverridesReaderStatus(t *testing.T) {
	a := testApp(t, true)
	statuses := []string{"to-read", "reading", "read", "abandoned", "archived"}
	for _, status := range statuses {
		r := records("Book " + status)[0]
		r.Status = status
		addRecords(t, a, []Record{r})
	}
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1,needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	before := mustBooks(t, a)
	incoming := []Record{}
	for i, status := range statuses {
		incoming = append(incoming, shelf("Book "+status, statuses[(i+1)%len(statuses)]))
		incoming = append(incoming, shelf("Book "+status, "to-read"))
	}
	if result := importResultFor(t, a, incoming); result.upgradedTotal() != 0 || result.Changed {
		t.Fatalf("%+v", result)
	}
	if after := mustBooks(t, a); !reflect.DeepEqual(before, after) {
		t.Fatalf("books changed:\n%+v\n%+v", before, after)
	}
}

func TestOnlyGoodreadsShelvesUpgrade(t *testing.T) {
	a := testApp(t, true)
	addRecords(t, a, []Record{libbyLoan("Borrowed")})
	// Library re-imports, manual and demo records, and a Goodreads-sourced
	// record that carries a library event all leave the book unreviewed.
	later := libbyLoan("Borrowed")
	later.Status = "to-read"
	later.Event.Status, later.Event.At = "returned", "2026-10-05T00:00:00.000000000+00:00"
	withEvent := shelf("Borrowed", "read")
	withEvent.Event = &Event{Format: "print", Status: "returned", Activity: "Returned", At: "2026-10-06T00:00:00.000000000+00:00"}
	manual := records("Borrowed")[0]
	demo := shelf("Borrowed", "to-read")
	demo.Source = "demo"
	for _, r := range []Record{later, withEvent, manual, demo} {
		if result := importResultFor(t, a, []Record{r}); result.upgradedTotal() != 0 {
			t.Fatalf("%s upgraded: %+v", r.Source, result)
		}
	}
	if b := bookNamed(t, a, "Borrowed"); b.Status != "unreviewed" {
		t.Fatal(b)
	}
	// Logging a loan through the web never changes status either.
	web := &browser{t: t, handler: a.Handler()}
	id := bookNamed(t, a, "Borrowed").ID
	expect(t, web.request("GET", fmt.Sprintf("/books/%d", id), nil), 200)
	expect(t, web.request("POST", fmt.Sprintf("/books/%d/loan", id), url.Values{"source": {"libby"}, "format": {"ebook"}, "status": {"returned"}}), 303)
	if b := bookNamed(t, a, "Borrowed"); b.Status != "unreviewed" {
		t.Fatal(b)
	}
}

func uploadCSV(b *browser, source, data string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("csrf", b.csrf)
	writer.WriteField("revision", b.rev)
	writer.WriteField("source", source)
	part, _ := writer.CreateFormFile("file", "books.csv")
	io.WriteString(part, data)
	writer.Close()
	req := httptest.NewRequest("POST", "/imports", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return b.serve(req)
}

func TestUpgradeSurfacedInPreviewNoticeAndSync(t *testing.T) {
	a := testApp(t, true)
	addRecords(t, a, []Record{libbyLoan("Wanted"), libbyLoan("Finished"), libbyLoan("Synced")})
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	csv := "Title,Author,Exclusive Shelf\nWanted,Author,to-read\nFinished,Author,read\n"
	rr := uploadCSV(b, "goodreads", csv)
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	rr = b.request("GET", planURL, nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "2 library-only books will move out of To review to match Goodreads: 1 to your queue, 1 to your finished list.") {
		t.Fatal(rr.Body)
	}
	if bookNamed(t, a, "Wanted").Status != "unreviewed" {
		t.Fatal("preview changed the library")
	}
	rr = b.request("POST", planURL, nil)
	expect(t, rr, 303)
	if loc := rr.Header().Get("Location"); loc != "/?notice=imported&moved=2" {
		t.Fatal(loc)
	}
	rr = b.request("GET", rr.Header().Get("Location"), nil)
	if !strings.Contains(rr.Body.String(), "2 library-only books moved out of To review") {
		t.Fatal(rr.Body)
	}
	// The same import again moves nothing and does not bump the revision.
	rev := b.rev
	rr = uploadCSV(b, "goodreads", csv)
	planURL = rr.Header().Get("Location")
	if body := b.request("GET", planURL, nil).Body.String(); strings.Contains(body, "will move out of To review") {
		t.Fatal("repeat preview promised a move")
	}
	rr = b.request("POST", planURL, nil)
	if loc := rr.Header().Get("Location"); loc != "/?notice=imported&moved=0" {
		t.Fatal(loc)
	}
	if now, _ := revision(a.store.DB); fmt.Sprint(now) != rev {
		t.Fatalf("revision %s -> %d", rev, now)
	}
	// RSS records come from Goodreads too.
	a.fetch = func(context.Context, Connection) ([]Record, error) { return []Record{shelf("Synced", "to-read")}, nil }
	expect(t, b.request("GET", "/settings", nil), 200)
	expect(t, b.request("POST", "/sync", nil), 303)
	if s := bookNamed(t, a, "Synced"); s.Status != "to-read" || !s.NeedsPlacement {
		t.Fatal(s)
	}
	last, err := readMeta(a.store.DB, "last_sync", SyncResult{})
	if err != nil || !strings.Contains(last.Message, "moved 1 library-only books out of To review") {
		t.Fatalf("%+v %v", last, err)
	}
}

func TestQuickTriage(t *testing.T) {
	a := testApp(t, true)
	addRecords(t, a, records("Queued"))
	addRecords(t, a, []Record{libbyLoan("Wanted"), libbyLoan("Finished"), libbyLoan("Skipped")})
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1 WHERE title='Wanted'"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/?view=unreviewed", nil), 200)
	triage := func(title, action string, extra url.Values) int {
		values := url.Values{"action": {action}}
		for k, v := range extra {
			values[k] = v
		}
		return b.request("POST", fmt.Sprintf("/books/%d/triage", bookNamed(t, a, title).ID), values).Code
	}
	// Failures change nothing.
	before := mustBooks(t, a)
	for _, code := range []int{
		triage("Wanted", "want", url.Values{"csrf": {"forged"}}),
		triage("Wanted", "want", url.Values{"revision": {"999"}}),
		triage("Wanted", "delete", nil),
		triage("Queued", "read", nil),
	} {
		if code != 400 && code != 409 {
			t.Fatalf("status %d", code)
		}
	}
	if !reflect.DeepEqual(before, mustBooks(t, a)) {
		t.Fatal("rejected triage changed books")
	}
	for title, want := range map[string]string{"Wanted": "to-read", "Finished": "read", "Skipped": "archived"} {
		action := map[string]string{"to-read": "want", "read": "read", "archived": "skip"}[want]
		old := bookNamed(t, a, title)
		rev := b.rev
		rr := b.request("POST", fmt.Sprintf("/books/%d/triage", old.ID), url.Values{"action": {action}, "q": {"  "}})
		expect(t, rr, 303)
		if loc := rr.Header().Get("Location"); loc != "/?notice=triaged&view=unreviewed" {
			t.Fatal(loc)
		}
		expect(t, b.request("GET", rr.Header().Get("Location"), nil), 200)
		if b.rev == rev {
			t.Fatal("revision not bumped")
		}
		got := bookNamed(t, a, title)
		if got.Status != want || got.Pinned || got.NeedsPlacement != (want == "to-read") {
			t.Fatalf("%s: %+v", title, got)
		}
		if want == "to-read" && got.Position <= maxPosition(t, a, got.ID) {
			t.Fatal("not appended to the queue")
		}
		unchangedExceptStatus(t, old, got)
	}
	// Already sorted books are rejected.
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/triage", bookNamed(t, a, "Wanted").ID), url.Values{"action": {"read"}}), 400)
}

func TestBulkTriageNeedsConfirmation(t *testing.T) {
	a := testApp(t, true)
	addRecords(t, a, []Record{libbyLoan("Cozy mystery"), libbyLoan("Dark mystery"), libbyLoan("Poetry")})
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/?view=unreviewed&q=Mystery", nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "Mark all 2 matching books as") {
		t.Fatal(rr.Body)
	}
	expect(t, b.request("POST", "/triage", url.Values{"action": {"archive-everything"}}), 400)
	expect(t, b.request("POST", "/triage", url.Values{"action": {"skip"}, "q": {"nothing matches"}}), 400)
	rr = b.request("POST", "/triage", url.Values{"action": {"skip"}, "q": {"Mystery"}})
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	rr = b.request("GET", planURL, nil)
	expect(t, rr, 200)
	body := rr.Body.String()
	for _, want := range []string{"Sort 2 books at once.", "Cozy mystery", "Dark mystery", "Mark 2 books as Not interested"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Poetry") {
		t.Fatal("hidden book in the plan")
	}
	if bookNamed(t, a, "Cozy mystery").Status != "unreviewed" {
		t.Fatal("preview changed the library")
	}
	rr = b.request("POST", planURL, nil)
	expect(t, rr, 303)
	for title, want := range map[string]string{"Cozy mystery": "archived", "Dark mystery": "archived", "Poetry": "unreviewed"} {
		if got := bookNamed(t, a, title).Status; got != want {
			t.Fatalf("%s: %s", title, got)
		}
	}
	// A plan made stale by another change cannot be applied.
	expect(t, b.request("GET", "/?view=unreviewed", nil), 200)
	planURL = b.request("POST", "/triage", url.Values{"action": {"want"}}).Header().Get("Location")
	expect(t, b.request("GET", "/?view=unreviewed", nil), 200)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/triage", bookNamed(t, a, "Poetry").ID), url.Values{"action": {"read"}}), 303)
	expect(t, b.request("POST", planURL, nil), 409)
	if bookNamed(t, a, "Poetry").Status != "read" {
		t.Fatal("stale plan applied")
	}
}

func TestUnreviewedViewAndBadge(t *testing.T) {
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/?view=unreviewed", nil)
	expect(t, rr, 200)
	body := rr.Body.String()
	if !strings.Contains(body, "Nothing waiting for review.") || strings.Contains(body, "nav-count") || strings.Contains(body, "/triage") {
		t.Fatal(body)
	}
	addRecords(t, a, append(records("Queued"), libbyLoan("One"), libbyLoan("Two"), libbyLoan("Three")))
	for _, path := range []string{"/", "/?view=unreviewed", "/settings", "/history"} {
		rr = b.request("GET", path, nil)
		expect(t, rr, 200)
		if !strings.Contains(rr.Body.String(), `<strong class="nav-count">3</strong>`) {
			t.Fatalf("%s badge: %s", path, rr.Body)
		}
	}
	rr = b.request("GET", "/?view=unreviewed", nil)
	body = rr.Body.String()
	if strings.Count(body, `action="/books/`) != 3 || !strings.Contains(body, "Mark all 3 visible books as") || strings.Contains(body, ">Queued<") {
		t.Fatal(body)
	}
	for _, v := range views {
		rr = b.request("GET", "/?view="+v.Key, nil)
		expect(t, rr, 200)
		if !strings.Contains(rr.Body.String(), v.Heading) || !strings.Contains(rr.Body.String(), `href="/?view=`+v.Key+`" class="selected"`) {
			t.Fatalf("%s: %s", v.Key, rr.Body)
		}
	}
	if !strings.Contains(b.request("GET", "/?view=bogus", nil).Body.String(), views[0].Heading) {
		t.Fatal("unknown view did not fall back to the queue")
	}
	// Signed-out visitors see no counts.
	p := &browser{t: t, handler: testApp(t, false).Handler()}
	if strings.Contains(p.request("GET", "/login", nil).Body.String(), "nav-count") {
		t.Fatal("badge shown before sign-in")
	}
}

func mustBooks(t *testing.T, a *App) []Book {
	t.Helper()
	all, err := loadBooks(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	return all
}
