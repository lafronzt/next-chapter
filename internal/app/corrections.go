package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Corrections let the reader fix or void one library event. The event keeps
// the observation as it was imported; the correction is stored beside it and
// applied when current loans are worked out. A replay of the same CSV finds
// the event already there and the correction still in force. Corrections
// change only library state (current loans), never reading completion,
// ratings, or queue order.

// eventCorrection is the reader's override of one event.
type eventCorrection struct {
	Fingerprint string `json:"fingerprint"`
	BookID      int64  `json:"book_id"`
	Action      string `json:"action"` // "edit" or "void"
	Source      string `json:"source"`
	Format      string `json:"format"`
	Status      string `json:"status"`
	At          string `json:"at"`
	DueDate     string `json:"due_date"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// Voided reports whether the event is voided.
func (e Event) Voided() bool { return e.Correction != nil && e.Correction.Action == "void" }

// Edited reports whether the event carries an edit.
func (e Event) Edited() bool { return e.Correction != nil && e.Correction.Action == "edit" }

// Effective returns the event as current loans see it: the reader's values
// when corrected, otherwise the observation as recorded.
func (e Event) Effective() Event {
	if c := e.Correction; c != nil && c.Action == "edit" {
		e.Source, e.Format, e.Status, e.At, e.DueDate, e.Activity = c.Source, c.Format, c.Status, c.At, c.DueDate, c.Status
	}
	return e
}

// attachCorrections fills Event.Correction for the given events.
func attachCorrections(q queryer, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	index := map[string][]int{}
	fps := make([]string, 0, len(events))
	for i, e := range events {
		index[e.Fingerprint] = append(index[e.Fingerprint], i)
		fps = append(fps, e.Fingerprint)
	}
	raw, _ := jsonStrings(fps)
	rows, err := q.Query("SELECT fingerprint,book_id,action,source,format,status,at,due_date,created_at,updated_at FROM event_corrections WHERE fingerprint IN (SELECT value FROM json_each(?))", raw)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c eventCorrection
		if err = rows.Scan(&c.Fingerprint, &c.BookID, &c.Action, &c.Source, &c.Format, &c.Status, &c.At, &c.DueDate, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return err
		}
		for _, i := range index[c.Fingerprint] {
			c := c
			events[i].Correction = &c
		}
	}
	return rows.Err()
}

func loadAllCorrections(q queryer) ([]eventCorrection, error) {
	rows, err := q.Query("SELECT fingerprint,book_id,action,source,format,status,at,due_date,created_at,updated_at FROM event_corrections ORDER BY fingerprint")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []eventCorrection{}
	for rows.Next() {
		var c eventCorrection
		if err = rows.Scan(&c.Fingerprint, &c.BookID, &c.Action, &c.Source, &c.Format, &c.Status, &c.At, &c.DueDate, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// correctionInput is what the reader asks for.
type correctionInput struct {
	// Action is "edit", "void", or "revert" (remove the correction).
	Action  string `json:"action"`
	Source  string `json:"source,omitempty"`
	Format  string `json:"format,omitempty"`
	Status  string `json:"status,omitempty"`
	Date    string `json:"date,omitempty"`     // the observation date, YYYY-MM-DD
	DueDate string `json:"due_date,omitempty"` // YYYY-MM-DD or empty
}

// correctionPlan is a previewed correction.
type correctionPlan struct {
	EventID     int64           `json:"event_id"`
	Fingerprint string          `json:"fingerprint"`
	BookID      int64           `json:"book_id"`
	Title       string          `json:"title"`
	Input       correctionInput `json:"input"`
	// Original and Effective describe the event now and after the change.
	Original  eventLine `json:"original"`
	Effective eventLine `json:"effective"`
	After     eventLine `json:"after"`
	// Before and After are the book's current loans, in words.
	LoansBefore []string `json:"loans_before"`
	LoansAfter  []string `json:"loans_after"`
}

// eventLine is an event's values in words for a preview.
type eventLine struct {
	Source, Format, Status, Date, DueDate string
	Voided                                bool
}

func (l eventLine) String() string {
	if l.Voided {
		return "voided (ignored)"
	}
	s := fmt.Sprintf("%s · %s · %s · %s", strings.ToUpper(l.Source), l.Format, l.Status, l.Date)
	if l.DueDate != "" {
		s += " · due " + l.DueDate
	}
	return s
}

func lineOf(e Event) eventLine {
	return eventLine{e.Source, e.Format, e.Status, dateOf(e.At), e.DueDate, e.Voided()}
}

// day is the calendar date of a stored timestamp.
func dateOf(at string) string {
	if len(at) >= 10 {
		return at[:10]
	}
	return at
}

func loadEvent(q queryer, id int64) (Event, error) {
	var e Event
	err := q.QueryRow(`SELECT e.id,e.book_id,e.source,e.format,e.status,e.activity,e.at,e.due_date,e.fingerprint,b.title,b.author FROM events e JOIN books b ON b.id=e.book_id WHERE e.id=?`, id).Scan(&e.ID, &e.BookID, &e.Source, &e.Format, &e.Status, &e.Activity, &e.At, &e.DueDate, &e.Fingerprint, &e.Title, &e.Author)
	if err != nil {
		return e, err
	}
	events := []Event{e}
	if err = attachCorrections(q, events); err != nil {
		return e, err
	}
	return events[0], nil
}

// correctionFromForm validates what the reader entered against the event.
func correctionFromForm(r *http.Request, ev Event) (correctionInput, error) {
	input := correctionInput{Action: r.FormValue("action")}
	switch input.Action {
	case "void", "revert":
		return input, nil
	case "edit":
	default:
		return input, bad("Choose whether to edit, void, or restore this event.")
	}
	cur := ev.Effective()
	input.Source, input.Format, input.Status = strings.TrimSpace(r.FormValue("source")), strings.TrimSpace(r.FormValue("format")), strings.TrimSpace(r.FormValue("status"))
	input.Date, input.DueDate = strings.TrimSpace(r.FormValue("date")), strings.TrimSpace(r.FormValue("due_date"))
	switch {
	case !in(input.Source, loanSources):
		return input, bad("Choose a library source.")
	case input.Format == "" || len([]rune(input.Format)) > 100:
		return input, bad("Enter a format of up to 100 characters.")
	case !in(input.Status, libraryStates):
		return input, bad("Choose a library activity.")
	case !validFinishDate(input.Date):
		return input, bad("Enter the observation date as a calendar date.")
	case input.DueDate != "" && !validFinishDate(input.DueDate):
		return input, bad("Enter the due date as a calendar date, or leave it blank.")
	}
	if input.Source == cur.Source && input.Format == cur.Format && input.Status == cur.Status && input.Date == dateOf(cur.At) && input.DueDate == cur.DueDate {
		return input, bad("Change at least one value, or choose Void or Restore.")
	}
	return input, nil
}

// setCorrection applies a correction and works the book's current loans out
// again from every effective event. Preview and apply both call it, the first
// inside a savepoint that is rolled back.
func setCorrection(q queryer, ev Event, in correctionInput, now time.Time) error {
	stamp := canonicalTime(now)
	switch in.Action {
	case "revert":
		if _, err := q.Exec("DELETE FROM event_corrections WHERE fingerprint=?", ev.Fingerprint); err != nil {
			return err
		}
	case "void":
		if _, err := q.Exec(`INSERT INTO event_corrections(fingerprint,book_id,action,created_at,updated_at) VALUES(?,?,'void',?,?)
ON CONFLICT(fingerprint) DO UPDATE SET action='void',source='',format='',status='',at='',due_date='',updated_at=excluded.updated_at`, ev.Fingerprint, ev.BookID, stamp, stamp); err != nil {
			return err
		}
	case "edit":
		at := ev.At
		if dateOf(ev.Effective().At) != in.Date {
			t, err := time.Parse(time.DateOnly, in.Date)
			if err != nil {
				return bad("Enter the observation date as a calendar date.")
			}
			at = canonicalTime(t)
		} else {
			at = ev.Effective().At
		}
		if _, err := q.Exec(`INSERT INTO event_corrections(fingerprint,book_id,action,source,format,status,at,due_date,created_at,updated_at) VALUES(?,?,'edit',?,?,?,?,?,?,?)
ON CONFLICT(fingerprint) DO UPDATE SET action='edit',source=excluded.source,format=excluded.format,status=excluded.status,at=excluded.at,due_date=excluded.due_date,updated_at=excluded.updated_at`, ev.Fingerprint, ev.BookID, in.Source, in.Format, in.Status, at, in.DueDate, stamp, stamp); err != nil {
			return err
		}
	default:
		return bad("Choose whether to edit, void, or restore this event.")
	}
	return rebuildLoans(q, ev.BookID, ev.BookID)
}

// loanLines describes a book's current loans in words.
func loanLines(q queryer, bookID int64) ([]string, error) {
	rows, err := q.Query("SELECT source,format,status,due_date,observed_at FROM loans WHERE book_id=? ORDER BY source,format", bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s, f, st, due, at string
		if err = rows.Scan(&s, &f, &st, &due, &at); err != nil {
			return nil, err
		}
		line := fmt.Sprintf("%s %s: %s (observed %s)", strings.ToUpper(s), f, st, dateOf(at))
		if due != "" {
			line += ", due " + due
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

// correctEvent shows the correction form for one event.
func (a *App) correctEventPage(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return bad("Choose a valid event.")
	}
	ev, err := loadEvent(a.store.DB, id)
	if err != nil {
		return err
	}
	loans, err := loanLines(a.store.DB, ev.BookID)
	if err != nil {
		return err
	}
	return a.render(w, r, "correct_event.html", page{"Title": "Correct a library event", "Event": ev, "Effective": ev.Effective(), "Loans": loans, "Sources": loanSources, "States": libraryStates, "Formats": loanFormats, "Nav": "history"}, 200)
}

// previewCorrection computes what a correction would do, in a savepoint that
// is rolled back, and saves it as a plan to confirm.
func (a *App) previewCorrection(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return bad("Choose a valid event.")
	}
	var planID string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		ev, err := loadEvent(tx, id)
		if err != nil {
			return err
		}
		in, err := correctionFromForm(r, ev)
		if err != nil {
			return err
		}
		if in.Action == "revert" && ev.Correction == nil {
			return bad("This event has no correction to restore.")
		}
		p := correctionPlan{EventID: ev.ID, Fingerprint: ev.Fingerprint, BookID: ev.BookID, Title: ev.Title, Input: in, Original: eventLine{ev.Source, ev.Format, ev.Status, dateOf(ev.At), ev.DueDate, false}, Effective: lineOf(ev.Effective())}
		if ev.Voided() {
			p.Effective = eventLine{Voided: true}
		}
		if p.LoansBefore, err = loanLines(tx, ev.BookID); err != nil {
			return err
		}
		if _, err = tx.Exec("SAVEPOINT correction_preview"); err != nil {
			return err
		}
		err = setCorrection(tx, ev, in, a.now())
		if err == nil {
			var after Event
			if after, err = loadEvent(tx, id); err == nil {
				p.After = lineOf(after.Effective())
				if after.Voided() {
					p.After = eventLine{Voided: true}
				}
				if in.Action == "revert" {
					p.After = lineOf(after)
				}
				p.LoansAfter, err = loanLines(tx, ev.BookID)
			}
		}
		if _, e := tx.Exec("ROLLBACK TO correction_preview; RELEASE correction_preview"); err == nil {
			err = e
		}
		if err != nil {
			return err
		}
		planID, err = savePlan(tx, "correction", Plan{Correction: &p})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+planID)
}

// applyCorrectionPlan is the apply branch for a correction plan. The caller
// has already checked the revision, so the library is as it was previewed.
func applyCorrectionPlan(q queryer, p *Plan, now time.Time) (string, error) {
	if p.Correction == nil {
		return "", errors.New("correction plan has no data")
	}
	c := p.Correction
	ev, err := loadEvent(q, c.EventID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", conflict("This event no longer exists. Start again from the book's page.")
	}
	if err != nil {
		return "", err
	}
	if ev.Fingerprint != c.Fingerprint || ev.BookID != c.BookID {
		return "", conflict("This event changed since the preview. Start again.")
	}
	if err = setCorrection(q, ev, c.Input, now); err != nil {
		return "", err
	}
	return fmt.Sprintf("/books/%d?notice=saved#library", ev.BookID), nil
}
