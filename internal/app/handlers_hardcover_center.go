package app

import (
	"database/sql"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// centerData is what the sync center shows beyond the connection test.
func (a *App) centerData(data page, prefs Preferences) error {
	data["SyncPull"], data["SyncPush"] = prefs.HardcoverSyncPull, prefs.HardcoverSyncPush
	history, err := readMeta(a.store.DB, hardcoverHistoryKey, []hcHistoryEntry{})
	if err != nil {
		return err
	}
	data["History"] = history
	since, err := readMeta(a.store.DB, hardcoverSinceKey, "")
	if err != nil {
		return err
	}
	data["Since"] = since
	var optedIn int
	var unlinkedN int
	if err = a.store.DB.QueryRow("SELECT (SELECT count(*) FROM book_hardcover WHERE progress_sync=1),(SELECT count(*) FROM books b WHERE b.reading_status IN ('to-read','reading','read','abandoned') AND NOT EXISTS (SELECT 1 FROM book_hardcover h WHERE h.book_id=b.id))").Scan(&optedIn, &unlinkedN); err != nil {
		return err
	}
	data["ProgressBooks"], data["UnlinkedCount"] = optedIn, unlinkedN
	rows, err := a.store.DB.Query("SELECT b.id,b.title,b.author FROM books b WHERE b.reading_status IN ('to-read','reading','read','abandoned') AND NOT EXISTS (SELECT 1 FROM book_hardcover h WHERE h.book_id=b.id) ORDER BY b.id LIMIT 20")
	if err != nil {
		return err
	}
	defer rows.Close()
	type unlinked struct {
		ID            int64
		Title, Author string
	}
	var list []unlinked
	for rows.Next() {
		var u unlinked
		if err = rows.Scan(&u.ID, &u.Title, &u.Author); err != nil {
			return err
		}
		list = append(list, u)
	}
	data["Unlinked"] = list
	if a.hardcover != nil {
		scopes, failure, blocked := a.hardcover.Status()
		type scopeRow struct {
			Name, State string
			At          string
		}
		var seen []scopeRow
		for _, name := range []string{"read:me", "read:library", "write:library", "read:catalog", "read:journal"} {
			row := scopeRow{Name: name, State: "not used yet"}
			if s, ok := scopes[name]; ok {
				row.State, row.At = "worked", s.At.UTC().Format(time.RFC3339)
				if !s.OK {
					row.State = "refused"
				}
			}
			seen = append(seen, row)
		}
		data["Scopes"], data["Banner"] = seen, bannerFor(failure, blocked, a.now())
	}
	return rows.Err()
}

// hardcoverSwitches saves the sync switches. Both are off until the reader
// turns them on, and turning one off stops that part of the center.
func (a *App) hardcoverSwitches(w http.ResponseWriter, r *http.Request) error {
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		prefs, err := readMeta(tx, "preferences", defaultPreferences)
		if err != nil {
			return err
		}
		prefs.HardcoverSyncPull, prefs.HardcoverSyncPush = r.FormValue("pull") != "", r.FormValue("push") != ""
		if err = setMeta(tx, "preferences", prefs); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover?notice=saved")
}

// hardcoverCheck fetches what changed on Hardcover since the last import (one
// request per 100 changed books, at most 8) and sorts it. It is a POST the
// reader presses: nothing runs on a page load or a timer. It writes only the
// result, as a preview; no book changes.
func (a *App) hardcoverCheck(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	link, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	if link == nil {
		return bad("Test the Hardcover connection first, so the check knows which account is yours.")
	}
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	if !prefs.HardcoverSyncPull {
		return bad("Turn on “Check Hardcover for changes” on this page first. It is off by default.")
	}
	since, err := readMeta(a.store.DB, hardcoverSinceKey, "")
	if err != nil {
		return err
	}
	a.hardcoverMu.Lock()
	run, err := a.hardcover.pullRun(r.Context(), link.UserID, since, 0)
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	var id string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		check, err := buildCheck(tx, run, canonicalTime(a.now()))
		if err != nil {
			return err
		}
		id, err = savePlan(tx, "hc-check", Plan{HardcoverCheck: &check})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/changes/"+id)
}

func (a *App) readCheck(q queryer, id string) (*hcCheck, int64, error) {
	kind, p, rev, err := readPlan(q, id)
	if err != nil {
		return nil, 0, err
	}
	if kind != "hc-check" || p.HardcoverCheck == nil {
		return nil, 0, sql.ErrNoRows
	}
	return p.HardcoverCheck, rev, nil
}

func (a *App) hardcoverChanges(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	c, rev, err := a.readCheck(a.store.DB, id)
	if err != nil {
		return err
	}
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	return a.render(w, r, "hardcover_changes.html", page{"Title": "Hardcover changes", "Nav": "hardcover", "PlanID": id, "Check": c, "Stale": rev != state(r).Revision, "SyncPush": prefs.HardcoverSyncPush}, 200)
}

// hardcoverChangesPull turns part of a check into the ordinary previewed
// import: Hardcover's side of one group, the new books, or one book. The rows
// were fetched by the check, so this costs no request, and nothing applies
// until the reader confirms on the import page.
func (a *App) hardcoverChangesPull(w http.ResponseWriter, r *http.Request) error {
	c, rev, err := a.readCheck(a.store.DB, r.PathValue("id"))
	if err != nil {
		return err
	}
	if now, err := revision(a.store.DB); err != nil {
		return err
	} else if now != rev {
		return conflict("Your library changed since this check. Check for changes again.")
	}
	want := map[int64]bool{}
	switch r.FormValue("group") {
	case groupHardcover:
		for _, row := range c.Ahead() {
			want[row.HardcoverBookID] = true
		}
	case "new":
		for _, it := range c.New {
			want[it.HardcoverBookID] = true
		}
	case groupBoth:
		bookID, err := strconv.ParseInt(r.FormValue("hardcover_book_id"), 10, 64)
		if err != nil {
			return bad("Choose a book from this check.")
		}
		for _, row := range c.Both() {
			if row.HardcoverBookID == bookID {
				want[bookID] = true
			}
		}
	default:
		return bad("Choose which changes to preview.")
	}
	var items []hcPullItem
	for _, it := range slices.Concat(c.Items, c.New) {
		if want[it.HardcoverBookID] {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return bad("There is nothing in that group to preview.")
	}
	plan := hcPullPlan{Items: items, Complete: true}
	var planID string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		planID, err = savePlan(tx, "hc-pull", Plan{HardcoverPull: &plan})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/pulls/"+planID)
}

// hardcoverChangesSend previews sending the books this library is ahead on,
// through the ordinary send preview (which reads Hardcover's rows fresh).
func (a *App) hardcoverChangesSend(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	c, rev, err := a.readCheck(a.store.DB, r.PathValue("id"))
	if err != nil {
		return err
	}
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	if !prefs.HardcoverSyncPush {
		return bad("Turn on “Offer to send local changes” on this page first. It is off by default.")
	}
	if now, err := revision(a.store.DB); err != nil {
		return err
	} else if now != rev {
		return conflict("Your library changed since this check. Check for changes again.")
	}
	link, err := loadHardcoverLink(a.store.DB)
	if err != nil || link == nil {
		return firstErr(err, bad("Test the Hardcover connection first."))
	}
	var ids []int64
	for _, row := range c.Local() {
		ids = append(ids, row.BookID)
	}
	if len(ids) == 0 {
		return bad("Nothing here is ahead of Hardcover.")
	}
	if len(ids) > hardcoverPushLimit {
		ids = ids[:hardcoverPushLimit]
	}
	id, err := a.previewSend(r, link, ids)
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/pushes/"+id)
}

// hardcoverHistoryClear empties the sync history list. It changes no book.
func (a *App) hardcoverHistoryClear(w http.ResponseWriter, r *http.Request) error {
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM meta WHERE key=?", hardcoverHistoryKey)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover?notice=saved")
}

// ---- forget ids ----

func (a *App) hardcoverForgetPreview(w http.ResponseWriter, r *http.Request) error {
	var id string
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		p, err := countForget(tx)
		if err != nil {
			return err
		}
		id, err = savePlan(tx, "hc-forget", Plan{HardcoverForget: &p})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/forget/"+id)
}

func (a *App) hardcoverForgetPlan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		kind, p, rev, err := readPlan(a.store.DB, id)
		if err != nil {
			return err
		}
		if kind != "hc-forget" || p.HardcoverForget == nil {
			return sql.ErrNoRows
		}
		return a.render(w, r, "hardcover_forget.html", page{"Title": "Forget Hardcover ids", "Nav": "hardcover", "PlanID": id, "Plan": p.HardcoverForget, "Stale": rev != state(r).Revision}, 200)
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		kind, p, rev, err := readPlan(tx, id)
		if err != nil {
			return err
		}
		if kind != "hc-forget" || p.HardcoverForget == nil {
			return sql.ErrNoRows
		}
		if now, err := revision(tx); err != nil {
			return err
		} else if now != rev {
			return conflict("Your library changed. Preview again.")
		}
		if err = forgetIDs(tx); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM plans WHERE id=?", id); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover?notice=hc-forgotten")
}
