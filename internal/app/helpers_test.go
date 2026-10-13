package app

import (
	"database/sql"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
	// A copy, so a caller can post the same values twice.
	values = maps.Clone(values)
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

// TestMain lowers the password key cost: it is the same derivation, but the
// default 600,000 PBKDF2 rounds dominate the suite under -race.
func TestMain(m *testing.M) {
	passwordIterations = 1000
	os.Exit(m.Run())
}
