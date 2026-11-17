package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHardcoverFailureDiagnostics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, phase, cause string
		transportErr       error
		status             int
		body               string
	}{
		{name: "dns", phase: "transport", cause: "dns", transportErr: &net.DNSError{Err: "private-token", Name: "private-title"}},
		{name: "timeout", phase: "transport", cause: "timeout", transportErr: context.DeadlineExceeded},
		{name: "connection", phase: "transport", cause: "connection", transportErr: errors.New("private-token private-title")},
		{name: "auth", phase: "http_status", status: 401, body: "private-token private-title"},
		{name: "schema", phase: "graphql", cause: "validation-failed", status: 200, body: `{"errors":[{"message":"private-token private-title","extensions":{"code":"validation-failed"}}]}`},
		{name: "untrusted code", phase: "graphql", status: 200, body: `{"errors":[{"extensions":{"code":"private-token private-title"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			c := newHardcoverClient(HardcoverConfig{Token: "private-token"}, nil, "https://example.invalid")
			c.logger = slog.New(slog.NewTextHandler(&logs, nil))
			c.http.Transport = hardcoverTransport(func(r *http.Request) (*http.Response, error) {
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			var out any
			if err := c.do(context.Background(), hardcoverConnectQuery, map[string]string{"title": "private-title"}, &out); err == nil {
				t.Fatal("expected failure")
			}
			got := logs.String()
			for _, want := range []string{"outbound request finished", "service=hardcover", "operation=read:me", "phase=" + tc.phase, "http_status=" + strconv.Itoa(tc.status)} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %s", want, got)
				}
			}
			if tc.cause != "" && !strings.Contains(got, "cause="+tc.cause) {
				t.Fatalf("missing cause: %s", got)
			}
			if strings.Contains(got, "private-token") || strings.Contains(got, "private-title") {
				t.Fatal("private data in diagnostics")
			}
		})
	}
}

func hardcoverFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/hardcover/connection.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestHardcoverClientResponses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
		scope  string
	}{
		{"success", 200, string(hardcoverFixture(t)), nil, ""},
		{"bad query", 400, `token-secret`, errHardcoverResponse, ""},
		{"auth", 401, `token-secret`, errHardcoverAuth, ""},
		{"scope", 403, `{"error":"insufficient_scope read:me token-secret"}`, nil, "read:me"},
		{"capacity", 403, `{"code":"request_exceeds_capacity"}`, errHardcoverCapacity, ""},
		{"timeout", 408, ``, errHardcoverTimeout, ""},
		{"unavailable", 503, `token-secret`, errHardcoverUnavailable, ""},
		{"graphql", 200, `{"data":{"me":[]},"errors":[{"message":"token-secret"}]}`, errHardcoverResponse, ""},
		{"graphql scope", 200, `{"errors":[{"extensions":{"code":"insufficient_scope","required_scope":"read:library"}}]}`, nil, "read:library"},
		{"malformed", 200, `{"data":{} token-secret`, errHardcoverResponse, ""},
		{"null", 200, `{"data":null}`, errHardcoverResponse, ""},
		{"oversized", 200, strings.Repeat("x", 1_048_577), errHardcoverResponse, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer token-secret" || r.Header.Get("User-Agent") != "NextChapter/"+Version+" (+self-hosted)" || r.Method != "POST" {
					t.Error("request headers or method")
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := newHardcoverClient(HardcoverConfig{Token: "token-secret"}, server.Client(), server.URL)
			var out hardcoverConnectionResponse
			err := c.do(context.Background(), hardcoverConnectQuery, nil, &out)
			if tc.scope != "" {
				var e hardcoverScopeError
				if !errors.As(err, &e) || e.Need != tc.scope {
					t.Fatalf("scope error: %v", err)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "token-secret") {
				t.Fatal("token in error")
			}
			if tc.name == "success" && (len(out.Me) != 1 || out.Me[0].ID != 123) {
				t.Fatal("fixture not decoded")
			}
		})
	}
}
func TestHardcoverRedirectAndTransportError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c := newHardcoverClient(HardcoverConfig{Token: "secret"}, nil, server.URL)
	var out any
	if err := c.do(context.Background(), "{ me { id } }", nil, &out); err != errHardcoverResponse || calls.Load() != 0 {
		t.Fatal(err, calls.Load())
	}
	c.http.Transport = hardcoverTransport(func(r *http.Request) (*http.Response, error) { return nil, errors.New("secret") })
	if err := c.do(context.Background(), "{ me { id } }", nil, &out); err != errHardcoverUnavailable {
		t.Fatal(err)
	}
	c.http.Transport = hardcoverTransport(func(r *http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	if err := c.do(context.Background(), "{ me { id } }", nil, &out); err != errHardcoverTimeout {
		t.Fatal(err)
	}
}

type hardcoverTransport func(*http.Request) (*http.Response, error)

func (f hardcoverTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestHardcoverBudget(t *testing.T) {
	t.Parallel()
	c := newHardcoverClient(HardcoverConfig{Token: "secret"}, nil, "")
	now := time.Now()
	c.now = func() time.Time { return now }
	c.minuteAt = now
	c.dayAt = now
	if err := c.reserve(10); err != nil {
		t.Fatal(err)
	}
	if err := c.reserve(1); err == nil {
		t.Fatal("burst exceeded")
	}
	now = now.Add(time.Second)
	if err := c.reserve(1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	c.observe(http.Header{"Ratelimit": {`"minute";r=2;t=15, "daily";r=4000;t=500`}}, 200)
	if c.knownRemaining() != 2 {
		t.Fatal("header not used")
	}
	if err := c.reserve(3); err == nil {
		t.Fatal("server budget exceeded")
	}
	now = now.Add(16 * time.Second)
	if err := c.reserve(3); err != nil {
		t.Fatal("minute budget did not reset", err)
	}
	c.observe(http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(time.Minute).Unix(), 10)}}, 200)
	if err := c.reserve(1); err == nil {
		t.Fatal("legacy limit ignored")
	}
	now = now.Add(time.Minute)
	if err := c.reserve(1); err != nil {
		t.Fatal(err)
	}
	c.daily = 0
	if err := c.reserve(1); err == nil {
		t.Fatal("daily budget ignored")
	}
	now = now.Add(24 * time.Hour)
	if err := c.reserve(1); err != nil {
		t.Fatal(err)
	}
}
func TestHardcover429StopsCalls(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(429)
	}))
	defer s.Close()
	c := newHardcoverClient(HardcoverConfig{Token: "secret"}, nil, s.URL)
	var out any
	for range 2 {
		var e hardcoverRateError
		if err := c.do(context.Background(), "{ me { id } }", nil, &out); !errors.As(err, &e) || e.RetryAfter > 20*time.Second {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("retried after 429")
	}
}
func TestHardcoverCost(t *testing.T) {
	for _, tc := range []struct {
		query  string
		cost   int
		search bool
	}{
		{hardcoverConnectQuery, 3, false},
		{`query Test($where: books_bool_exp!) { a: books(where: $where) { id contributions { author { name } } } b: books { id } }`, 2, false},
		{`{ x: search(query: "search { foo }", query_type: "Book") { results } }`, 1, true},
		{"{ me { id } # books\n library: __type(name: \"user_books\") { name } }", 2, false},
		{`{ ...Hidden } fragment Hidden on query_root { me { id } }`, 0, false},
	} {
		if n, s := hardcoverCost(tc.query); n != tc.cost || s != tc.search {
			t.Errorf("%s: %d %v", tc.query, n, s)
		}
	}
}
func TestHardcoverOptInAndConnection(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("Synthetic book"))
	before := snapshotHardcoverLibrary(t, a)
	var calls atomic.Int32
	fixture := hardcoverFixture(t)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Prove the HTTP call holds no database connection.
		if _, err := revision(a.store.DB); err != nil {
			t.Error(err)
		}
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Query != hardcoverConnectQuery {
			t.Error("unexpected query")
		}
		w.Write(fixture)
	}))
	defer s.Close()
	a.config.Hardcover = HardcoverConfig{Token: "secret"}
	a.hardcover = newHardcoverClient(a.config.Hardcover, nil, s.URL)
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/hardcover", nil), 200)
	expect(t, b.request("POST", "/hardcover/test", nil), 400)
	if calls.Load() != 0 {
		t.Fatal("request while off")
	}
	if err := a.store.Write(func(tx *sql.Tx) error {
		p := defaultPreferences
		p.Hardcover = true
		return setMeta(tx, "preferences", p)
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("GET", "/hardcover", nil), 200)
	expect(t, b.request("POST", "/hardcover/test", url.Values{"csrf": {"bad"}}), 400)
	expect(t, b.request("POST", "/hardcover/test", url.Values{"revision": {"-1"}}), 409)
	expect(t, b.request("POST", "/hardcover/test", nil), 303)
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	body := b.request("GET", "/hardcover", nil).Body.String()
	if !strings.Contains(body, "example-reader") || strings.Contains(body, "secret") {
		t.Fatal("missing link or exposed token")
	}
	if snapshotHardcoverLibrary(t, a) != before {
		t.Fatal("connection changed books")
	}
	expect(t, b.request("POST", "/hardcover/disconnect", nil), 303)
	l, err := loadHardcoverLink(a.store.DB)
	if err != nil || l != nil {
		t.Fatal(l, err)
	}
	if snapshotHardcoverLibrary(t, a) != before {
		t.Fatal("disconnect changed books")
	}
	p, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil || p.Hardcover {
		t.Fatal("disconnect did not disable requests")
	}
	a.hardcover = nil
	expect(t, b.request("GET", "/hardcover", nil), 200)
}
func snapshotHardcoverLibrary(t *testing.T, a *App) string {
	t.Helper()
	books, err := loadBooks(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(books)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestHardcoverConcurrentBudget(t *testing.T) {
	t.Parallel()
	c := newHardcoverClient(HardcoverConfig{Token: "secret"}, nil, "")
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			if c.reserve(1) == nil {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if allowed.Load() != 10 {
		t.Fatal(allowed.Load())
	}
}
func TestHardcoverMigrationAndRestore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:16]); err != nil {
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
	if scalar[int](t, path, "SELECT count(*) FROM books") != 1 || scalar[int](t, path, "SELECT count(*) FROM hardcover_link") != 0 {
		t.Fatal("migration changed books")
	}
	if found := backups(t, path); len(found) != 1 || found[0] != path+".pre-v17.bak" {
		t.Fatal(found)
	}
	a := testApp(t, true)
	addRecords(t, a, records("Synthetic book"))
	if _, err = a.store.DB.Exec(`INSERT INTO hardcover_link VALUES(1,123,'example-reader','2026-01-01T00:00:00Z')`); err != nil {
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
	if err != nil || len(problems) > 0 || f.HardcoverLink == nil {
		t.Fatal(err, problems)
	}
	target := testApp(t, true)
	if _, err = target.store.DB.Exec(`INSERT INTO hardcover_link VALUES(1,456,'other-reader','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err = target.store.Write(func(tx *sql.Tx) error { _, e := restoreExport(tx, f); return e }); err != nil {
		t.Fatal(err)
	}
	l, err := loadHardcoverLink(target.store.DB)
	if err != nil || l != nil {
		t.Fatal("restored account link", l, err)
	}
	if books, err := loadBooks(target.store.DB); err != nil || len(books) != 1 {
		t.Fatal("books not restored", err)
	}
}

func TestHardcoverMissingTokenAndSchemaDrift(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	if err := a.store.Write(func(tx *sql.Tx) error {
		p := defaultPreferences
		p.Hardcover = true
		return setMeta(tx, "preferences", p)
	}); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	b.request("GET", "/hardcover", nil)
	expect(t, b.request("POST", "/hardcover/test", nil), 400)
	for _, response := range []string{`{"data":{"me":[{"id":123,"username":"reader"}],"library":null,"reads":null}}`, `{"errors":[{"extensions":{"code":"insufficient_scope","required_scope":"read:me"}}]}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(response)) }))
		a.hardcover = newHardcoverClient(HardcoverConfig{Token: "secret"}, nil, s.URL)
		b.request("GET", "/hardcover", nil)
		expect(t, b.request("POST", "/hardcover/test", nil), 303)
		l, err := loadHardcoverLink(a.store.DB)
		if err != nil || l != nil {
			t.Fatal("linked incompatible schema", l, err)
		}
		last, err := readMeta(a.store.DB, "hardcover_test", hardcoverTestResult{})
		if err != nil || last.OK || last.Message == "" {
			t.Fatal(last, err)
		}
		s.Close()
	}
}
func TestHardcoverConfig(t *testing.T) {
	for _, raw := range []string{"", "hc_example_token", " token "} {
		c, err := LoadHardcoverConfig(func(string) string { return raw })
		if err != nil || c.Token != strings.TrimSpace(raw) {
			t.Fatal(err)
		}
		data, err := json.Marshal(c)
		if err != nil || string(data) != "{}" {
			t.Fatal("serialized credentials")
		}
	}
	for _, raw := range []string{"a b", "a\nb", "a\x00b", strings.Repeat("t", 8193)} {
		if _, err := LoadHardcoverConfig(func(string) string { return raw }); err == nil || strings.Contains(err.Error(), raw) {
			t.Fatal("invalid token accepted or echoed")
		}
	}
}
