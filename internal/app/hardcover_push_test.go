package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pushRemote is a synthetic Hardcover account that applies mutations.
type pushRemote struct {
	mu       sync.Mutex
	rows     map[int]*pushRow // by Hardcover book id
	bodies   []string         // every mutation request, query and variables
	mutating int
	nextRead int
	// hook may replace the answer to the nth request; ok reports it did.
	hook func(n int, query string) (status int, header map[string]string, body string, ok bool)
	// applyThenHang applies the mutation but answers too late to be heard.
	hang func(n int) bool
}

type pushRow struct {
	ID, Status int
	Rating     float64
	Privacy    int
	Reads      []int
}

func (p *pushRemote) row(hc int) string {
	r := p.rows[hc]
	reads := []string{}
	for _, id := range r.Reads {
		reads = append(reads, fmt.Sprintf(`{"id":%d,"finished_at":"2026-11-17","finished_at_precision":1}`, id))
	}
	rating := "null"
	if r.Rating > 0 {
		rating = strconv.FormatFloat(r.Rating, 'f', -1, 64)
	}
	return fmt.Sprintf(`{"id":%d,"book_id":%d,"status_id":%d,"rating":%s,"privacy_setting_id":%d,"updated_at":"2026-05-01T00:00:00+00:00","book":%s,"edition":null,"user_book_reads":[%s]}`, r.ID, hc, r.Status, rating, r.Privacy, hcBook(hc, fmt.Sprintf("Title %d", hc), "Author", 2000), strings.Join(reads, ","))
}

var mutationField = regexp.MustCompile(`m(\d+): (insert|update)_user_book`)

func (p *pushRemote) handle(query string, vars map[string]any, n int) (int, map[string]string, string) {
	// Hooks and a late answer run outside the lock, so later requests are served meanwhile.
	if p.hook != nil {
		if s, h, b, ok := p.hook(n, query); ok {
			return s, h, b
		}
	}
	s, h, b := p.serve(query, vars, n)
	if p.hang != nil && p.hang(n) {
		time.Sleep(600 * time.Millisecond)
	}
	return s, h, b
}

func (p *pushRemote) serve(query string, vars map[string]any, n int) (int, map[string]string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.Contains(query, "NextChapterShelf"):
		ids, _ := vars["ids"].([]any)
		rows := []string{}
		for _, v := range ids {
			if _, ok := p.rows[int(v.(float64))]; ok {
				rows = append(rows, p.row(int(v.(float64))))
			}
		}
		return 200, nil, `{"data":{"user_books":[` + strings.Join(rows, ",") + `]}}`
	case strings.Contains(query, "NextChapterLibrary"):
		rows := []string{}
		for hc := range p.rows {
			rows = append(rows, p.row(hc))
		}
		return 200, nil, `{"data":{"user_books":[` + strings.Join(rows, ",") + `]}}`
	case strings.Contains(query, "NextChapterPush"):
		raw, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
		p.bodies = append(p.bodies, string(raw))
		p.mutating++
		out := []string{}
		for _, m := range mutationField.FindAllStringSubmatch(query, -1) {
			obj, _ := vars["o"+m[1]].(map[string]any)
			var r *pushRow
			if m[2] == "insert" {
				hc := int(obj["book_id"].(float64))
				r = &pushRow{ID: 7000 + hc, Privacy: int(obj["privacy_setting_id"].(float64))}
				p.rows[hc] = r
			} else {
				id := int(vars["i"+m[1]].(float64))
				for _, c := range p.rows {
					if c.ID == id {
						r = c
					}
				}
			}
			if s, ok := obj["status_id"].(float64); ok {
				// The server starts and finishes reads itself.
				if int(s) == 3 && r.Status != 3 {
					p.nextRead++
					r.Reads = append(r.Reads, 777+p.nextRead)
				}
				r.Status = int(s)
			}
			if v, ok := obj["rating"].(float64); ok {
				r.Rating = v
			}
			reads := []string{}
			for _, id := range r.Reads {
				reads = append(reads, fmt.Sprintf(`{"id":%d}`, id))
			}
			out = append(out, fmt.Sprintf(`"m%s":{"id":%d,"error":null,"user_book":{"id":%d,"status_id":%d,"rating":null,"privacy_setting_id":%d,"user_book_reads":[%s]}}`, m[1], r.ID, r.ID, r.Status, r.Privacy, strings.Join(reads, ",")))
		}
		return 200, nil, `{"data":{` + strings.Join(out, ",") + `}}`
	}
	return 500, nil, `{}`
}

// pushApp makes an app with linked books. Each spec is "status:hardcover id".
func pushApp(t *testing.T, remote *pushRemote, specs ...string) (*App, *browser, []int64) {
	t.Helper()
	a, b, _ := newHCApp(t, remote.handle)
	linkAccount(t, a)
	books := []Book{}
	for i := range specs {
		books = append(books, Book{Title: fmt.Sprintf("Book %d", i+1), Author: "Author"})
	}
	ids := addBooks(t, a, books...)
	for i, spec := range specs {
		status, hc, _ := strings.Cut(spec, ":")
		// Same page count as the fake's, so a later pull has nothing to fill in.
		if _, err := a.store.DB.Exec("UPDATE books SET reading_status=?,pages=300 WHERE id=?", status, ids[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.DB.Exec("INSERT INTO book_hardcover(book_id,hardcover_book_id,linked_at) VALUES(?,?,'2026-01-01T00:00:00Z')", ids[i], hc); err != nil {
			t.Fatal(err)
		}
	}
	b.request("GET", "/hardcover", nil)
	return a, b, ids
}

func sendPreview(t *testing.T, b *browser, ids ...int64) string {
	t.Helper()
	v := url.Values{}
	for _, id := range ids {
		v.Add("book", strconv.FormatInt(id, 10))
	}
	return locationOf(t, b.request("POST", "/hardcover/send", v))
}

func rate(t *testing.T, a *App, id int64, rating float64) {
	t.Helper()
	if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(?,'2025-03-03',?,'manual',?)", id, rating, fmt.Sprint("fp", id)); err != nil {
		t.Fatal(err)
	}
}

func TestHalfStars(t *testing.T) {
	t.Parallel()
	for in, want := range map[float64]float64{0: 0, 0.25: 0.5, 0.5: 0.5, 3.75: 4, 4.25: 4.5, 4.5: 4.5, 4.75: 5, 5: 5, 1.1: 1} {
		if got := halfStars(in); got != want {
			t.Errorf("halfStars(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestClassifyPush(t *testing.T) {
	t.Parallel()
	book := func(status string, rating float64) hcLocalBook {
		return hcLocalBook{ID: 1, Title: "T", Status: status, HardcoverBookID: 5, Rating: rating}
	}
	for _, c := range []struct {
		name   string
		local  hcLocalBook
		remote *hcRemoteEntry
		action string
		target int
		rating float64
	}{
		{"add to want", book("to-read", 0), nil, "add", 1, 0},
		{"add read with rounding", book("read", 4.25), nil, "add", 3, 4.5},
		{"advance", book("reading", 0), &hcRemoteEntry{UserBookID: 9, StatusID: 1}, "update", 2, 0},
		{"finish with rating", book("read", 3.75), &hcRemoteEntry{UserBookID: 9, StatusID: 2}, "update", 3, 4},
		{"same", book("read", 4), &hcRemoteEntry{UserBookID: 9, StatusID: 3, Rating: 4}, "none", 0, 0},
		{"remote ahead", book("to-read", 0), &hcRemoteEntry{UserBookID: 9, StatusID: 3}, "conflict", 0, 0},
		{"remote paused is not overwritten", book("reading", 0), &hcRemoteEntry{UserBookID: 9, StatusID: 4}, "conflict", 0, 0},
		{"remote rating not ours", book("read", 5), &hcRemoteEntry{UserBookID: 9, StatusID: 3, Rating: 3}, "conflict", 0, 0},
		{"remote rating was ours", hcLocalBook{ID: 1, Status: "read", Rating: 5, SyncedRating: 3, HardcoverBookID: 5}, &hcRemoteEntry{UserBookID: 9, StatusID: 3, Rating: 3}, "update", 0, 5},
		{"rating only on a read book", book("reading", 4), &hcRemoteEntry{UserBookID: 9, StatusID: 2}, "none", 0, 0},
		{"archived has no shelf", book("archived", 0), nil, "skip", 0, 0},
	} {
		it := classifyPush(c.local, c.remote, 3)
		if it.Action != c.action || it.Target != c.target || it.SendRating != c.rating {
			t.Errorf("%s: got action=%s target=%d rating=%v, want %s %d %v", c.name, it.Action, it.Target, it.SendRating, c.action, c.target, c.rating)
		}
	}
}

func TestPushAddAdvanceFinish(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{
		12: {ID: 112, Status: 1, Privacy: 3},
		13: {ID: 113, Status: 2, Privacy: 3},
	}}
	a, b, ids := pushApp(t, remote, "to-read:11", "reading:12", "read:13")
	rate(t, a, ids[2], 4.25)
	if _, err := a.store.DB.Exec("UPDATE reading_log SET source='manual'"); err != nil {
		t.Fatal(err)
	}
	// Things that must not be sent, or changed.
	if _, err := a.store.DB.Exec("UPDATE books SET reason='SECRET-REASON' WHERE id=?", ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO book_notes(book_id,body,created_at,updated_at) VALUES(?,'SECRET-NOTE','2026-01-01','2026-01-01')", ids[0]); err != nil {
		t.Fatal(err)
	}
	before := queueOrderOf(t, a)
	statuses := rowsOf(t, a, "SELECT id,reading_status,pinned FROM books ORDER BY id")
	rev := currentRevision(t, a)

	loc := sendPreview(t, b, ids...)
	page := b.request("GET", loc, nil)
	expect(t, page, 200)
	body := page.Body.String()
	for _, want := range []string{"Will add", "Want to Read", "private", "Rating 4.25 will be sent as 4.5", "Currently Reading", "Read"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	if remote.mutating != 0 {
		t.Fatal("previewing sent a mutation")
	}
	expect(t, b.request("POST", loc, nil), 303)
	if remote.mutating != 1 {
		t.Fatalf("%d mutation requests, want one batch", remote.mutating)
	}
	if r := remote.rows[11]; r == nil || r.Status != 1 || r.Privacy != 3 {
		t.Fatalf("book 11 not added to Want to Read, private: %+v", r)
	}
	if r := remote.rows[12]; r.Status != 2 {
		t.Fatalf("book 12: %+v", r)
	}
	if r := remote.rows[13]; r.Status != 3 || r.Rating != 4.5 {
		t.Fatalf("book 13: %+v", r)
	}
	// Request bodies hold ids, status, rating, and privacy only.
	all := strings.Join(remote.bodies, "\n")
	for _, bad := range []string{"SECRET", "review", "private_notes", "reason", "journal", "_user_book_read", "Book 1", "started_at", "finished_at", "position", "pinned"} {
		if strings.Contains(all, bad) {
			t.Errorf("a request carries %q: %s", bad, all)
		}
	}
	for _, key := range regexp.MustCompile(`"(\w+)":`).FindAllStringSubmatch(all, -1) {
		switch key[1] {
		case "query", "variables", "o0", "o1", "o2", "i1", "i2", "book_id", "status_id", "rating", "privacy_setting_id":
		default:
			t.Errorf("unexpected key %q in a mutation request", key[1])
		}
	}
	// Local state is untouched, including the revision.
	if queueOrderOf(t, a) != before || fmt.Sprint(rowsOf(t, a, "SELECT id,reading_status,pinned FROM books ORDER BY id")) != fmt.Sprint(statuses) {
		t.Fatal("sending changed local status, order, or pins")
	}
	if got := dbValue[float64](t, a, "SELECT rating FROM reading_log WHERE book_id=?", ids[2]); got != 4.25 {
		t.Fatalf("local rating became %v", got)
	}
	if currentRevision(t, a) != rev {
		t.Fatal("sending bumped the revision")
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync WHERE field='status'"); n != 3 {
		t.Fatalf("%d status history rows, want 3", n)
	}
	if got := rowsOf(t, a, "SELECT local_value,remote_value FROM hardcover_sync WHERE field='rating'"); fmt.Sprint(got) != "[[4.25 4.5]]" {
		t.Fatal(got)
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync WHERE field='read'"); n != 1 {
		t.Fatalf("%d read rows, want the one Hardcover created", n)
	}

	// Pressing again has nothing left, and a fresh preview shows no change.
	expect(t, b.request("POST", loc, nil), 400)
	b.request("GET", "/hardcover", nil)
	page = b.request("GET", sendPreview(t, b, ids...), nil)
	if strings.Contains(page.Body.String(), "Will add") || strings.Contains(page.Body.String(), "Will update") {
		t.Fatal("a second preview still wants to send")
	}
}

func TestPushThenPullChangesNothing(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{23: {ID: 123, Status: 2, Privacy: 3}}}
	a, b, ids := pushApp(t, remote, "read:23")
	rate(t, a, ids[0], 4.25)
	loc := sendPreview(t, b, ids...)
	expect(t, b.request("POST", loc, nil), 303)
	if remote.rows[23].Status != 3 || len(remote.rows[23].Reads) != 1 {
		t.Fatalf("%+v", remote.rows[23])
	}
	logBefore := rowsOf(t, a, "SELECT id,finished_at,rating,source FROM reading_log")
	b.request("GET", "/hardcover", nil)
	plan := locationOf(t, b.request("POST", "/hardcover/pull", url.Values{"mode": {"full"}}))
	page := b.request("GET", plan, nil)
	expect(t, page, 200)
	if !strings.Contains(page.Body.String(), "Nothing to change") {
		t.Fatalf("a pull after a push changes things:\n%s", page.Body)
	}
	expect(t, b.request("POST", plan, nil), 303)
	if fmt.Sprint(rowsOf(t, a, "SELECT id,finished_at,rating,source FROM reading_log")) != fmt.Sprint(logBefore) {
		t.Fatal("pull changed the reading log after a push")
	}
}

func TestPushPrivacyPreference(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, ids := pushApp(t, remote, "to-read:31", "to-read:32")
	if err := a.store.Write(func(tx *sql.Tx) error {
		p := defaultPreferences
		p.Hardcover, p.HardcoverPrivacy = true, 1
		if err := setMeta(tx, "preferences", p); err != nil {
			return err
		}
		return bump(tx)
	}); err != nil {
		t.Fatal(err)
	}
	b.request("GET", "/hardcover", nil)
	loc := sendPreview(t, b, ids[0])
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "public") || !strings.Contains(body, "show on your Hardcover profile") {
		t.Fatal("a public row is not called out in the preview")
	}
	expect(t, b.request("POST", loc, nil), 303)
	if remote.rows[31].Privacy != 1 {
		t.Fatalf("privacy %d, want the chosen 1", remote.rows[31].Privacy)
	}
	// An out-of-range preference falls back to private.
	if hcPrivacy(Preferences{HardcoverPrivacy: 9}) != 3 || hcPrivacy(Preferences{}) != 3 {
		t.Fatal("privacy default is not private")
	}
}

func TestPushRejectedInA200(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	remote.hook = func(n int, q string) (int, map[string]string, string, bool) {
		if strings.Contains(q, "NextChapterPush") {
			return 200, nil, `{"data":{"m0":{"id":null,"error":"cannot add secret-detail","user_book":null}}}`, true
		}
		return 0, nil, "", false
	}
	a, b, ids := pushApp(t, remote, "to-read:41")
	loc := sendPreview(t, b, ids[0])
	expect(t, b.request("POST", loc, nil), 303)
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "Not sent") || strings.Contains(body, "secret-detail") {
		t.Fatalf("a rejected change is not reported plainly, or leaks provider text:\n%s", body)
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync"); n != 0 {
		t.Fatal("a rejected change was recorded as sent")
	}
	// It can be retried.
	remote.hook = nil
	a.hardcover.tokens = 10
	expect(t, b.request("POST", loc, nil), 303)
	if remote.rows[41] == nil {
		t.Fatal("the retry did not send")
	}
}

func TestPushMissingScope(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	remote.hook = func(n int, q string) (int, map[string]string, string, bool) {
		if strings.Contains(q, "NextChapterPush") {
			return 403, nil, `{"errors":[{"message":"x","extensions":{"code":"insufficient_scope","required_scope":"write:library"}}]}`, true
		}
		return 0, nil, "", false
	}
	a, b, ids := pushApp(t, remote, "to-read:51")
	loc := sendPreview(t, b, ids[0])
	expect(t, b.request("POST", loc, nil), 303)
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "write:library") {
		t.Fatalf("the missing scope is not named:\n%s", body)
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync"); n != 0 || remote.rows[51] != nil {
		t.Fatal("something was recorded or added")
	}
}

func TestPushRateLimitMidBatch(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	specs := []string{}
	for i := 1; i <= 12; i++ {
		specs = append(specs, fmt.Sprintf("to-read:%d", 60+i))
	}
	remote.hook = func(n int, q string) (int, map[string]string, string, bool) {
		if n == 3 {
			return 429, map[string]string{"Retry-After": "30"}, `{}`, true
		}
		return 0, nil, "", false
	}
	a, b, ids := pushApp(t, remote, specs...)
	loc := sendPreview(t, b, ids...)
	expect(t, b.request("POST", loc, nil), 303)
	if len(remote.rows) != 9 {
		t.Fatalf("%d added by the first request, want 9", len(remote.rows))
	}
	// The second request is refused with a 429: the rest stay unsent.
	a.hardcover.mu.Lock()
	a.hardcover.tokens = 10
	a.hardcover.mu.Unlock()
	expect(t, b.request("POST", loc, nil), 303)
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "rate limit") || len(remote.rows) != 9 {
		t.Fatalf("a 429 was not reported, or something was sent: %d rows\n%s", len(remote.rows), body)
	}
	// Once the limit passes, only the three unsent books go, and the sent ones are not resent.
	a.hardcover.mu.Lock()
	a.hardcover.tokens, a.hardcover.blockedUntil = 10, time.Time{}
	a.hardcover.mu.Unlock()
	expect(t, b.request("POST", loc, nil), 303)
	if len(remote.rows) != 12 || remote.mutating != 2 {
		t.Fatalf("%d rows after %d mutation requests, want 12 after 2", len(remote.rows), remote.mutating)
	}
	if last := remote.bodies[len(remote.bodies)-1]; strings.Contains(last, "m3:") {
		t.Fatal("the last request carried more than the three unsent books")
	}
}

func TestPushTimeoutVerifiesBeforeRetry(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	remote.hang = func(n int) bool { return n == 2 }
	a, b, ids := pushApp(t, remote, "to-read:71")
	a.hardcover.http.Timeout = 200 * time.Millisecond
	loc := sendPreview(t, b, ids[0])
	expect(t, b.request("POST", loc, nil), 303)
	if remote.rows[71] == nil {
		t.Fatal("the fake did not apply the write")
	}
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "Unconfirmed") {
		t.Fatalf("a timeout is not shown as unconfirmed:\n%s", body)
	}
	// Pressing again looks first; it must not insert a second time.
	a.hardcover.mu.Lock()
	a.hardcover.tokens = 10
	a.hardcover.mu.Unlock()
	expect(t, b.request("POST", loc, nil), 303)
	if remote.mutating != 1 {
		t.Fatalf("%d mutation requests: the book was sent again before checking", remote.mutating)
	}
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "confirmed on Hardcover") {
		t.Fatalf("the verification is not shown:\n%s", body)
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync WHERE field='status'"); n != 1 {
		t.Fatalf("%d history rows after verification, want 1", n)
	}
}

func TestPushTimeoutNotApplied(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	remote.hook = func(n int, q string) (int, map[string]string, string, bool) {
		if n == 2 {
			time.Sleep(600 * time.Millisecond)
			return 503, nil, `{}`, true
		}
		return 0, nil, "", false
	}
	a, b, ids := pushApp(t, remote, "to-read:81")
	a.hardcover.http.Timeout = 200 * time.Millisecond
	loc := sendPreview(t, b, ids[0])
	expect(t, b.request("POST", loc, nil), 303)
	a.hardcover.mu.Lock()
	a.hardcover.tokens = 10
	a.hardcover.mu.Unlock()
	expect(t, b.request("POST", loc, nil), 303) // verifies: not applied
	if remote.rows[81] != nil || remote.mutating != 0 {
		t.Fatal("sent during the check")
	}
	a.hardcover.mu.Lock()
	a.hardcover.tokens = 10
	a.hardcover.mu.Unlock()
	expect(t, b.request("POST", loc, nil), 303) // now sends
	if remote.rows[81] == nil || remote.mutating != 1 {
		t.Fatal("the retry after a failed check did not send")
	}
}

func TestPushRemoteAheadIsAConflict(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{91: {ID: 191, Status: 3, Privacy: 3}}}
	a, b, ids := pushApp(t, remote, "to-read:91")
	loc := sendPreview(t, b, ids[0])
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "Conflict, not sent") || !strings.Contains(body, "Take Hardcover") {
		t.Fatalf("no conflict shown:\n%s", body)
	}
	expect(t, b.request("POST", loc, nil), 400) // nothing to send
	if remote.mutating != 0 {
		t.Fatal("a conflict was sent")
	}
	// Taking Hardcover's version is an ordinary previewed pull for that book.
	take := locationOf(t, b.request("POST", loc+"/take", url.Values{"book_id": {strconv.FormatInt(ids[0], 10)}}))
	if !strings.HasPrefix(take, "/hardcover/pulls/") {
		t.Fatal(take)
	}
	page := b.request("GET", take, nil)
	if !strings.Contains(page.Body.String(), "Title 91") {
		t.Fatal("the pull preview does not show the book")
	}
	expect(t, b.request("POST", take, nil), 303)
	if got := dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", ids[0]); got != "read" {
		t.Fatalf("taking Hardcover's version left status %q", got)
	}
}

func TestPushRefusesUnlinkedAndStale(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, ids := pushApp(t, remote, "to-read:101")
	other := addBooks(t, a, Book{Title: "Unlinked", Author: "Author"})
	b.request("GET", "/hardcover", nil)
	expect(t, b.request("POST", "/hardcover/send", url.Values{"book": {strconv.FormatInt(other[0], 10)}}), 400)
	expect(t, b.request("POST", "/books/"+strconv.FormatInt(other[0], 10)+"/hardcover/send", nil), 400)
	expect(t, b.request("POST", "/hardcover/send", url.Values{"status": {"archived"}}), 400)
	if remote.mutating != 0 {
		t.Fatal("something was sent")
	}
	// A shelf preview takes the linked books on that shelf.
	if loc := locationOf(t, b.request("POST", "/hardcover/send", url.Values{"status": {"to-read"}})); !strings.HasPrefix(loc, "/hardcover/pushes/") {
		t.Fatal(loc)
	}
	// An out-of-date preview is refused, and sends nothing.
	loc := locationOf(t, b.request("POST", "/books/"+strconv.FormatInt(ids[0], 10)+"/hardcover/send", nil))
	if err := a.store.Write(func(tx *sql.Tx) error { return bump(tx) }); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", loc, nil), 409)
	if !strings.Contains(b.request("GET", loc, nil).Body.String(), "changed since this preview") {
		t.Fatal("a stale preview is not explained")
	}
	if remote.mutating != 0 {
		t.Fatal("a stale preview sent")
	}
}

func TestPushPostsNeedCSRF(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, ids := pushApp(t, remote, "to-read:111")
	loc := sendPreview(t, b, ids[0])
	id := strconv.FormatInt(ids[0], 10)
	for _, path := range []string{"/hardcover/send", "/books/" + id + "/hardcover/send", loc, loc + "/take"} {
		expect(t, b.request("POST", path, url.Values{"csrf": {"bad"}, "book": {id}}), 400)
	}
	if remote.mutating != 0 || a.hardcover == nil {
		t.Fatal("a forged post sent")
	}
}

func TestHardcoverSyncMigrationHistoryAndRestore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:19]); err != nil {
		t.Fatal(err)
	}
	if err := migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if found := backups(t, path); len(found) == 0 {
		t.Fatal("no backup taken")
	}
	if got := scalar[int](t, path, "SELECT count(*) FROM hardcover_sync"); got != 0 {
		t.Fatal("existing books have sync history")
	}

	remote := &pushRemote{rows: map[int]*pushRow{121: {ID: 1121, Status: 2, Privacy: 3}}}
	a, b, ids := pushApp(t, remote, "read:121")
	rate(t, a, ids[0], 4)
	loc := sendPreview(t, b, ids[0])
	expect(t, b.request("POST", loc, nil), 303)
	var data []byte
	if err := a.store.Write(func(tx *sql.Tx) error {
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
	if err != nil || len(problems) > 0 || len(f.HardcoverSync) != 3 {
		t.Fatal(err, problems, len(f.HardcoverSync))
	}
	target := testApp(t, true)
	if err = target.store.Write(func(tx *sql.Tx) error { _, e := restoreExport(tx, f); return e }); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint(rowsOf(t, a, "SELECT field,local_value,remote_value,direction FROM hardcover_sync ORDER BY id"))
	if got := fmt.Sprint(rowsOf(t, target, "SELECT field,local_value,remote_value,direction FROM hardcover_sync ORDER BY id")); got != want {
		t.Fatalf("restore changed the history:\n%s\n%s", got, want)
	}
	// A row for a book that is not in the file is rejected.
	f.HardcoverSync[0].BookID = 9999
	bad, _ := json.Marshal(f)
	if _, problems, _ = decodeExport(bad); len(problems) == 0 {
		t.Fatal("a sync row for an unknown book was accepted")
	}

	// Unlinking, and deleting the book, clear the history.
	if err = a.store.Write(func(tx *sql.Tx) error {
		if err := resetSync(tx, ids[0]); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync"); n != 0 {
		t.Fatal("history survived a reset")
	}
}

func TestPushHistoryResetOnRelinkAndDelete(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, ids := pushApp(t, remote, "to-read:131", "to-read:132")
	expect(t, b.request("POST", sendPreview(t, b, ids...), nil), 303)
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync"); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
	if err := a.store.Write(func(tx *sql.Tx) error {
		return saveBookLink(tx, ids[0], hcCandidate{BookID: 999}, "2026-01-02T00:00:00Z")
	}); err != nil {
		t.Fatal(err)
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync WHERE book_id=?", ids[0]); n != 0 {
		t.Fatal("relinking kept the old history")
	}
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, ids[1]) }); err != nil {
		t.Fatal(err)
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync"); n != 0 {
		t.Fatal("deleting a book kept its history")
	}
}

func dbValue[T any](t *testing.T, a *App, query string, args ...any) T {
	t.Helper()
	var v T
	if err := a.store.DB.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPushRecordsWhatWasSentEvenIfTheLibraryChangedMeanwhile(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	var a *App
	remote.hook = func(n int, q string) (int, map[string]string, string, bool) {
		if strings.Contains(q, "NextChapterPush") {
			// The library changes while the request is in flight.
			if err := a.store.Write(func(tx *sql.Tx) error { return bump(tx) }); err != nil {
				t.Error(err)
			}
		}
		return 0, nil, "", false
	}
	var b *browser
	var ids []int64
	a, b, ids = pushApp(t, remote, "to-read:141")
	loc := sendPreview(t, b, ids[0])
	expect(t, b.request("POST", loc, nil), 303)
	if remote.rows[141] == nil {
		t.Fatal("the send did not happen")
	}
	if n := dbValue[int](t, a, "SELECT count(*) FROM hardcover_sync WHERE field='status'"); n != 1 {
		t.Fatalf("a sent book was not recorded after a concurrent change (%d rows), so it could be sent again", n)
	}
}
