package app

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The borrowing calendar shows the library commitments the reader has
// recorded: loan due dates, ready-for-pickup deadlines, and their own
// estimates of when holds arrive. It reads current loans only. It never
// contacts a library or any other service, and never moves a book.

const (
	calendarDefaultDays = 30
	calendarMaxDays     = 92
	maxCalendarEntries  = 500
	// clusterDays is how close dated loans must be to be called out together.
	clusterDays = 3
)

// calendarRanges are the range lengths the page offers.
var calendarRanges = []int{7, 14, 30, 60, 90}

// Entry kinds. Due and pickup are firm deadlines from library records; a
// hold arrival is only the reader's own estimate.
const (
	kindDue    = "due"
	kindPickup = "pickup"
	kindHold   = "hold"
)

// calendarLoan is a current loan with what the calendar needs of its book.
type calendarLoan struct {
	Book   Book
	Source string
	Format string
	Status string
	Due    string
	Ready  string // expected_ready
}

// calendarEntry is one dated line of the agenda.
type calendarEntry struct {
	Date   string // YYYY-MM-DD
	Kind   string
	Book   Book
	Source string
	Format string
	// Days is whole days from today to Date (negative when past).
	Days int
	// Pace is what is left to read before a deadline, when progress is known.
	Pace string
}

// Firm reports whether the date is a library deadline rather than an estimate.
func (e calendarEntry) Firm() bool { return e.Kind != kindHold }

// Label names the event in words, so no meaning depends on color.
func (e calendarEntry) Label() string {
	switch e.Kind {
	case kindDue:
		return "Due back (library deadline)"
	case kindPickup:
		return "Pickup deadline (library deadline)"
	}
	return "Hold expected (your estimate, not confirmed)"
}

// Overdue is true for a firm deadline that has passed. A passed estimate is
// reported as an estimate that has passed, never as an overdue loan.
func (e calendarEntry) Overdue() bool { return e.Firm() && e.Days < 0 }

// When says how far away the date is.
func (e calendarEntry) When() string {
	switch {
	case e.Days < 0:
		return fmt.Sprintf("%d %s ago", -e.Days, plural(-e.Days, "day", "days"))
	case e.Days == 0:
		return "today"
	}
	return fmt.Sprintf("in %d %s", e.Days, plural(e.Days, "day", "days"))
}

// calendarDay is the entries of one date, for the agenda.
type calendarDay struct {
	Date    string
	Entries []calendarEntry
}

// calendar is everything the page and the download show, computed from loans
// and a date, so tests can fix both.
type calendar struct {
	Start, End string // inclusive range, YYYY-MM-DD
	Days       int
	Today      string
	Overdue    []calendarEntry
	Agenda     []calendarDay
	// Undated lists current loans with no usable date, as recorded.
	Undated []calendarLoan
	// Overlap is a factual summary of borrowed loans with due dates.
	Overlap []string
	// Truncated is set when the range had more entries than maxCalendarEntries.
	Truncated bool
	// Entries are the range's entries in order, for the download.
	Entries []calendarEntry
}

// loadCalendarLoans reads every current loan with a status that can carry a
// commitment, in one query.
func loadCalendarLoans(q queryer) ([]calendarLoan, error) {
	rows, err := q.Query(`SELECT b.id,b.title,b.author,b.pages,b.progress_page,b.progress_percent,b.progress_seconds,b.audio_seconds,l.source,l.format,l.status,l.due_date,l.expected_ready
FROM loans l JOIN books b ON b.id=l.book_id WHERE l.status IN ('borrowed','ready','hold') ORDER BY l.book_id,l.source,l.format`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []calendarLoan
	for rows.Next() {
		var l calendarLoan
		if err = rows.Scan(&l.Book.ID, &l.Book.Title, &l.Book.Author, &l.Book.Pages, &l.Book.ProgressPage, &l.Book.ProgressPercent, &l.Book.ProgressSeconds, &l.Book.AudioSeconds, &l.Source, &l.Format, &l.Status, &l.Due, &l.Ready); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func isDate(s string) bool { _, err := time.Parse(time.DateOnly, s); return err == nil }

// entryFor turns a loan into a dated entry, or reports that it has none.
func entryFor(l calendarLoan, today time.Time) (calendarEntry, bool) {
	var date, kind string
	switch {
	case l.Status == "borrowed" && isDate(l.Due):
		date, kind = l.Due, kindDue
	case l.Status == "ready" && isDate(l.Due):
		date, kind = l.Due, kindPickup
	case l.Status == "hold" && isDate(l.Ready):
		date, kind = l.Ready, kindHold
	default:
		return calendarEntry{}, false
	}
	days, _ := daysUntil(date, today)
	e := calendarEntry{Date: date, Kind: kind, Book: l.Book, Source: l.Source, Format: l.Format, Days: days}
	if kind == kindDue {
		e.Pace = dueSoon{Book: l.Book, Days: days}.Pace()
	}
	return e, true
}

// buildCalendar sorts loans into overdue, the agenda for [start, start+days),
// and undated. Returned loans, cancelled loans, and loans that are merely
// available are not current commitments and never appear.
func buildCalendar(loans []calendarLoan, today, start time.Time, days int) calendar {
	today, start = midnight(today), midnight(start)
	end := start.AddDate(0, 0, days-1)
	c := calendar{Start: start.Format(time.DateOnly), End: end.Format(time.DateOnly), Days: days, Today: today.Format(time.DateOnly)}
	var dated []calendarEntry
	for _, l := range loans {
		e, ok := entryFor(l, today)
		if !ok {
			c.Undated = append(c.Undated, l)
			continue
		}
		dated = append(dated, e)
	}
	slices.SortFunc(dated, func(a, b calendarEntry) int {
		return cmp.Or(cmp.Compare(a.Date, b.Date), cmp.Compare(a.Kind, b.Kind), cmp.Compare(strings.ToLower(a.Book.Title), strings.ToLower(b.Book.Title)), cmp.Compare(a.Book.ID, b.Book.ID), cmp.Compare(a.Source, b.Source), cmp.Compare(a.Format, b.Format))
	})
	for _, e := range dated {
		// Every past date is listed as overdue, wherever the range is: a firm
		// deadline is overdue, and a passed estimate says so in words.
		if e.Days < 0 {
			c.Overdue = append(c.Overdue, e)
		}
		if e.Date < c.Start || e.Date > c.End {
			continue
		}
		if len(c.Entries) >= maxCalendarEntries {
			c.Truncated = true
			continue
		}
		c.Entries = append(c.Entries, e)
		if e.Days < 0 {
			continue // shown under Overdue, not again in the agenda
		}
		if n := len(c.Agenda); n > 0 && c.Agenda[n-1].Date == e.Date {
			c.Agenda[n-1].Entries = append(c.Agenda[n-1].Entries, e)
		} else {
			c.Agenda = append(c.Agenda, calendarDay{Date: e.Date, Entries: []calendarEntry{e}})
		}
	}
	slices.SortFunc(c.Undated, func(a, b calendarLoan) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Book.Title), strings.ToLower(b.Book.Title)), cmp.Compare(a.Book.ID, b.Book.ID), cmp.Compare(a.Source, b.Source), cmp.Compare(a.Format, b.Format))
	})
	c.Overlap = overlapSummary(dated)
	return c
}

// overlapSummary describes the borrowed loans with due dates without
// inventing checkout dates: how many are out at once, the span of their due
// dates, and which fall within clusterDays of each other. It makes no promise
// about whether they can all be read.
func overlapSummary(dated []calendarEntry) []string {
	var due []calendarEntry
	for _, e := range dated {
		if e.Kind == kindDue {
			due = append(due, e)
		}
	}
	if len(due) < 2 {
		return nil
	}
	out := []string{fmt.Sprintf("%d borrowed loans with due dates are open at once, due from %s to %s.", len(due), due[0].Date, due[len(due)-1].Date)}
	for i := 0; i < len(due); {
		j := i + 1
		for j < len(due) && dayDiff(due[j-1].Date, due[j].Date) <= clusterDays {
			j++
		}
		if j-i >= 2 {
			names := []string{}
			for _, e := range due[i:j] {
				names = append(names, "“"+e.Book.Title+"”")
			}
			out = append(out, fmt.Sprintf("%d loans are due within %d days of each other, %s to %s: %s.", j-i, clusterDays, due[i].Date, due[j-1].Date, strings.Join(names, ", ")))
		}
		i = j
	}
	return out
}

func dayDiff(a, b string) int {
	x, _ := time.Parse(time.DateOnly, a)
	y, _ := time.Parse(time.DateOnly, b)
	return int(y.Sub(x).Hours() / 24)
}

// calendarParams reads start and days from the query. Unreadable values fall
// back to today and 30 days, and ok is false so the page can say so.
func calendarParams(r *http.Request, today time.Time) (start time.Time, days int, notes []string) {
	start, days = midnight(today), calendarDefaultDays
	if s := r.URL.Query().Get("start"); s != "" {
		if t, err := time.Parse(time.DateOnly, s); err == nil && t.Year() >= 1000 {
			start = t
		} else {
			notes = append(notes, "Ignored the start date; showing from today. Use YYYY-MM-DD.")
		}
	}
	if s := r.URL.Query().Get("days"); s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n >= 1 && n <= calendarMaxDays && fmt.Sprint(n) == s {
			days = n
		} else {
			notes = append(notes, fmt.Sprintf("Ignored the range length; showing %d days. The most is %d.", calendarDefaultDays, calendarMaxDays))
		}
	}
	return start, days, notes
}

// calendarPage shows the agenda. It reads the database once and formats
// afterwards; it makes no outbound requests and changes nothing.
func (a *App) calendarPage(w http.ResponseWriter, r *http.Request) error {
	today := a.now()
	start, days, notes := calendarParams(r, today)
	loans, err := loadCalendarLoans(a.store.DB)
	if err != nil {
		return err
	}
	c := buildCalendar(loans, today, start, days)
	step := func(sign int) string {
		return fmt.Sprintf("/calendar?start=%s&days=%d", start.AddDate(0, 0, sign*days).Format(time.DateOnly), days)
	}
	return a.render(w, r, "calendar.html", page{"Title": "Borrowing calendar", "Cal": c, "Notes": notes, "Prev": step(-1), "Next": step(1), "Ranges": calendarRanges, "Entries": len(c.Entries), "Max": maxCalendarEntries}, 200)
}

// calendarDownload sends a one-time .ics snapshot of the selected range. It
// is a manual import for a calendar app, not a subscription: nothing here is
// public, and later changes are not pushed anywhere.
func (a *App) calendarDownload(w http.ResponseWriter, r *http.Request) error {
	now := a.now()
	start, days, notes := calendarParams(r, now)
	if len(notes) > 0 {
		return bad(notes[0])
	}
	loans, err := loadCalendarLoans(a.store.DB)
	if err != nil {
		return err
	}
	c := buildCalendar(loans, now, start, days)
	body := buildICS(c.Entries, now)
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="next-chapter-borrowing-%s.ics"`, c.Start))
	w.Header().Set("Cache-Control", "no-store")
	_, err = w.Write([]byte(body))
	return err
}

// icsEscape escapes text for an iCalendar value (RFC 5545 section 3.3.11).
func icsEscape(s string) string {
	s = strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`).Replace(s)
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// foldLine folds a content line at 75 octets with CRLF and a space, never
// inside a multibyte character.
func foldLine(line string) string {
	const limit = 75
	if len(line) <= limit {
		return line
	}
	var b strings.Builder
	size := 0
	for _, r := range line {
		n := utf8.RuneLen(r)
		if size+n > limit {
			b.WriteString("\r\n ")
			size = 1 // the leading space counts
		}
		b.WriteRune(r)
		size += n
	}
	return b.String()
}

func icsUID(e calendarEntry) string {
	return fmt.Sprintf("loan-%d-%s-%s-%s@next-chapter", e.Book.ID, e.Source, e.Format, e.Kind)
}

// buildICS writes the entries as RFC 5545 all-day events: deterministic UIDs
// (book, source, format, and kind, so a changed date updates the same event
// when re-imported), UTC DTSTAMP, an exclusive next-day DTEND, escaped text,
// CRLF lines. It carries no ratings, notes, credentials, or links.
func buildICS(entries []calendarEntry, now time.Time) string {
	var lines []string
	add := func(l string) { lines = append(lines, foldLine(l)) }
	add("BEGIN:VCALENDAR")
	add("VERSION:2.0")
	add("PRODID:-//Next Chapter//Borrowing calendar//EN")
	add("CALSCALE:GREGORIAN")
	add("X-WR-CALNAME:" + icsEscape("Next Chapter borrowing"))
	stamp := now.UTC().Format("20060102T150405Z")
	for _, e := range entries {
		day, _ := time.Parse(time.DateOnly, e.Date)
		var prefix, note string
		switch e.Kind {
		case kindDue:
			prefix, note = "Due back", "A library deadline from the records you imported or entered."
		case kindPickup:
			prefix, note = "Pickup deadline", "A library deadline from the records you imported or entered."
		default:
			prefix, note = "Hold expected (estimate)", "Your own estimate of when this hold will be ready. The library has not confirmed it."
		}
		summary := fmt.Sprintf("%s: %s (%s %s)", prefix, e.Book.Title, strings.ToUpper(e.Source), e.Format)
		add("BEGIN:VEVENT")
		add("UID:" + icsUID(e))
		add("DTSTAMP:" + stamp)
		add("DTSTART;VALUE=DATE:" + day.Format("20060102"))
		add("DTEND;VALUE=DATE:" + day.AddDate(0, 0, 1).Format("20060102"))
		add("SUMMARY:" + icsEscape(summary))
		add("DESCRIPTION:" + icsEscape(note+" This calendar file is a one-time snapshot; later changes in Next Chapter are not sent to your calendar app."))
		add("TRANSP:TRANSPARENT")
		add("END:VEVENT")
	}
	add("END:VCALENDAR")
	return strings.Join(lines, "\r\n") + "\r\n"
}
