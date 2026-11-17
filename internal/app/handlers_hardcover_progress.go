package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
)

// hcProgressPlan is a previewed pull of the current read's progress.
type hcProgressPlan struct {
	BookID     int64  `json:"book_id"`
	Title      string `json:"title"`
	LocalText  string `json:"local_text"`
	RemoteText string `json:"remote_text"`
	// State is same, remote, local, both, or none (nothing usable).
	State     string `json:"state"`
	Page      *int   `json:"page,omitempty"`
	Percent   *int   `json:"percent,omitempty"`
	Note      string `json:"note,omitempty"`
	RemoteKey string `json:"remote_key"`
}

// hcReadPlan is a previewed send of one finished reading's date.
type hcReadPlan struct {
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
	if p.Seconds > 0 {
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
	plan := hcProgressPlan{BookID: id, Title: b.Title, LocalText: b.ProgressText(), RemoteText: remoteProgressText(*open), Page: conv.Page, Percent: conv.Percent, Note: conv.Note, RemoteKey: remoteProgressKey(*open)}
	if plan.LocalText == "" {
		plan.LocalText = "none saved"
	}
	if conv.Skip != "" {
		plan.State, plan.Note = "none", conv.Skip
	} else {
		equal := conv.Page != nil && b.ProgressPage != nil && b.ProgressPercent == nil && *conv.Page == *b.ProgressPage ||
			conv.Percent != nil && b.ProgressPercent != nil && *conv.Percent == *b.ProgressPercent
		plan.State = progressState(last, localProgressKey(b), plan.RemoteKey, equal)
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
	case r.Seconds > 0 && r.Pages == 0:
		return fmt.Sprintf("%d seconds", r.Seconds)
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
		if value == nil {
			return bad("There is nothing usable to pull.")
		}
		var page, percent *int
		if unit == "page" {
			page = value
		} else {
			percent = value
		}
		if _, err = tx.Exec("UPDATE books SET progress_page=?,progress_percent=? WHERE id=?", page, percent, p.BookID); err != nil {
			return err
		}
		if err = recordCheckpoint(tx, b, unit, value, midnight(a.now())); err != nil {
			return err
		}
		nb := b
		nb.ProgressPage, nb.ProgressPercent = page, percent
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

func (a *App) hardcoverSendRead(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
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
	e := entries[0]
	link, err := getBookLink(a.store.DB, e.BookID)
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
	case e.FinishedAt == nil || len(*e.FinishedAt) < 10:
		return bad("This reading has no finish date to send.")
	}
	date, prec := (*e.FinishedAt)[:10], max(1, e.FinishedPrecision)
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
	plan := hcReadPlan{BookID: e.BookID, Title: e.Title, ReadingID: rid, HardcoverBookID: link.HardcoverBookID, UserBookID: int64(u.ID), Date: date, Precision: prec, LocalText: periodText(date, prec), Action: "insert"}
	type remoteRead struct {
		id        int64
		date      string
		precision int
	}
	var reads []remoteRead
	for _, rd := range u.Reads {
		if d, p, ok := periodStart(rd.FinishedAt, int(rd.Precision)); ok {
			reads = append(reads, remoteRead{int64(rd.ID), d, p})
		}
	}
	find := func(f func(remoteRead) bool) *remoteRead {
		for i := range reads {
			if f(reads[i]) {
				return &reads[i]
			}
		}
		return nil
	}
	if e.HardcoverReadID != nil {
		m := find(func(x remoteRead) bool { return x.id == *e.HardcoverReadID })
		if m == nil {
			plan.Action, plan.Note = "missing", "Hardcover no longer has the read this entry was tied to, or it has no finish date there. Nothing is sent."
		} else {
			plan.RemoteReadID, plan.RemoteDate, plan.RemotePrec, plan.RemoteText = m.id, m.date, m.precision, periodText(m.date, m.precision)
			last, err := lastSync(a.store.DB, e.BookID, "dates")
			if err != nil {
				return err
			}
			if dateKey(date, prec) == dateKey(m.date, m.precision) {
				plan.Action = "none"
			} else {
				plan.Action = "update"
				plan.State = progressState(last, dateKey(date, prec), dateKey(m.date, m.precision), false)
			}
		}
	} else if m := find(func(x remoteRead) bool { return dateKey(x.date, x.precision) == dateKey(date, prec) }); m != nil {
		plan.Action, plan.RemoteReadID, plan.RemoteDate, plan.RemotePrec, plan.RemoteText = "map", m.id, m.date, m.precision, plan.LocalText
		plan.Note = "Hardcover already has a read finished on this date. Nothing is sent; this entry is tied to it, so later edits update that read instead of adding another."
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
	remoteID := p.RemoteReadID
	if choice == "mine" && (p.Action == "insert" || p.Action == "update") {
		read := map[string]any{"finished_at": p.Date, "finished_at_precision": p.Precision}
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
		entries, err := queryReadings(tx, "WHERE l.id=?", p.ReadingID)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		localKey, remoteKey := dateKey(p.Date, p.Precision), dateKey(p.Date, p.Precision)
		switch {
		case choice == "theirs":
			localKey, remoteKey = dateKey(p.RemoteDate, p.RemotePrec), dateKey(p.RemoteDate, p.RemotePrec)
			if _, err = tx.Exec("UPDATE reading_log SET finished_at=?,finished_precision=?,source=CASE WHEN source='goodreads' THEN 'manual' ELSE source END WHERE id=?", p.RemoteDate, p.RemotePrec, p.ReadingID); err != nil {
				return err
			}
		case remoteID > 0:
			if _, err = tx.Exec("UPDATE reading_log SET hardcover_read_id=? WHERE id=? AND hardcover_read_id IS NULL", remoteID, p.ReadingID); err != nil {
				return conflict("Another reading here is already tied to that Hardcover read.")
			}
		}
		dir := map[bool]string{true: "pull", false: "push"}[choice == "theirs"]
		if err = addHistory(tx, hcHistoryEntry{At: canonicalTime(a.now()), Direction: dir, Summary: "A reading's finish date was synced.", Details: []hcHistoryDetail{{BookID: p.BookID, Title: p.Title, What: "finish date " + p.LocalText}}}); err != nil {
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
