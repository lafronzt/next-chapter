package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func must[T any](t *testing.T, v T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func testApp(t *testing.T, demo bool) *App {
	t.Helper()
	a, err := New(Config{Database: filepath.Join(t.TempDir(), "library.sqlite3"), Demo: demo, Secret: strings.Repeat("s", 40), Password: "correct-password"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}
func addRecords(t *testing.T, a *App, records []Record) {
	t.Helper()
	if err := a.store.Write(func(tx *sql.Tx) error { _, err := importRecords(tx, records); return err }); err != nil {
		t.Fatal(err)
	}
}
func records(names ...string) []Record {
	out := []Record{}
	for _, name := range names {
		out = append(out, Record{Book: Book{Title: name, Author: "Author", Status: "to-read", Source: "manual", Interest: 3}})
	}
	return out
}
func ids(rows []Suggestion) []int64 {
	out := []int64{}
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
func TestRanking(t *testing.T) {
	today := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	books := []Book{{ID: 1, Interest: 1}, {ID: 2, Interest: 4}, {ID: 3, Interest: 2}, {ID: 4, Interest: 5}}
	rows, err := Propose(books, defaultPreferences, 4, today)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(rows), []int64{4, 1, 2, 3}) {
		t.Fatal(rows)
	}
	books[0].Pinned = true
	rows, err = Propose(books, defaultPreferences, 4, today)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(rows), []int64{1, 4, 2, 3}) {
		t.Fatal(rows)
	}
	rows, err = Propose(books, defaultPreferences, 0, today)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(rows), []int64{1, 4, 2, 3}) {
		t.Fatal(rows)
	}
	books[2].Loans = []Loan{{Status: "borrowed", DueDate: "2026-10-12"}}
	rows, err = Propose(books, defaultPreferences, 0, today)
	if err != nil {
		t.Fatal(err)
	}
	if rows[1].ID != 3 {
		t.Fatal(rows)
	}
	// Adding a second format must not double the library bonus.
	score1, _ := score(books[2], defaultPreferences, today)
	books[2].Loans = append(books[2].Loans, books[2].Loans[0])
	score2, _ := score(books[2], defaultPreferences, today)
	if score1 != score2 {
		t.Fatal("stacked formats")
	}
	if books[0].ID != 1 || books[3].ID != 4 {
		t.Fatal("mutated input")
	}
}
func TestSeriesPinsReservePrerequisites(t *testing.T) {
	one, two := 1.0, 2.0
	books := []Book{{ID: 1, Interest: 1, Series: "A", SeriesNumber: &one}, {ID: 2, Interest: 5}, {ID: 3, Interest: 4, Series: "A", SeriesNumber: &two, Pinned: true}}
	rows, err := Propose(books, defaultPreferences, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(rows), []int64{2, 1, 3}) {
		t.Fatal(rows)
	}
	books[0], books[2] = books[2], books[0]
	if _, err = Propose(books, defaultPreferences, 0, time.Now()); err == nil {
		t.Fatal("allowed impossible pin")
	}
}
func TestCSVAndRSS(t *testing.T) {
	for _, source := range []string{"goodreads", "libby", "nypl"} {
		t.Run(source, func(t *testing.T) {
			data, err := assets.ReadFile("web/examples/" + source + ".csv")
			if err != nil {
				t.Fatal(err)
			}
			rows, problems, err := ParseCSV(data, source)
			if err != nil || len(problems) != 0 || len(rows) == 0 {
				t.Fatalf("%v %v %v", rows, problems, err)
			}
		})
	}
	_, problems, err := ParseCSV([]byte("Title,Author,Timestamp,Activity\nBook,Person,2026-01-01,unknown\n"), "libby")
	if err != nil || len(problems) != 1 {
		t.Fatalf("%v %v", problems, err)
	}
	for _, data := range []string{"Title,Title\nA,B", "Bad,Header\nA,B", strings.Repeat("x", maxUpload+1)} {
		if _, _, err := ParseCSV([]byte(data), "goodreads"); err == nil {
			t.Fatal("accepted invalid CSV")
		}
	}
	rss := `<rss><channel><item><book_title>A &amp; B</book_title><author_name>Person</author_name><book_id>42</book_id><book><num_pages>250</num_pages></book></item></channel></rss>`
	rows, err := ParseRSS([]byte(rss))
	if err != nil || len(rows) != 1 || rows[0].Title != "A & B" || rows[0].Pages == nil {
		t.Fatalf("%v %v", rows, err)
	}
	if _, err = ParseRSS([]byte(`<!DOCTYPE rss><rss/>`)); err == nil {
		t.Fatal("accepted DTD")
	}
}
func TestImportReconciliationAndBackup(t *testing.T) {
	a := testApp(t, true)
	r := records("Book")[0]
	addRecords(t, a, []Record{r})
	r.Source = "libby"
	r.Status = "unreviewed"
	r.Event = &Event{Format: "ebook", Status: "returned", Activity: "Returned", At: "2026-10-10T00:00:00.000000000+00:00"}
	addRecords(t, a, []Record{r})
	err := a.store.Write(func(tx *sql.Tx) error {
		result, e := importRecords(tx, []Record{r})
		if result.Added != 0 || result.Events != 0 || result.Changed {
			t.Fatal("duplicate changed state")
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Event = &Event{Format: "ebook", Status: "borrowed", Activity: "Borrowed", At: "2026-10-01T00:00:00.000000000+00:00"}
	addRecords(t, a, []Record{r})
	books, err := loadBooks(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || books[0].Status != "to-read" || books[0].Loans[0].Status != "returned" {
		t.Fatal(books)
	}
	data, err := a.store.Backup()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.sqlite3")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	events, err := loadEvents(restored.DB, 0, 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("%v %v", events, err)
	}
	// A failure later in a batch rolls back earlier inserts.
	err = a.store.Write(func(tx *sql.Tx) error {
		rs := records("Other", "Book")
		rs[0].GoodreadsID = "same"
		rs[1].GoodreadsID = "same"
		_, e := importRecords(tx, rs)
		if e != nil {
			return e
		}
		return fmt.Errorf("abort")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	books, _ = loadBooks(a.store.DB)
	if len(books) != 1 {
		t.Fatal("partial import")
	}
}
func TestLegacyMigration(t *testing.T) {
	path := pythonEraDB(t)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	books, err := loadBooks(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || !books[0].Pinned || books[0].Interest != 5 || books[0].Loans[0].ObservedAt != "2026-10-10T12:30:00.000000000+00:00" {
		t.Fatal(books)
	}
	r := records("Legacy")[0]
	r.Source = "nypl"
	r.Event = &Event{Format: "print", Status: "borrowed", Activity: "Borrowed", At: books[0].Loans[0].ObservedAt}
	if result, err := importRecords(s.DB, []Record{r}); err != nil || result.Events != 0 || result.Changed {
		t.Fatalf("legacy duplicate: %+v %v", result, err)
	}
}

type browser struct {
	t         *testing.T
	handler   http.Handler
	cookie    *http.Cookie
	csrf, rev string
	remote    string // client address; httptest's default when empty
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
var revisionPattern = regexp.MustCompile(`name="revision" value="([^"]+)"`)

func (b *browser) request(method, path string, values url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	if values == nil {
		values = url.Values{}
	}
	if method == "POST" {
		if !values.Has("csrf") {
			values.Set("csrf", b.csrf)
		}
		if !values.Has("revision") {
			values.Set("revision", b.rev)
		}
	}
	req := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return b.serve(req)
}
func (b *browser) serve(req *http.Request) *httptest.ResponseRecorder {
	b.t.Helper()
	if b.remote != "" {
		req.RemoteAddr = b.remote
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	rr := httptest.NewRecorder()
	b.handler.ServeHTTP(rr, req)
	for _, c := range rr.Result().Cookies() {
		if c.Name == "nc_session" {
			b.cookie = c
		}
	}
	if m := csrfPattern.FindStringSubmatch(rr.Body.String()); m != nil {
		b.csrf = m[1]
	}
	if m := revisionPattern.FindStringSubmatch(rr.Body.String()); m != nil {
		b.rev = m[1]
	}
	if rr.Code >= 500 {
		b.t.Fatalf("%s %s: %d %s", req.Method, req.URL, rr.Code, rr.Body)
	}
	return rr
}
func expect(t *testing.T, r *httptest.ResponseRecorder, code int) {
	t.Helper()
	if r.Code != code {
		t.Fatalf("status %d, want %d: %s", r.Code, code, r.Body)
	}
}
func TestAuthenticatedWebFlow(t *testing.T) {
	a := testApp(t, false)
	b := browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 303)
	expect(t, b.request("GET", "/login", nil), 200)
	expect(t, b.request("POST", "/login", url.Values{"password": {"wrong"}}), 401)
	expect(t, b.request("POST", "/login", url.Values{"password": {"correct-password"}}), 303)
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", "/books/new", url.Values{"csrf": {"bad"}}), 400)
	create := url.Values{"title": {"<script>Book</script>"}, "author": {"Writer"}, "interest": {"5"}, "reading_status": {"to-read"}}
	rr := b.request("POST", "/books/new", create)
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	rr = b.request("GET", planURL, nil)
	expect(t, rr, 200)
	if strings.Contains(rr.Body.String(), "<script>Book") {
		t.Fatal("unescaped title")
	}
	expect(t, b.request("POST", planURL, nil), 303)
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", "/undo", nil), 303)
	for _, path := range []string{"/", "/books/1", "/books/new", "/settings", "/imports", "/history", "/examples/nypl.csv", "/export.json", "/backup.sqlite3"} {
		expect(t, b.request("GET", path, nil), 200)
	}
	expect(t, b.request("GET", "/", nil), 200)
	oldRev := b.rev
	rr = b.request("POST", "/suggest", nil)
	expect(t, rr, 303)
	planURL = rr.Header().Get("Location")
	expect(t, b.request("POST", "/books/1/action", url.Values{"action": {"pin"}}), 303)
	expect(t, b.request("POST", planURL, nil), 409)
	expect(t, b.request("POST", "/books/1/action", url.Values{"action": {"pin"}, "revision": {oldRev}}), 409)
	expect(t, b.request("GET", "/books/1", nil), 200)
	expect(t, b.request("POST", "/books/1/loan", url.Values{"source": {"nypl"}, "format": {"print"}, "status": {"returned"}}), 303)
	book, err := getBook(a.store.DB, 1)
	if err != nil || book.Status != "to-read" {
		t.Fatalf("%v %v", book, err)
	}
	expect(t, b.request("POST", "/logout", nil), 303)
	expect(t, b.request("GET", "/export.json", nil), 303)
}
func TestImportPreviewAndSettings(t *testing.T) {
	a := testApp(t, true)
	b := browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	upload := func(data string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		writer.WriteField("csrf", b.csrf)
		writer.WriteField("revision", b.rev)
		writer.WriteField("source", "goodreads")
		part, _ := writer.CreateFormFile("file", "books.csv")
		io.WriteString(part, data)
		writer.Close()
		req := httptest.NewRequest("POST", "/imports", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		return b.serve(req)
	}
	expect(t, upload("Title,Author\nMissing,\n"), 400)
	books, _ := loadBooks(a.store.DB)
	if len(books) != 0 {
		t.Fatal("invalid import wrote books")
	}
	rr := upload("Title,Author\nBook,Writer\n")
	expect(t, rr, 303)
	target := rr.Header().Get("Location")
	expect(t, b.request("GET", target, nil), 200)
	books, _ = loadBooks(a.store.DB)
	if len(books) != 0 {
		t.Fatal("preview imported books")
	}
	expect(t, b.request("POST", target, nil), 303)
	expect(t, b.request("GET", "/settings", nil), 200)
	expect(t, b.request("POST", "/settings", url.Values{"library_weight": {"2"}, "length": {"short"}, "mood": {"hopeful"}, "shelf": {"to-read"}, "user_id": {"42"}, "enabled": {"on"}}), 303)
	rr = b.request("GET", "/settings", nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), `value="2" selected`) {
		t.Fatal("preference not rendered")
	}
	a.fetch = func(context.Context, Connection) ([]Record, error) { return records("RSS book"), nil }
	expect(t, b.request("POST", "/sync", nil), 303)
	books, _ = loadBooks(a.store.DB)
	if len(books) != 2 {
		t.Fatal(books)
	}
	rr = b.request("GET", "/export.json", nil)
	var data map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
}
func TestDemoAndLoginLimit(t *testing.T) {
	a := testApp(t, true)
	b := browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", "/demo", nil), 303)
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", "/demo", nil), 400)
	prod := testApp(t, false)
	p := browser{t: t, handler: prod.Handler(), remote: "198.51.100.1:4000"}
	expect(t, p.request("GET", "/login", nil), 200)
	for range loginClientLimit {
		expect(t, p.request("POST", "/login", url.Values{"password": {"wrong"}}), 401)
	}
	rr := p.request("POST", "/login", url.Values{"password": {"correct-password"}})
	expect(t, rr, 429)
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	// Another client is not locked out by the first client's failures.
	other := browser{t: t, handler: prod.Handler(), remote: "198.51.100.2:4000"}
	expect(t, other.request("GET", "/login", nil), 200)
	expect(t, other.request("POST", "/login", url.Values{"password": {"correct-password"}}), 303)
	expect(t, other.request("GET", "/", nil), 200)
}
