package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// hcProgressPlan is a previewed pull of the current read's progress.
type hcProgressPlan struct {
	AudioSeconds *int   `json:"audio_seconds,omitempty"`
	BookID       int64  `json:"book_id"`
	Title        string `json:"title"`
	LocalText    string `json:"local_text"`
	RemoteText   string `json:"remote_text"`
	// State is same, remote, local, both, or none (nothing usable).
	State     string `json:"state"`
	Page      *int   `json:"page,omitempty"`
	Percent   *int   `json:"percent,omitempty"`
	Seconds   *int   `json:"seconds,omitempty"`
	Note      string `json:"note,omitempty"`
	RemoteKey string `json:"remote_key"`
}

// hcReadPlan is a previewed send of one finished reading's date.
type hcReadPlan struct {
	StartedAt       string `json:"started_at,omitempty"`
	RemoteStartedAt string `json:"remote_started_at,omitempty"`
	UserID          int64  `json:"user_id"`
	RemoteSnapshot  string `json:"remote_snapshot"`
	Current         bool   `json:"current,omitempty"`

	BookID          int64  `json:"book_id"`
	Title           string `json:"title"`
	ReadingID       int64  `json:"reading_id"`
	HardcoverBookID int64  `json:"hardcover_book_id"`
	UserBookID      int64  `json:"user_book_id"`
	Date            string `json:"date"`
	Precision       int    `json:"precision"`
	LocalText       string `json:"local_text"`
	// Action is insert, map (already there; tie it), update, none, or missing.
	Action       string `json:"action"`
	State        string `json:"state,omitempty"`
	RemoteReadID int64  `json:"remote_read_id,omitempty"`
	RemoteText   string `json:"remote_text,omitempty"`
	RemoteDate   string `json:"remote_date,omitempty"`
	RemotePrec   int    `json:"remote_precision,omitempty"`
	Note         string `json:"note,omitempty"`
}

func periodText(date string, precision int) string {
	return readingEntry{FinishedAt: &date, FinishedPrecision: precision}.When()
}
func dateKey(date string, precision int) string { return date + "/" + strconv.Itoa(precision) }

// ---- opt-in ----

// hardcoverProgressSync turns "also update Hardcover progress" on or off for
// one linked book. It changes nothing else and sends nothing.
func (a *App) hardcoverProgressSync(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		res, err := tx.Exec("UPDATE book_hardcover SET progress_sync=? WHERE book_id=?", r.FormValue("enabled") == "1", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return bad("Link this book to Hardcover first.")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved", id))
}

// pushProgress runs after a progress save has committed, for a book the
// reader opted in: one read-in-progress query, then at most one mutation. The
// local save stands whatever happens here. It returns a notice key, or "".
func (a *App) pushProgress(ctx context.Context, bookID int64) string {
	link, err := getBookLink(a.store.DB, bookID)
	if err != nil || link == nil || !link.ProgressSync || a.hardcoverReady() != nil {
		return ""
	}
	acct, err := loadHardcoverLink(a.store.DB)
	if err != nil || acct == nil {
		return ""
	}
	b, err := getBook(a.store.DB, bookID)
	if err != nil {
		return ""
	}
	a.hardcoverMu.Lock()
	defer a.hardcoverMu.Unlock()
	open, err := a.hardcover.openRead(ctx, acct.UserID, link.HardcoverBookID)
	if err != nil {
		return "hc-progress-failed"
	}
	if open == nil || open.ReadID == 0 {
		return "hc-progress-noread"
	}
	p := progressToRemote(b, *open)
	if p.Skip != "" {
		return "hc-progress-skip"
	}
	sent := "p:" + strconv.Itoa(p.Pages)
	if p.Audio || p.Seconds > 0 {
		sent = "s:" + strconv.Itoa(p.Seconds)
	}
	if remoteProgressKey(*open) != sent {
		if _, _, err = a.hardcover.readMutation(ctx, hcUpdateReadMutation, progressVars(open.ReadID, p, "")); err != nil {
			return "hc-progress-failed"
		}
	}
	if err = a.store.Write(func(tx *sql.Tx) error {
		return addSync(tx, bookID, "progress", localProgressKey(b), sent, "push", canonicalTime(a.now()))
	}); err != nil {
		return "hc-progress-failed"
	}
	return "hc-progress-sent"
}

// ---- pull progress ----

func (a *App) hardcoverProgressPull(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	id, err := requestID(r)
	if err != nil {
		return err
	}
	b, err := getBook(a.store.DB, id)
	if err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, id)
	if err != nil {
		return err
	}
	acct, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	switch {
	case link == nil || acct == nil:
		return bad("Link this book to Hardcover, and test the connection, first.")
	case b.Status != "reading":
		return bad("Only a book you are reading has progress to pull. Pulling progress never starts a book.")
	}
	a.hardcoverMu.Lock()
	open, err := a.hardcover.openRead(r.Context(), acct.UserID, link.HardcoverBookID)
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	if open == nil || open.ReadID == 0 {
		return bad("Hardcover has no read in progress for this book.")
	}
	last, err := lastSync(a.store.DB, id, "progress")
	if err != nil {
		return err
	}
	conv := progressFromRemote(b, *open)
	plan := hcProgressPlan{BookID: id, Title: b.Title, LocalText: b.ProgressText(), RemoteText: remoteProgressText(*open), Page: conv.Page, Percent: conv.Percent, Seconds: conv.Seconds, Note: conv.Note, RemoteKey: remoteProgressKey(*open)}

	if b.AudioSeconds == nil && open.EdAudio > 0 && open.EdAudio <= maxProgressSeconds {
		n := open.EdAudio
		plan.AudioSeconds = &n
		plan.Note += " Total audiobook duration (" + audioPosition(n) + ") will also be filled from Hardcover's selected edition."
	}
	if plan.LocalText == "" {
		plan.LocalText = "none saved"
	}
	if conv.Skip != "" {
		plan.State, plan.Note = "none", conv.Skip
	} else {
		equal := conv.Page != nil && b.ProgressPage != nil && b.ProgressPercent == nil && *conv.Page == *b.ProgressPage ||
			conv.Percent != nil && b.ProgressPercent != nil && *conv.Percent == *b.ProgressPercent ||
			conv.Seconds != nil && b.ProgressSeconds != nil && *conv.Seconds == *b.ProgressSeconds
		plan.State = progressState(last, localProgressKey(b), plan.RemoteKey, equal)
		if plan.State == "same" && plan.AudioSeconds != nil {
			plan.State = "remote"
		}
	}
	var planID string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		planID, err = savePlan(tx, "hc-progress", Plan{HardcoverProgress: &plan})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/progress/"+planID)
}

func remoteProgressText(r hcOpenRead) string {
	switch {
	case r.EdAudio > 0 || r.Seconds > 0 && r.Pages == 0:
		return audioPosition(r.Seconds) + " elapsed"
	case r.Pages > 0 && r.EdPages > 0:
		return fmt.Sprintf("page %d of %d", r.Pages, r.EdPages)
	case r.Pages > 0:
		return fmt.Sprintf("page %d", r.Pages)
	}
	return "none"
}

func (a *App) hardcoverProgressPlan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	read := func(q queryer) (hcProgressPlan, int64, error) {
		kind, p, rev, err := readPlan(q, id)
		if err != nil {
			return hcProgressPlan{}, 0, err
		}
		if kind != "hc-progress" || p.HardcoverProgress == nil {
			return hcProgressPlan{}, 0, sql.ErrNoRows
		}
		return *p.HardcoverProgress, rev, nil
	}
	if r.Method == http.MethodGet {
		p, rev, err := read(a.store.DB)
		if err != nil {
			return err
		}
		return a.render(w, r, "hardcover_progress.html", page{"Title": "Pull progress", "Nav": "hardcover", "PlanID": id, "Plan": p, "Stale": rev != state(r).Revision}, 200)
	}
	var back string
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		p, rev, err := read(tx)
		if err != nil {
			return err
		}
		if now, err := revision(tx); err != nil {
			return err
		} else if now != rev {
			return conflict("Your library changed. Pull the progress again.")
		}
		back = fmt.Sprintf("/books/%d?notice=", p.BookID)
		if r.FormValue("use") != "hardcover" || (p.State != "remote" && p.State != "both") {
			back += "hc-progress-kept"
			return nil
		}
		b, err := getBook(tx, p.BookID)
		if err != nil {
			return err
		}
		if b.Status != "reading" {
			return conflict("This book is no longer being read. Nothing was changed.")
		}
		unit, value := "page", p.Page
		if p.Percent != nil {
			unit, value = "percent", p.Percent
		}
		if p.Seconds != nil {
			unit, value = "seconds", p.Seconds
		}
		if value == nil {
			return bad("There is nothing usable to pull.")
		}
		var page, percent, seconds *int
		if unit == "page" {
			page = value
		} else if unit == "percent" {
			percent = value
		} else {
			seconds = value
		}

		if p.AudioSeconds != nil && b.AudioSeconds == nil {
			if _, err = tx.Exec("UPDATE books SET audio_seconds=? WHERE id=?", p.AudioSeconds, p.BookID); err != nil {
				return err
			}
			b.AudioSeconds = p.AudioSeconds
			if err = bump(tx); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("UPDATE books SET progress_page=?,progress_percent=?,progress_seconds=? WHERE id=?", page, percent, seconds, p.BookID); err != nil {
			return err
		}
		if err = recordCheckpoint(tx, b, unit, value, midnight(a.now())); err != nil {
			return err
		}
		nb := b
		nb.ProgressPage, nb.ProgressPercent, nb.ProgressSeconds = page, percent, seconds
		if err = addSync(tx, p.BookID, "progress", localProgressKey(nb), p.RemoteKey, "pull", canonicalTime(a.now())); err != nil {
			return err
		}
		if err = addHistory(tx, hcHistoryEntry{At: canonicalTime(a.now()), Direction: "pull", Summary: "Progress pulled from Hardcover.", Details: []hcHistoryDetail{{BookID: p.BookID, Title: p.Title, What: "progress " + p.RemoteText}}}); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM plans WHERE id=?", id); err != nil {
			return err
		}
		back += "hc-progress-pulled"
		return nil
	})
	if err != nil {
		return err
	}
	return redirect(w, r, back)
}

// ---- send a reading's date ----

// hardcoverCurrentDates previews the start date of the current reading.
func (a *App) hardcoverCurrentDates(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	b, err := getBook(a.store.DB, id)
	if err != nil {
		return err
	}
	if b.Status != "reading" {
		return bad("Only a book you are reading has a current start date to sync.")
	}
	return a.previewReadDates(w, r, readingEntry{BookID: id, Title: b.Title, StartedAt: b.StartedAt}, true)
}

func (a *App) hardcoverSendRead(w http.ResponseWriter, r *http.Request) error {
	rid, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil || rid < 1 {
		return bad("Choose a valid reading.")
	}
	entries, err := queryReadings(a.store.DB, "WHERE l.id=?", rid)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return sql.ErrNoRows
	}
	return a.previewReadDates(w, r, entries[0], false)
}

func readDatesText(started, finished string, precision int) string {
	text := periodText(finished, precision)
	if text == "" {
		text = "finish not recorded"
	}
	if started != "" {
		text = "Started " + started + " · " + text
	}
	return text
}

func readDatesKey(readID int64, started, finished string, precision int) string {
	return strconv.FormatInt(readID, 10) + ":" + started + ":" + dateKey(finished, precision)
}

func (a *App) previewReadDates(w http.ResponseWriter, r *http.Request, e readingEntry, currentRead bool) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, e.BookID)
	if err != nil {
		return err
	}
	acct, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	if link == nil || acct == nil {
		return bad("Link this book to Hardcover, and test the connection, first.")
	}
	if !currentRead && e.FinishedAt == nil {
		return bad("This reading has no finish date to send.")
	}
	date, started := "", ""
	if e.FinishedAt != nil {
		date = *e.FinishedAt
	}
	if e.StartedAt != nil {
		started = *e.StartedAt
	}
	prec := max(1, e.FinishedPrecision)
	if err := checkReadingDates(e.StartedAt, e.FinishedAt, prec); err != nil {
		return err
	}
	a.hardcoverMu.Lock()
	remote, err := a.hardcover.shelf(r.Context(), acct.UserID, []int64{link.HardcoverBookID})
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	u, ok := remote[link.HardcoverBookID]
	if !ok {
		return bad("This book is not on your Hardcover shelves yet. Send its status first, from the book page.")
	}
	snapshot, _ := json.Marshal(u.Reads)
	plan := hcReadPlan{BookID: e.BookID, Title: e.Title, ReadingID: e.ID, HardcoverBookID: link.HardcoverBookID, UserBookID: int64(u.ID), UserID: acct.UserID, Date: date, Precision: prec, StartedAt: started, LocalText: readDatesText(started, date, prec), Action: "insert", Current: currentRead, RemoteSnapshot: string(snapshot)}
	type remoteRead struct {
		id            int64
		started, date string
		precision     int
	}
	var match *remoteRead
	for _, rd := range u.Reads {
		d, p, finished := periodStart(rd.FinishedAt, int(rd.Precision))
		start, _, _ := periodStart(rd.StartedAt, 1)
		candidate := remoteRead{int64(rd.ID), start, d, max(1, p)}
		if currentRead {
			if !finished && u.StatusID == 2 {
				match = &candidate
			}
			continue
		}
		if e.HardcoverReadID != nil {
			if candidate.id == *e.HardcoverReadID && finished {
				match = &candidate
				break
			}
			continue
		}
		if finished && dateKey(d, p) == dateKey(date, prec) {
			var n int
			if err := a.store.DB.QueryRow("SELECT count(*) FROM reading_log WHERE hardcover_read_id=? AND id!=?", candidate.id, e.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				match = &candidate
				break
			}
		}
	}
	if match != nil {
		plan.RemoteReadID, plan.RemoteDate, plan.RemotePrec, plan.RemoteStartedAt = match.id, match.date, match.precision, match.started
		plan.RemoteText = readDatesText(match.started, match.date, match.precision)
		localKey := readDatesKey(match.id, started, date, prec)
		remoteKey := readDatesKey(match.id, match.started, match.date, match.precision)
		if localKey == remoteKey {
			if e.HardcoverReadID == nil && !currentRead {
				plan.Action = "map"
				plan.Note = "Hardcover already has this reading. Nothing is sent; this entry is tied to it, so later edits update that read instead of adding another."
			} else {
				plan.Action = "none"
			}
		} else {
			plan.Action = "update"
			last, err := lastSync(a.store.DB, e.BookID, "dates")
			if err != nil {
				return err
			}
			// A different reread's sync is not a baseline for this reading.
			if last != nil && !strings.HasPrefix(last.Local, strconv.FormatInt(match.id, 10)+":") {
				last = nil
			}
			plan.State = progressState(last, localKey, remoteKey, false)
		}
	} else if currentRead {
		plan.Action, plan.Note = "missing", "Hardcover has no read in progress. Send its Currently Reading status first, then sync the start date."
	} else if e.HardcoverReadID != nil {
		plan.Action, plan.Note = "missing", "Hardcover no longer has the finished read this entry was tied to. Nothing is sent."
	} else {
		plan.Note = "A new finished read is added on Hardcover. Its shelf status is not changed."
	}
	var planID string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		planID, err = savePlan(tx, "hc-read", Plan{HardcoverRead: &plan})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/reads/"+planID)
}

func (a *App) hardcoverReadPlan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	read := func(q queryer) (hcReadPlan, int64, error) {
		kind, p, rev, err := readPlan(q, id)
		if err != nil {
			return hcReadPlan{}, 0, err
		}
		if kind != "hc-read" || p.HardcoverRead == nil {
			return hcReadPlan{}, 0, sql.ErrNoRows
		}
		return *p.HardcoverRead, rev, nil
	}
	if r.Method == http.MethodGet {
		p, rev, err := read(a.store.DB)
		if err != nil {
			return err
		}
		return a.render(w, r, "hardcover_read.html", page{"Title": "Send a reading", "Nav": "hardcover", "PlanID": id, "Plan": p, "Stale": rev != state(r).Revision}, 200)
	}
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	// One apply at a time: the plan is read, sent, and deleted under this lock,
	// so a double press finds it gone instead of adding the read twice.
	a.hardcoverMu.Lock()
	defer a.hardcoverMu.Unlock()
	p, rev, err := read(a.store.DB)
	if err != nil {
		return err
	}
	if now, err := revision(a.store.DB); err != nil {
		return err
	} else if now != rev {
		return conflict("Your library changed. Preview this reading again.")
	}
	choice := r.FormValue("use")
	if !in(choice, []string{"keep", "mine", "theirs"}) {
		return bad("Choose what to do with this reading.")
	}
	if choice == "keep" || p.Action == "none" || p.Action == "missing" {
		return redirect(w, r, fmt.Sprintf("/books/%d?notice=hc-read-kept", p.BookID))
	}
	if choice == "theirs" && p.Action != "update" {
		return bad("There is no Hardcover date to take for this reading.")
	}

	acct, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, p.BookID)
	if err != nil {
		return err
	}
	if acct == nil || link == nil || acct.UserID != p.UserID || link.HardcoverBookID != p.HardcoverBookID {
		return conflict("The Hardcover account or book link changed. Preview the reading again.")
	}
	remote, err := a.hardcover.shelf(r.Context(), acct.UserID, []int64{link.HardcoverBookID})
	if err != nil {
		return hardcoverHTTPError(err)
	}
	u, ok := remote[link.HardcoverBookID]
	snapshot, _ := json.Marshal(u.Reads)
	if !ok || int64(u.ID) != p.UserBookID || string(snapshot) != p.RemoteSnapshot || p.Current && u.StatusID != 2 {
		return conflict("Hardcover's reading dates changed. Preview this reading again.")
	}
	remoteID := p.RemoteReadID
	if choice == "mine" && (p.Action == "insert" || p.Action == "update") {

		read := map[string]any{}
		if !p.Current {
			read["finished_at"], read["finished_at_precision"] = p.Date, p.Precision
		}
		if p.StartedAt != "" {
			read["started_at"] = p.StartedAt
		} else if p.RemoteStartedAt != "" {
			read["started_at"] = nil
		}

		query, vars := hcInsertReadMutation, map[string]any{"ub": p.UserBookID, "read": read}
		if p.Action == "update" {
			query, vars = hcUpdateReadMutation, map[string]any{"id": p.RemoteReadID, "read": read}
		}
		got, ambiguous, err := a.hardcover.readMutation(r.Context(), query, vars)
		if err != nil {
			if ambiguous {
				return httpError{Status: http.StatusBadGateway, Message: "Hardcover did not answer, so it may or may not have saved this. Nothing was recorded here. Preview the reading again to see what Hardcover has before sending."}
			}
			return hardcoverHTTPError(err)
		}
		remoteID = got
	}
	// The send has happened: record it whatever else changed meanwhile.
	err = a.store.Write(func(tx *sql.Tx) error {

		if err := current(tx, r); err != nil && choice == "theirs" {
			return err
		}
		entries, err := queryReadings(tx, "WHERE l.id=?", p.ReadingID)
		if err != nil {
			return err
		}
		if len(entries) == 0 && !p.Current {
			return nil
		}
		localKey, remoteKey := readDatesKey(remoteID, p.StartedAt, p.Date, p.Precision), readDatesKey(remoteID, p.StartedAt, p.Date, p.Precision)
		switch {
		case choice == "theirs":
			localKey, remoteKey = readDatesKey(remoteID, p.RemoteStartedAt, p.RemoteDate, p.RemotePrec), readDatesKey(remoteID, p.RemoteStartedAt, p.RemoteDate, p.RemotePrec)

			if p.Current {
				err = saveCurrentStart(tx, p.BookID, nullable(p.RemoteStartedAt), true)
			} else {
				if err = checkReadingDates(nullable(p.RemoteStartedAt), nullable(p.RemoteDate), p.RemotePrec); err != nil {
					return err
				}
				_, err = tx.Exec("UPDATE reading_log SET started_at=?,finished_at=?,finished_precision=?,source='manual' WHERE id=?", nullable(p.RemoteStartedAt), p.RemoteDate, p.RemotePrec, p.ReadingID)
			}
			if err != nil {
				return err
			}

			if !p.Current {
				if _, err = tx.Exec("UPDATE reading_log SET hardcover_read_id=? WHERE id=? AND hardcover_read_id IS NULL", remoteID, p.ReadingID); err != nil {
					return err
				}
			}
			if err = bump(tx); err != nil {
				return err
			}
		case remoteID > 0 && !p.Current:

			changed, e := execChanged(tx, "UPDATE reading_log SET hardcover_read_id=? WHERE id=? AND hardcover_read_id IS NULL", remoteID, p.ReadingID)
			if e != nil {
				return conflict("Another reading here is already tied to that Hardcover read.")
			}
			if changed {
				if err = bump(tx); err != nil {
					return err
				}
			}

		}
		dir := map[bool]string{true: "pull", false: "push"}[choice == "theirs"]
		if err = addHistory(tx, hcHistoryEntry{At: canonicalTime(a.now()), Direction: dir, Summary: "A reading's dates were synced.", Details: []hcHistoryDetail{{BookID: p.BookID, Title: p.Title, What: "reading dates " + map[bool]string{true: p.RemoteText, false: p.LocalText}[choice == "theirs"]}}}); err != nil {
			return err
		}
		if err = addSync(tx, p.BookID, "dates", localKey, remoteKey, map[bool]string{true: "pull", false: "push"}[choice == "theirs"], canonicalTime(a.now())); err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM plans WHERE id=?", id)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=hc-read-sent", p.BookID))
}
