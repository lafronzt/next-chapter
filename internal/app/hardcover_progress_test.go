package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// progFake is a synthetic Hardcover account with one book: an optional read in
// progress, finished reads, and the length of the edition.
type progFake struct {
	mu               sync.Mutex
	hasRead          bool
	status, ub       int
	readID           int
	pages, seconds   int // progress on the open read
	edPages, edAudio int
	starts           map[int]string
	finished         []fakeRead
	mutations        []map[string]any // variables of each mutation
	mutationQueries  []string
	opened           int
	fail             func(query string) (int, string, bool)
	nextID           int
}

type fakeRead struct {
	ID        int
	Date      string
	Precision int
}

func (f *progFake) handle(query string, vars map[string]any, n int) (int, map[string]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil && (strings.Contains(query, "NextChapterUpdateRead") || strings.Contains(query, "NextChapterInsertRead") || strings.Contains(query, "NextChapterOpenRead")) {
		if s, b, ok := f.fail(query); ok {
			return s, nil, b
		}
	}
	reads := func() string {
		out := []string{}
		for _, r := range f.finished {
			out = append(out, fmt.Sprintf(`{"id":%d,"started_at":%q,"finished_at":%q,"finished_at_precision":%d}`, r.ID, f.starts[r.ID], r.Date, r.Precision))
		}
		if f.hasRead {
			out = append(out, fmt.Sprintf(`{"id":%d,"started_at":%q,"finished_at":null}`, f.readID, f.starts[f.readID]))
		}
		return strings.Join(out, ",")
	}
	switch {
	case strings.Contains(query, "NextChapterOpenRead"):
		f.opened++
		open := ""
		if f.hasRead {
			open = fmt.Sprintf(`{"id":%d,"progress_pages":%d,"progress_seconds":%d,"edition":{"pages":%d,"audio_seconds":%d}}`, f.readID, f.pages, f.seconds, f.edPages, f.edAudio)
		}
		return 200, nil, fmt.Sprintf(`{"data":{"user_books":[{"id":%d,"status_id":%d,"book":{"pages":%d},"edition":{"pages":%d,"audio_seconds":%d},"user_book_reads":[%s]}]}}`, f.ub, f.status, f.edPages, f.edPages, f.edAudio, open)
	case strings.Contains(query, "NextChapterShelf"):
		return 200, nil, fmt.Sprintf(`{"data":{"user_books":[{"id":%d,"book_id":500,"status_id":%d,"rating":null,"updated_at":"2026-05-01T00:00:00+00:00","book":%s,"edition":null,"user_book_reads":[%s]}]}}`, f.ub, f.status, hcBook(500, "Prog Book", "Author", 2000), reads())
	case strings.Contains(query, "NextChapterUpdateRead"), strings.Contains(query, "NextChapterInsertRead"):
		f.mutations = append(f.mutations, vars)
		f.mutationQueries = append(f.mutationQueries, query)
		read, _ := vars["read"].(map[string]any)
		key, id := "update", f.readID
		if strings.Contains(query, "Insert") {
			key = "insert"
			f.nextID++
			id = 9000 + f.nextID
			d, _ := read["finished_at"].(string)
			p, _ := read["finished_at_precision"].(float64)
			f.finished = append(f.finished, fakeRead{id, d, int(p)})
		} else if rid, ok := vars["id"].(float64); ok {
			id = int(rid)
			for i := range f.finished {
				if f.finished[i].ID == id {
					if d, ok := read["finished_at"].(string); ok {
						f.finished[i].Date = d
					}
					if p, ok := read["finished_at_precision"].(float64); ok {
						f.finished[i].Precision = int(p)
					}
				}
			}
		}
		if start, ok := read["started_at"]; ok {
			if f.starts == nil {
				f.starts = map[int]string{}
			}
			f.starts[id], _ = start.(string)
		}
		if v, ok := read["progress_pages"].(float64); ok {
			f.pages = int(v)
			if f.edPages > 0 && f.pages >= f.edPages {
				// The server finishes the read and marks the book Read itself.
				f.status, f.hasRead = 3, false
			}
		}
		if v, ok := read["progress_seconds"].(float64); ok {
			f.seconds = int(v)
		}
		return 200, nil, fmt.Sprintf(`{"data":{%q:{"id":%d,"error":null,"user_book_read":{"id":%d}}}}`, key, id, id)
	}
	return 500, nil, `{}`
}

func progApp(t *testing.T, f *progFake, status string) (*App, *browser, int64) {
	t.Helper()
	a, b, _ := newHCApp(t, f.handle)
	linkAccount(t, a)
	id := addBooks(t, a, Book{Title: "Prog Book", Author: "Author"})[0]
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status=?,pages=300,pinned=0 WHERE id=?", status, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO book_hardcover(book_id,hardcover_book_id,linked_at) VALUES(?,500,'2026-01-01T00:00:00Z')", id); err != nil {
		t.Fatal(err)
	}
	b.request("GET", "/hardcover", nil)
	return a, b, id
}

func refill(a *App) {
	a.hardcover.mu.Lock()
	a.hardcover.tokens = 10
	a.hardcover.mu.Unlock()
}

func bookPath(id int64) string { return "/books/" + strconv.FormatInt(id, 10) }

func saveProgress(t *testing.T, a *App, b *browser, id int64, unit, value string) string {
	t.Helper()
	b.request("GET", bookPath(id), nil)
	refill(a)
	return locationOf(t, b.request("POST", bookPath(id)+"/progress", url.Values{"unit": {unit}, "progress": {value}, "back": {"book"}}))
}

func optIn(t *testing.T, b *browser, id int64) {
	t.Helper()
	b.request("GET", bookPath(id), nil)
	expect(t, b.request("POST", bookPath(id)+"/hardcover/progress-sync", url.Values{"enabled": {"1"}}), 303)
}

func TestProgressConversion(t *testing.T) {
	t.Parallel()
	p := func(v int) *int { return &v }
	for _, c := range []struct {
		name       string
		book       Book
		remote     hcOpenRead
		pages, sec int
		skip       bool
	}{
		{"same edition length", Book{Pages: p(300), ProgressPage: p(120)}, hcOpenRead{EdPages: 300}, 120, 0, false},
		{"different edition length goes through percent", Book{Pages: p(300), ProgressPage: p(120)}, hcOpenRead{EdPages: 400}, 160, 0, false},
		{"percent to pages", Book{ProgressPercent: p(25)}, hcOpenRead{EdPages: 400}, 100, 0, false},
		{"percent to audiobook seconds", Book{ProgressPercent: p(50)}, hcOpenRead{EdAudio: 36000}, 0, 18000, false},
		{"pages in an audiobook edition", Book{Pages: p(300), ProgressPage: p(150)}, hcOpenRead{EdAudio: 36000}, 0, 18000, false},
		{"past the end is clamped", Book{ProgressPage: p(480)}, hcOpenRead{EdPages: 300}, 300, 0, false},
		{"no length to convert a percentage", Book{ProgressPercent: p(25)}, hcOpenRead{}, 0, 0, true},
		{"no progress", Book{Pages: p(300)}, hcOpenRead{EdPages: 300}, 0, 0, true},
	} {
		got := progressToRemote(c.book, c.remote)
		if got.Pages != c.pages || got.Seconds != c.sec || (got.Skip != "") != c.skip {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}
	if got := progressToRemote(Book{ProgressPage: p(480)}, hcOpenRead{EdPages: 300}); !strings.Contains(got.Note, "past the end") || !strings.Contains(got.Note, "finishes the read") {
		t.Errorf("clamping is not explained: %q", got.Note)
	}
	for _, c := range []struct {
		name         string
		book         Book
		remote       hcOpenRead
		page, pct    int
		skip, isPage bool
	}{
		{"same length", Book{Pages: p(300), ProgressPage: p(10)}, hcOpenRead{Pages: 150, EdPages: 300}, 150, 0, false, true},
		{"other length", Book{Pages: p(300), ProgressPage: p(10)}, hcOpenRead{Pages: 200, EdPages: 400}, 150, 0, false, true},
		{"percent unit", Book{ProgressPercent: p(10)}, hcOpenRead{Pages: 200, EdPages: 400}, 0, 50, false, false},
		{"audiobook seconds", Book{ProgressPercent: p(10)}, hcOpenRead{Seconds: 9000, EdAudio: 36000}, 0, 25, false, false},
		{"audiobook ignores stale pages", Book{ProgressPercent: p(10)}, hcOpenRead{Pages: 300, EdPages: 300, Seconds: 9000, EdAudio: 36000}, 0, 25, false, false},
		{"audiobook reset ignores stale pages", Book{ProgressPercent: p(10)}, hcOpenRead{Pages: 300, EdPages: 300, EdAudio: 36000}, 0, 0, false, false},
		{"nothing on Hardcover", Book{Pages: p(300)}, hcOpenRead{EdPages: 300}, 0, 0, true, false},
	} {
		got := progressFromRemote(c.book, c.remote)
		switch {
		case (got.Skip != "") != c.skip:
			t.Errorf("%s: %+v", c.name, got)
		case c.skip:
		case c.isPage && (got.Page == nil || *got.Page != c.page), !c.isPage && (got.Percent == nil || *got.Percent != c.pct):
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	for _, c := range []struct {
		last         *hcSyncRow
		l, r, expect string
		equal        bool
	}{
		{nil, "p:1", "p:2", "both", false},
		{nil, "", "p:2", "remote", false},
		{nil, "p:1", "p:1", "same", true},
		{&hcSyncRow{Local: "p:1", Remote: "p:2"}, "p:1", "p:5", "remote", false},
		{&hcSyncRow{Local: "p:1", Remote: "p:2"}, "p:9", "p:2", "local", false},
		{&hcSyncRow{Local: "p:1", Remote: "p:2"}, "p:9", "p:5", "both", false},
	} {
		if got := progressState(c.last, c.l, c.r, c.equal); got != c.expect {
			t.Errorf("progressState(%+v,%s,%s) = %s, want %s", c.last, c.l, c.r, got, c.expect)
		}
	}
	if v := progressVars(7, remoteProgress{Pages: 5}, "2026-01-02T03:04:05Z"); fmt.Sprint(v["read"]) != "map[action_at:2026-01-02T03:04:05Z progress_pages:5]" {
		t.Errorf("backdated update: %v", v)
	}
}

func TestProgressPushIsOptInAndOneMutation(t *testing.T) {
	t.Parallel()
	f := &progFake{hasRead: true, status: 2, ub: 61, readID: 71, edPages: 400}
	a, b, id := progApp(t, f, "reading")
	order := queueOrderOf(t, a)
	// Off by default: saving progress sends nothing.
	if loc := saveProgress(t, a, b, id, "page", "100"); !strings.HasSuffix(loc, "notice=progress-saved") || f.opened != 0 {
		t.Fatalf("a save without the opt-in contacted Hardcover: %s", loc)
	}
	optIn(t, b, id)
	loc := saveProgress(t, a, b, id, "page", "120")
	if !strings.HasSuffix(loc, "notice=hc-progress-sent") {
		t.Fatal(loc)
	}
	if f.opened != 1 || len(f.mutations) != 1 {
		t.Fatalf("%d reads and %d mutations, want one of each", f.opened, len(f.mutations))
	}
	raw, _ := json.Marshal(f.mutations[0])
	if string(raw) != `{"id":71,"read":{"progress_pages":160}}` {
		t.Fatalf("mutation variables: %s", raw)
	}
	if strings.Contains(f.mutationQueries[0], "journal") || strings.Contains(f.mutationQueries[0], "insert_user_book_read") {
		t.Fatalf("a progress request writes a journal entry or a read: %s", f.mutationQueries[0])
	}
	if f.status != 2 {
		t.Fatal("a progress push changed the Hardcover status")
	}
	if got := dbValue[int](t, a, "SELECT progress_page FROM books WHERE id=?", id); got != 120 {
		t.Fatalf("local progress %d", got)
	}
	if queueOrderOf(t, a) != order || dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "reading" {
		t.Fatal("pushing progress changed status or order")
	}
	// Saving the same value again sends nothing new.
	saveProgress(t, a, b, id, "page", "120")
	if len(f.mutations) != 1 {
		t.Fatal("an unchanged value was sent again")
	}
	// Reaching the end of Hardcover's edition finishes the read there, by the
	// server's own rule, and does not finish the book here.
	saveProgress(t, a, b, id, "percent", "100")
	if f.status != 3 {
		t.Fatalf("the fake server did not finish the read: %d", f.status)
	}
	if dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "reading" {
		t.Fatal("Hardcover finishing the read finished the book here")
	}
}

func TestProgressPushUsesReadEditionLength(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, shelf, read, progress, want string
	}{
		{"audio shelf with print book fallback", `{"pages":null,"audio_seconds":36000}`, `null`, "50", `{"progress_seconds":18000}`},
		{"audio read with print shelf", `{"pages":300,"audio_seconds":null}`, `{"pages":null,"audio_seconds":36000}`, "50", `{"progress_seconds":18000}`},
		{"print read with audio shelf", `{"pages":null,"audio_seconds":36000}`, `{"pages":400,"audio_seconds":null}`, "50", `{"progress_pages":200}`},
		{"audio with catalog pages", `null`, `{"pages":300,"audio_seconds":36000}`, "50", `{"progress_seconds":18000}`},
		{"audio reset to zero", `null`, `{"pages":null,"audio_seconds":36000}`, "0", `{"progress_seconds":0}`},
		{"selected edition length unknown", `{"pages":300,"audio_seconds":null}`, `{"pages":null,"audio_seconds":null}`, "50", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &progFake{hasRead: true, status: 2, ub: 61, readID: 71}
			f.fail = func(query string) (int, string, bool) {
				if strings.Contains(query, "NextChapterOpenRead") {
					return 200, fmt.Sprintf(`{"data":{"user_books":[{"id":61,"status_id":2,"book":{"pages":300},"edition":%s,"user_book_reads":[{"id":71,"progress_pages":0,"progress_seconds":20,"edition":%s}]}]}}`, tc.shelf, tc.read), true
				}
				return 0, "", false
			}
			a, b, id := progApp(t, f, "reading")
			optIn(t, b, id)
			loc := saveProgress(t, a, b, id, "percent", tc.progress)
			if tc.want == "" {
				if !strings.HasSuffix(loc, "notice=hc-progress-skip") || len(f.mutations) != 0 {
					t.Fatalf("unknown edition length should not borrow another edition's length: %s", loc)
				}
				return
			}
			if !strings.HasSuffix(loc, "notice=hc-progress-sent") || len(f.mutations) != 1 {
				t.Fatalf("expected progress mutation: %s, %v", loc, f.mutations)
			}
			raw, _ := json.Marshal(f.mutations[0]["read"])
			if string(raw) != tc.want {
				t.Fatalf("got %s, want %s", raw, tc.want)
			}
		})
	}
}

func TestProgressPushFailuresKeepTheLocalSave(t *testing.T) {
	t.Parallel()
	f := &progFake{hasRead: false, status: 1, ub: 62, readID: 72, edPages: 400}
	a, b, id := progApp(t, f, "reading")
	optIn(t, b, id)
	if loc := saveProgress(t, a, b, id, "page", "50"); !strings.HasSuffix(loc, "notice=hc-progress-noread") || len(f.mutations) != 0 {
		t.Fatalf("no read in progress: %s", loc)
	}
	f.hasRead = true
	f.fail = func(q string) (int, string, bool) {
		if strings.Contains(q, "UpdateRead") {
			return 429, `{}`, true
		}
		return 0, "", false
	}
	if loc := saveProgress(t, a, b, id, "page", "60"); !strings.HasSuffix(loc, "notice=hc-progress-failed") {
		t.Fatal(loc)
	}
	if dbValue[int](t, a, "SELECT progress_page FROM books WHERE id=?", id) != 60 {
		t.Fatal("a Hardcover failure undid the local save")
	}
	f.fail = nil
	a.hardcover.mu.Lock()
	a.hardcover.blockedUntil = time.Time{}
	a.hardcover.mu.Unlock()
	f.edPages = 0
	if loc := saveProgress(t, a, b, id, "percent", "30"); !strings.HasSuffix(loc, "notice=hc-progress-skip") {
		t.Fatalf("an unconvertible percentage: %s", loc)
	}
}

func TestProgressPullStatesAndNoOp(t *testing.T) {
	t.Parallel()
	f := &progFake{hasRead: true, status: 2, ub: 63, readID: 73, edPages: 400, pages: 200}
	a, b, id := progApp(t, f, "reading")
	order := queueOrderOf(t, a)
	pull := func() (string, string) {
		b.request("GET", bookPath(id), nil)
		refill(a)
		loc := locationOf(t, b.request("POST", bookPath(id)+"/hardcover/progress/pull", nil))
		return loc, b.request("GET", loc, nil).Body.String()
	}
	// Nothing saved here, and no history: Hardcover's page 200 of 400 is 150 of 300.
	loc, body := pull()
	if !strings.Contains(body, "Converted through 50%") || !strings.Contains(body, "page 150") {
		t.Fatalf("the conversion is not explained:\n%s", body)
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"hardcover"}}), 303)
	if got := dbValue[int](t, a, "SELECT progress_page FROM books WHERE id=?", id); got != 150 {
		t.Fatalf("progress %d, want 150", got)
	}
	// Pull then pull: in step, nothing to apply.
	if _, body = pull(); !strings.Contains(body, "Already in step") {
		t.Fatalf("a second pull is not a no-op:\n%s", body)
	}
	// Only this side changed: nothing to pull.
	if _, err := a.store.DB.Exec("UPDATE books SET progress_page=250 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if _, body = pull(); !strings.Contains(body, "Only your progress has changed") {
		t.Fatalf("local-only change:\n%s", body)
	}
	// Both changed: both values shown, and the default is no change.
	f.mu.Lock()
	f.pages = 300
	f.mu.Unlock()
	loc, body = pull()
	if !strings.Contains(body, "Both sides changed") || !strings.Contains(body, `value="keep" checked`) {
		t.Fatalf("a conflict does not default to no change:\n%s", body)
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"keep"}}), 303)
	if dbValue[int](t, a, "SELECT progress_page FROM books WHERE id=?", id) != 250 {
		t.Fatal("keeping mine changed progress")
	}
	// Choosing Hardcover's takes it, and nothing but progress changes.
	loc, _ = pull()
	expect(t, b.request("POST", loc, url.Values{"use": {"hardcover"}}), 303)
	if dbValue[int](t, a, "SELECT progress_page FROM books WHERE id=?", id) != 225 {
		t.Fatal("taking Hardcover's progress did not apply")
	}
	if queueOrderOf(t, a) != order || dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "reading" || len(f.mutations) != 0 {
		t.Fatal("pulling progress changed status or order, or sent a mutation")
	}
}

func TestProgressPullRefusals(t *testing.T) {
	t.Parallel()
	f := &progFake{hasRead: false, status: 1, ub: 64, readID: 74, edPages: 400}
	a, b, id := progApp(t, f, "reading")
	b.request("GET", bookPath(id), nil)
	expect(t, b.request("POST", bookPath(id)+"/hardcover/progress/pull", nil), 400) // no read in progress
	f.hasRead, f.pages = true, 10
	refill(a)
	loc := locationOf(t, b.request("POST", bookPath(id)+"/hardcover/progress/pull", nil))
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='to-read' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	refill(a)
	expect(t, b.request("POST", bookPath(id)+"/hardcover/progress/pull", nil), 400) // not being read
	if err := a.store.Write(func(tx *sql.Tx) error { return bump(tx) }); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"hardcover"}}), 409) // stale
	expect(t, b.request("POST", loc, url.Values{"csrf": {"bad"}}), 400)
}

func TestSendReadInsertsEachPrecision(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		date string
		prec int
		text string
	}{{"2025-03-03", 1, "2025-03-03"}, {"2025-03-01", 2, "March 2025"}, {"2025-01-01", 3, "2025"}} {
		f := &progFake{status: 3, ub: 65, edPages: 300}
		a, b, id := progApp(t, f, "read")
		if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,finished_precision,source,fingerprint) VALUES(?,?,?,'manual','m1')", id, c.date, c.prec); err != nil {
			t.Fatal(err)
		}
		rid := dbValue[int64](t, a, "SELECT id FROM reading_log")
		b.request("GET", bookPath(id), nil)
		refill(a)
		loc := locationOf(t, b.request("POST", "/readings/"+strconv.FormatInt(rid, 10)+"/hardcover/send", nil))
		if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, c.text) {
			t.Fatalf("preview lacks %q", c.text)
		}
		if len(f.mutations) != 0 {
			t.Fatal("previewing sent a mutation")
		}
		expect(t, b.request("POST", loc, url.Values{"use": {"mine"}}), 303)
		raw, _ := json.Marshal(f.mutations)
		want := fmt.Sprintf(`[{"read":{"finished_at":%q,"finished_at_precision":%d},"ub":65}]`, c.date, c.prec)
		if string(raw) != want {
			t.Fatalf("request: %s, want %s", raw, want)
		}
		if got := dbValue[int](t, a, "SELECT hardcover_read_id FROM reading_log"); got != 9001 {
			t.Fatalf("the reading is not tied to the new Hardcover read: %d", got)
		}
		if dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "read" || f.status != 3 {
			t.Fatal("sending a read changed a status")
		}
		// Round trip: the read is on Hardcover with the same precision, so a
		// second preview has nothing to send.
		b.request("GET", bookPath(id), nil)
		refill(a)
		loc = locationOf(t, b.request("POST", "/readings/"+strconv.FormatInt(rid, 10)+"/hardcover/send", nil))
		if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "already match") {
			t.Fatalf("the round trip did not match for precision %d:\n%s", c.prec, body)
		}
	}
}

func TestSendReadMapUpdateAndReread(t *testing.T) {
	t.Parallel()
	f := &progFake{status: 3, ub: 66, edPages: 300, finished: []fakeRead{{801, "2024-05-05", 1}}}
	a, b, id := progApp(t, f, "read")
	ins := func(date, fp string) int64 {
		if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,source,fingerprint) VALUES(?,?,'goodreads',?)", id, date, fp); err != nil {
			t.Fatal(err)
		}
		return dbValue[int64](t, a, "SELECT id FROM reading_log WHERE fingerprint=?", fp)
	}
	preview := func(rid int64) string {
		b.request("GET", bookPath(id), nil)
		refill(a)
		return locationOf(t, b.request("POST", "/readings/"+strconv.FormatInt(rid, 10)+"/hardcover/send", nil))
	}
	// Hardcover already has this date: tie, send nothing.
	first := ins("2024-05-05", "a")
	loc := preview(first)
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "Nothing is sent; this entry is tied") {
		t.Fatalf("map not explained:\n%s", body)
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"mine"}}), 303)
	if len(f.mutations) != 0 || dbValue[int](t, a, "SELECT hardcover_read_id FROM reading_log WHERE id=?", first) != 801 {
		t.Fatal("the same-day read was not tied without a send")
	}
	// A re-read is a second read on Hardcover and a second entry here.
	second := ins("2025-08-08", "b")
	expect(t, b.request("POST", preview(second), url.Values{"use": {"mine"}}), 303)
	if len(f.finished) != 2 || len(f.mutations) != 1 || dbValue[int](t, a, "SELECT hardcover_read_id FROM reading_log WHERE id=?", second) != 9001 {
		t.Fatalf("a re-read is not a second read: %+v", f.finished)
	}
	// Edit the date here; only this side changed since the sync: mine is the default.
	if _, err := a.store.DB.Exec("UPDATE reading_log SET finished_at='2025-08-09' WHERE id=?", second); err != nil {
		t.Fatal(err)
	}
	loc = preview(second)
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, `value="mine" checked`) {
		t.Fatalf("a local-only edit does not default to sending:\n%s", body)
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"mine"}}), 303)
	raw, _ := json.Marshal(f.mutations[1])
	if string(raw) != `{"id":9001,"read":{"finished_at":"2025-08-09","finished_at_precision":1}}` || len(f.finished) != 2 || f.finished[1].Date != "2025-08-09" {
		t.Fatalf("the edit created a duplicate or sent the wrong body: %s %+v", raw, f.finished)
	}
	// Both sides edited: both shown, default keep, nothing changes until chosen.
	if _, err := a.store.DB.Exec("UPDATE reading_log SET finished_at='2025-08-10' WHERE id=?", second); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.finished[1].Date = "2025-08-11"
	f.mu.Unlock()
	loc = preview(second)
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "Both dates changed") || !strings.Contains(body, `value="keep" checked`) {
		t.Fatalf("a date conflict is not shown with no default change:\n%s", body)
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"keep"}}), 303)
	if len(f.mutations) != 2 || dbValue[string](t, a, "SELECT finished_at FROM reading_log WHERE id=?", second) != "2025-08-10" {
		t.Fatal("keeping both changed something")
	}
	// Taking Hardcover's date sets it here, and a Goodreads row becomes the reader's.
	loc = preview(second)
	expect(t, b.request("POST", loc, url.Values{"use": {"theirs"}}), 303)
	if dbValue[string](t, a, "SELECT finished_at FROM reading_log WHERE id=?", second) != "2025-08-11" || dbValue[string](t, a, "SELECT source FROM reading_log WHERE id=?", second) != "manual" {
		t.Fatal("Hardcover's date was not taken, or the row stayed importable")
	}
	// A timeout may have saved: say to look, and record nothing.
	if _, err := a.store.DB.Exec("UPDATE reading_log SET finished_at='2026-01-01' WHERE id=?", second); err != nil {
		t.Fatal(err)
	}
	loc = preview(second)
	f.fail = func(q string) (int, string, bool) { return 503, `{}`, strings.Contains(q, "UpdateRead") }
	b.gateway = true
	rr := b.request("POST", loc, url.Values{"use": {"mine"}})
	if rr.Code != 502 || !strings.Contains(rr.Body.String(), "may or may not have saved") {
		t.Fatalf("an ambiguous failure is not explained: %d %s", rr.Code, rr.Body)
	}
	// None of this touched status, pins, or loans, and requests carry dates only.
	if dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "read" {
		t.Fatal("status changed")
	}
	for _, m := range f.mutations {
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), "journal") || strings.Contains(string(raw), "progress") || strings.Contains(string(raw), "status") {
			t.Fatalf("a date request carries more than dates: %s", raw)
		}
	}
}

func TestSendReadRefusals(t *testing.T) {
	t.Parallel()
	f := &progFake{status: 3, ub: 67, edPages: 300}
	a, b, id := progApp(t, f, "read")
	if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,source,fingerprint) VALUES(?,'manual','undated')", id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,source,fingerprint) VALUES(?,'2025-01-02','manual','dated')", id); err != nil {
		t.Fatal(err)
	}
	b.request("GET", bookPath(id), nil)
	undated := dbValue[int64](t, a, "SELECT id FROM reading_log WHERE fingerprint='undated'")
	dated := dbValue[int64](t, a, "SELECT id FROM reading_log WHERE fingerprint='dated'")
	expect(t, b.request("POST", fmt.Sprintf("/readings/%d/hardcover/send", undated), nil), 400)
	refill(a)
	loc := locationOf(t, b.request("POST", fmt.Sprintf("/readings/%d/hardcover/send", dated), nil))
	expect(t, b.request("POST", loc, url.Values{"csrf": {"bad"}}), 400)
	expect(t, b.request("POST", loc, url.Values{"use": {"bogus"}}), 400)
	if err := a.store.Write(func(tx *sql.Tx) error { return bump(tx) }); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"mine"}}), 409)
	if len(f.mutations) != 0 {
		t.Fatal("a refused request sent")
	}
}

func TestYearOnlyReadingSortsInItsOwnBucket(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	id := addBooks(t, a, Book{Title: "Sorted", Author: "A"})[0]
	for i, r := range []struct {
		date string
		prec int
	}{{"2025-01-01", 3}, {"2025-06-15", 1}, {"2025-03-01", 2}, {"2024-12-31", 1}, {"2024-01-01", 3}} {
		if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,finished_precision,source,fingerprint) VALUES(?,?,?,'manual',?)", id, r.date, r.prec, fmt.Sprint("f", i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := bookReadings(a.store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{}
	for _, e := range got {
		order = append(order, e.When())
	}
	// Within a year, known days and months first, newest first; the year-only
	// reading after them, never promoted to the year's end or its start.
	if fmt.Sprint(order) != "[2025-06-15 March 2025 2025 2024-12-31 2024]" {
		t.Fatalf("order: %v", order)
	}
}

func TestProgressMigrationOptInAndRestore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:20]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO books(id,title,author,identity,position,added_at,source) VALUES(1,'Old','Author','old|author',0,'2026-01-01T00:00:00Z','manual')`,
		`INSERT INTO book_hardcover(book_id,hardcover_book_id,linked_at) VALUES(1,5,'2026-01-01')`,
		`INSERT INTO hardcover_sync(book_id,field,local_value,remote_value,direction,at) VALUES(1,'status','to-read','1','push','2026-01-01')`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[int](t, path, "SELECT progress_sync FROM book_hardcover") != 0 || scalar[int](t, path, "SELECT count(*) FROM hardcover_sync") != 1 {
		t.Fatal("migration turned progress on, or lost history")
	}
	db, _ = sql.Open("sqlite", path)
	if _, err = db.Exec("INSERT INTO hardcover_sync(book_id,field,local_value,remote_value,direction,at) VALUES(1,'progress','p:1','p:2','push','t'),(1,'dates','a','b','pull','t')"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO hardcover_sync(book_id,field,direction,at) VALUES(1,'bogus','push','t')"); err == nil {
		t.Fatal("an unknown sync field was accepted")
	}
	db.Close()

	f := &progFake{status: 2, ub: 68}
	a, b, id := progApp(t, f, "reading")
	optIn(t, b, id)
	var data []byte
	if err = a.store.Write(func(tx *sql.Tx) error {
		if err := addSync(tx, id, "progress", "p:1", "p:2", "push", "t"); err != nil {
			return err
		}
		x, e := exportLibrary(tx)
		if e != nil {
			return e
		}
		data, e = json.Marshal(x)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	x, problems, err := decodeExport(data)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	target := testApp(t, true)
	if err = target.store.Write(func(tx *sql.Tx) error { _, e := restoreExport(tx, x); return e }); err != nil {
		t.Fatal(err)
	}
	if dbValue[int](t, target, "SELECT progress_sync FROM book_hardcover") != 1 || dbValue[string](t, target, "SELECT field FROM hardcover_sync") != "progress" {
		t.Fatal("restore lost the opt-in or the history")
	}
	// Relinking resets the opt-in.
	if err = a.store.Write(func(tx *sql.Tx) error { return saveBookLink(tx, id, hcCandidate{BookID: 501}, "t") }); err != nil {
		t.Fatal(err)
	}
	if dbValue[int](t, a, "SELECT progress_sync FROM book_hardcover") != 0 {
		t.Fatal("relinking kept the progress opt-in")
	}
}

func TestProgressPostsNeedCSRF(t *testing.T) {
	t.Parallel()
	f := &progFake{hasRead: true, status: 2, ub: 69, readID: 79, edPages: 300}
	a, b, id := progApp(t, f, "reading")
	b.request("GET", bookPath(id), nil)
	for _, path := range []string{bookPath(id) + "/hardcover/progress-sync", bookPath(id) + "/hardcover/progress/pull", "/readings/1/hardcover/send"} {
		expect(t, b.request("POST", path, url.Values{"csrf": {"bad"}}), 400)
	}
	expect(t, b.request("POST", bookPath(id)+"/hardcover/progress-sync", url.Values{"revision": {"999999"}, "enabled": {"1"}}), 409)
	if f.opened != 0 || len(f.mutations) != 0 || a.hardcover == nil {
		t.Fatal("a forged post reached Hardcover")
	}
}
