package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAuthenticatedWebFlow(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	a := testApp(t, true)
	b := browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", "/demo", nil), 303)
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", "/demo", nil), 400)
	for _, book := range mustBooks(t, a) {
		if want := map[bool]int{true: 5, false: 3}[book.ID == 5]; book.Interest != want {
			t.Fatalf("demo %s interest %d", book.Title, book.Interest)
		}
	}
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

// jsonKeys returns the sorted keys of a JSON object.
func jsonKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(m))
}

func TestExportJSON(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("Exported"))
	r := records("Exported")[0]
	r.Source, r.Event = "libby", &Event{Format: "ebook", Status: "borrowed", Activity: "Borrowed", At: "2026-10-01T00:00:00.000000000+00:00", DueDate: "2026-10-21"}
	addRecords(t, a, []Record{r})
	b := browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/export.json", nil)
	expect(t, rr, 200)
	if rr.Header().Get("Content-Type") != "application/json" || !strings.Contains(rr.Header().Get("Content-Disposition"), "next-chapter.json") {
		t.Fatal(rr.Header())
	}
	var export map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &export); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(export)); !slices.Equal(got, []string{"aliases", "books", "events", "exported_at", "preferences", "schema_version", "sync"}) {
		t.Fatalf("top-level keys %v", got)
	}
	var meta struct {
		SchemaVersion int    `json:"schema_version"`
		ExportedAt    string `json:"exported_at"`
		Books         []json.RawMessage
		Events        []json.RawMessage
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.SchemaVersion != latestVersion(migrations) {
		t.Fatalf("schema_version %d", meta.SchemaVersion)
	}
	if again, err := parseTime(meta.ExportedAt); err != nil || again != meta.ExportedAt {
		t.Fatalf("exported_at %q", meta.ExportedAt)
	}
	if len(meta.Books) != 1 || len(meta.Events) != 1 {
		t.Fatalf("%d books, %d events", len(meta.Books), len(meta.Events))
	}
	want := []string{"added_at", "author", "goodreads_id", "id", "identity", "interest", "isbn", "loans", "needs_placement", "pages", "pinned", "position", "reading_status", "series", "series_number", "source", "tags", "title"}
	if got := jsonKeys(t, meta.Books[0]); !slices.Equal(got, want) {
		t.Fatalf("book keys %v", got)
	}
	want = []string{"activity", "at", "author", "book_id", "due_date", "fingerprint", "format", "id", "source", "status", "title"}
	if got := jsonKeys(t, meta.Events[0]); !slices.Equal(got, want) {
		t.Fatalf("event keys %v", got)
	}
	if got := jsonKeys(t, export["preferences"]); !slices.Equal(got, []string{"length", "library_weight", "mood"}) {
		t.Fatalf("preference keys %v", got)
	}
}

func TestSessionCookies(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	b := signIn(t, a, "")
	// The same cookie keeps working across requests.
	for range 3 {
		expect(t, b.request("GET", "/settings", nil), 200)
	}
	epoch, err := readMeta(a.store.DB, "session_epoch", "")
	if err != nil {
		t.Fatal(err)
	}
	valid := sessionData{Authenticated: true, CSRF: randomToken(), Expires: time.Now().Add(time.Hour).Unix(), Binding: a.sessionBinding(epoch)}
	signed := a.signSession(valid)
	body, sig, _ := strings.Cut(signed, ".")
	expired := valid
	expired.Expires = time.Now().Add(-time.Second).Unix()
	shortCSRF := valid
	shortCSRF.CSRF = "short"
	other := &App{config: Config{Secret: strings.Repeat("o", 40)}}
	flipped := []byte(sig)
	flipped[0] ^= 1
	cases := []struct {
		name, value string
		ok          bool
	}{
		{"valid", signed, true},
		{"expired", a.signSession(expired), false},
		{"short CSRF token", a.signSession(shortCSRF), false},
		{"another secret", other.signSession(valid), false},
		{"tampered body", strings.Replace(body, body[:4], "AAAA", 1) + "." + sig, false},
		{"tampered signature", body + "." + string(flipped), false},
		{"no signature", body, false},
		{"extra part", signed + ".x", false},
		{"signature not base64", body + ".!!!", false},
		{"oversized", signed + strings.Repeat("A", 4097), false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/settings", nil)
			req.AddCookie(&http.Cookie{Name: "nc_session", Value: c.value})
			_, ok := a.readSession(req)
			if ok != c.ok {
				t.Fatalf("readSession ok=%v, want %v", ok, c.ok)
			}
			rr := httptest.NewRecorder()
			a.Handler().ServeHTTP(rr, req)
			if want := map[bool]int{true: 200, false: 303}[c.ok]; rr.Code != want {
				t.Fatalf("status %d, want %d", rr.Code, want)
			}
			// A rejected cookie is replaced with a fresh anonymous one.
			if !c.ok && len(rr.Result().Cookies()) != 1 {
				t.Fatal("no replacement cookie")
			}
		})
	}
	// The cookie carries the security attributes and its own expiry.
	p := &browser{t: t, handler: a.Handler()}
	p.request("GET", "/login", nil)
	if c := p.cookie; !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure {
		t.Fatalf("%+v", c)
	}
	a.config.SecureCookie = true
	p.cookie = nil
	p.request("GET", "/login", nil)
	if !p.cookie.Secure {
		t.Fatal("COOKIE_SECURE not honored")
	}
}

func TestLoginLimitWindow(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	const client = "203.0.113.9"
	// Failures older than a minute no longer count and are pruned.
	if _, err := a.store.DB.Exec("INSERT INTO login_failures(client,at) VALUES(?,?)", client, float64(time.Now().Add(-61*time.Second).Unix())); err != nil {
		t.Fatal(err)
	}
	seedFailures(t, a, client, loginClientLimit-1)
	b := &browser{t: t, handler: a.Handler(), remote: client + ":1234"}
	expect(t, b.request("GET", "/login", nil), 200)
	expect(t, b.request("POST", "/login", url.Values{"password": {"wrong"}}), 401)
	if n := failureCount(t, a, client); n != loginClientLimit {
		t.Fatalf("%d failures stored", n)
	}
	rr := b.request("POST", "/login", url.Values{"password": {"correct-password"}})
	expect(t, rr, 429)
	if rr.Header().Get("Retry-After") != "60" || !strings.Contains(rr.Body.String(), "Too many login attempts") || strings.Contains(rr.Body.String(), "not recognized") {
		t.Fatalf("%v %s", rr.Header(), rr.Body)
	}
	// A limited attempt is not recorded, so waiting is enough to recover.
	if n := failureCount(t, a, client); n != loginClientLimit {
		t.Fatalf("%d failures after a limited attempt", n)
	}
	if _, err := a.store.DB.Exec("UPDATE login_failures SET at=at-61"); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", "/login", url.Values{"password": {"correct-password"}}), 303)
	expect(t, b.request("GET", "/", nil), 200)
}

// form posts values after loading a page, so the CSRF token and revision are current.
func (b *browser) form(path string, values url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	expect(b.t, b.request("GET", "/settings", nil), 200)
	return b.request("POST", path, values)
}

func bookForm(title, status string, extra ...string) url.Values {
	v := url.Values{"title": {title}, "author": {"Author"}, "interest": {"3"}, "reading_status": {status}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v
}

func TestEditBookStatusTransitions(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("First", "Second", "Third"))
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1, needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	edit := func(title string, values url.Values) Book {
		t.Helper()
		id := bookNamed(t, a, title).ID
		expect(t, b.form(fmt.Sprintf("/books/%d", id), values), 303)
		book, err := getBook(a.store.DB, id)
		if err != nil {
			t.Fatal(err)
		}
		return book
	}
	before := bookNamed(t, a, "First")
	// Editing other fields keeps pin, placement, and position.
	got := edit("First", bookForm("First", "to-read", "interest", "5", "pages", "320", "tags", "calm", "series", "Saga", "series_number", "1.5"))
	if !got.Pinned || got.NeedsPlacement || got.Position != before.Position || got.Interest != 5 || *got.Pages != 320 || got.Tags != "calm" || *got.SeriesNumber != 1.5 {
		t.Fatalf("%+v", got)
	}
	// Leaving the queue clears the pin and keeps the position.
	got = edit("First", bookForm("First", "reading"))
	if got.Pinned || got.NeedsPlacement || got.Position != before.Position || got.Status != "reading" {
		t.Fatalf("leaving the queue: %+v", got)
	}
	// Returning appends to the end and asks for placement.
	got = edit("First", bookForm("First", "to-read"))
	if got.Pinned || !got.NeedsPlacement || got.Position != maxPosition(t, a, got.ID)+1 {
		t.Fatalf("re-entering the queue: %+v", got)
	}
	// Renaming changes the identity used to match imports.
	got = edit("Second", bookForm("Second Edition", "to-read"))
	if got.Identity != identity("Second Edition", "Author") {
		t.Fatalf("%+v", got)
	}
	// Another live book's title and author conflict; the database is untouched.
	rev, _ := revision(a.store.DB)
	rr := b.form(fmt.Sprintf("/books/%d", got.ID), bookForm("third", "to-read"))
	expect(t, rr, 409)
	if after, _ := revision(a.store.DB); !strings.Contains(rr.Body.String(), "Another book already has this title and author") || bookNamed(t, a, "Second Edition").ID != got.ID || after != rev {
		t.Fatal(rr.Body)
	}
	// A deleted book's title is free again.
	third := bookNamed(t, a, "Third")
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, third.ID) }); err != nil {
		t.Fatal(err)
	}
	edit("Second Edition", bookForm("Third", "to-read"))
	// Stale revision, unknown book, and malformed IDs.
	expect(t, b.request("POST", fmt.Sprintf("/books/%d", got.ID), bookForm("x", "to-read", "revision", "0")), 409)
	expect(t, b.form("/books/999", bookForm("Nope", "to-read")), 404)
	expect(t, b.request("GET", "/books/999", nil), 404)
	expect(t, b.request("GET", "/books/0", nil), 400)
	expect(t, b.request("GET", "/books/abc", nil), 400)
}

func TestBookFormValidation(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("Kept"))
	b := &browser{t: t, handler: a.Handler()}
	cases := map[string]url.Values{
		"Enter a title and author": bookForm(" ", "to-read"),
		"within the field limits":  bookForm(strings.Repeat("t", 501), "to-read"),
		"Choose a reading status":  bookForm("Book", "finished"),
		"Interest must be between": bookForm("Book", "to-read", "interest", "6"),
		"between 1 and 5":          bookForm("Book", "to-read", "interest", "x"),
		"Pages must be between":    bookForm("Book", "to-read", "pages", "100000"),
		"1 and 99,999":             bookForm("Book", "to-read", "pages", "0"),
		"valid series number":      bookForm("Book", "to-read", "series_number", "NaN"),
		"a valid series number":    bookForm("Book", "to-read", "series_number", "10000"),
		"Enter a valid series":     bookForm("Book", "to-read", "series_number", "-1"),
		"Tags or series name":      bookForm("Book", "to-read", "tags", strings.Repeat("t", 501)),
		"series name are too long": bookForm("Book", "to-read", "series", strings.Repeat("s", 201)),
	}
	for message, values := range cases {
		for _, path := range []string{"/books/new", "/books/1"} {
			rr := b.form(path, values)
			expect(t, rr, 400)
			if !strings.Contains(rr.Body.String(), message) {
				t.Fatalf("%s %v: %s", path, values, rr.Body)
			}
		}
	}
	if books := mustBooks(t, a); len(books) != 1 || books[0].Title != "Kept" {
		t.Fatalf("%+v", books)
	}
}

func TestBookActions(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("A", "B", "C", "D"))
	addRecords(t, a, []Record{shelf("Done", "read")})
	b := &browser{t: t, handler: a.Handler()}
	act := func(title, action string) *httptest.ResponseRecorder {
		t.Helper()
		return b.form(fmt.Sprintf("/books/%d/action", bookNamed(t, a, title).ID), url.Values{"action": {action}, "return": {"/?view=all&page=1"}})
	}
	order := func() string {
		t.Helper()
		titles, _ := queueTitles(t, a)
		return strings.Join(titles, "")
	}
	rr := act("B", "up")
	expect(t, rr, 303)
	if order() != "BACD" || rr.Header().Get("Location") != "/?page=1&view=all" {
		t.Fatal(order(), rr.Header())
	}
	expect(t, act("B", "down"), 303)
	expect(t, act("C", "down"), 303)
	if order() != "ABDC" {
		t.Fatal(order())
	}
	expect(t, act("D", "placed"), 303)
	if bookNamed(t, a, "D").NeedsPlacement || !bookNamed(t, a, "A").NeedsPlacement {
		t.Fatal("placed")
	}
	expect(t, act("B", "pin"), 303)
	if !bookNamed(t, a, "B").Pinned {
		t.Fatal("not pinned")
	}
	// Neither the pinned book nor its neighbors can cross it.
	for _, move := range [][2]string{{"B", "up"}, {"B", "down"}, {"A", "down"}, {"D", "up"}, {"A", "up"}, {"C", "down"}} {
		rr := act(move[0], move[1])
		expect(t, rr, 400)
		if !strings.Contains(rr.Body.String(), "cross a pinned book or the end of the queue") {
			t.Fatal(rr.Body)
		}
	}
	if order() != "ABDC" {
		t.Fatal(order())
	}
	expect(t, act("B", "pin"), 303)
	if bookNamed(t, a, "B").Pinned {
		t.Fatal("not unpinned")
	}
	expect(t, act("Done", "up"), 400)
	expect(t, act("A", "sideways"), 400)
	expect(t, b.form("/books/x/action", url.Values{"action": {"up"}}), 400)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/action", bookNamed(t, a, "A").ID), url.Values{"action": {"pin"}, "revision": {"0"}}), 409)
}

func TestLoanForm(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{shelf("Shelved", "read")})
	id := bookNamed(t, a, "Shelved").ID
	b := &browser{t: t, handler: a.Handler()}
	path := fmt.Sprintf("/books/%d/loan", id)
	loan := func(source, format, status, due string) url.Values {
		return url.Values{"source": {source}, "format": {format}, "status": {status}, "due_date": {due}}
	}
	for _, values := range []url.Values{
		loan("nypl", "print", "borrowed", "2026-02-30"),
		loan("nypl", "print", "borrowed", "10/20/2026"),
		loan("nypl", "vinyl", "borrowed", ""),
		loan("goodreads", "print", "borrowed", ""),
		loan("nypl", "print", "lost", ""),
		loan("", "", "", ""),
	} {
		rr := b.form(path, values)
		expect(t, rr, 400)
		if !strings.Contains(rr.Body.String(), "Choose a library source") && !strings.Contains(rr.Body.String(), "valid due date") {
			t.Fatalf("%v: %s", values, rr.Body)
		}
	}
	if eventCount(t, a) != 0 {
		t.Fatal("invalid loan recorded an event")
	}
	expect(t, b.form("/books/999/loan", loan("nypl", "print", "borrowed", "")), 404)
	rr := b.form(path, loan("libby", "audiobook", "borrowed", "2026-10-20"))
	expect(t, rr, 303)
	got := bookNamed(t, a, "Shelved")
	if eventCount(t, a) != 1 || got.Status != "read" || len(got.Loans) != 1 || got.Loans[0].DueDate != "2026-10-20" || got.Loans[0].Format != "audiobook" {
		t.Fatalf("%+v", got)
	}
}

// The upload limit applies to the whole request body, multipart framing included.
func TestUploadLimit(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	head := "Title,Author,Notes\nBook,Writer,"
	rr := uploadCSV(b, "goodreads", head+strings.Repeat("x", maxUpload-len(head)-1000))
	expect(t, rr, 303)
	expect(t, b.request("GET", "/imports", nil), 200)
	rr = uploadCSV(b, "goodreads", head+strings.Repeat("x", maxUpload+1-len(head)))
	expect(t, rr, 413)
	if !strings.Contains(rr.Body.String(), "at most 2 MB") {
		t.Fatal(rr.Body)
	}
	// Plain forms have the same limit.
	expect(t, b.request("GET", "/settings", nil), 200)
	expect(t, b.request("POST", "/settings", url.Values{"mood": {strings.Repeat("m", maxUpload)}}), 413)
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	db := func() string { return filepath.Join(t.TempDir(), "x.sqlite3") }
	for _, c := range []struct {
		config Config
		err    string
	}{
		{Config{Database: db(), Secret: "short", Password: "correct-password"}, "APP_SECRET_KEY"},
		{Config{Database: db(), Secret: strings.Repeat("s", 40), Password: "short"}, "APP_PASSWORD"},
		{Config{Database: db(), Secret: strings.Repeat("s", 40)}, "APP_PASSWORD"},
	} {
		if a, err := New(c.config); err == nil || !strings.Contains(err.Error(), c.err) {
			if a != nil {
				a.Close()
			}
			t.Fatalf("%+v: %v", c.config, err)
		}
	}
	// Demo mode needs neither and invents a secret.
	a, err := New(Config{Database: db(), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(a.config.Secret) < 32 {
		t.Fatal("demo secret too short")
	}
}

func TestSuggestAndUndoErrors(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("A", "B"))
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.form("/undo", nil), 404) // nothing to undo yet
	for _, candidate := range []string{"x", "0", "-1"} {
		expect(t, b.form("/suggest", url.Values{"candidate_id": {candidate}}), 400)
	}
	expect(t, b.form("/suggest", url.Values{"candidate_id": {"999"}}), 400)
	expect(t, b.request("POST", "/suggest", url.Values{"revision": {"99"}}), 409)
	expect(t, b.request("GET", "/plans/nope", nil), 404)
	rr := b.form("/suggest", nil)
	expect(t, rr, 303)
	expect(t, b.form(rr.Header().Get("Location"), nil), 303)
	// Any later change expires the undo snapshot.
	expect(t, b.form(fmt.Sprintf("/books/%d/action", bookNamed(t, a, "A").ID), url.Values{"action": {"pin"}}), 303)
	rr = b.form("/undo", nil)
	expect(t, rr, 409)
	if !strings.Contains(rr.Body.String(), "Undo expired") {
		t.Fatal(rr.Body)
	}
}

func TestSettingsValidation(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	valid := url.Values{"library_weight": {"1"}, "length": {"any"}, "shelf": {"to-read"}}
	with := func(pairs ...string) url.Values {
		v := url.Values{}
		for k, vs := range valid {
			v[k] = slices.Clone(vs)
		}
		for i := 0; i+1 < len(pairs); i += 2 {
			v.Set(pairs[i], pairs[i+1])
		}
		return v
	}
	for message, values := range map[string]url.Values{
		"library timing preference": with("library_weight", "3"),
		"timing preference":         with("library_weight", "lots"),
		"length preference":         with("length", "medium"),
		"before enabling polling":   with("enabled", "on"),
		"numeric Goodreads user ID": with("user_id", "me"),
		"letters, digits, hyphens":  with("user_id", "42", "shelf", "to read"),
	} {
		rr := b.form("/settings", values)
		expect(t, rr, 400)
		if !strings.Contains(rr.Body.String(), message) {
			t.Fatalf("%v: %s", values, rr.Body)
		}
	}
	if conn, err := readMeta(a.store.DB, "sync", Connection{}); err != nil || conn != (Connection{}) {
		t.Fatalf("invalid settings saved: %+v %v", conn, err)
	}
	expect(t, b.form("/settings", with("mood", strings.Repeat("m", 150))), 303)
	if prefs, _ := readMeta(a.store.DB, "preferences", defaultPreferences); len(prefs.Mood) != 100 {
		t.Fatalf("mood not truncated: %d", len(prefs.Mood))
	}
}

func TestRatingsInImportPreview(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{shelf("Already Here", "to-read")})
	if _, err := a.store.DB.Exec("UPDATE books SET interest=2"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	rr := uploadCSV(b, "goodreads", "Title,Author,My Rating\nNew Five,Author,5\nNew Four,Author,4\nNew Unrated,Author,0\nAlready Here,Author,5\nOdd,Author,11\n")
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	rr = b.request("GET", planURL, nil)
	expect(t, rr, 200)
	for _, text := range []string{"2 new books start with your Goodreads rating as interest.", "1 rated book is already here and keeps its current interest.", "1 rating was not a whole number from 0 to 5 and was ignored"} {
		if !strings.Contains(rr.Body.String(), text) {
			t.Fatalf("preview missing %q", text)
		}
	}
	expect(t, b.request("POST", planURL, nil), 303)
	for title, want := range map[string]int{"New Five": 5, "New Four": 4, "New Unrated": 3, "Already Here": 2, "Odd": 3} {
		if got := bookNamed(t, a, title).Interest; got != want {
			t.Errorf("%s: interest %d, want %d", title, got, want)
		}
	}
	// A manual add takes the form's interest directly.
	expect(t, b.form("/books/new", bookForm("By Hand", "read", "interest", "4")), 303)
	if got := bookNamed(t, a, "By Hand").Interest; got != 4 {
		t.Fatalf("manual interest %d", got)
	}
}
