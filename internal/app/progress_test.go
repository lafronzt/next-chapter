package app

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func intp(n int) *int { return &n }

func TestPaceText(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		left, days int
		want       string
	}{
		{0, 5, ""},
		{-3, 5, ""},
		{100, 4, "100 pages left · about 25 a day to finish in time"},
		{101, 4, "101 pages left · about 26 a day to finish in time"},
		{1, 3, "1 page left · about 1 a day to finish in time"},
		{80, 0, "80 pages left"},
		{80, -2, "80 pages left"},
	} {
		if got := paceText(c.left, c.days); got != c.want {
			t.Errorf("paceText(%d,%d) = %q, want %q", c.left, c.days, got, c.want)
		}
	}
}

func TestBookProgressHelpers(t *testing.T) {
	t.Parallel()
	b := Book{Pages: intp(200), ProgressPage: intp(50)}
	if left, ok := b.PagesLeft(); !ok || left != 150 || b.Percent() != 25 || b.ProgressText() != "page 50 of 200 · 25%" {
		t.Fatalf("%d %v %d %q", left, ok, b.Percent(), b.ProgressText())
	}
	// Past the end never goes negative.
	over := Book{Pages: intp(200), ProgressPage: intp(260)}
	if left, _ := over.PagesLeft(); left != 0 || over.Percent() != 100 {
		t.Fatalf("%d %d", left, over.Percent())
	}
	// No page count: no pace, but the page reached is still shown.
	none := Book{ProgressPage: intp(12)}
	if _, ok := none.PagesLeft(); ok || none.ProgressText() != "page 12" || none.Percent() != 0 {
		t.Fatalf("%v %q", ok, none.ProgressText())
	}
	if (Book{}).ProgressValue() != "" || (Book{Pages: intp(10)}).ProgressText() != "" {
		t.Fatal("untracked book shows progress")
	}
}

func TestSaveReadingProgress(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	two := 300
	rs := records("Reading", "Queued")
	rs[0].Pages = &two
	addRecords(t, a, rs)
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0, reading_status='reading' WHERE title='Reading'"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 200)
	id := bookNamed(t, a, "Reading").ID
	path := fmt.Sprintf("/books/%d/progress", id)
	before := revisionOf(t, a)

	expect(t, b.form(path, url.Values{"page": {"120"}}), 303)
	if got := bookNamed(t, a, "Reading"); got.ProgressPage == nil || *got.ProgressPage != 120 {
		t.Fatalf("%+v", got.ProgressPage)
	}
	// Progress is not a library change: previews and Undo stay valid.
	if revisionOf(t, a) != before {
		t.Fatal("saving a page changed the library revision")
	}
	for _, bad := range []string{"-1", "x", "301", "1.5", "100001"} {
		expect(t, b.form(path, url.Values{"page": {bad}}), 400)
	}
	if got := bookNamed(t, a, "Reading"); *got.ProgressPage != 120 {
		t.Fatal("a rejected page changed the saved one")
	}
	// A book that is not being read has no page to track.
	queued := bookNamed(t, a, "Queued").ID
	expect(t, b.form(fmt.Sprintf("/books/%d/progress", queued), url.Values{"page": {"3"}}), 400)

	body := b.request("GET", "/", nil).Body.String()
	for _, want := range []string{"page 120 of 300 · 40%", `action="/books/` + fmt.Sprint(id) + `/progress"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("home lacks %q", want)
		}
	}
	if !strings.Contains(b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String(), "page 120 of 300") {
		t.Fatal("book page lacks progress")
	}
	// Blank clears.
	expect(t, b.form(path, url.Values{"page": {""}}), 303)
	if bookNamed(t, a, "Reading").ProgressPage != nil {
		t.Fatal("blank did not clear")
	}
	// Finishing clears it.
	expect(t, b.form(path, url.Values{"page": {"299"}}), 303)
	expect(t, b.form(fmt.Sprintf("/books/%d/finish", id), url.Values{"finished_at": {"2026-10-01"}}), 303)
	if got := bookNamed(t, a, "Reading"); got.Status != "read" || got.ProgressPage != nil {
		t.Fatalf("%+v %v", got.Status, got.ProgressPage)
	}
}

func revisionOf(t *testing.T, a *App) int64 {
	t.Helper()
	var n int64
	if err := a.store.DB.QueryRow("SELECT CAST(value AS INTEGER) FROM meta WHERE key='revision'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDueSoonShowsPace(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	pages := 300
	r := dueLoan("Due", time.Now().AddDate(0, 0, 5).Format(time.DateOnly))
	r.Pages = &pages
	addRecords(t, a, []Record{r})
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='reading', progress_page=100 WHERE title='Due'"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/", nil).Body.String()
	if !strings.Contains(body, "200 pages left · about 40 a day to finish in time") {
		t.Fatalf("no pace on home: %s", body)
	}
}

func TestHoldLiftsScoreOnlyNearItsDate(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
	base := Book{Interest: 3}
	plain, _ := score(base, defaultPreferences, today)
	hold := func(day string) Book {
		return Book{Interest: 3, Loans: []Loan{{Status: "hold", ExpectedReady: day}}}
	}
	near, reasons := score(hold("2026-10-06"), defaultPreferences, today)
	if near != plain+10 || !strings.Contains(strings.Join(reasons, "|"), "Hold expected in 5 days") {
		t.Fatalf("%v %v", near, reasons)
	}
	for _, day := range []string{"", "2026-12-01", "garbage"} {
		if got, _ := score(hold(day), defaultPreferences, today); got != plain {
			t.Fatalf("hold %q changed the score to %v", day, got)
		}
	}
	// Ready beats a hold estimate on another format.
	ready := Book{Interest: 3, Loans: []Loan{{Status: "hold", ExpectedReady: "2026-10-06"}, {Status: "ready"}}}
	if got, _ := score(ready, defaultPreferences, today); got != plain+25 {
		t.Fatalf("%v", got)
	}
	// It respects the reader's library timing preference.
	off := defaultPreferences
	off.LibraryWeight = 0
	if got, _ := score(hold("2026-10-06"), off, today); got != plain {
		t.Fatalf("%v", got)
	}
	if got := loanReason(hold("2026-10-06"), today); got != "Library hold expected in 5 days" {
		t.Fatalf("%q", got)
	}
	if got := loanReason(hold("2026-12-06"), today); got != "" {
		t.Fatalf("%q", got)
	}
	if got := loanReason(Book{Loans: []Loan{{Status: "hold", ExpectedReady: "2026-10-06"}, {Status: "ready"}}}, today); got != "Library copy ready for pickup" {
		t.Fatalf("%q", got)
	}
}

func TestExpectedHoldDateLifecycle(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("Waiting", "Other"))
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 200)
	id := bookNamed(t, a, "Waiting").ID
	path := fmt.Sprintf("/books/%d/loan", id)
	soon := time.Now().AddDate(0, 0, 4).Format(time.DateOnly)
	hold := url.Values{"source": {"libby"}, "format": {"ebook"}, "status": {"hold"}, "expected_ready": {soon}}

	expect(t, b.form(path, url.Values{"source": {"libby"}, "format": {"ebook"}, "status": {"hold"}, "expected_ready": {"soon"}}), 400)
	expect(t, b.form(path, hold), 303)
	if got := bookNamed(t, a, "Waiting").Loans; len(got) != 1 || got[0].ExpectedReady != soon {
		t.Fatalf("%+v", got)
	}
	body := b.request("GET", "/", nil).Body.String()
	if !strings.Contains(body, "Holds arriving") || !strings.Contains(body, "Hold expected in 4 days") {
		t.Fatal("home lacks the hold")
	}
	if !strings.Contains(b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String(), "Hold expected "+soon) {
		t.Fatal("book page lacks the hold date")
	}
	// Logging another hold without a date keeps the estimate.
	expect(t, b.form(path, url.Values{"source": {"libby"}, "format": {"ebook"}, "status": {"hold"}}), 303)
	if got := bookNamed(t, a, "Waiting").Loans[0]; got.ExpectedReady != soon {
		t.Fatalf("estimate lost: %+v", got)
	}
	// A date on a non-hold event is ignored, and leaving the hold clears it.
	expect(t, b.form(path, url.Values{"source": {"libby"}, "format": {"ebook"}, "status": {"ready"}, "expected_ready": {soon}}), 303)
	if got := bookNamed(t, a, "Waiting").Loans[0]; got.Status != "ready" || got.ExpectedReady != "" {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(b.request("GET", "/", nil).Body.String(), "Holds arriving") {
		t.Fatal("a ready book is still listed as arriving")
	}
}

func TestProgressAndHoldsSurviveRestoreAndMerge(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	pages := 250
	rs := records("Keeper", "Dupe")
	rs[0].Pages = &pages
	addRecords(t, a, rs)
	soon := time.Now().AddDate(0, 0, 3).Format(time.DateOnly)
	keeper := bookNamed(t, a, "Keeper")
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.form(fmt.Sprintf("/books/%d/loan", keeper.ID), url.Values{"source": {"libby"}, "format": {"ebook"}, "status": {"hold"}, "expected_ready": {soon}}), 303)
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='reading', progress_page=77 WHERE title='Keeper'"); err != nil {
		t.Fatal(err)
	}

	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, exportOf(t, a))
	expect(t, rr, 303)
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	got := bookNamed(t, target, "Keeper")
	if got.ProgressPage == nil || *got.ProgressPage != 77 || len(got.Loans) != 1 || got.Loans[0].ExpectedReady != soon {
		t.Fatalf("restore lost them: %v %+v", got.ProgressPage, got.Loans)
	}

	// Merging a duplicate into the keeper keeps its hold date.
	dupe := bookNamed(t, a, "Dupe")
	err := a.store.Write(func(tx *sql.Tx) error { return rebuildLoans(tx, dupe.ID, keeper.ID) })
	if err != nil {
		t.Fatal(err)
	}
	if loans := bookNamed(t, a, "Keeper").Loans; len(loans) != 1 || loans[0].ExpectedReady != soon {
		t.Fatalf("merge lost the hold date: %+v", loans)
	}
}

func TestMigrateProgressAndHoldsFromVersionTen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:10]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	err = s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO books(id,title,author,identity,reading_status,position,added_at,source) VALUES (1,'Old','Writer','old|writer','reading',0,'2026-10-01T00:00:00.000000000+00:00','manual')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO loans(book_id,source,format,status,due_date,observed_at) VALUES (1,'libby','ebook','hold','','2026-10-01T00:00:00.000000000+00:00')`)
		return err
	})
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 11 {
		t.Fatal(v)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM books b JOIN loans l ON l.book_id=b.id WHERE b.title='Old' AND b.progress_page IS NULL AND l.expected_ready='' AND l.status='hold'"); n != 1 {
		t.Fatal("existing rows changed")
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v11.bak" {
		t.Fatalf("backups %v", b)
	}
}

func TestReadingGoalPace(t *testing.T) {
	t.Parallel()
	mid := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC) // day 181 of 365
	for _, c := range []struct {
		goal, done int
		status     string
	}{
		{24, 10, "2 books behind pace"},
		{25, 11, "1 book behind pace"},
		{24, 12, "On pace"},
		{24, 14, "2 books ahead of pace"},
		{24, 13, "1 book ahead of pace"},
		{24, 24, "Goal reached"},
		{24, 30, "Goal reached"},
		{0, 5, ""},
	} {
		if got := newReadingGoal(c.goal, c.done, mid).Status(); got != c.status {
			t.Errorf("goal %d done %d: %q, want %q", c.goal, c.done, got, c.status)
		}
	}
	g := newReadingGoal(24, 10, mid)
	if g.Needed() != "14 more books to go, about 2.3 a month for the rest of the year." || g.Percent() != 41 {
		t.Fatalf("%q %d", g.Needed(), g.Percent())
	}
	if newReadingGoal(24, 24, mid).Needed() != "" || newReadingGoal(0, 0, mid).Percent() != 0 {
		t.Fatal("finished or unset goal has a to-do")
	}
	// In the last month there is no "a month" to speak of.
	if got := newReadingGoal(12, 3, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)).Needed(); got != "9 more books to go before the year ends." {
		t.Fatalf("%q", got)
	}
}

func TestReadingGoalSettingAndCount(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("A", "B", "C"))
	year := time.Now().Year()
	for _, row := range []struct {
		title, date string
	}{{"A", fmt.Sprintf("%d-01-15", year)}, {"B", fmt.Sprintf("%d-02-01", year)}, {"C", fmt.Sprintf("%d-12-31", year-1)}} {
		if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,source,fingerprint) VALUES((SELECT id FROM books WHERE title=?),?,'manual',?)", row.title, row.date, "fp"+row.title); err != nil {
			t.Fatal(err)
		}
	}
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/reading", nil).Body.String()
	if !strings.Contains(body, "reading goal") || !strings.Contains(body, "Set goal") || strings.Contains(body, "Remove goal") {
		t.Fatal("no goal prompt")
	}
	for _, bad := range []string{"-1", "x", "1001", "2.5"} {
		expect(t, b.form("/reading/goal", url.Values{"goal": {bad}}), 400)
	}
	expect(t, b.form("/reading/goal", url.Values{"goal": {"12"}}), 303)
	body = b.request("GET", "/reading", nil).Body.String()
	if !strings.Contains(body, "<strong>2 of 12</strong>") || !strings.Contains(body, "Remove goal") {
		t.Fatalf("goal not shown: %s", body)
	}
	// Saving ranking preferences leaves the goal alone.
	expect(t, b.form("/settings", url.Values{"section": {"ranking"}, "library_weight": {"1"}, "length": {"any"}}), 303)
	if p, _ := readMeta(a.store.DB, "preferences", defaultPreferences); p.ReadingGoal != 12 {
		t.Fatalf("%+v", p)
	}
	expect(t, b.form("/reading/goal", url.Values{"goal": {"0"}}), 303)
	if p, _ := readMeta(a.store.DB, "preferences", defaultPreferences); p.ReadingGoal != 0 {
		t.Fatalf("%+v", p)
	}
	// The goal never touches the queue.
	if titles, _ := queueTitles(t, a); strings.Join(titles, ",") != "A,B,C" {
		t.Fatalf("%v", titles)
	}
}
