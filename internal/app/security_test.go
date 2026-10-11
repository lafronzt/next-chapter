package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func seedFailures(t *testing.T, a *App, client string, n int) {
	t.Helper()
	for range n {
		if _, err := a.store.DB.Exec("INSERT INTO login_failures(client,at) VALUES(?,?)", client, float64(time.Now().Unix())); err != nil {
			t.Fatal(err)
		}
	}
}
func failureCount(t *testing.T, a *App, client string) int {
	t.Helper()
	var n int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM login_failures WHERE client=?", client).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func signIn(t *testing.T, a *App, remote string) *browser {
	t.Helper()
	b := &browser{t: t, handler: a.Handler(), remote: remote}
	expect(t, b.request("GET", "/login", nil), 200)
	expect(t, b.request("POST", "/login", url.Values{"password": {"correct-password"}}), 303)
	expect(t, b.request("GET", "/", nil), 200)
	return b
}

func TestLoginCountsOnlyFailures(t *testing.T) {
	a := testApp(t, false)
	// A client just under its limit can still sign in, and success is not counted.
	seedFailures(t, a, "198.51.100.3", loginClientLimit-1)
	signIn(t, a, "198.51.100.3:5000")
	if n := failureCount(t, a, "198.51.100.3"); n != loginClientLimit-1 {
		t.Fatalf("successful login changed failures to %d", n)
	}
	b := &browser{t: t, handler: a.Handler(), remote: "198.51.100.4:5000"}
	expect(t, b.request("GET", "/login", nil), 200)
	expect(t, b.request("POST", "/login", url.Values{"password": {"wrong"}}), 401)
	if n := failureCount(t, a, "198.51.100.4"); n != 1 {
		t.Fatalf("failed login recorded %d rows", n)
	}
	// Failures older than a minute no longer count.
	if _, err := a.store.DB.Exec("UPDATE login_failures SET at=at-120"); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", "/login", url.Values{"password": {"wrong"}}), 401)
	if n := failureCount(t, a, "198.51.100.4"); n != 1 {
		t.Fatalf("expired failures kept: %d", n)
	}
}

func TestLoginGlobalBackstop(t *testing.T) {
	a := testApp(t, false)
	for i := range loginGlobalLimit {
		seedFailures(t, a, fmt.Sprintf("203.0.113.%d", i%200), 1)
	}
	b := &browser{t: t, handler: a.Handler(), remote: "198.51.100.9:5000"}
	expect(t, b.request("GET", "/login", nil), 200)
	rr := b.request("POST", "/login", url.Values{"password": {"correct-password"}})
	expect(t, rr, 429)
	if rr.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After %q", rr.Header().Get("Retry-After"))
	}
	// Below the backstop, other clients' failures do not block this one.
	if _, err := a.store.DB.Exec("DELETE FROM login_failures WHERE rowid IN (SELECT rowid FROM login_failures LIMIT 1)"); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", "/login", url.Values{"password": {"correct-password"}}), 303)
}

func TestClientIP(t *testing.T) {
	a := testApp(t, true)
	request := func(forwarded ...string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.0.0.2:3456"
		for _, f := range forwarded {
			r.Header.Add("X-Forwarded-For", f)
		}
		return r
	}
	cases := []struct {
		hops      int
		forwarded []string
		want      string
	}{
		{0, nil, "10.0.0.2"},
		{0, []string{"203.0.113.9"}, "10.0.0.2"},                       // header ignored entirely
		{1, []string{"6.6.6.6, 203.0.113.9"}, "203.0.113.9"},           // spoofed leftmost ignored
		{1, []string{"6.6.6.6", "203.0.113.9"}, "203.0.113.9"},         // repeated headers combine
		{2, []string{"6.6.6.6, 203.0.113.9, 10.0.0.9"}, "203.0.113.9"}, // two proxies
		{2, []string{"203.0.113.9"}, "10.0.0.2"},                       // shorter than hops
		{1, []string{"not-an-ip"}, "10.0.0.2"},
		{1, []string{"2001:db8::1"}, "2001:db8::1"},
	}
	for _, c := range cases {
		a.config.TrustedProxyHops = c.hops
		if got := a.clientIP(request(c.forwarded...)); got != c.want {
			t.Errorf("hops=%d %q: got %s want %s", c.hops, c.forwarded, got, c.want)
		}
	}
}

func TestTrustedProxyLimiter(t *testing.T) {
	a := testApp(t, false)
	a.config.TrustedProxyHops = 1
	seedFailures(t, a, "203.0.113.9", loginClientLimit)
	b := &browser{t: t, handler: a.Handler(), remote: "10.0.0.2:3456"}
	expect(t, b.request("GET", "/login", nil), 200)
	login := func(forwarded string) int {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"csrf": {b.csrf}, "password": {"wrong"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", forwarded)
		return b.serve(req).Code
	}
	// The blocked client cannot escape by prepending a fake address.
	if code := login("198.51.100.77, 203.0.113.9"); code != 429 {
		t.Fatalf("spoofed leftmost entry: %d", code)
	}
	if code := login("203.0.113.9, 198.51.100.77"); code != 401 {
		t.Fatalf("different proxied client: %d", code)
	}
	a.config.TrustedProxyHops = 0
	seedFailures(t, a, "10.0.0.2", loginClientLimit)
	if code := login("198.51.100.78"); code != 429 {
		t.Fatalf("header trusted without TRUSTED_PROXY_HOPS: %d", code)
	}
}

var referencePattern = regexp.MustCompile(`<code>([0-9a-f]{12})</code>`)

func TestPanicAndServerErrorShowRequestID(t *testing.T) {
	a := testApp(t, true)
	var logs bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&logs, nil))
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 200)
	a.fetch = func(context.Context, Connection) ([]Record, error) { panic("feed exploded") }
	req := httptest.NewRequest("POST", "/sync?token=secret-query", strings.NewReader(url.Values{"csrf": {b.csrf}, "revision": {b.rev}, "note": {"secret-form"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(b.cookie)
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Fatalf("status %d", rr.Code)
	}
	m := referencePattern.FindStringSubmatch(rr.Body.String())
	if m == nil {
		t.Fatalf("no request ID on page: %s", rr.Body)
	}
	line := logs.String()
	for _, want := range []string{"request_id=" + m[1], "method=POST", "path=/sync", "feed exploded", "stack="} {
		if !strings.Contains(line, want) {
			t.Fatalf("log missing %q: %s", want, line)
		}
	}
	for _, secret := range []string{"secret-query", "secret-form", b.cookie.Value} {
		if strings.Contains(line, secret) {
			t.Fatalf("log contains %q", secret)
		}
	}
	// An ordinary error takes the same path, and client errors show no ID.
	logs.Reset()
	rr = httptest.NewRecorder()
	a.fail(rr, withState(httptest.NewRequest("GET", "/x", nil), requestState{ID: "abc123abc123"}), errors.New("disk full"))
	if rr.Code != 500 || !strings.Contains(rr.Body.String(), "abc123abc123") || !strings.Contains(logs.String(), `request_id=abc123abc123`) || !strings.Contains(logs.String(), "disk full") {
		t.Fatalf("%d %s %s", rr.Code, rr.Body, logs.String())
	}
	if strings.Contains(rr.Body.String(), "disk full") {
		t.Fatal("internal error shown to the user")
	}
	rr = httptest.NewRecorder()
	a.fail(rr, withState(httptest.NewRequest("GET", "/x", nil), requestState{ID: "abc123abc123"}), bad("Choose a book."))
	if rr.Code != 400 || strings.Contains(rr.Body.String(), "abc123abc123") {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
}

func TestSignOutEverywhere(t *testing.T) {
	a := testApp(t, false)
	b := signIn(t, a, "")
	laptop := &browser{t: t, handler: a.Handler(), cookie: b.cookie}
	expect(t, laptop.request("GET", "/", nil), 200)
	before, err := revision(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", "/sessions/revoke", url.Values{"revision": {"999"}}), 409)
	expect(t, laptop.request("GET", "/", nil), 200)
	rr := b.request("POST", "/sessions/revoke", nil)
	expect(t, rr, 303)
	if rr.Header().Get("Location") != "/login?notice=signedout" {
		t.Fatal(rr.Header().Get("Location"))
	}
	after, err := revision(a.store.DB)
	if err != nil || after != before {
		t.Fatalf("revision %d -> %d %v", before, after, err)
	}
	expect(t, laptop.request("GET", "/", nil), 303)
	expect(t, b.request("GET", "/", nil), 303)
	signIn(t, a, "")
	// Demo mode has no sessions to revoke.
	demo := &browser{t: t, handler: testApp(t, true).Handler()}
	expect(t, demo.request("GET", "/", nil), 200)
	expect(t, demo.request("POST", "/sessions/revoke", nil), 404)
}

func TestPasswordChangeEndsSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	open := func(password string) *App {
		a, err := New(Config{Database: path, Secret: strings.Repeat("s", 40), Password: password})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close() })
		return a
	}
	first := open("correct-password")
	cookie := signIn(t, first, "").cookie
	// A restart with the same password keeps the session.
	same := &browser{t: t, handler: open("correct-password").Handler(), cookie: cookie}
	expect(t, same.request("GET", "/", nil), 200)
	changed := &browser{t: t, handler: open("a-brand-new-password").Handler(), cookie: cookie}
	expect(t, changed.request("GET", "/", nil), 303)
}

func TestSecurityHeaders(t *testing.T) {
	for _, secure := range []bool{false, true} {
		a := testApp(t, true)
		a.config.SecureCookie = secure
		for _, path := range []string{"/", "/static/app.css", "/healthz"} {
			rr := httptest.NewRecorder()
			a.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
			h := rr.Header()
			if (h.Get("Strict-Transport-Security") != "") != secure {
				t.Fatalf("secure=%v %s HSTS %q", secure, path, h.Get("Strict-Transport-Security"))
			}
			if h.Get("Permissions-Policy") != "camera=(), microphone=(), geolocation=()" || h.Get("X-Frame-Options") != "DENY" || h.Get("Content-Security-Policy") == "" {
				t.Fatalf("%s headers %v", path, h)
			}
			if (h.Get("Cache-Control") == "no-store") == (path == "/static/app.css") {
				t.Fatalf("%s Cache-Control %q", path, h.Get("Cache-Control"))
			}
		}
	}
}

func TestCSRFMessages(t *testing.T) {
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	// No session at all: explain the reset and do not issue a cookie yet.
	rr := b.request("POST", "/books/new", url.Values{"csrf": {"anything"}})
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "session expired or was reset") || b.cookie != nil {
		t.Fatalf("%s %v", rr.Body, b.cookie)
	}
	// Reloading gives a working session.
	expect(t, b.request("GET", "/", nil), 200)
	if b.cookie == nil {
		t.Fatal("no session after reload")
	}
	rr = b.request("POST", "/books/new", url.Values{"csrf": {"bad"}})
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "Your form expired") || strings.Contains(rr.Body.String(), "was reset") {
		t.Fatal(rr.Body)
	}
	// The login form explains a missing session too.
	p := &browser{t: t, handler: testApp(t, false).Handler()}
	rr = p.request("POST", "/login", url.Values{"password": {"correct-password"}})
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "session expired or was reset") {
		t.Fatal(rr.Body)
	}
}
