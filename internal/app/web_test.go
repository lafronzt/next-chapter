package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

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
