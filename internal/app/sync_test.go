package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const testRSS = `<rss><channel><item><book_title>Feed Book</book_title><author_name>Writer</author_name><book_id>77</book_id></item></channel></rss>`

func respond(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

// stubFeed serves Goodreads-like responses: paths in redirects answer with a
// redirect to the mapped location, anything else with status and body.
func stubFeed(status int, body string, redirects map[string]string) (*feedClient, *[]string) {
	seen := []string{}
	client := newFeedClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.String())
		if to, ok := redirects[r.URL.Path]; ok {
			return respond(http.StatusFound, "", http.Header{"Location": {to}}), nil
		}
		if r.URL.Host != feedHost || r.URL.Scheme != "https" {
			return nil, fmt.Errorf("test transport reached %s", r.URL)
		}
		return respond(status, body, nil), nil
	}))
	return client, &seen
}

var testConnection = Connection{UserID: "42", Shelf: "to-read", Enabled: true}

func TestFeedRedirectPolicy(t *testing.T) {
	ctx := context.Background()
	for name, location := range map[string]string{
		"other host":  "https://evil.example/feed",
		"http":        "http://www.goodreads.com/feed",
		"other port":  "https://www.goodreads.com:8443/feed",
		"credentials": "https://user:pw@www.goodreads.com/feed",
		"subdomain":   "https://goodreads.com/feed",
	} {
		t.Run(name, func(t *testing.T) {
			feed, seen := stubFeed(200, testRSS, map[string]string{"/review/list_rss/42": location})
			_, err := feed.Fetch(ctx, testConnection)
			if !errors.Is(err, errFeedUnavailable) || errorClass(err) != "redirect" || syncMessage(err, true) != feedOutage {
				t.Fatalf("followed redirect: %v", err)
			}
			if len(*seen) != 1 {
				t.Fatalf("requested %v", *seen)
			}
		})
	}
	feed, seen := stubFeed(200, testRSS, map[string]string{"/review/list_rss/42": "https://www.goodreads.com/canonical/42"})
	rows, err := feed.Fetch(ctx, testConnection)
	if err != nil || len(rows) != 1 || len(*seen) != 2 {
		t.Fatalf("%v %v %v", rows, err, *seen)
	}
	chain := map[string]string{"/review/list_rss/42": "/a", "/a": "/b", "/b": "/c"}
	feed, _ = stubFeed(200, testRSS, chain)
	if _, err = feed.Fetch(ctx, testConnection); err != nil {
		t.Fatalf("three hops: %v", err)
	}
	chain["/c"] = "/d"
	feed, seen = stubFeed(200, testRSS, chain)
	if _, err = feed.Fetch(ctx, testConnection); errorClass(err) != "redirect" || len(*seen) != 4 {
		t.Fatalf("fourth hop: %v %v", err, *seen)
	}
}

func TestFeedErrors(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		status int
		body   string
		class  string
	}{
		"status":    {503, testRSS, "status-503"},
		"oversized": {200, "<rss>" + strings.Repeat("x", maxUpload) + "</rss>", "too-large"},
		"xml":       {200, "<rss><channel><item>", "parse"},
	} {
		t.Run(name, func(t *testing.T) {
			feed, _ := stubFeed(c.status, c.body, nil)
			_, err := feed.Fetch(ctx, testConnection)
			if !errors.Is(err, errFeedUnavailable) || errorClass(err) != c.class {
				t.Fatalf("%v (%s)", err, errorClass(err))
			}
			if syncMessage(err, true) != feedOutage || syncMessage(err, false) != pollOutage {
				t.Fatal(syncMessage(err, true))
			}
			if strings.Contains(syncMessage(err, true), "42") || strings.Contains(syncMessage(err, true), "http") {
				t.Fatal("message leaks request details")
			}
		})
	}
	feed, seen := stubFeed(200, testRSS, nil)
	for _, conn := range []Connection{{Shelf: "to-read"}, {UserID: "abc", Shelf: "to-read"}} {
		if _, err := feed.Fetch(ctx, conn); !errors.Is(err, errSyncConfig) || syncMessage(err, true) == feedOutage {
			t.Fatalf("%+v: %v", conn, err)
		}
	}
	if len(*seen) != 0 {
		t.Fatal("invalid settings reached the network")
	}
}

// pollApp is an app with polling enabled, a stub fetch, and a clock under test control.
func pollApp(t *testing.T, fetch func(context.Context, Connection) ([]Record, error)) (*App, *int) {
	t.Helper()
	a := testApp(t, true)
	if err := setMeta(a.store.DB, "sync", testConnection); err != nil {
		t.Fatal(err)
	}
	calls := 0
	a.fetch = func(ctx context.Context, c Connection) ([]Record, error) { calls++; return fetch(ctx, c) }
	return a, &calls
}
func pollState(t *testing.T, a *App) PollState {
	t.Helper()
	state, err := readMeta(a.store.DB, "poll_state", PollState{})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPollSuccessThroughFeed(t *testing.T) {
	feed, _ := stubFeed(200, testRSS, nil)
	a, _ := pollApp(t, feed.Fetch)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	a.pollIfDue(context.Background(), now)
	books, err := loadBooks(a.store.DB)
	if err != nil || len(books) != 1 || books[0].Title != "Feed Book" || !books[0].NeedsPlacement {
		t.Fatalf("%v %v", books, err)
	}
	last, err := readMeta(a.store.DB, "last_sync", SyncResult{})
	if err != nil || !last.OK || last.At != canonicalTime(now) || !strings.Contains(last.Message, "added 1") {
		t.Fatalf("%+v %v", last, err)
	}
	if s := pollState(t, a); s.Failures != 0 || s.NextAttempt != canonicalTime(now.Add(time.Hour)) {
		t.Fatalf("%+v", s)
	}
}

func TestPollIfDue(t *testing.T) {
	a, calls := pollApp(t, func(context.Context, Connection) ([]Record, error) { return nil, nil })
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	if err := setMeta(a.store.DB, "sync", Connection{UserID: "42", Shelf: "to-read"}); err != nil {
		t.Fatal(err)
	}
	a.pollIfDue(ctx, now)
	if *calls != 0 {
		t.Fatal("polled while disabled")
	}
	if err := setMeta(a.store.DB, "sync", testConnection); err != nil {
		t.Fatal(err)
	}
	// Pre-backoff databases only have last_poll.
	if err := setMeta(a.store.DB, "last_poll", now.Add(-30*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	a.pollIfDue(ctx, now)
	if *calls != 0 {
		t.Fatal("polled before last_poll was an hour old")
	}
	a.pollIfDue(ctx, now.Add(30*time.Minute))
	if *calls != 1 {
		t.Fatal("did not poll when due")
	}
	now = now.Add(30 * time.Minute)
	a.pollIfDue(ctx, now.Add(59*time.Minute))
	if *calls != 1 {
		t.Fatal("polled before next attempt")
	}
	a.pollIfDue(ctx, now.Add(time.Hour))
	if *calls != 2 {
		t.Fatal("did not poll at next attempt")
	}
}

func TestPollBackoff(t *testing.T) {
	failing := true
	a, calls := pollApp(t, func(context.Context, Connection) ([]Record, error) {
		if failing {
			return nil, feedError("status-503", errors.New("unavailable"))
		}
		return records("Recovered"), nil
	})
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 24, 24} {
		a.pollIfDue(ctx, now)
		s := pollState(t, a)
		if *calls != i+1 || s.Failures != i+1 || s.LastErrorAt != canonicalTime(now) || s.NextAttempt != canonicalTime(now.Add(want*time.Hour)) {
			t.Fatalf("failure %d: %+v", i+1, s)
		}
		a.pollIfDue(ctx, now.Add(want*time.Hour-time.Minute))
		if *calls != i+1 {
			t.Fatal("retried during backoff")
		}
		now = now.Add(want * time.Hour)
	}
	last, _ := readMeta(a.store.DB, "last_sync", SyncResult{})
	if last.OK || last.Message != pollOutage {
		t.Fatalf("%+v", last)
	}
	failing = false
	a.pollIfDue(ctx, now)
	if s := pollState(t, a); s.Failures != 0 || s.NextAttempt != canonicalTime(now.Add(time.Hour)) {
		t.Fatalf("not reset: %+v", s)
	}
}

func TestPollRecoversPanics(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	panicking := true
	a, calls := pollApp(t, func(context.Context, Connection) ([]Record, error) {
		if panicking {
			panic("parser exploded")
		}
		return records("After panic"), nil
	})
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	a.pollIfDue(ctx, now)
	if !strings.Contains(logs.String(), "parser exploded") {
		t.Fatalf("panic not logged: %s", logs.String())
	}
	if s := pollState(t, a); s.Failures != 1 {
		t.Fatalf("%+v", s)
	}
	panicking = false
	a.pollIfDue(ctx, now.Add(time.Hour))
	books, _ := loadBooks(a.store.DB)
	if *calls != 2 || len(books) != 1 {
		t.Fatalf("poller did not recover: %d %v", *calls, books)
	}
}

func TestPollSkipsWhenBusy(t *testing.T) {
	a, calls := pollApp(t, func(context.Context, Connection) ([]Record, error) { return nil, nil })
	a.syncMu.Lock()
	a.pollIfDue(context.Background(), time.Now())
	a.syncMu.Unlock()
	last, _ := readMeta(a.store.DB, "last_sync", SyncResult{})
	if *calls != 0 || pollState(t, a).Failures != 0 || last.At != "" {
		t.Fatal("busy skip recorded as failure")
	}
}

func TestSyncNowMessages(t *testing.T) {
	a := testApp(t, true)
	b := browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/settings", nil), 200)
	expect(t, b.request("POST", "/settings", url.Values{"library_weight": {"1"}, "length": {"any"}, "shelf": {"to-read"}, "user_id": {"42"}}), 303)
	expect(t, b.request("GET", "/settings", nil), 200)

	a.syncMu.Lock()
	rr := b.request("POST", "/sync", nil)
	a.syncMu.Unlock()
	expect(t, rr, 409)
	if !strings.Contains(rr.Body.String(), "already running") {
		t.Fatal(rr.Body.String())
	}

	a.fetch = func(context.Context, Connection) ([]Record, error) {
		// The reader saves different settings while the feed is downloading.
		return records("Late"), setMeta(a.store.DB, "sync", Connection{UserID: "43", Shelf: "to-read"})
	}
	rr = b.request("POST", "/sync", nil)
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "settings changed") || strings.Contains(rr.Body.String(), "could not be checked") {
		t.Fatal(rr.Body.String())
	}

	a.fetch = func(context.Context, Connection) ([]Record, error) { panic("boom") }
	rr = b.request("POST", "/sync", nil)
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "could not be checked") {
		t.Fatal(rr.Body.String())
	}

	feed, _ := stubFeed(500, "", nil)
	a.fetch = feed.Fetch
	rr = b.request("POST", "/sync", nil)
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "could not be checked") {
		t.Fatal(rr.Body.String())
	}
}
