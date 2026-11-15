package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func cp(id int64, day, unit string, value *int, total *int) progressCheckpoint {
	return progressCheckpoint{ID: id, AttemptID: 1, BookID: 1, Day: day, Unit: unit, Value: value, TotalPages: total}
}

func TestEstimatePace(t *testing.T) {
	t.Parallel()
	today := day("2026-03-20")
	n := func(v int) *int { return &v }
	book := Book{ID: 1, Pages: n(400), ProgressPage: n(200)}
	for _, c := range []struct {
		name        string
		cps         []progressCheckpoint
		book        Book
		message     string // substring, or notEnoughProgress
		finish      string
		finishEmpty bool
	}{
		{"two updates a week apart", []progressCheckpoint{cp(1, "2026-03-10", "page", n(100), n(400)), cp(2, "2026-03-17", "page", n(200), n(400))}, book, "From 2026-03-10 to 2026-03-17 you saved 2 updates across 7 days: about 14 pages a day", "around 2026-04-03 (an approximation, about 14 days from today)", false},
		{"nothing saved", nil, book, notEnoughProgress, "", true},
		{"a single update", []progressCheckpoint{cp(1, "2026-03-17", "page", n(200), nil)}, book, notEnoughProgress, "", true},
		{"stale by one day", []progressCheckpoint{cp(1, "2026-02-26", "page", n(100), nil), cp(2, "2026-03-05", "page", n(150), nil)}, book, notEnoughProgress, "", true},
		{"newest exactly 14 days old", []progressCheckpoint{cp(1, "2026-02-28", "page", n(100), nil), cp(2, "2026-03-06", "page", n(150), nil)}, book, "about 8.3 pages a day", "", false},
		{"older than the window is ignored", []progressCheckpoint{cp(1, "2026-02-01", "page", n(10), nil), cp(2, "2026-03-15", "page", n(100), nil), cp(3, "2026-03-19", "page", n(140), nil)}, book, "From 2026-03-15 to 2026-03-19 you saved 2 updates across 4 days: about 10 pages a day", "", false},
		{"only one inside the window", []progressCheckpoint{cp(1, "2026-02-01", "page", n(10), nil), cp(2, "2026-03-19", "page", n(140), nil)}, book, notEnoughProgress, "", true},
		{"a reset starts a fresh run", []progressCheckpoint{cp(1, "2026-03-05", "page", n(10), nil), cp(2, "2026-03-07", "page", n(120), nil), cp(3, "2026-03-09", "page", nil, nil), cp(4, "2026-03-12", "page", n(5), nil), cp(5, "2026-03-18", "page", n(35), nil)}, book, "From 2026-03-12 to 2026-03-18 you saved 2 updates across 6 days: about 5 pages a day", "", false},
		{"progress just cleared", []progressCheckpoint{cp(1, "2026-03-05", "page", n(10), nil), cp(2, "2026-03-09", "page", n(50), nil), cp(3, "2026-03-18", "page", nil, nil)}, book, notEnoughProgress, "", true},
		{"a backward correction is not negative speed", []progressCheckpoint{cp(1, "2026-03-05", "page", n(10), nil), cp(2, "2026-03-10", "page", n(60), nil), cp(3, "2026-03-15", "page", n(40), nil)}, book, notEnoughProgress, "", true},
		{"after a correction the run grows again", []progressCheckpoint{cp(1, "2026-03-05", "page", n(10), nil), cp(2, "2026-03-10", "page", n(60), nil), cp(3, "2026-03-12", "page", n(40), nil), cp(4, "2026-03-17", "page", n(90), nil)}, book, "From 2026-03-12 to 2026-03-17 you saved 2 updates across 5 days: about 10 pages a day", "", false},
		{"a unit switch is not comparable", []progressCheckpoint{cp(1, "2026-03-10", "page", n(100), nil), cp(2, "2026-03-17", "percent", n(60), nil)}, Book{ProgressPercent: n(60)}, notEnoughProgress, "", true},
		{"percent after a unit switch", []progressCheckpoint{cp(1, "2026-03-05", "page", n(100), nil), cp(2, "2026-03-10", "percent", n(20), nil), cp(3, "2026-03-17", "percent", n(48), nil)}, Book{ProgressPercent: n(48)}, "about 4 % of the book a day", "around 2026-04-02 (an approximation, about 13 days from today)", false},
		{"no progress is never extrapolated", []progressCheckpoint{cp(1, "2026-03-10", "page", n(100), nil), cp(2, "2026-03-17", "page", n(100), nil)}, book, notEnoughProgress, "", true},
		{"a page count change breaks the run", []progressCheckpoint{cp(1, "2026-03-10", "page", n(100), n(300)), cp(2, "2026-03-17", "page", n(150), n(400))}, book, notEnoughProgress, "", true},
		{"an unchanged page count keeps it", []progressCheckpoint{cp(1, "2026-03-10", "page", n(100), n(400)), cp(2, "2026-03-17", "page", n(150), n(400))}, book, "about 7.1 pages a day", "", false},
		{"pace without a finish when pages are unknown", []progressCheckpoint{cp(1, "2026-03-10", "page", n(100), nil), cp(2, "2026-03-17", "page", n(170), nil)}, Book{ProgressPage: n(170)}, "about 10 pages a day", "", true},
		{"reaching the end never finishes the book", []progressCheckpoint{cp(1, "2026-03-10", "page", n(300), n(400)), cp(2, "2026-03-17", "page", n(400), n(400))}, Book{Pages: n(400), ProgressPage: n(400)}, "about 14 pages a day", "Mark it finished yourself", false},
		{"too far away", []progressCheckpoint{cp(1, "2026-03-10", "page", n(1), nil), cp(2, "2026-03-17", "page", n(2), nil)}, Book{Pages: n(20000), ProgressPage: n(2)}, "about 0.1 pages a day", "too far away", false},
	} {
		got := estimatePace(c.cps, c.book, nil, today)
		if !strings.Contains(got.Message, c.message) {
			t.Errorf("%s: message %q lacks %q", c.name, got.Message, c.message)
		}
		if c.message == notEnoughProgress && (got.Message != notEnoughProgress || got.Finish != "") {
			t.Errorf("%s: %+v", c.name, got)
		}
		if c.finishEmpty && got.Finish != "" {
			t.Errorf("%s: finish %q", c.name, got.Finish)
		}
		if c.finish != "" && !strings.Contains(got.Finish, c.finish) {
			t.Errorf("%s: finish %q lacks %q", c.name, got.Finish, c.finish)
		}
	}
	// A finish date is compared with a borrowed loan's due date.
	cps := []progressCheckpoint{cp(1, "2026-03-10", "page", n(100), nil), cp(2, "2026-03-17", "page", n(200), nil)}
	got := estimatePace(cps, book, []Loan{
		{Source: "libby", Format: "ebook", Status: "borrowed", DueDate: "2026-03-30"},
		{Source: "nypl", Format: "print", Status: "borrowed", DueDate: "2026-04-10"},
		{Source: "libby", Format: "audiobook", Status: "returned", DueDate: "2026-03-01"},
	}, today)
	if len(got.Deadline) != 2 || !strings.Contains(got.Deadline[0], "4 days after the LIBBY ebook due date (2026-03-30)") || !strings.Contains(got.Deadline[1], "on or before the NYPL print due date (2026-04-10)") {
		t.Fatalf("%v", got.Deadline)
	}
}

func historyOn(t *testing.T, a *App, on bool) {
	t.Helper()
	p := defaultPreferences
	p.ProgressHistory = on
	ensurePreferences(t, a, p)
}

func checkpointRows(t *testing.T, a *App, bookID int64) []string {
	t.Helper()
	rows, err := a.store.DB.Query("SELECT a.id,c.day,c.unit,COALESCE(c.value,-1),c.version FROM progress_checkpoints c JOIN reading_attempts a ON a.id=c.attempt_id WHERE c.book_id=? ORDER BY c.id", bookID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var attempt, value, version int
		var day, unit string
		rows.Scan(&attempt, &day, &unit, &value, &version)
		out = append(out, fmt.Sprintf("a%d %s %s=%d v%d", attempt, day, unit, value, version))
	}
	return out
}

// readingApp is a library with one book being read, a fixed clock, and a browser.
func readingApp(t *testing.T) (*App, *browser, *time.Time, int64) {
	t.Helper()
	a := testApp(t, true)
	clock := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return clock }
	rs := records("Reading", "Other", "Third")
	rs[0].Pages = intp(400)
	addRecords(t, a, rs)
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0; UPDATE books SET reading_status='reading' WHERE title='Reading'"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 200)
	return a, b, &clock, bookIDs(t, a)["Reading"]
}

func save(t *testing.T, b *browser, id int64, value, unit string, want int) {
	t.Helper()
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/progress", id), url.Values{"progress": {value}, "unit": {unit}}), want)
}

func TestProgressHistoryRetention(t *testing.T) {
	t.Parallel()
	a, b, clock, id := readingApp(t)
	// Off by default: only the latest value is kept, and no attempt appears.
	save(t, b, id, "50", "page", 303)
	if rows := checkpointRows(t, a, id); len(rows) != 0 {
		t.Fatalf("history off recorded %v", rows)
	}
	if got := bookNamed(t, a, "Reading"); got.ProgressPage == nil || *got.ProgressPage != 50 {
		t.Fatal("latest value not saved")
	}
	if n := scalarDB[int](t, a, "SELECT count(*) FROM reading_attempts"); n != 0 {
		t.Fatalf("%d attempts with history off", n)
	}
	// Turn it on in Preferences. The book was already being read, so its
	// attempt has no start date and no earlier updates are made up.
	expect(t, b.form("/settings", url.Values{"section": {"privacy"}, "progress_history": {"on"}}), 303)
	if on, _ := progressHistoryOn(a.store.DB); !on {
		t.Fatal("the setting did not save")
	}
	save(t, b, id, "100", "page", 303)
	rows := checkpointRows(t, a, id)
	if len(rows) != 1 || !strings.HasSuffix(rows[0], "2026-03-10 page=100 v1") {
		t.Fatalf("%v", rows)
	}
	if started := scalarDB[string](t, a, "SELECT started_on FROM reading_attempts WHERE book_id="+fmt.Sprint(id)); started != "" {
		t.Fatalf("a start date was invented: %q", started)
	}
	// A second save the same day replaces the day's update.
	save(t, b, id, "120", "page", 303)
	if rows = checkpointRows(t, a, id); len(rows) != 1 || !strings.HasSuffix(rows[0], "page=120 v2") {
		t.Fatalf("%v", rows)
	}
	// Another day adds one; a lower number is a correction, not negative speed.
	*clock = clock.AddDate(0, 0, 3)
	save(t, b, id, "110", "page", 303)
	*clock = clock.AddDate(0, 0, 2)
	save(t, b, id, "", "page", 303) // progress cleared
	if rows = checkpointRows(t, a, id); len(rows) != 3 || !strings.HasSuffix(rows[1], "2026-03-13 page=110 v1") || !strings.HasSuffix(rows[2], "2026-03-15 page=-1 v1") {
		t.Fatalf("%v", rows)
	}
	// Switching units is recorded as such.
	*clock = clock.AddDate(0, 0, 1)
	save(t, b, id, "30", "percent", 303)
	if rows = checkpointRows(t, a, id); !strings.HasSuffix(rows[3], "2026-03-16 percent=30 v1") {
		t.Fatalf("%v", rows)
	}
	// The page labels the updates plainly and explains an unusable pace.
	page := b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String()
	for _, want := range []string{"not measured time spent reading", "Not enough recent progress", "Page 110", "percent", "Progress cleared"} {
		if !strings.Contains(strings.ToLower(page), strings.ToLower(want)) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Reaching the end never finishes the book.
	save(t, b, id, "100", "percent", 303)
	save(t, b, id, "400", "page", 303)
	if got := bookNamed(t, a, "Reading"); got.Status != "reading" {
		t.Fatalf("status %s after reaching the end", got.Status)
	}
	// Turning history off stops new entries and keeps what is there.
	before := len(checkpointRows(t, a, id))
	expect(t, b.form("/settings", url.Values{"section": {"privacy"}}), 303)
	*clock = clock.AddDate(0, 0, 1)
	save(t, b, id, "10", "percent", 303)
	if rows = checkpointRows(t, a, id); len(rows) != before {
		t.Fatalf("history off recorded %v", rows)
	}
	if got := bookNamed(t, a, "Reading"); got.ProgressPercent == nil || *got.ProgressPercent != 10 {
		t.Fatal("latest value not saved while off")
	}
	// Clearing deletes the updates and keeps the current value and open attempt.
	expect(t, b.form("/progress-history/clear", url.Values{}), 400)
	expect(t, b.form("/progress-history/clear", url.Values{"confirm": {"yes"}}), 303)
	if rows = checkpointRows(t, a, id); len(rows) != 0 {
		t.Fatal("updates remain after clearing")
	}
	if got := bookNamed(t, a, "Reading"); got.ProgressPercent == nil || scalarDB[int](t, a, "SELECT count(*) FROM reading_attempts WHERE ended_on IS NULL") != 1 {
		t.Fatal("clearing touched the current progress or the open attempt")
	}
}

func scalarDB[T any](t *testing.T, a *App, query string) T {
	t.Helper()
	var v T
	if err := a.store.DB.QueryRow(query).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAttemptsRereadsAndUndo(t *testing.T) {
	t.Parallel()
	a, b, clock, _ := readingApp(t)
	historyOn(t, a, true)
	ids := bookIDs(t, a)
	other := ids["Other"]
	attemptsOf := func(id int64) string {
		rows, _ := a.store.DB.Query("SELECT id,started_on,COALESCE(ended_on,'open') FROM reading_attempts WHERE book_id=? ORDER BY id", id)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var aid int
			var s, e string
			rows.Scan(&aid, &s, &e)
			out = append(out, fmt.Sprintf("%d:%s>%s", aid, map[bool]string{true: "today", false: s}[s == time.Now().Format(time.DateOnly)], map[bool]string{true: "today", false: e}[e == time.Now().Format(time.DateOnly)]))
		}
		return strings.Join(out, ",")
	}
	// Starting a book opens an attempt dated today.
	expect(t, b.form(fmt.Sprintf("/books/%d/status", other), url.Values{"action": {"start"}}), 303)
	if got := attemptsOf(other); got != "1:today>open" {
		t.Fatal(got)
	}
	save(t, b, other, "20", "page", 303)
	// A queue action's Undo, with progress saved in between, keeps the update.
	third := ids["Third"]
	expect(t, b.form(fmt.Sprintf("/books/%d/action", third), url.Values{"action": {"pin"}}), 303)
	*clock = clock.AddDate(0, 0, 2)
	save(t, b, other, "60", "page", 303)
	expect(t, b.form("/undo", nil), 303)
	if rows := checkpointRows(t, a, other); len(rows) != 2 || !strings.HasPrefix(rows[0], "a1 ") || !strings.HasPrefix(rows[1], "a1 ") {
		t.Fatalf("%v", rows)
	}
	// Stopping ends the attempt; Undo reopens that same attempt.
	expect(t, b.form(fmt.Sprintf("/books/%d/status", other), url.Values{"action": {"stop"}}), 303)
	if got := attemptsOf(other); got != "1:today>today" {
		t.Fatal(got)
	}
	expect(t, b.form("/undo", nil), 303)
	if got := attemptsOf(other); got != "1:today>open" || bookNamed(t, a, "Other").Status != "reading" {
		t.Fatalf("%s %s", got, bookNamed(t, a, "Other").Status)
	}
	// Finish it: the attempt ends, its updates stay, progress clears.
	expect(t, b.form(fmt.Sprintf("/books/%d/finish", other), url.Values{"finished_at": {"2026-03-12"}}), 303)
	if got := attemptsOf(other); got != "1:today>today" {
		t.Fatal(got)
	}
	// A reread is a new attempt; the old one's pace is not reused.
	*clock = clock.AddDate(0, 1, 0)
	edit := bookForm("Other", "reading")
	expect(t, b.form(fmt.Sprintf("/books/%d/edit", other), edit), 303)
	if got := attemptsOf(other); got != "1:today>today,2:2026-04-12>open" {
		t.Fatal(got)
	}
	save(t, b, other, "10", "page", 303)
	rows := checkpointRows(t, a, other)
	if len(rows) != 3 || !strings.HasPrefix(rows[2], "a2 2026-04-12") {
		t.Fatalf("%v", rows)
	}
	page := b.request("GET", fmt.Sprintf("/books/%d", other), nil).Body.String()
	if !strings.Contains(page, "Earlier readings of this book keep their own updates") || !strings.Contains(page, "2 updates") || !strings.Contains(page, "Not enough recent progress") {
		t.Fatalf("page: %s", page)
	}
	// Undo of a start closes the attempt without discarding its updates.
	fresh := ids["Third"]
	expect(t, b.form(fmt.Sprintf("/books/%d/status", fresh), url.Values{"action": {"start"}}), 303)
	save(t, b, fresh, "5", "page", 303)
	expect(t, b.form("/undo", nil), 303)
	if bookNamed(t, a, "Third").Status != "to-read" || len(checkpointRows(t, a, fresh)) != 1 || scalarDB[int](t, a, "SELECT count(*) FROM reading_attempts WHERE ended_on IS NULL AND book_id="+fmt.Sprint(fresh)) != 0 {
		t.Fatal("Undo of a start lost or misplaced progress data")
	}
	// Starting again makes a new attempt that does not pick up the old update.
	expect(t, b.form(fmt.Sprintf("/books/%d/status", fresh), url.Values{"action": {"start"}}), 303)
	save(t, b, fresh, "7", "page", 303)
	if rows = checkpointRows(t, a, fresh); len(rows) != 2 || strings.Split(rows[0], " ")[0] == strings.Split(rows[1], " ")[0] {
		t.Fatalf("updates share an attempt: %v", rows)
	}
}

func TestAttemptsClosedWhileHistoryIsOff(t *testing.T) {
	t.Parallel()
	a, b, _, id := readingApp(t)
	historyOn(t, a, true)
	save(t, b, id, "10", "page", 303)
	// History goes off, the book is finished, and read again: the second
	// reading must not land in the first attempt once history is back on.
	historyOn(t, a, false)
	expect(t, b.form(fmt.Sprintf("/books/%d/finish", id), url.Values{"finished_at": {"2026-03-09"}}), 303)
	expect(t, b.form(fmt.Sprintf("/books/%d/edit", id), bookForm("Reading", "reading")), 303)
	historyOn(t, a, true)
	save(t, b, id, "20", "page", 303)
	rows := checkpointRows(t, a, id)
	if len(rows) != 2 || strings.Split(rows[0], " ")[0] == strings.Split(rows[1], " ")[0] {
		t.Fatalf("the reread joined the old attempt: %v", rows)
	}
}

func TestCheckpointCorrections(t *testing.T) {
	t.Parallel()
	a, b, clock, id := readingApp(t)
	historyOn(t, a, true)
	for _, v := range []string{"50", "120", "200"} {
		save(t, b, id, v, "page", 303)
		*clock = clock.AddDate(0, 0, 2)
	}
	*clock = clock.AddDate(0, 0, -2) // today is the day of the last update, 2026-03-14
	rows := a.store.DB
	var ids []int64
	r, _ := rows.Query("SELECT id FROM progress_checkpoints ORDER BY day")
	for r.Next() {
		var x int64
		r.Scan(&x)
		ids = append(ids, x)
	}
	r.Close()
	if len(ids) != 3 {
		t.Fatalf("%v", ids)
	}
	post := func(cid int64, v url.Values) int {
		return b.form(fmt.Sprintf("/checkpoints/%d", cid), v).Code
	}
	progress := func() string {
		got := bookNamed(t, a, "Reading")
		if got.ProgressPage == nil {
			return "none"
		}
		return fmt.Sprint(*got.ProgressPage)
	}
	// Validation and stale versions change nothing.
	for name, v := range map[string]url.Values{
		"bad date":     {"action": {"save"}, "day": {"soon"}, "value": {"5"}, "version": {"1"}},
		"future date":  {"action": {"save"}, "day": {"2030-01-01"}, "value": {"5"}, "version": {"1"}},
		"negative":     {"action": {"save"}, "day": {"2026-03-10"}, "value": {"-1"}, "version": {"1"}},
		"past the end": {"action": {"save"}, "day": {"2026-03-10"}, "value": {"401"}, "version": {"1"}},
		"no version":   {"action": {"save"}, "day": {"2026-03-10"}, "value": {"5"}},
		"bad action":   {"action": {"explode"}, "version": {"1"}},
	} {
		if code := post(ids[0], v); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code := post(ids[0], url.Values{"action": {"save"}, "day": {"2026-03-12"}, "value": {"5"}, "version": {"7"}}); code != 409 {
		t.Fatalf("stale: %d", code)
	}
	if code := post(ids[0], url.Values{"action": {"save"}, "day": {"2026-03-12"}, "value": {"5"}, "version": {"1"}}); code != 409 {
		t.Fatalf("a date already used: %d", code)
	}
	if code := post(999, url.Values{"action": {"delete"}, "version": {"1"}}); code != 409 {
		t.Fatal(code)
	}
	if progress() != "200" {
		t.Fatal("a rejected edit changed progress")
	}
	// An older update can be corrected without touching the current value.
	if code := post(ids[0], url.Values{"action": {"save"}, "day": {"2026-03-09"}, "value": {"40"}, "version": {"1"}}); code != 303 || progress() != "200" {
		t.Fatalf("older edit: %d, progress %s", code, progress())
	}
	// Correcting the newest one sets the current value to match.
	if code := post(ids[2], url.Values{"action": {"save"}, "day": {"2026-03-14"}, "value": {"180"}, "version": {"1"}}); code != 303 || progress() != "180" {
		t.Fatalf("latest edit: %d, progress %s", code, progress())
	}
	// Deleting the newest returns to the one before it.
	if code := post(ids[2], url.Values{"action": {"delete"}, "version": {"2"}}); code != 303 || progress() != "120" {
		t.Fatalf("latest delete: %d, progress %s", code, progress())
	}
	// Deleting the newest again returns to the one before, which is the
	// corrected older update.
	if code := post(ids[1], url.Values{"action": {"delete"}, "version": {"1"}}); code != 303 || progress() != "40" {
		t.Fatalf("second delete: %d, progress %s", code, progress())
	}
	// With nothing left to agree with, the current value stays.
	post(ids[0], url.Values{"action": {"delete"}, "version": {"2"}})
	if len(checkpointRows(t, a, id)) != 0 || progress() != "40" {
		t.Fatalf("progress %s", progress())
	}
}

func TestProgressHistoryConcurrentSaves(t *testing.T) {
	t.Parallel()
	a, _, _, id := readingApp(t)
	historyOn(t, a, true)
	book := bookNamed(t, a, "Reading")
	var wg sync.WaitGroup
	today := day("2026-03-10")
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := i
			err := a.store.Write(func(tx *sql.Tx) error { return recordCheckpoint(tx, book, "page", &v, today) })
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if rows := checkpointRows(t, a, id); len(rows) != 1 || !strings.HasSuffix(rows[0], "v20") {
		t.Fatalf("%v", rows)
	}
	if n := scalarDB[int](t, a, "SELECT count(*) FROM reading_attempts"); n != 1 {
		t.Fatalf("%d attempts", n)
	}
}

func TestProgressHistoryMergeDeleteRestore(t *testing.T) {
	t.Parallel()
	a, b, clock, id := readingApp(t)
	historyOn(t, a, true)
	save(t, b, id, "50", "page", 303)
	*clock = clock.AddDate(0, 0, 4)
	save(t, b, id, "150", "page", 303)
	ids := bookIDs(t, a)
	// A second book with a finished attempt.
	if _, err := a.store.DB.Exec("INSERT INTO reading_attempts(book_id,started_on,ended_on) VALUES(?,'2026-01-01','2026-01-20')", ids["Other"]); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO progress_checkpoints(attempt_id,book_id,day,unit,value) VALUES(2,?, '2026-01-10','percent',40)", ids["Other"]); err != nil {
		t.Fatal(err)
	}
	original := exportOf(t, a)
	var f exportFile
	if err := json.Unmarshal(original, &f); err != nil || len(f.ReadingAttempts) != 2 || len(f.ProgressCheckpoints) != 3 || f.Preferences == nil || !f.Preferences.ProgressHistory {
		t.Fatalf("%v %d %d", err, len(f.ReadingAttempts), len(f.ProgressCheckpoints))
	}
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, original)
	expect(t, rr, 303)
	if p := tb.request("GET", rr.Header().Get("Location"), nil).Body.String(); !strings.Contains(p, "2 reading attempts with 3 progress updates come along") {
		t.Fatal("preview does not mention progress history")
	}
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	if !equalJSON(normalizedExport(t, exportOf(t, target)), normalizedExport(t, original)) {
		t.Fatal("progress history did not round trip")
	}
	if n := scalarDB[int](t, target, "SELECT count(*) FROM progress_checkpoints c JOIN reading_attempts a ON a.id=c.attempt_id AND a.book_id=c.book_id"); n != 3 {
		t.Fatalf("%d updates joined to their attempts", n)
	}
	// An older export has none, and restores.
	var raw map[string]any
	json.Unmarshal(original, &raw)
	delete(raw, "reading_attempts")
	delete(raw, "progress_checkpoints")
	raw["schema_version"] = 14
	older, _ := json.Marshal(raw)
	fresh := testApp(t, true)
	fb := &browser{t: t, handler: fresh.Handler()}
	expect(t, fb.request("GET", "/imports", nil), 200)
	rr = uploadRestore(fb, older)
	expect(t, rr, 303)
	expect(t, fb.request("POST", rr.Header().Get("Location"), nil), 303)
	if scalarDB[int](t, fresh, "SELECT count(*) FROM reading_attempts") != 0 {
		t.Fatal("attempts from nowhere")
	}
	// Invalid history is refused with nothing written.
	for name, edit := range map[string]func(map[string]any){
		"unknown book":      func(m map[string]any) { m["reading_attempts"].([]any)[0].(map[string]any)["book_id"] = 999 },
		"bad date":          func(m map[string]any) { m["reading_attempts"].([]any)[1].(map[string]any)["started_on"] = "soon" },
		"two open attempts": func(m map[string]any) { m["reading_attempts"].([]any)[1].(map[string]any)["ended_on"] = nil },
		"unknown attempt":   func(m map[string]any) { m["progress_checkpoints"].([]any)[0].(map[string]any)["attempt_id"] = 77 },
		"wrong book":        func(m map[string]any) { m["progress_checkpoints"].([]any)[0].(map[string]any)["book_id"] = 2 },
		"bad unit":          func(m map[string]any) { m["progress_checkpoints"].([]any)[0].(map[string]any)["unit"] = "furlong" },
		"percent too high":  func(m map[string]any) { m["progress_checkpoints"].([]any)[2].(map[string]any)["value"] = 101 },
		"negative value":    func(m map[string]any) { m["progress_checkpoints"].([]any)[0].(map[string]any)["value"] = -5 },
		"repeated day": func(m map[string]any) {
			m["progress_checkpoints"].([]any)[1].(map[string]any)["day"] = m["progress_checkpoints"].([]any)[0].(map[string]any)["day"]
		},
	} {
		var m map[string]any
		json.Unmarshal(original, &m)
		edit(m)
		data, _ := json.Marshal(m)
		other := testApp(t, true)
		ob := &browser{t: t, handler: other.Handler()}
		expect(t, ob.request("GET", "/imports", nil), 200)
		if rr := uploadRestore(ob, data); rr.Code != 400 {
			t.Errorf("%s: status %d", name, rr.Code)
		}
		if countRows(t, other)["books"] != 0 {
			t.Errorf("%s wrote books", name)
		}
	}
	// Merge: the kept book is not being read, so the open attempt ends and
	// every update moves with its own attempt.
	if err := a.store.Write(func(tx *sql.Tx) error { _, err := mergeBooks(tx, ids["Reading"], ids["Other"], true); return err }); err != nil {
		t.Fatal(err)
	}
	if n := scalarDB[int](t, a, fmt.Sprintf("SELECT count(*) FROM reading_attempts WHERE book_id=%d AND ended_on IS NULL", ids["Other"])); n != 0 {
		t.Fatal("an open attempt survived on a book that is not being read")
	}
	if n := scalarDB[int](t, a, fmt.Sprintf("SELECT count(*) FROM progress_checkpoints WHERE book_id=%d", ids["Other"])); n != 3 {
		t.Fatalf("%d updates moved", n)
	}
	// Delete removes them.
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, ids["Other"]) }); err != nil {
		t.Fatal(err)
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM reading_attempts")+scalarDB[int](t, a, "SELECT count(*) FROM progress_checkpoints") != 0 {
		t.Fatal("a deleted book left progress history")
	}
}

func TestMigrateProgressHistoryFromVersionFourteen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:14]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source,reading_status,progress_page) VALUES(1,'Midway','A','midway|a',0,'x','manual','reading',120)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = migrateFile(path, migrations[:15]); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 15 {
		t.Fatal(v)
	}
	// No attempt, start date, or update is made up for a book in progress.
	if n := scalar[int](t, path, "SELECT count(*) FROM reading_attempts") + scalar[int](t, path, "SELECT count(*) FROM progress_checkpoints"); n != 0 {
		t.Fatalf("%d rows created", n)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM books WHERE progress_page=120"); n != 1 {
		t.Fatal("progress changed")
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v15.bak" {
		t.Fatalf("backups %v", b)
	}
}
