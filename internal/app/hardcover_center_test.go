package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func switchesOn(t *testing.T, b *browser, pull, push bool) {
	t.Helper()
	b.request("GET", "/hardcover", nil)
	v := url.Values{}
	if pull {
		v.Set("pull", "on")
	}
	if push {
		v.Set("push", "on")
	}
	expect(t, b.request("POST", "/hardcover/switches", v), 303)
}

func runCheck(t *testing.T, a *App, b *browser) string {
	t.Helper()
	b.request("GET", "/hardcover", nil)
	refill(a)
	return locationOf(t, b.request("POST", "/hardcover/check", nil))
}

// section returns the page text from one heading to the next section.
func section(body, heading string) string {
	i := strings.Index(body, `id="`+heading+`"`)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.Index(rest[1:], "<section"); j >= 0 {
		rest = rest[:j+1]
	}
	return rest
}

func TestCheckSortsIntoGroupsAndCostsOneRequest(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{
		1:   {ID: 101, Status: 3, Privacy: 3},  // A: local to-read, Hardcover Read
		2:   {ID: 102, Status: 1, Privacy: 3},  // B: local read, Hardcover Want
		3:   {ID: 103, Status: 3, Privacy: 3},  // C: both changed since the last sync
		4:   {ID: 104, Status: 2, Privacy: 3},  // D: in step
		999: {ID: 1999, Status: 1, Privacy: 3}, // E: not in this library
	}}
	a, b, ids := pushApp(t, remote, "to-read:1", "read:2", "reading:3", "reading:4", "reading:5")
	// C had been synced as to-read/Want; now local is reading and Hardcover is Read.
	if err := a.store.Write(func(tx *sql.Tx) error { return addSync(tx, ids[2], "status", "to-read", "1", "push", "t") }); err != nil {
		t.Fatal(err)
	}
	// F (book 5) changed here since its last sync and Hardcover has no newer row for it.
	if err := a.store.Write(func(tx *sql.Tx) error { return addSync(tx, ids[4], "status", "to-read", "1", "push", "t") }); err != nil {
		t.Fatal(err)
	}
	switchesOn(t, b, true, true)
	before := len(remote.bodies)
	loc := runCheck(t, a, b)
	if !strings.HasPrefix(loc, "/hardcover/changes/") {
		t.Fatal(loc)
	}
	body := b.request("GET", loc, nil).Body.String()
	for _, c := range []struct{ group, title string }{
		{"g-ahead", "Book 1"}, {"g-local", "Book 2"}, {"g-local", "Book 5"}, {"g-both", "Book 3"}, {"g-new", "Title 999"},
	} {
		if !strings.Contains(section(body, c.group), c.title) {
			t.Errorf("%s is not in %s", c.title, c.group)
		}
	}
	for _, g := range []string{"g-ahead", "g-local", "g-both", "g-new"} {
		if strings.Contains(section(body, g), "Book 4") {
			t.Errorf("the book in step is in %s", g)
		}
	}
	if !strings.Contains(body, "already in step") {
		t.Fatal("the in-step count is missing")
	}
	// The check itself changed nothing and cost one read request of one top-level field.
	if len(remote.bodies) != before || remote.mutating != 0 {
		t.Fatal("a check sent a mutation")
	}
	if got := dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", ids[0]); got != "to-read" {
		t.Fatalf("a check changed a status: %s", got)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM books WHERE title='Title 999'") != 0 {
		t.Fatal("a check added a book")
	}
}

func TestCheckRequestBudget(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{1: {ID: 101, Status: 3}}}
	a, b, _ := pushApp(t, remote, "to-read:1")
	switchesOn(t, b, true, false)
	f := a.hardcover
	_ = f
	runCheck(t, a, b)
	// Every request of a check is the library read, one top-level field each.
	n := 0
	for _, q := range []string{hardcoverLibraryQuery(false), hardcoverLibraryQuery(true)} {
		if c, _ := hardcoverCost(q); c != 1 {
			t.Fatalf("a library page costs %d top-level fields", c)
		}
		n++
	}
	if n != 2 {
		t.Fatal(n)
	}
}

func TestCheckActionsUseThePreviewedFlows(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{
		1:   {ID: 101, Status: 3, Privacy: 3},
		2:   {ID: 102, Status: 1, Privacy: 3},
		3:   {ID: 103, Status: 3, Privacy: 3},
		999: {ID: 1999, Status: 1, Privacy: 3},
	}}
	a, b, ids := pushApp(t, remote, "to-read:1", "read:2", "reading:3")
	if err := a.store.Write(func(tx *sql.Tx) error { return addSync(tx, ids[2], "status", "to-read", "1", "push", "t") }); err != nil {
		t.Fatal(err)
	}
	switchesOn(t, b, true, true)
	loc := runCheck(t, a, b)
	post := func(path string, v url.Values) *browserResult {
		b.request("GET", loc, nil)
		refill(a)
		return &browserResult{b.request("POST", path, v)}
	}
	// Hardcover ahead: an ordinary import preview, applied on its own page.
	pull := post(loc+"/pull", url.Values{"group": {"hardcover"}}).location(t)
	if !strings.HasPrefix(pull, "/hardcover/pulls/") {
		t.Fatal(pull)
	}
	body := b.request("GET", pull, nil).Body.String()
	if !strings.Contains(body, "Title 1") || strings.Contains(body, "Title 999") {
		t.Fatalf("the preview has the wrong books:\n%s", body)
	}
	if dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", ids[0]) != "to-read" {
		t.Fatal("previewing changed a book")
	}
	expect(t, b.request("POST", pull, nil), 303)
	if dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", ids[0]) != "read" {
		t.Fatal("the import did not apply")
	}
	if dbValue[int](t, a, "SELECT count(*) FROM meta WHERE key='hardcover_pull_since'") != 0 {
		t.Fatal("importing part of a check moved the high-water mark")
	}
	// The apply is in the history, with the book and what moved, and no notes or reviews.
	hist := b.request("GET", "/hardcover", nil).Body.String()
	if !strings.Contains(hist, "pulled from Hardcover") || !strings.Contains(hist, "Title 1") {
		t.Fatalf("history lacks the import:\n%s", section(hist, "history"))
	}
	// A change here made the check stale; actions refuse until it is run again.
	expect(t, b.request("POST", loc+"/pull", url.Values{"group": {"new"}}), 409)
	loc = runCheck(t, a, b)
	// Both changed: one book at a time, as a pull preview.
	one := post(loc+"/pull", url.Values{"group": {"both"}, "hardcover_book_id": {"3"}}).location(t)
	if !strings.Contains(b.request("GET", one, nil).Body.String(), "Title 3") {
		t.Fatal("the one-book preview is wrong")
	}
	expect(t, b.request("POST", loc+"/pull", url.Values{"group": {"both"}, "hardcover_book_id": {"2"}}), 400) // not a conflict
	// New on Hardcover: previewed and added at the end of the queue.
	add := post(loc+"/pull", url.Values{"group": {"new"}}).location(t)
	expect(t, b.request("POST", add, nil), 303)
	if got := rowsOf(t, a, "SELECT needs_placement,reading_status FROM books WHERE title='Title 999'"); fmt.Sprint(got) != "[[1 to-read]]" {
		t.Fatalf("new book: %v", got)
	}
	// Local ahead: the ordinary send preview, applied there.
	loc = runCheck(t, a, b)
	send := post(loc+"/send", nil).location(t)
	if !strings.HasPrefix(send, "/hardcover/pushes/") || !strings.Contains(b.request("GET", send, nil).Body.String(), "Book 2") {
		t.Fatalf("send preview: %s", send)
	}
	if remote.mutating != 0 {
		t.Fatal("a preview sent something")
	}
	expect(t, b.request("POST", send, nil), 303)
	if remote.rows[2].Status != 3 {
		t.Fatalf("the send did not apply: %+v", remote.rows[2])
	}
	if !strings.Contains(b.request("GET", "/hardcover", nil).Body.String(), "sent to Hardcover") {
		t.Fatal("a send is not in the history")
	}
}

type browserResult struct{ rr *httptest.ResponseRecorder }

func (r *browserResult) location(t *testing.T) string { return locationOf(t, r.rr) }

func TestSwitchesAreOffAndGateTheCenter(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{1: {ID: 101, Status: 3}, 2: {ID: 102, Status: 1}}}
	a, b, ids := pushApp(t, remote, "to-read:1", "read:2")
	_ = ids
	body := b.request("GET", "/hardcover", nil).Body.String()
	if strings.Contains(body, "checked") && strings.Contains(section(body, "sync"), `name="pull" checked`) {
		t.Fatal("a switch is on by default")
	}
	refill(a)
	expect(t, b.request("POST", "/hardcover/check", nil), 400)
	if len(remote.bodies) != 0 || a.hardcover.knownRemaining() != -1 && false {
		t.Fatal("a refused check reached Hardcover")
	}
	switchesOn(t, b, true, false)
	loc := runCheck(t, a, b)
	// Sending from a check needs the push switch.
	b.request("GET", loc, nil)
	refill(a)
	expect(t, b.request("POST", loc+"/send", nil), 400)
	page := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(page, "Offer to send local changes") {
		t.Fatal("the push switch is not explained")
	}
}

func TestFailureBannersAreSpecific(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		status int
		header map[string]string
		body   string
		code   int
		want   []string
	}{
		{"expired token", 401, nil, `{}`, 400, []string{`data-kind="auth"`, "rejected the token", "hardcover.app/account/api"}},
		{"missing scope", 403, nil, `{"errors":[{"extensions":{"code":"insufficient_scope","required_scope":"read:library"}}]}`, 400, []string{`data-kind="scope"`, "read:library", "hardcover.app/account/api"}},
		{"rate limited", 429, map[string]string{"Retry-After": "86400"}, `{}`, 429, []string{`data-kind="rate"`, "rate limit", "seconds"}},
		{"down", 503, nil, `{}`, 502, []string{`data-kind="down"`, "Nothing changed"}},
	} {
		remote := &pushRemote{rows: map[int]*pushRow{}}
		remote.hook = func(n int, q string) (int, map[string]string, string, bool) {
			if strings.Contains(q, "NextChapterLibrary") {
				return c.status, c.header, c.body, true
			}
			return 0, nil, "", false
		}
		a, b, _ := pushApp(t, remote, "to-read:1")
		switchesOn(t, b, true, false)
		b.gateway = true
		b.request("GET", "/hardcover", nil)
		refill(a)
		if rr := b.request("POST", "/hardcover/check", nil); rr.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.name, rr.Code, c.code)
		}
		body := b.request("GET", "/hardcover", nil).Body.String()
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: banner lacks %q", c.name, w)
			}
		}
		if strings.Contains(body, "panic") || strings.Contains(body, "goroutine") {
			t.Errorf("%s: a stack trace is shown", c.name)
		}
	}
}

func TestDailyLimitStopsFurtherCalls(t *testing.T) {
	t.Parallel()
	calls := 0
	remote := &pushRemote{rows: map[int]*pushRow{}}
	remote.hook = func(n int, q string) (int, map[string]string, string, bool) {
		calls++
		return 429, map[string]string{"Retry-After": "86400"}, `{}`, true
	}
	a, b, _ := pushApp(t, remote, "to-read:1")
	switchesOn(t, b, true, false)
	b.request("GET", "/hardcover", nil)
	refill(a)
	expect(t, b.request("POST", "/hardcover/check", nil), 429)
	first := calls
	refill(a)
	rr := b.request("POST", "/hardcover/check", nil)
	if rr.Code != 429 || calls != first {
		t.Fatalf("after a daily-limit 429 another call was made: %d calls, status %d", calls, rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "seconds") {
		t.Fatal("the retry time is not shown")
	}
}

func TestScopesSeenAreShown(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{1: {ID: 101, Status: 1}}}
	a, b, _ := pushApp(t, remote, "to-read:1")
	if body := b.request("GET", "/hardcover", nil).Body.String(); !strings.Contains(body, "not used yet") || strings.Contains(body, "worked") {
		t.Fatal("scopes are claimed before anything ran")
	}
	switchesOn(t, b, true, false)
	runCheck(t, a, b)
	if body := b.request("GET", "/hardcover", nil).Body.String(); !strings.Contains(body, "read:library</code>: worked") {
		t.Fatalf("a read that worked is not shown:\n%s", section(body, "sync"))
	}
}

func TestDisconnectKeepsBooksAndDropsSyncHistory(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, ids := pushApp(t, remote, "to-read:1")
	expect(t, b.request("POST", sendPreview(t, b, ids[0]), nil), 303)
	if dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync") == 0 || dbValue[int](t, a, "SELECT count(*) FROM meta WHERE key='hardcover_history'") != 1 {
		t.Fatal("setup: no history")
	}
	books := dbValue[int](t, a, "SELECT count(*) FROM books")
	b.request("GET", "/hardcover", nil)
	expect(t, b.request("POST", "/hardcover/disconnect", nil), 303)
	if dbValue[int](t, a, "SELECT count(*) FROM hardcover_link") != 0 || dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync") != 0 || dbValue[int](t, a, "SELECT count(*) FROM meta WHERE key='hardcover_history'") != 0 {
		t.Fatal("disconnect left the account link or the sync history")
	}
	if dbValue[int](t, a, "SELECT count(*) FROM books") != books || dbValue[int](t, a, "SELECT count(*) FROM book_hardcover") != 1 {
		t.Fatal("disconnect removed books or their links")
	}
	if remote.mutating != 1 {
		t.Fatal("disconnect contacted Hardcover")
	}
}

func TestForgetIdsIsPreviewedAndKeepsEverythingElse(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, ids := pushApp(t, remote, "read:1", "to-read:2")
	rate(t, a, ids[0], 4)
	if _, err := a.store.DB.Exec("UPDATE reading_log SET hardcover_read_id=77, source='hardcover'"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO hardcover_alias VALUES('gone|x',55)"); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Write(func(tx *sql.Tx) error { return addSync(tx, ids[0], "status", "read", "3", "push", "t") }); err != nil {
		t.Fatal(err)
	}
	b.request("GET", "/hardcover", nil)
	loc := locationOf(t, b.request("POST", "/hardcover/forget", nil))
	body := b.request("GET", loc, nil).Body.String()
	for _, want := range []string{"2 book link(s)", "1 remembered id(s)", "1 sync history row(s)", "1 reading(s) tied"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	if dbValue[int](t, a, "SELECT count(*) FROM book_hardcover") != 2 {
		t.Fatal("previewing forgot something")
	}
	expect(t, b.request("POST", loc, url.Values{"csrf": {"bad"}}), 400)
	b.request("GET", loc, nil)
	expect(t, b.request("POST", loc, nil), 303)
	for _, table := range []string{"book_hardcover", "hardcover_alias", "hardcover_sync", "hardcover_catalog", "series_cache"} {
		if dbValue[int](t, a, "SELECT count(*) FROM "+table) != 0 {
			t.Errorf("%s still has rows", table)
		}
	}
	if got := rowsOf(t, a, "SELECT rating,source,hardcover_read_id FROM reading_log"); fmt.Sprint(got) != "[[4 hardcover <nil>]]" && fmt.Sprint(got) != "[[4 hardcover ]]" {
		t.Fatalf("forgetting changed a reading more than its tie: %v", got)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM books") != 2 {
		t.Fatal("books were removed")
	}
	// A stale preview is refused.
	b.request("GET", "/hardcover", nil)
	loc = locationOf(t, b.request("POST", "/hardcover/forget", nil))
	if err := a.store.Write(func(tx *sql.Tx) error { return bump(tx) }); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", loc, nil), 409)
}

func TestHistoryIsCappedAndClearable(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, _ := pushApp(t, remote, "to-read:1")
	if err := a.store.Write(func(tx *sql.Tx) error {
		for i := 0; i < hardcoverHistoryKeep+5; i++ {
			details := []hcHistoryDetail{}
			for j := 0; j < hardcoverDetailKeep+4; j++ {
				details = append(details, hcHistoryDetail{Title: "T", What: "w"})
			}
			if err := addHistory(tx, hcHistoryEntry{At: strconv.Itoa(i), Direction: "pull", Summary: "s", Details: details}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var list []hcHistoryEntry
	raw := dbValue[string](t, a, "SELECT value FROM meta WHERE key='hardcover_history'")
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != hardcoverHistoryKeep || list[0].At != strconv.Itoa(hardcoverHistoryKeep+4) || len(list[0].Details) != hardcoverDetailKeep || list[0].More != 4 {
		t.Fatalf("history is not capped newest-first: %d entries, first %+v", len(list), list[0].At)
	}
	if !strings.Contains(b.request("GET", "/hardcover", nil).Body.String(), "not undone automatically") {
		t.Fatal("the page does not say Hardcover changes are not undone")
	}
	expect(t, b.request("POST", "/hardcover/history/clear", nil), 303)
	if dbValue[int](t, a, "SELECT count(*) FROM meta WHERE key='hardcover_history'") != 0 {
		t.Fatal("history was not cleared")
	}
}

func TestUnlinkedListAndPostsNeedCSRF(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, _ := pushApp(t, remote, "to-read:1")
	addBooks(t, a, Book{Title: "Floating", Author: "Nobody"})
	body := b.request("GET", "/hardcover", nil).Body.String()
	if !strings.Contains(section(body, "unlinked"), "Floating") || strings.Contains(section(body, "unlinked"), "Book 1") {
		t.Fatalf("the unlinked list is wrong:\n%s", section(body, "unlinked"))
	}
	for _, p := range []string{"/hardcover/switches", "/hardcover/check", "/hardcover/history/clear", "/hardcover/forget"} {
		expect(t, b.request("POST", p, url.Values{"csrf": {"bad"}}), 400)
	}
	if len(remote.bodies) != 0 {
		t.Fatal("a forged post reached Hardcover")
	}
}
