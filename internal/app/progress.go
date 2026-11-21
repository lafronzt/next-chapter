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
	maxProgressPage    = 100000
	maxProgressSeconds = 35999999 // 9,999:59:59
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

// HasProgress reports whether there is progress to show as a bar: a percent,
// or a page/audio position with a known total length.
func (b Book) HasProgress() bool {
	return (b.ProgressSeconds != nil && b.AudioSeconds != nil && *b.AudioSeconds > 0) || b.ProgressPercent != nil || (b.ProgressPage != nil && b.Pages != nil && *b.Pages > 0)
}

// ProgressUnit is "seconds" for an audiobook position, "percent" for a saved
// percentage or a book without a page count, and "page" otherwise.
func (b Book) ProgressUnit() string {
	if b.ProgressSeconds != nil {
		return "seconds"
	}
	if b.AudioSeconds != nil && b.ProgressPercent == nil && b.ProgressPage == nil {
		return "seconds"
	}
	if b.ProgressPercent != nil || (b.ProgressPage == nil && (b.Pages == nil || *b.Pages <= 0)) {
		return "percent"
	}
	return "page"
}

// ProgressValue is the saved form value (hours:minutes with optional seconds for time, empty when untracked).
func (b Book) ProgressValue() string {
	switch {
	case b.ProgressSeconds != nil:
		return audioPosition(*b.ProgressSeconds)
	case b.ProgressPercent != nil:
		return strconv.Itoa(*b.ProgressPercent)
	case b.ProgressPage != nil:
		return strconv.Itoa(*b.ProgressPage)
	}
	return ""
}

// Percent is how far through the book the reader is, 0 to 100.
func (b Book) Percent() int {
	switch {
	case b.ProgressSeconds != nil && b.AudioSeconds != nil && *b.AudioSeconds > 0:
		return min(100, *b.ProgressSeconds*100 / *b.AudioSeconds)
	case b.ProgressPercent != nil:
		return *b.ProgressPercent
	case b.ProgressPage != nil && b.Pages != nil && *b.Pages > 0:
		return min(100, *b.ProgressPage*100 / *b.Pages)
	}
	return 0
}

// PagesLeft is how many pages remain, and false when the page count is
// unknown or the reader tracks this book in percent or elapsed audio time.
func (b Book) PagesLeft() (int, bool) {
	if b.Pages == nil || *b.Pages <= 0 || b.ProgressPercent != nil || b.ProgressSeconds != nil {
		return 0, false
	}
	read := 0
	if b.ProgressPage != nil {
		read = min(*b.ProgressPage, *b.Pages)
	}
	return *b.Pages - read, true
}

// ProgressText says where the reader is, or "" when there is nothing to say.
func (b Book) ProgressText() string {
	switch {
	case b.ProgressSeconds != nil:
		if b.AudioSeconds != nil && *b.AudioSeconds > 0 {
			return fmt.Sprintf("%s of %s · %d%% · %s left", audioPosition(*b.ProgressSeconds), b.AudioDuration(), b.Percent(), audioPosition(max(0, *b.AudioSeconds-*b.ProgressSeconds)))
		}
		return audioPosition(*b.ProgressSeconds) + " elapsed"
	case b.ProgressPercent != nil:
		return fmt.Sprintf("%d%%", *b.ProgressPercent)
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
	return fmt.Sprintf("%d %s left · about %d %s a day to finish in time", left, plural(left, "page", "pages"), per, plural(per, "page", "pages"))
}

// percentPaceText is paceText for a book tracked in percent, such as an
// audiobook: how much is left and the share to get through each day.
func percentPaceText(left, days int) string {
	switch {
	case left <= 0:
		return ""
	case days <= 0:
		return fmt.Sprintf("%d%% left", left)
	}
	return fmt.Sprintf("%d%% left · about %d%% a day to finish in time", left, (left+days-1)/days)
}

// audioPaceText describes remaining audio and the daily amount needed before
// a due date. Round up to whole minutes so a short remainder never looks done.
func audioPaceText(left, days int) string {
	if left <= 0 {
		return ""
	}
	minutes := (left + 59) / 60
	remaining := fmt.Sprintf("%d %s", minutes, plural(minutes, "minute", "minutes"))
	if minutes >= 60 {
		hours, rest := minutes/60, minutes%60
		remaining = fmt.Sprintf("%d %s", hours, plural(hours, "hour", "hours"))
		if rest > 0 {
			remaining += fmt.Sprintf(" %d %s", rest, plural(rest, "minute", "minutes"))
		}
	}
	text := remaining + " of audio left"
	if days > 0 {
		per := (left + days*60 - 1) / (days * 60)
		text += fmt.Sprintf(" · about %d %s a day to finish in time", per, plural(per, "minute", "minutes"))
	}
	return text
}

// Pace says what is left before a loan is due, using audio duration when
// available, otherwise the reader's saved percentage or page count.
func (d dueSoon) Pace() string {
	b := d.Book
	if b.AudioSeconds != nil && *b.AudioSeconds > 0 && b.ProgressPage == nil {
		left := *b.AudioSeconds
		if b.ProgressSeconds != nil {
			left -= *b.ProgressSeconds
		} else if b.ProgressPercent != nil {
			left = (left*(100-*b.ProgressPercent) + 99) / 100
		}
		return audioPaceText(left, d.Days)
	}
	if b.ProgressSeconds != nil {
		return ""
	}
	if b.ProgressPercent != nil {
		return percentPaceText(100-*b.ProgressPercent, d.Days)
	}
	left, ok := b.PagesLeft()
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

// bookProgress saves how far the reader is in a book they are reading, as a
// page, percentage, or elapsed audiobook position with an optional duration.
// Saving one unit clears the others; a blank clears the position. It never
// moves a book, finishes one, or invalidates a preview or Undo.
func (a *App) bookProgress(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	unit := r.FormValue("unit")
	if unit != "page" && unit != "percent" && unit != "seconds" {
		return bad("Choose pages, percentage, or an audio position in hours:minutes:seconds.")
	}
	raw := strings.TrimSpace(r.FormValue("progress"))
	var value *int
	if raw != "" {
		n, e := strconv.Atoi(raw)
		if unit == "seconds" {
			n, e = parseAudioPosition(raw)
		}
		limit := maxProgressPage
		if unit == "percent" {
			limit = 100
		} else if unit == "seconds" {
			limit = maxProgressSeconds
		}
		switch {
		case unit == "seconds" && e != nil:
			return e
		case e != nil || n < 0:
			return bad("Enter your progress as a whole number, or leave it blank.")
		case n > limit && unit == "percent":
			return bad("A percentage can be at most 100.")
		case n > limit:
			return bad(fmt.Sprintf("Enter a page up to %d.", limit))
		}
		value = &n
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		b, err := getBook(tx, id)
		if err != nil {
			return err
		}
		if b.Status != "reading" {
			return bad("Only a book you are reading has progress to track. Start it first.")
		}
		if r.Form.Has("audio_duration") {
			b.AudioSeconds = nil
			if raw := strings.TrimSpace(r.FormValue("audio_duration")); raw != "" {
				n, e := parseAudioPosition(raw)
				if e != nil {
					return e
				}
				if n <= 0 {
					return bad("Total audiobook duration must be greater than 0:00.")
				}
				b.AudioSeconds = &n
			}
		}
		var page, percent, seconds *int
		if unit == "page" {
			if value != nil && b.Pages != nil && *b.Pages > 0 && *value > *b.Pages {
				return bad(fmt.Sprintf("This book has %d pages. Enter a page up to %d.", *b.Pages, *b.Pages))
			}
			page = value
		} else if unit == "percent" {
			percent = value
		} else {
			seconds = value
		}
		if _, err = tx.Exec("UPDATE books SET progress_page=?,progress_percent=?,progress_seconds=?,audio_seconds=? WHERE id=?", page, percent, seconds, b.AudioSeconds, id); err != nil {
			return err
		}
		// With progress history on, the same save also stores a dated update.
		return recordCheckpoint(tx, b, unit, value, midnight(a.now()))
	})
	if err != nil {
		return err
	}
	// A book the reader opted in also sends its progress to Hardcover, after
	// the local save; whatever happens there, the save stands.
	notice := "progress-saved"
	if value == nil {
		notice = "progress-cleared"
	}
	if value != nil {
		if n := a.pushProgress(r.Context(), id); n != "" {
			notice = n
		}
	}
	if r.FormValue("back") == "book" {
		return redirect(w, r, fmt.Sprintf("/books/%d?notice=%s", id, notice))
	}
	dest := listReturn(r)
	if notice != "" {
		dest += map[bool]string{true: "&", false: "?"}[strings.Contains(dest, "?")] + "notice=" + notice
	}
	return redirect(w, r, dest)
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

// audioPosition is an elapsed position. Keep seconds from Hardcover precise.
func audioPosition(seconds int) string {
	if seconds%60 != 0 {
		return fmt.Sprintf("%d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
	}
	return fmt.Sprintf("%d:%02d", seconds/3600, seconds/60%60)
}

func parseAudioPosition(raw string) (int, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	invalid := bad("Enter hours:minutes:seconds, such as 2:30:15, or hours:minutes, such as 2:30 (minutes and seconds 00–59, up to 9999:59:59).")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, invalid
	}
	values := make([]int, len(parts))
	for i, part := range parts {
		if part == "" {
			return 0, invalid
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, invalid
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil || n > 9999 || (i > 0 && n > 59) {
			return 0, invalid
		}
		values[i] = n
	}
	seconds := values[0]*3600 + values[1]*60
	if len(values) == 3 {
		seconds += values[2]
	}
	return seconds, nil
}

// AudioDuration is the total edition length as an editable hours:minutes value with optional seconds.
func (b Book) AudioDuration() string {
	if b.AudioSeconds == nil {
		return ""
	}
	return audioPosition(*b.AudioSeconds)
}

// ProgressLabel names the value the reader is entering.
func (b Book) ProgressLabel() string {
	switch b.ProgressUnit() {
	case "seconds":
		return "Audio position"
	case "percent":
		return "Progress (%)"
	default:
		return "Page reached"
	}
}
func (b Book) ProgressMethod() string {
	switch b.ProgressUnit() {
	case "seconds":
		return "Audio position"
	case "percent":
		return "Percentage"
	default:
		return "Page"
	}
}
