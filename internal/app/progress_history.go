package app

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Optional progress history. While the reader keeps it (Preferences, off by
// default), each progress save also stores a dated checkpoint for the book's
// current reading attempt. Checkpoints are progress updates, not measured
// time spent reading. Pace and a finish date are worked out from them and
// shown, never acted on: nothing here finishes a book, moves the queue, or
// sends a reminder.

const (
	// paceWindowDays is how far back checkpoints count toward pace, and
	// paceStaleDays how recent the newest one must be.
	paceWindowDays = 28
	paceStaleDays  = 14
	// maxFinishDays is the farthest finish date worth stating.
	maxFinishDays = 730
	// recentCheckpoints is how many updates the book page lists.
	recentCheckpoints = 10
	// Restore bounds.
	maxRestoreAttempts    = 100_000
	maxRestoreCheckpoints = 200_000
	notEnoughProgress     = "Not enough recent progress"
)

// readingAttempt is one reading of a book. StartedOn is empty when the book
// was already being read when attempts began, and the start is not guessed.
type readingAttempt struct {
	ID        int64   `json:"id"`
	BookID    int64   `json:"book_id"`
	StartedOn string  `json:"started_on"`
	EndedOn   *string `json:"ended_on"`
}

// progressCheckpoint is one dated progress update. Value is nil when progress
// was cleared. TotalPages is the book's page count then, for page updates.
type progressCheckpoint struct {
	ID         int64  `json:"id"`
	AttemptID  int64  `json:"attempt_id"`
	BookID     int64  `json:"book_id"`
	Day        string `json:"day"`
	Unit       string `json:"unit"`
	Value      *int   `json:"value"`
	TotalPages *int   `json:"total_pages"`
	Version    int    `json:"-"`
}

// Text describes the update in words.
func (c progressCheckpoint) Text() string {
	switch {
	case c.Value == nil:
		return "Progress cleared"
	case c.Unit == "seconds":
		return audioPosition(*c.Value) + " elapsed"
	case c.Unit == "percent":
		return fmt.Sprintf("%d%%", *c.Value)
	}
	return fmt.Sprintf("page %d", *c.Value)
}

func progressHistoryOn(q queryer) (bool, error) {
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	return prefs.ProgressHistory, err
}

func openAttemptID(q queryer, bookID int64) (int64, error) {
	var id int64
	err := q.QueryRow("SELECT id FROM reading_attempts WHERE book_id=? AND ended_on IS NULL ORDER BY id DESC LIMIT 1", bookID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// noteStatusChange keeps attempts in step with reading status. Leaving
// Reading always ends the open attempt, so a reread never reuses it, even if
// history was switched off meanwhile. Entering Reading starts a new attempt
// dated today when history is on; undo returning a book to Reading reopens
// its latest attempt instead. A book already Reading gets an attempt, with an
// unknown start, only when its first update is saved.
func noteStatusChange(q queryer, bookID int64, old, status string, today time.Time, undo bool) error {
	if old == status {
		return nil
	}
	day := today.Format(time.DateOnly)
	if old == "reading" {
		if _, err := q.Exec("UPDATE reading_attempts SET ended_on=? WHERE book_id=? AND ended_on IS NULL", day, bookID); err != nil {
			return err
		}
	}
	if status != "reading" {
		return nil
	}
	if undo {
		_, err := q.Exec("UPDATE reading_attempts SET ended_on=NULL WHERE id=(SELECT max(id) FROM reading_attempts WHERE book_id=?) AND ended_on IS NOT NULL", bookID)
		return err
	}
	on, err := progressHistoryOn(q)
	if err != nil || !on {
		return err
	}
	_, err = q.Exec("INSERT INTO reading_attempts(book_id,started_on) VALUES(?,?)", bookID, day)
	return err
}

// recordCheckpoint stores today's update for the book's open attempt (made
// now, without a start date, if it has none). A second update the same day
// replaces the first, so a day has one checkpoint: its last value. A blank
// save is stored as a reset only when there is history to reset.
func recordCheckpoint(q queryer, b Book, unit string, value *int, today time.Time) error {
	on, err := progressHistoryOn(q)
	if err != nil || !on {
		return err
	}
	attempt, err := openAttemptID(q, b.ID)
	if err != nil {
		return err
	}
	if attempt == 0 {
		res, err := q.Exec("INSERT INTO reading_attempts(book_id,started_on) VALUES(?,'')", b.ID)
		if err != nil {
			return err
		}
		if attempt, err = res.LastInsertId(); err != nil {
			return err
		}
	}
	if value == nil {
		var n int
		if err = q.QueryRow("SELECT count(*) FROM progress_checkpoints WHERE attempt_id=?", attempt).Scan(&n); err != nil || n == 0 {
			return err
		}
	}
	var total any
	if unit == "page" && b.Pages != nil {
		total = *b.Pages
	}
	_, err = q.Exec(`INSERT INTO progress_checkpoints(attempt_id,book_id,day,unit,value,total_pages) VALUES(?,?,?,?,?,?)
ON CONFLICT(attempt_id,day) DO UPDATE SET unit=excluded.unit,value=excluded.value,total_pages=excluded.total_pages,version=version+1`,
		attempt, b.ID, today.Format(time.DateOnly), unit, value, total)
	return err
}

func loadCheckpoints(q queryer, attemptID int64) ([]progressCheckpoint, error) {
	rows, err := q.Query("SELECT id,attempt_id,book_id,day,unit,value,total_pages,version FROM progress_checkpoints WHERE attempt_id=? ORDER BY day,id", attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []progressCheckpoint{}
	for rows.Next() {
		var c progressCheckpoint
		if err = rows.Scan(&c.ID, &c.AttemptID, &c.BookID, &c.Day, &c.Unit, &c.Value, &c.TotalPages, &c.Version); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func loadAllAttempts(q queryer) ([]readingAttempt, error) {
	rows, err := q.Query("SELECT id,book_id,started_on,ended_on FROM reading_attempts ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []readingAttempt{}
	for rows.Next() {
		var a readingAttempt
		if err = rows.Scan(&a.ID, &a.BookID, &a.StartedOn, &a.EndedOn); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func loadAllCheckpoints(q queryer) ([]progressCheckpoint, error) {
	rows, err := q.Query("SELECT id,attempt_id,book_id,day,unit,value,total_pages FROM progress_checkpoints ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []progressCheckpoint{}
	for rows.Next() {
		var c progressCheckpoint
		if err = rows.Scan(&c.ID, &c.AttemptID, &c.BookID, &c.Day, &c.Unit, &c.Value, &c.TotalPages); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// paceInfo is what the book page says about pace and finishing.
type paceInfo struct {
	Rate, Basis string
	FinishDate  string
	// Message is the whole statement: the pace, or "Not enough recent progress".
	Message string
	// Finish is the approximate finish sentence, empty when there is none.
	Finish string
	// Deadline compares the finish date with a loan due date, when there is one.
	Deadline []string
}

// estimatePace works out pace from the current attempt's checkpoints only.
// It uses the newest unbroken run: same unit, same page count, and never
// going backward, so a correction or reset starts a fresh run and is never
// read as negative speed. The run must end within paceStaleDays of today,
// and only checkpoints within paceWindowDays count. It needs two dated
// updates on different days with positive progress between them, and
// speaks in calendar days. A pace of zero is never extrapolated.
func estimatePace(cps []progressCheckpoint, b Book, loans []Loan, today time.Time) paceInfo {
	none := paceInfo{Message: notEnoughProgress}
	today = midnight(today)
	if len(cps) == 0 || cps[len(cps)-1].Value == nil {
		return none
	}
	run := []progressCheckpoint{cps[len(cps)-1]}
	for i := len(cps) - 2; i >= 0; i-- {
		c, next := cps[i], run[0]
		if c.Value == nil || c.Unit != next.Unit || *c.Value > *next.Value || (c.Unit == "page" && !samePages(c.TotalPages, next.TotalPages)) {
			break
		}
		run = append([]progressCheckpoint{c}, run...)
	}
	cutoff := today.AddDate(0, 0, -paceWindowDays).Format(time.DateOnly)
	run = slices.DeleteFunc(run, func(c progressCheckpoint) bool { return c.Day < cutoff })
	if len(run) < 2 {
		return none
	}
	first, last := run[0], run[len(run)-1]
	if days, ok := daysUntil(last.Day, today); !ok || -days > paceStaleDays {
		return none
	}
	elapsed := dayDiff(first.Day, last.Day)
	gained := *last.Value - *first.Value
	if elapsed <= 0 || gained <= 0 {
		return none
	}
	per := float64(gained) / float64(elapsed)
	unit := map[string]string{"page": "pages", "percent": "% of the book", "seconds": "seconds of audio"}[last.Unit]
	info := paceInfo{Message: fmt.Sprintf("From %s to %s you saved %d %s across %d %s: about %s %s a day. This is based on the dates you saved progress, not time spent reading.", first.Day, last.Day, len(run), plural(len(run), "update", "updates"), elapsed, plural(elapsed, "day", "days"), perDayText(per), unit)}
	info.Rate = "About " + perDayText(per) + " pages a day"
	if last.Unit == "percent" {
		info.Rate = "About " + perDayText(per) + "% a day"
	}
	if last.Unit == "seconds" {
		info.Rate = "About " + perDayText(per/60) + " minutes of audio a day"
	}
	info.Basis = fmt.Sprintf("%d updates · %s to %s", len(run), first.Day, last.Day)
	// What is left, in the same unit the pace is in.
	var left int
	switch {
	case last.Unit == "page" && b.ProgressPage != nil && b.ProgressPercent == nil && b.Pages != nil && *b.Pages > 0:
		left = *b.Pages - *b.ProgressPage
	case last.Unit == "percent" && b.ProgressPercent != nil:
		left = 100 - *b.ProgressPercent
	default:
		return info
	}
	if left <= 0 {
		info.Finish = "You have reached the end. Mark it finished yourself when you are done; reaching the last page never does that."
		return info
	}
	needed := int(math.Ceil(float64(left) / per))
	if needed > maxFinishDays {
		info.Finish = "At this pace the finish is too far away to estimate."
		return info
	}
	finish := today.AddDate(0, 0, needed)
	info.FinishDate = finish.Format(time.DateOnly)
	info.Finish = fmt.Sprintf("At that pace you would finish around %s (an approximation, about %d %s from today).", finish.Format(time.DateOnly), needed, plural(needed, "day", "days"))
	for _, l := range loans {
		if l.Status != "borrowed" || l.DueDate == "" {
			continue
		}
		due, err := time.Parse(time.DateOnly, l.DueDate)
		if err != nil {
			continue
		}
		label := strings.ToUpper(l.Source) + " " + l.Format
		switch diff := int(finish.Sub(due).Hours() / 24); {
		case diff > 0:
			info.Deadline = append(info.Deadline, fmt.Sprintf("That is %d %s after the %s due date (%s).", diff, plural(diff, "day", "days"), label, l.DueDate))
		default:
			info.Deadline = append(info.Deadline, fmt.Sprintf("That is on or before the %s due date (%s).", label, l.DueDate))
		}
	}
	return info
}

func samePages(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func perDayText(per float64) string {
	if per >= 10 {
		return strconv.Itoa(int(math.Round(per)))
	}
	return strconv.FormatFloat(math.Round(per*10)/10, 'f', -1, 64)
}

// attemptSummary describes an earlier reading in words.
type attemptSummary struct {
	StartedOn, EndedOn string
	Updates            int
}

func (s attemptSummary) Text() string {
	start := "start date not known"
	if s.StartedOn != "" {
		start = "started " + s.StartedOn
	}
	end := "still open"
	if s.EndedOn != "" {
		end = "ended " + s.EndedOn
	}
	return fmt.Sprintf("%s, %s, %d %s", start, end, s.Updates, plural(s.Updates, "update", "updates"))
}

// progressPageData adds the progress history of a book to its page data.
func (a *App) progressPageData(data page, b Book) error {
	on, err := progressHistoryOn(a.store.DB)
	if err != nil {
		return err
	}
	data["HistoryOn"] = on
	rows, err := a.store.DB.Query(`SELECT a.id,a.started_on,COALESCE(a.ended_on,''),(SELECT count(*) FROM progress_checkpoints c WHERE c.attempt_id=a.id) FROM reading_attempts a WHERE a.book_id=? ORDER BY a.id DESC`, b.ID)
	if err != nil {
		return err
	}
	type attemptRow struct {
		id int64
		attemptSummary
	}
	var attempts []attemptRow
	for rows.Next() {
		var x attemptRow
		if err = rows.Scan(&x.id, &x.StartedOn, &x.EndedOn, &x.Updates); err != nil {
			rows.Close()
			return err
		}
		attempts = append(attempts, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var earlier []attemptSummary
	var current *attemptRow
	for i := range attempts {
		if i == 0 && b.Status == "reading" && attempts[i].EndedOn == "" {
			current = &attempts[i]
			continue
		}
		if attempts[i].Updates > 0 {
			earlier = append(earlier, attempts[i].attemptSummary)
		}
	}
	data["EarlierAttempts"] = earlier
	if current == nil {
		return nil
	}
	cps, err := loadCheckpoints(a.store.DB, current.id)
	if err != nil {
		return err
	}
	recent := slices.Clone(cps)
	slices.Reverse(recent)
	if len(recent) > recentCheckpoints {
		recent = recent[:recentCheckpoints]
	}
	data["Checkpoints"], data["CheckpointTotal"] = recent, len(cps)
	data["Pace"] = estimatePace(cps, b, b.Loans, a.now())
	data["AttemptStart"] = current.StartedOn
	return nil
}

// checkpointFields reads a corrected day and value from the form.
func checkpointFields(r *http.Request, c progressCheckpoint, pages *int, today time.Time) (day string, value int, err error) {
	day = strings.TrimSpace(r.FormValue("day"))
	if !validFinishDate(day) {
		return "", 0, bad("Enter the date as a calendar date.")
	}
	if day > today.Format(time.DateOnly) {
		return "", 0, bad("An update cannot be dated in the future.")
	}
	if c.Unit == "seconds" {
		value, err = parseAudioPosition(r.FormValue("value"))
		return day, value, err
	}
	value, e := strconv.Atoi(strings.TrimSpace(r.FormValue("value")))
	limit := maxProgressPage
	if c.Unit == "percent" {
		limit = 100
	} else if pages != nil && *pages > 0 {
		limit = *pages
	}
	if e != nil || value < 0 || value > limit {
		return "", 0, bad(fmt.Sprintf("Enter a whole number from 0 to %d.", limit))
	}
	return day, value, nil
}

// editCheckpoint corrects or deletes one progress update. The form carries
// the update's version, so a change from an out-of-date page is refused.
// Changing or deleting the newest update of the current attempt also sets the
// book's current progress to the newest remaining update, so the number shown
// and the history agree; older updates leave the current value alone.
func (a *App) editCheckpoint(w http.ResponseWriter, r *http.Request) error {
	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || cid < 1 {
		return bad("Choose a valid update.")
	}
	version, err := strconv.Atoi(r.FormValue("version"))
	if err != nil {
		return bad("Reload the page and try again.")
	}
	action := r.FormValue("action")
	if action != "save" && action != "delete" {
		return bad("Choose a valid action.")
	}
	today := midnight(a.now())
	var bookID int64
	err = a.store.Write(func(tx *sql.Tx) error {
		var c progressCheckpoint
		err := tx.QueryRow("SELECT id,attempt_id,book_id,day,unit,value,total_pages,version FROM progress_checkpoints WHERE id=?", cid).Scan(&c.ID, &c.AttemptID, &c.BookID, &c.Day, &c.Unit, &c.Value, &c.TotalPages, &c.Version)
		if errors.Is(err, sql.ErrNoRows) {
			return conflict("This update was already deleted. Reload the page.")
		}
		if err != nil {
			return err
		}
		if c.Version != version {
			return conflict("This update changed since the page loaded. Reload it before editing.")
		}
		bookID = c.BookID
		b, err := getBook(tx, c.BookID)
		if err != nil {
			return err
		}
		before, err := loadCheckpoints(tx, c.AttemptID)
		if err != nil {
			return err
		}
		wasLatest := before[len(before)-1].ID == c.ID
		if action == "delete" {
			_, err = tx.Exec("DELETE FROM progress_checkpoints WHERE id=?", cid)
		} else {
			day, value, e := checkpointFields(r, c, b.Pages, today)
			if e != nil {
				return e
			}
			var clash int64
			if q := tx.QueryRow("SELECT id FROM progress_checkpoints WHERE attempt_id=? AND day=? AND id!=?", c.AttemptID, day, cid).Scan(&clash); q == nil {
				return conflict("Another update in this reading already has that date. Edit that one, or choose another date.")
			}
			_, err = tx.Exec("UPDATE progress_checkpoints SET day=?,value=?,version=version+1 WHERE id=?", day, value, cid)
		}
		if err != nil {
			return err
		}
		after, err := loadCheckpoints(tx, c.AttemptID)
		if err != nil || len(after) == 0 {
			return err
		}
		newest := after[len(after)-1]
		open, err := openAttemptID(tx, c.BookID)
		if err != nil {
			return err
		}
		if open != c.AttemptID || b.Status != "reading" || !(wasLatest || newest.ID == c.ID) {
			return nil
		}
		var page, percent, seconds *int
		if newest.Value != nil {
			if newest.Unit == "percent" {
				percent = newest.Value
			} else if newest.Unit == "seconds" {
				seconds = newest.Value
			} else {
				page = newest.Value
			}
		}
		_, err = tx.Exec("UPDATE books SET progress_page=?,progress_percent=?,progress_seconds=? WHERE id=?", page, percent, seconds, c.BookID)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved#progress-history", bookID))
}

// clearProgressHistory deletes every saved progress update and the attempts
// that are over. It leaves each book's current progress, and any attempt in
// progress, as they are.
func (a *App) clearProgressHistory(w http.ResponseWriter, r *http.Request) error {
	if r.FormValue("confirm") != "yes" {
		return bad("Confirm that you want to delete the saved progress updates.")
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM progress_checkpoints"); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM reading_attempts WHERE ended_on IS NOT NULL")
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/settings?notice=saved&open=privacy#privacy")
}

func (c progressCheckpoint) FormValue() string {
	if c.Value == nil {
		return ""
	}
	if c.Unit == "seconds" {
		return audioPosition(*c.Value)
	}
	return strconv.Itoa(*c.Value)
}
