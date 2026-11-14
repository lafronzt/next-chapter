package app

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Reading progress, expected library holds, and the yearly reading goal.
// Everything here informs the reader; none of it moves a book on its own.

const (
	// maxProgressPage bounds a page number for a book with no page count.
	maxProgressPage = 100000
	// holdSoonDays is how far ahead an expected hold date counts for ranking
	// and the home screen.
	holdSoonDays = 14
	// maxReadingGoal is the largest yearly goal accepted.
	maxReadingGoal = 1000
)

// midnight drops the time of day, so day counts are whole days.
func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// daysUntil is the number of whole days from today to a YYYY-MM-DD date
// (negative once past), and false when the date is empty or unreadable.
func daysUntil(day string, today time.Time) (int, bool) {
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return 0, false
	}
	return int(d.Sub(midnight(today)).Hours() / 24), true
}

// ProgressValue is the page reached, for a form field ("" when not tracked).
func (b Book) ProgressValue() string {
	if b.ProgressPage == nil {
		return ""
	}
	return strconv.Itoa(*b.ProgressPage)
}

// PagesLeft is how many pages remain, and false when the page count is unknown.
func (b Book) PagesLeft() (int, bool) {
	if b.Pages == nil || *b.Pages <= 0 {
		return 0, false
	}
	read := 0
	if b.ProgressPage != nil {
		read = min(*b.ProgressPage, *b.Pages)
	}
	return *b.Pages - read, true
}

// Percent is how far through the book the reader is, 0 to 100.
func (b Book) Percent() int {
	if b.Pages == nil || *b.Pages <= 0 || b.ProgressPage == nil {
		return 0
	}
	return min(100, *b.ProgressPage*100 / *b.Pages)
}

// ProgressText says where the reader is, or "" when there is nothing to say.
func (b Book) ProgressText() string {
	switch {
	case b.ProgressPage == nil:
		return ""
	case b.Pages != nil && *b.Pages > 0:
		return fmt.Sprintf("page %d of %d · %d%%", *b.ProgressPage, *b.Pages, b.Percent())
	}
	return fmt.Sprintf("page %d", *b.ProgressPage)
}

// paceText says how much reading is left before a due date: pages left, and
// pages a day to finish on time. It is a fact about the arithmetic, not a
// nudge, and says nothing once the book is done.
func paceText(left, days int) string {
	switch {
	case left <= 0:
		return ""
	case days <= 0:
		return fmt.Sprintf("%d %s left", left, plural(left, "page", "pages"))
	}
	per := (left + days - 1) / days
	return fmt.Sprintf("%d %s left · about %d a day to finish in time", left, plural(left, "page", "pages"), per)
}

// Pace is paceText for a loan coming due; "" when the page count is unknown.
func (d dueSoon) Pace() string {
	left, ok := d.Book.PagesLeft()
	if !ok {
		return ""
	}
	return paceText(left, d.Days)
}

// holdText describes an expected hold date relative to today.
func holdText(days int) string {
	switch {
	case days < 0:
		return fmt.Sprintf("Hold was expected %d %s ago", -days, plural(-days, "day", "days"))
	case days == 0:
		return "Hold expected today"
	}
	return fmt.Sprintf("Hold expected in %d %s", days, plural(days, "day", "days"))
}

// holdSoon is a queued or reading book whose hold is expected to be ready soon.
type holdSoon struct {
	Book Book
	Loan Loan
	Days int
}

// Text says when the hold is expected.
func (h holdSoon) Text() string { return holdText(h.Days) }

// loadHolds lists holds with an expected date within holdSoonDays (or already
// past), soonest first.
func loadHolds(q queryer, today time.Time) ([]holdSoon, error) {
	limit := midnight(today).AddDate(0, 0, holdSoonDays).Format(time.DateOnly)
	rows, err := q.Query(`SELECT l.book_id,l.source,l.format,l.status,l.due_date,l.observed_at,l.expected_ready
FROM loans l JOIN books b ON b.id=l.book_id
WHERE l.status='hold' AND l.expected_ready!='' AND l.expected_ready<=? AND b.reading_status IN ('to-read','reading')
ORDER BY l.expected_ready,l.book_id`, limit)
	if err != nil {
		return nil, err
	}
	var loans []Loan
	for rows.Next() {
		var l Loan
		if err = rows.Scan(&l.BookID, &l.Source, &l.Format, &l.Status, &l.DueDate, &l.ObservedAt, &l.ExpectedReady); err != nil {
			rows.Close()
			return nil, err
		}
		loans = append(loans, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(loans) == 0 {
		return nil, err
	}
	ids := make([]int64, len(loans))
	for i, l := range loans {
		ids[i] = l.BookID
	}
	books, err := getBooks(q, ids)
	if err != nil {
		return nil, err
	}
	var out []holdSoon
	for _, l := range loans {
		days, ok := daysUntil(l.ExpectedReady, today)
		if b, found := books[l.BookID]; found && ok {
			out = append(out, holdSoon{Book: b, Loan: l, Days: days})
		}
	}
	return out, nil
}

// bookProgress saves the page a reader has reached in a book they are
// reading. A blank page clears it. It changes only that number: it never
// moves a book, finishes one, or invalidates a preview or Undo.
func (a *App) bookProgress(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	raw := strings.TrimSpace(r.FormValue("page"))
	var page *int
	if raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 0 || n > maxProgressPage {
			return bad("Enter the page you are on as a whole number, or leave it blank.")
		}
		page = &n
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		b, err := getBook(tx, id)
		if err != nil {
			return err
		}
		if b.Status != "reading" {
			return bad("Only a book you are reading has a page to track. Start it first.")
		}
		if page != nil && b.Pages != nil && *b.Pages > 0 && *page > *b.Pages {
			return bad(fmt.Sprintf("This book has %d pages. Enter a page up to %d.", *b.Pages, *b.Pages))
		}
		_, err = tx.Exec("UPDATE books SET progress_page=? WHERE id=?", page, id)
		return err
	})
	if err != nil {
		return err
	}
	if r.FormValue("back") == "book" {
		return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved", id))
	}
	return redirect(w, r, listReturn(r))
}

// readingGoal is the reader's yearly target against what they have finished,
// as of a day. Finished means readings logged with a finish date in that year;
// a re-read counts again.
type readingGoal struct {
	Goal, Done, Year int
	Expected         float64
	MonthsLeft       float64
}

// newReadingGoal measures done finished books against a yearly goal.
func newReadingGoal(goal, done int, today time.Time) readingGoal {
	g := readingGoal{Goal: goal, Done: done, Year: today.Year()}
	if goal <= 0 {
		return g
	}
	daysInYear := midnight(time.Date(today.Year(), 12, 31, 0, 0, 0, 0, time.UTC)).YearDay()
	g.Expected = float64(goal) * float64(today.YearDay()) / float64(daysInYear)
	g.MonthsLeft = float64(daysInYear-today.YearDay()+1) * 12 / float64(daysInYear)
	return g
}

// Percent is how much of the goal is done, capped at 100.
func (g readingGoal) Percent() int {
	if g.Goal <= 0 {
		return 0
	}
	return min(100, g.Done*100/g.Goal)
}

// Status says plainly whether the reader is ahead of, on, or behind an even
// pace through the year.
func (g readingGoal) Status() string {
	diff := float64(g.Done) - g.Expected
	switch {
	case g.Goal <= 0:
		return ""
	case g.Done >= g.Goal:
		return "Goal reached"
	case diff >= 1:
		n := int(math.Floor(diff))
		return fmt.Sprintf("%d %s ahead of pace", n, plural(n, "book", "books"))
	case diff <= -1:
		n := int(math.Round(-diff))
		return fmt.Sprintf("%d %s behind pace", n, plural(n, "book", "books"))
	}
	return "On pace"
}

// Needed says what finishing in time takes, or "" when the goal is met.
func (g readingGoal) Needed() string {
	if g.Goal <= 0 || g.Done >= g.Goal {
		return ""
	}
	left := g.Goal - g.Done
	if g.MonthsLeft < 1 {
		return fmt.Sprintf("%d more %s to go before the year ends.", left, plural(left, "book", "books"))
	}
	return fmt.Sprintf("%d more %s to go, about %.1f a month for the rest of the year.", left, plural(left, "book", "books"), float64(left)/g.MonthsLeft)
}

// loadReadingGoal counts this year's finished readings against the goal.
func loadReadingGoal(q queryer, prefs Preferences, today time.Time) (readingGoal, error) {
	var done int
	err := q.QueryRow("SELECT count(*) FROM reading_log WHERE substr(finished_at,1,4)=?", strconv.Itoa(today.Year())).Scan(&done)
	return newReadingGoal(prefs.ReadingGoal, done, today), err
}

// saveReadingGoal sets or clears the yearly goal. A blank or zero clears it.
func (a *App) saveReadingGoal(w http.ResponseWriter, r *http.Request) error {
	goal := 0
	if raw := strings.TrimSpace(r.FormValue("goal")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxReadingGoal {
			return bad(fmt.Sprintf("Enter a goal from 1 to %d books, or leave it blank to have none.", maxReadingGoal))
		}
		goal = n
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		prefs, err := readMeta(tx, "preferences", defaultPreferences)
		if err != nil {
			return err
		}
		prefs.ReadingGoal = goal
		return setMeta(tx, "preferences", prefs)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/reading?notice=saved")
}
