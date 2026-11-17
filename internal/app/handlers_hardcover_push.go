package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strconv"
)

// sendIDs are the books a send preview covers: one book from its page, the
// books ticked on a form, or the linked books on one shelf.
func (a *App) sendIDs(r *http.Request) ([]int64, error) {
	if raw := r.PathValue("id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			return nil, bad("Choose a valid book.")
		}
		return []int64{id}, nil
	}
	var ids []int64
	if status := r.FormValue("status"); status != "" {
		if _, ok := pushStatuses[status]; !ok {
			return nil, bad("Choose a shelf to send.")
		}
		rows, err := a.store.DB.Query("SELECT b.id FROM books b JOIN book_hardcover h ON h.book_id=b.id WHERE b.reading_status=? ORDER BY b.id LIMIT ?", status, hardcoverPushLimit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	} else {
		for _, raw := range r.Form["book"] {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id < 1 {
				return nil, bad("Choose valid books.")
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, bad("There are no linked books to send. Link books to Hardcover first.")
	}
	if len(ids) > hardcoverPushLimit {
		return nil, bad("Send at most 50 books at a time.")
	}
	return ids, nil
}

// hardcoverSend reads Hardcover's current rows for the chosen books and stores
// what sending would do as a preview. It sends nothing.
func (a *App) hardcoverSend(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	link, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	if link == nil {
		return bad("Test the Hardcover connection first, so the app knows which account is yours.")
	}
	ids, err := a.sendIDs(r)
	if err != nil {
		return err
	}
	id, err := a.previewSend(r, link, ids)
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/pushes/"+id)
}

// previewSend reads Hardcover's rows for the books and stores what sending
// would do as a preview. It sends nothing.
func (a *App) previewSend(r *http.Request, link *hardcoverLink, ids []int64) (string, error) {
	books, unlinked, err := loadPushBooks(a.store.DB, ids)
	if err != nil {
		return "", err
	}
	if len(unlinked) > 0 {
		return "", bad("Link " + strconv.Itoa(len(unlinked)) + " of these books to Hardcover first. A book that is not linked is never sent.")
	}
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return "", err
	}
	hcIDs := make([]int64, 0, len(books))
	for _, b := range books {
		hcIDs = append(hcIDs, b.HardcoverBookID)
	}
	a.hardcoverMu.Lock()
	remote, err := a.hardcover.shelf(r.Context(), link.UserID, hcIDs)
	a.hardcoverMu.Unlock()
	if err != nil {
		return "", hardcoverHTTPError(err)
	}
	plan := buildPushPlan(books, remote, link.UserID, hcPrivacy(prefs))
	var id string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = savePlan(tx, "hc-push", Plan{HardcoverPush: &plan})
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (a *App) pushPlan(q queryer, id string) (Plan, int64, error) {
	kind, p, rev, err := readPlan(q, id)
	if err != nil {
		return p, 0, err
	}
	if kind != "hc-push" || p.HardcoverPush == nil {
		return p, 0, sql.ErrNoRows
	}
	return p, rev, nil
}

func (a *App) hardcoverPushPlan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		p, rev, err := a.pushPlan(a.store.DB, id)
		if err != nil {
			return err
		}
		return a.render(w, r, "hardcover_push.html", page{"Title": "Send to Hardcover", "Nav": "hardcover", "PlanID": id, "Push": p.HardcoverPush, "Stale": rev != state(r).Revision}, 200)
	}
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	// One send at a time: the plan is read, sent, and saved under this lock, so a
	// double press cannot send the same books twice.
	a.hardcoverMu.Lock()
	defer a.hardcoverMu.Unlock()
	p, rev, err := a.pushPlan(a.store.DB, id)
	if err != nil {
		return err
	}
	if now, err := revision(a.store.DB); err != nil {
		return err
	} else if now != rev {
		return conflict("Your library changed. Generate a fresh preview before sending.")
	}
	plan := p.HardcoverPush
	var unknown, pending []*hcPushItem
	for i := range plan.Items {
		it := &plan.Items[i]
		switch {
		case it.State == "unknown":
			unknown = append(unknown, it)
		case it.pending() && len(pending) < hardcoverPushBatch:
			pending = append(pending, it)
		}
	}
	if len(unknown) == 0 && len(pending) == 0 {
		return bad("There is nothing left to send.")
	}
	plan.Stopped = ""
	// A request that timed out may have been applied. Look before sending again;
	// the check and the next send are separate presses so each stays in budget.
	var sendErr error
	if len(unknown) > 0 {
		sendErr = a.hardcover.verifyUnknown(r.Context(), plan.UserID, unknown)
	} else {
		sendErr = a.hardcover.pushBatch(r.Context(), pending)
	}
	if sendErr != nil {
		plan.Stopped = hardcoverHTTPError(sendErr).Error()
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		// A change here since the preview does not undo what was already sent, so it
		// is recorded whatever the revision is now; a refusal here would forget
		// books Hardcover already has. Nothing is bumped: sending changes no
		// ranking input.
		at := canonicalTime(a.now())
		// Only books sent or confirmed by this press are recorded.
		var details []hcHistoryDetail
		for _, it := range append(unknown, pending...) {
			if it.State == "sent" {
				if err := recordPush(tx, *it, at); err != nil {
					return err
				}
				what := ""
				if it.Target != 0 {
					what = "status " + it.TargetLabel()
				}
				if it.SendRating > 0 {
					what += map[bool]string{true: ", ", false: ""}[what != ""] + "rating " + trimStars(it.SendRating)
				}
				details = append(details, hcHistoryDetail{BookID: it.BookID, Title: it.Title, What: what})
			}
		}
		if len(details) > 0 {
			if err := addHistory(tx, hcHistoryEntry{At: at, Direction: "push", Summary: fmt.Sprintf("Sent %d book(s) to Hardcover.", len(details)), Details: details}); err != nil {
				return err
			}
		}
		raw, err := encodePlan(Plan{HardcoverPush: plan})
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE plans SET payload=? WHERE id=?", raw, id)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/pushes/"+id)
}

// hardcoverTakeBook resolves one conflict toward Hardcover: it reads that
// book's current Hardcover row and previews it as an ordinary pull. Keeping
// the local side needs no action, because a conflict is never sent.
func (a *App) hardcoverTakeBook(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	p, _, err := a.pushPlan(a.store.DB, r.PathValue("id"))
	if err != nil {
		return err
	}
	bookID, err := strconv.ParseInt(r.FormValue("book_id"), 10, 64)
	if err != nil {
		return bad("Choose a book from this preview.")
	}
	var item *hcPushItem
	for i := range p.HardcoverPush.Items {
		if it := &p.HardcoverPush.Items[i]; it.BookID == bookID && it.Action == "conflict" {
			item = it
		}
	}
	if item == nil {
		return bad("That book is not a conflict in this preview.")
	}
	a.hardcoverMu.Lock()
	remote, err := a.hardcover.shelf(r.Context(), p.HardcoverPush.UserID, []int64{item.HardcoverBookID})
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	u, ok := remote[item.HardcoverBookID]
	if !ok {
		return conflict("Hardcover no longer has this book on your shelves.")
	}
	it, ok := u.item()
	if !ok {
		return errHardcoverResponse
	}
	plan := hcPullPlan{Items: []hcPullItem{it}, Complete: true}
	var id string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = savePlan(tx, "hc-pull", Plan{HardcoverPull: &plan})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/pulls/"+id)
}
