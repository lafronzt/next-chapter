package app

import (
	"database/sql"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

const hardcoverSinceKey = "hardcover_pull_since"

// hcTimestamp is the shape of an updated_at value passed back by the reader's
// own form: an ISO timestamp, nothing that could change the query.
var hcTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[T ][0-9:.]+(Z|[+-]\d{2}:?\d{2})?$`)

// hardcoverPull fetches a run of the reader's own library and stores it as a
// preview. It is a POST the reader presses; nothing fetches on a page load.
func (a *App) hardcoverPull(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	link, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	if link == nil {
		return bad("Test the Hardcover connection first, so the pull knows which account is yours.")
	}
	since, offset, high := "", 0, ""
	if r.FormValue("continue") == "1" {
		for _, v := range []string{r.FormValue("since"), r.FormValue("high")} {
			if v != "" && !hcTimestamp.MatchString(v) {
				return bad("That pull could not be continued. Start a new one.")
			}
		}
		since, high = r.FormValue("since"), r.FormValue("high")
		if offset, err = strconv.Atoi(r.FormValue("offset")); err != nil || offset < 0 {
			return bad("That pull could not be continued. Start a new one.")
		}
	} else if r.FormValue("mode") != "full" {
		if since, err = readMeta(a.store.DB, hardcoverSinceKey, ""); err != nil {
			return err
		}
	}
	a.hardcoverMu.Lock()
	plan, err := a.hardcover.pullRun(r.Context(), link.UserID, since, offset)
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	if high > plan.HighWater {
		plan.HighWater = high
	}
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

const pullRowsPerPage = 50

func (a *App) hardcoverPullPlan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		kind, p, rev, err := readPlan(a.store.DB, id)
		if err != nil {
			return err
		}
		if kind != "hc-pull" || p.HardcoverPull == nil {
			return sql.ErrNoRows
		}
		data := page{"Title": "Import from Hardcover", "Nav": "hardcover", "PlanID": id, "Pull": p.HardcoverPull}
		if rev != state(r).Revision {
			data["Stale"] = true
			return a.render(w, r, "hardcover_pull.html", data, 200)
		}
		var res pullResult
		var decisions []pullDecision
		if err = a.store.Write(func(tx *sql.Tx) error {
			var err error
			res, decisions, err = previewPull(tx, p.HardcoverPull.Items, pullOptions{})
			return err
		}); err != nil {
			return err
		}
		r2 := *r
		u := *r.URL
		q := u.Query()
		q.Set("per_page", strconv.Itoa(pullRowsPerPage))
		u.RawQuery = q.Encode()
		r2.URL = &u
		pager := newPager(&r2, len(decisions), "/hardcover/pulls/"+id, url.Values{})
		rows := decisions[min(pager.offset(), len(decisions)):min(pager.offset()+pullRowsPerPage, len(decisions))]
		choices := res.RatingChoices
		if len(choices) > 100 {
			choices = choices[:100]
		}
		data["Result"], data["Rows"], data["Pager"], data["Choices"], data["MoreChoices"] = res, rows, pager, choices, len(res.RatingChoices)-len(choices)
		data["NoOp"] = !res.Changed && len(res.RatingChoices) == 0
		return a.render(w, r, "hardcover_pull.html", data, 200)
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		kind, p, rev, err := readPlan(tx, id)
		if err != nil {
			return err
		}
		if kind != "hc-pull" || p.HardcoverPull == nil {
			return sql.ErrNoRows
		}
		if now, err := revision(tx); err != nil {
			return err
		} else if now != rev {
			return conflict("Your library changed. Generate a fresh preview before applying it.")
		}
		opt := pullOptions{UseRating: map[int64]bool{}}
		for _, it := range p.HardcoverPull.Items {
			if r.FormValue("use_rating_"+strconv.FormatInt(it.HardcoverBookID, 10)) != "" {
				opt.UseRating[it.HardcoverBookID] = true
			}
		}
		res, err := applyPull(tx, p.HardcoverPull.Items, opt, nil)
		if err != nil {
			return err
		}
		if res.Changed {
			if err = bump(tx); err != nil {
				return err
			}
		}
		if p.HardcoverPull.Complete && p.HardcoverPull.HighWater != "" {
			if err = setMeta(tx, hardcoverSinceKey, p.HardcoverPull.HighWater); err != nil {
				return err
			}
		}
		_, err = tx.Exec("DELETE FROM plans WHERE id=?", id)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover?notice=hardcover-pulled")
}
