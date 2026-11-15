package app

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func loanOf(id int64, title, source, format, status, due, ready string) calendarLoan {
	return calendarLoan{Book: Book{ID: id, Title: title}, Source: source, Format: format, Status: status, Due: due, Ready: ready}
}

func TestBuildCalendar(t *testing.T) {
	t.Parallel()
	loans := []calendarLoan{
		loanOf(1, "Due Soon", "libby", "ebook", "borrowed", "2026-03-02", ""),
		loanOf(1, "Due Soon", "nypl", "print", "borrowed", "2026-03-15", ""), // another source and format
		loanOf(2, "Late", "libby", "audiobook", "borrowed", "2026-02-20", ""),
		loanOf(3, "Pickup", "nypl", "print", "ready", "2026-02-28", ""),
		loanOf(4, "Hold Later", "libby", "ebook", "hold", "", "2026-03-20"),
		loanOf(5, "Hold Passed", "libby", "ebook", "hold", "", "2026-02-10"),
		loanOf(6, "No Date", "libby", "ebook", "borrowed", "", ""),
		loanOf(7, "Hold No Estimate", "libby", "ebook", "hold", "", ""),
		loanOf(8, "Bad Date", "libby", "ebook", "borrowed", "soon", ""),
		loanOf(9, "Outside", "libby", "ebook", "borrowed", "2026-06-30", ""),
	}
	// Today is 27 February 2026: the range crosses a month end in a non-leap year.
	c := buildCalendar(loans, day("2026-02-27"), day("2026-02-27"), 30)
	if c.Start != "2026-02-27" || c.End != "2026-03-28" {
		t.Fatalf("range %s to %s", c.Start, c.End)
	}
	dates := []string{}
	for _, d := range c.Agenda {
		for _, e := range d.Entries {
			dates = append(dates, fmt.Sprintf("%s %s %s %s", d.Date, e.Kind, e.Book.Title, e.Source))
		}
	}
	want := "2026-02-28 pickup Pickup nypl|2026-03-02 due Due Soon libby|2026-03-15 due Due Soon nypl|2026-03-20 hold Hold Later libby"
	if strings.Join(dates, "|") != want {
		t.Fatalf("agenda\n got %s\nwant %s", strings.Join(dates, "|"), want)
	}
	over := []string{}
	for _, e := range c.Overdue {
		over = append(over, fmt.Sprintf("%s %s %v", e.Book.Title, e.Kind, e.Overdue()))
	}
	if strings.Join(over, "|") != "Hold Passed hold false|Late due true" {
		t.Fatalf("overdue %v", over)
	}
	undated := []string{}
	for _, l := range c.Undated {
		undated = append(undated, l.Book.Title)
	}
	if strings.Join(undated, ",") != "Bad Date,Hold No Estimate,No Date" {
		t.Fatalf("undated %v", undated)
	}
	if len(c.Entries) != 4 || c.Truncated {
		t.Fatalf("%d entries", len(c.Entries))
	}
	// Returned, cancelled, and available loans are not commitments.
	gone := buildCalendar([]calendarLoan{
		loanOf(1, "R", "libby", "ebook", "returned", "2026-03-01", ""),
		loanOf(2, "C", "libby", "ebook", "cancelled", "2026-03-01", ""),
		loanOf(3, "A", "libby", "ebook", "available", "2026-03-01", ""),
	}, day("2026-02-27"), day("2026-02-27"), 30)
	if len(gone.Entries)+len(gone.Overdue)+len(gone.Undated) != 3 && len(gone.Entries) != 0 {
		t.Fatal("non-current loans appeared")
	}
	if len(gone.Entries) != 0 || len(gone.Overdue) != 0 {
		t.Fatalf("%+v", gone)
	}
	// Range navigation: the later range holds what the first did not reach.
	later := buildCalendar(loans, day("2026-02-27"), day("2026-03-29"), 30)
	if len(later.Entries) != 0 || len(later.Overdue) != 2 {
		t.Fatalf("later range: %d entries, %d overdue", len(later.Entries), len(later.Overdue))
	}
	// A leap day and a year boundary.
	leap := buildCalendar([]calendarLoan{loanOf(1, "Leap", "libby", "ebook", "borrowed", "2028-02-29", "")}, day("2028-02-27"), day("2028-02-27"), 7)
	if len(leap.Agenda) != 1 || leap.Agenda[0].Date != "2028-02-29" || leap.Agenda[0].Entries[0].Days != 2 || leap.End != "2028-03-04" {
		t.Fatalf("%+v", leap)
	}
	year := buildCalendar([]calendarLoan{loanOf(1, "NY", "libby", "ebook", "borrowed", "2027-01-02", "")}, day("2026-12-20"), day("2026-12-20"), 30)
	if len(year.Agenda) != 1 || year.Agenda[0].Entries[0].Days != 13 || year.End != "2027-01-18" {
		t.Fatalf("%+v", year)
	}
	// Bounded entry count.
	many := []calendarLoan{}
	for i := range maxCalendarEntries + 20 {
		many = append(many, loanOf(int64(i+1), fmt.Sprintf("B%03d", i), "libby", "ebook", "borrowed", "2026-03-01", ""))
	}
	if b := buildCalendar(many, day("2026-02-27"), day("2026-02-27"), 30); len(b.Entries) != maxCalendarEntries || !b.Truncated {
		t.Fatalf("%d entries, truncated %v", len(b.Entries), b.Truncated)
	}
}

func TestCalendarOverlapSummary(t *testing.T) {
	t.Parallel()
	loans := []calendarLoan{
		loanOf(1, "One", "libby", "ebook", "borrowed", "2026-03-01", ""),
		loanOf(2, "Two", "libby", "ebook", "borrowed", "2026-03-03", ""),
		loanOf(3, "Three", "nypl", "print", "borrowed", "2026-03-20", ""),
		loanOf(4, "Hold", "libby", "ebook", "hold", "", "2026-03-02"),
	}
	c := buildCalendar(loans, day("2026-02-27"), day("2026-02-27"), 30)
	got := strings.Join(c.Overlap, "|")
	if !strings.Contains(got, "3 borrowed loans with due dates are open at once, due from 2026-03-01 to 2026-03-20.") || !strings.Contains(got, "2 loans are due within 3 days of each other, 2026-03-01 to 2026-03-03: “One”, “Two”.") || strings.Contains(got, "Hold") || strings.Contains(got, "feasible") {
		t.Fatalf("%q", got)
	}
	if one := buildCalendar(loans[:1], day("2026-02-27"), day("2026-02-27"), 30); len(one.Overlap) != 0 {
		t.Fatal("a single loan overlaps nothing")
	}
}

func TestICSFormatting(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 120) + "; with, commas \\ and\nnewline"
	entries := []calendarEntry{
		{Date: "2028-02-28", Kind: kindDue, Book: Book{ID: 7, Title: long}, Source: "libby", Format: "ebook"},
		{Date: "2028-02-29", Kind: kindPickup, Book: Book{ID: 8, Title: "Leap"}, Source: "nypl", Format: "print"},
		{Date: "2028-12-31", Kind: kindHold, Book: Book{ID: 9, Title: "Year"}, Source: "libby", Format: "audiobook"},
	}
	now := time.Date(2026, 11, 15, 9, 30, 5, 0, time.FixedZone("EST", -5*3600))
	out := buildICS(entries, now)
	if !strings.HasPrefix(out, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n") || !strings.HasSuffix(out, "END:VCALENDAR\r\n") || strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatal("lines are not CRLF")
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
		if len(line) > 75 || !utf8.ValidString(line) {
			t.Fatalf("bad physical line (%d bytes, valid %v): %q", len(line), utf8.ValidString(line), line)
		}
	}
	// Unfolding restores the escaped text exactly.
	unfolded := strings.ReplaceAll(out, "\r\n ", "")
	wantSummary := "SUMMARY:" + icsEscape("Due back: "+long+" (LIBBY ebook)")
	if !strings.Contains(unfolded, wantSummary) || !strings.Contains(unfolded, `\;`) || !strings.Contains(unfolded, `\,`) || !strings.Contains(unfolded, `\\`) || !strings.Contains(unfolded, `\n`) {
		t.Fatalf("escaping:\n%s", unfolded)
	}
	for _, want := range []string{
		"UID:loan-7-libby-ebook-due@next-chapter", "UID:loan-8-nypl-print-pickup@next-chapter", "UID:loan-9-libby-audiobook-hold@next-chapter",
		"DTSTAMP:20261115T143005Z", // 09:30:05 EST is 14:30:05 UTC
		"DTSTART;VALUE=DATE:20280228", "DTEND;VALUE=DATE:20280229",
		"DTSTART;VALUE=DATE:20280229", "DTEND;VALUE=DATE:20280301",
		"DTSTART;VALUE=DATE:20281231", "DTEND;VALUE=DATE:20290101",
		"SUMMARY:Hold expected (estimate): Year (LIBBY audiobook)",
		"Your own estimate of when this hold will be ready",
	} {
		if !strings.Contains(unfolded, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(unfolded, "http") || strings.Contains(unfolded, "METHOD") {
		t.Fatal("the file has a link or a method")
	}
	// Folding never splits a multibyte character: a line of 2-byte runes.
	folded := foldLine("X:" + strings.Repeat("é", 100))
	for _, part := range strings.Split(folded, "\r\n") {
		if len(part) > 75 || !utf8.ValidString(part) {
			t.Fatalf("%q", part)
		}
	}
	if strings.ReplaceAll(folded, "\r\n ", "") != "X:"+strings.Repeat("é", 100) {
		t.Fatal("folding lost text")
	}
	if foldLine("short") != "short" {
		t.Fatal("short lines are unchanged")
	}
}

// calendarApp is a library with a fixed clock and loans set directly.
func calendarApp(t *testing.T) (*App, *browser) {
	t.Helper()
	a := testApp(t, true)
	a.now = func() time.Time { return time.Date(2026, 2, 27, 8, 0, 0, 0, time.UTC) }
	return a, &browser{t: t, handler: a.Handler()}
}

func setLoan(t *testing.T, a *App, title, source, format, status, due, ready string) {
	t.Helper()
	if _, err := a.store.DB.Exec("INSERT OR REPLACE INTO loans(book_id,source,format,status,due_date,observed_at,expected_ready) VALUES(?,?,?,?,?,'2026-02-01T00:00:00.000000000+00:00',?)", bookIDs(t, a)[title], source, format, status, due, ready); err != nil {
		t.Fatal(err)
	}
}

func TestCalendarPageAndDownload(t *testing.T) {
	t.Parallel()
	a, b := calendarApp(t)
	a.ai = newAIClient(AIConfig{Provider: "anthropic", Key: testAIKey, Model: "m"}, failTransport{t})
	addRecords(t, a, records("Alpha", "Beta", "Gamma"))
	secret := "PRIVATE-NOTE-4471"
	ids := bookIDs(t, a)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.store.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE books SET reason=?,pages=300,progress_page=100 WHERE id=?", secret, ids["Alpha"])
	exec("INSERT INTO book_notes(book_id,body,created_at,updated_at) VALUES(?,?,'x','x')", ids["Alpha"], secret)
	exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(?,'2025-01-01',4.75,'manual','m:1')", ids["Beta"])
	setLoan(t, a, "Alpha", "libby", "ebook", "borrowed", "2026-03-04", "")
	setLoan(t, a, "Beta", "nypl", "print", "borrowed", "2026-02-20", "")
	setLoan(t, a, "Gamma", "libby", "ebook", "hold", "", "2026-03-10")
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='reading' WHERE id=?", ids["Alpha"]); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("GET", "/", nil), 200)
	rev, snapshot := revisionOf(t, a), queueSnapshot(t, a)

	rr := b.request("GET", "/calendar", nil)
	expect(t, rr, 200)
	page := rr.Body.String()
	for _, want := range []string{"Overdue", "Beta", "Due back (library deadline)", "Hold expected (your estimate, not confirmed)", "Alpha", "200 pages left · about 40 a day to finish in time", "/calendar.ics?start=2026-02-27&amp;days=30", "later changes here are not sent to your calendar app", `rel="next"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, secret) || strings.Contains(page, "4.75") {
		t.Fatal("the calendar page shows private data")
	}
	// Navigation and bounds.
	if p := b.request("GET", "/calendar?start=2026-03-29&days=14", nil).Body.String(); !strings.Contains(p, "Nothing is due, expected, or waiting for pickup") || !strings.Contains(p, "2026-04-11") {
		t.Fatal("the later range is wrong")
	}
	if p := b.request("GET", "/calendar?start=nonsense&days=500", nil).Body.String(); !strings.Contains(p, "Ignored the start date") || !strings.Contains(p, "Ignored the range length") {
		t.Fatal("bad parameters not reported")
	}

	// The download.
	dl := b.request("GET", "/calendar.ics?start=2026-02-27&days=30", nil)
	expect(t, dl, 200)
	if ct := dl.Header().Get("Content-Type"); ct != "text/calendar; charset=utf-8" {
		t.Fatal(ct)
	}
	if cd := dl.Header().Get("Content-Disposition"); cd != `attachment; filename="next-chapter-borrowing-2026-02-27.ics"` {
		t.Fatal(cd)
	}
	if cc := dl.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatal(cc)
	}
	ics := dl.Body.String()
	if !strings.Contains(ics, "UID:loan-1-libby-ebook-due@next-chapter") || !strings.Contains(ics, "UID:loan-3-libby-ebook-hold@next-chapter") || !strings.Contains(ics, "Hold expected (estimate): Gamma") {
		t.Fatalf("%s", ics)
	}
	for _, private := range []string{secret, "4.75", "http", "password", "csrf"} {
		if strings.Contains(strings.ToLower(ics), strings.ToLower(private)) {
			t.Fatalf("the file contains %q", private)
		}
	}
	if !regexp.MustCompile(`DTSTAMP:20260227T080000Z`).MatchString(ics) {
		t.Fatal("DTSTAMP is not the UTC clock time")
	}
	expect(t, b.request("GET", "/calendar.ics?days=0", nil), 400)
	expect(t, b.request("GET", "/calendar.ics?start=bad", nil), 400)
	expect(t, b.request("GET", "/calendar.ics?days=93", nil), 400)

	// A changed deadline keeps the UID and moves the date; a recorded return
	// removes the commitment; a missing record changes nothing.
	setLoan(t, a, "Alpha", "libby", "ebook", "borrowed", "2026-03-18", "")
	ics2 := b.request("GET", "/calendar.ics?start=2026-02-27&days=30", nil).Body.String()
	if !strings.Contains(ics2, "UID:loan-1-libby-ebook-due@next-chapter") || !strings.Contains(ics2, "DTSTART;VALUE=DATE:20260318") || strings.Contains(ics2, "DTSTART;VALUE=DATE:20260304") {
		t.Fatal("the changed deadline is wrong")
	}
	setLoan(t, a, "Alpha", "libby", "ebook", "returned", "2026-03-18", "")
	if strings.Contains(b.request("GET", "/calendar.ics?start=2026-02-27&days=30", nil).Body.String(), "loan-1-libby") {
		t.Fatal("a returned loan stayed on the calendar")
	}
	// Viewing and downloading change nothing: queue, revision, Undo.
	if revisionOf(t, a) != rev || queueSnapshot(t, a) != snapshot {
		t.Fatal("the calendar changed the library")
	}
}

func TestCalendarLeavesUndoAndNeedsLogin(t *testing.T) {
	t.Parallel()
	a, b := calendarApp(t)
	addRecords(t, a, records("A", "B", "C"))
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	ids := bookIDs(t, a)
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/action", ids["A"]), url.Values{"action": {"down"}}), 303)
	expect(t, b.request("GET", "/calendar", nil), 200)
	expect(t, b.request("GET", "/calendar.ics", nil), 200)
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", "/undo", nil), 303)
	if titles, _ := queueTitles(t, a); strings.Join(titles, ",") != "A,B,C" {
		t.Fatalf("Undo no longer works after viewing the calendar: %v", titles)
	}
	locked := testApp(t, false)
	lb := &browser{t: t, handler: locked.Handler()}
	for _, path := range []string{"/calendar", "/calendar.ics"} {
		if rr := lb.request("GET", path, nil); rr.Code != 303 || !strings.HasPrefix(rr.Header().Get("Location"), "/login") {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Header().Get("Location"))
		}
	}
}
