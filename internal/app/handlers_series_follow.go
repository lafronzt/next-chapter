package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// followCandidate is a Hardcover series a local series could follow, found in
// the cache of its linked books. The id is shown because two series can share
// a name.
type followCandidate struct {
	SeriesID int64
	Name     string
	Books    int
}

// followCandidatesByGroup reads the cache once and returns, for each local
// series (by folded name), the Hardcover series its refreshed books belong
// to. A book in two series contributes both.
func followCandidatesByGroup(q queryer) (map[string][]followCandidate, error) {
	rows, err := q.Query(`SELECT b.series,sc.series_id,sc.series_name,sc.source_book_id FROM series_cache sc JOIN books b ON b.id=sc.source_book_id
 WHERE trim(b.series)!='' GROUP BY b.series,sc.series_id,sc.series_name,sc.source_book_id ORDER BY sc.series_id,sc.source_book_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		group string
		id    int64
	}
	byKey := map[key]*followCandidate{}
	out := map[string][]followCandidate{}
	order := []key{}
	for rows.Next() {
		var series, name string
		var id, src int64
		if err = rows.Scan(&series, &id, &name, &src); err != nil {
			return nil, err
		}
		k := key{foldKey(strings.TrimSpace(series)), id}
		if c, ok := byKey[k]; ok {
			c.Books++
			continue
		}
		byKey[k] = &followCandidate{SeriesID: id, Name: name, Books: 1}
		order = append(order, k)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, k := range order {
		out[k.group] = append(out[k.group], *byKey[k])
	}
	for g := range out {
		slices.SortFunc(out[g], func(a, b followCandidate) int {
			if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
				return c
			}
			return int(a.SeriesID - b.SeriesID)
		})
	}
	return out, nil
}

func seriesIDParam(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		return 0, bad("Choose a followed series.")
	}
	return id, nil
}

// seriesFollowAction shows what following a series means, then follows it.
// Following reads no Hardcover data and changes no book.
func (a *App) seriesFollowAction(w http.ResponseWriter, r *http.Request) error {
	group := strings.TrimSpace(r.FormValue("group"))
	sid, err := strconv.ParseInt(r.FormValue("series"), 10, 64)
	if group == "" || err != nil || sid < 1 {
		return bad("Choose a series to follow.")
	}
	cands, err := followCandidatesByGroup(a.store.DB)
	if err != nil {
		return err
	}
	var chosen *followCandidate
	for _, c := range cands[foldKey(group)] {
		if c.SeriesID == sid {
			c := c
			chosen = &c
		}
	}
	if chosen == nil {
		return bad("That Hardcover series is not one of the choices for this series. Refresh a linked book from Hardcover first.")
	}
	existing, err := getFollow(a.store.DB, sid)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		return a.render(w, r, "series_follow.html", page{"Title": "Follow a series", "Nav": "series", "Group": group, "Candidate": *chosen, "Existing": existing}, 200)
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		follows, err := loadFollows(tx)
		if err != nil {
			return err
		}
		if len(follows) >= maxSeriesFollows && !slices.ContainsFunc(follows, func(f seriesFollow) bool { return f.SeriesID == sid }) {
			return bad(fmt.Sprintf("You can follow at most %d series. Unfollow one first.", maxSeriesFollows))
		}
		_, err = tx.Exec("INSERT INTO series_follows(series_id,name,local_series,followed_at) VALUES(?,?,?,?) ON CONFLICT(series_id) DO NOTHING", sid, chosen.Name, group, canonicalTime(a.now()))
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/series/followed/%d?notice=series-followed", sid))
}

// seriesUnfollow stops following and clears that series' discovery history.
// It never touches a book or the queue.
func (a *App) seriesUnfollow(w http.ResponseWriter, r *http.Request) error {
	sid, err := seriesIDParam(r, "sid")
	if err != nil {
		return err
	}
	if r.FormValue("confirm") != "unfollow" {
		return bad("Confirm that you want to stop following this series.")
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM series_discoveries WHERE series_id=?", sid); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM series_follows WHERE series_id=?", sid)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/series?notice=series-unfollowed")
}

// seriesCheck reads the chosen followed series from Hardcover, a bounded
// number of requests each, and records what it saw. It is the only place the
// catalog is read for follows, and only on this button.
func (a *App) seriesCheck(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	var ids []int64
	for _, raw := range r.Form["series"] {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	switch {
	case len(ids) == 0:
		return bad("Choose at least one followed series to check.")
	case len(ids) > hcSeriesCheckMaxBatch:
		return bad(fmt.Sprintf("Check at most %d series at a time.", hcSeriesCheckMaxBatch))
	}
	restart := r.FormValue("restart") == "1"
	var outcomes []seriesOutcome
	var firstErr error
	for _, id := range ids {
		f, err := getFollow(a.store.DB, id)
		if err != nil {
			return err
		}
		if f == nil {
			return conflict("You no longer follow one of these series.")
		}
		offset := f.NextOffset
		if restart {
			offset = 0
		}
		a.hardcoverMu.Lock()
		res := a.readSeries(r.Context(), id, offset)
		a.hardcoverMu.Unlock()
		var out seriesOutcome
		err = a.store.Write(func(tx *sql.Tx) error {
			cur, err := getFollow(tx, id)
			if err != nil {
				return err
			}
			if cur == nil || cur.NextOffset != f.NextOffset || cur.Run != f.Run {
				return conflict("This series was checked or changed meanwhile. Check it again.")
			}
			out, err = recordSeriesCheck(tx, *cur, res, canonicalTime(a.now()))
			return err
		})
		if err != nil {
			return err
		}
		outcomes = append(outcomes, out)
		if res.Err != nil {
			if firstErr == nil {
				firstErr = res.Err
			}
			// A refusal that applies to every request ends the batch.
			if class := out.Error; class == "rate-limit" || class == "scope" || class == "auth" {
				break
			}
		}
	}
	// If nothing was read at all, say why instead of reporting a check.
	if firstErr != nil && !slices.ContainsFunc(outcomes, func(o seriesOutcome) bool { return o.Error == "" || o.Seen > 0 }) {
		return hardcoverHTTPError(firstErr)
	}
	if len(ids) == 1 {
		return redirect(w, r, fmt.Sprintf("/series/followed/%d?notice=series-checked", ids[0]))
	}
	return redirect(w, r, "/series?notice=series-checked")
}

// discoveryViews classifies one follow's members.
func (a *App) discoveryViews(q queryer, sid int64) ([]discoveryView, error) {
	ix, err := loadLibraryIndex(q)
	if err != nil {
		return nil, err
	}
	ds, err := loadDiscoveries(q, sid)
	if err != nil {
		return nil, err
	}
	today := a.now().UTC().Truncate(24 * time.Hour)
	views := make([]discoveryView, len(ds))
	for i, d := range ds {
		views[i] = classify(d, ix, today)
	}
	slices.SortFunc(views, byDiscoveryOrder)
	// Two members sharing a number stay visible and are marked, not merged.
	seen := map[float64]int{}
	for _, v := range views {
		if v.Position != nil {
			seen[*v.Position]++
		}
	}
	for i := range views {
		views[i].SamePosition = views[i].Position != nil && seen[*views[i].Position] > 1
	}
	return views, nil
}

var discoveryTabs = []struct{ Key, Label string }{
	{catNew, "New"}, {catUpcoming, "Upcoming"}, {catKnown, "Not in your library"}, {catPossible, "Possible matches"},
	{catLibrary, "In your library"}, {catRemoved, "Removed earlier"}, {catUnlisted, "No longer listed"}, {catDismissed, "Dismissed"},
}

type discoveryTab struct {
	Key, Label string
	Count      int
	Current    bool
}

// seriesFollowed shows one followed series: its status, what the last checks
// found, and the actions on each result. It reads only saved data.
func (a *App) seriesFollowed(w http.ResponseWriter, r *http.Request) error {
	sid, err := seriesIDParam(r, "sid")
	if err != nil {
		return err
	}
	f, err := getFollow(a.store.DB, sid)
	if err != nil {
		return err
	}
	if f == nil {
		return sql.ErrNoRows
	}
	views, err := a.discoveryViews(a.store.DB, sid)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, v := range views {
		counts[v.Category]++
	}
	view := r.URL.Query().Get("view")
	valid := false
	for _, t := range discoveryTabs {
		valid = valid || t.Key == view
	}
	if !valid {
		view = catKnown
		for _, k := range []string{catNew, catUpcoming, catKnown, catPossible, catLibrary} {
			if counts[k] > 0 {
				view = k
				break
			}
		}
	}
	tabs := make([]discoveryTab, len(discoveryTabs))
	for i, t := range discoveryTabs {
		tabs[i] = discoveryTab{t.Key, t.Label, counts[t.Key], t.Key == view}
	}
	var shown []discoveryView
	for _, v := range views {
		if v.Category == view {
			shown = append(shown, v)
		}
	}
	pages := newPager(r, len(shown), fmt.Sprintf("/series/followed/%d", sid), url.Values{"view": {view}})
	if len(shown) > 0 {
		shown = shown[pages.offset():min(pages.offset()+pages.PerPage, len(shown))]
	}
	now := a.now()
	return a.render(w, r, "series_followed.html", page{"Title": f.Name + ": new books", "Nav": "series", "Follow": *f, "Tabs": tabs, "View": view, "Items": shown, "Pager": pages,
		"Offerable": view == catNew || view == catUpcoming || view == catKnown || view == catPossible, "Stale": f.staleAt(now), "ErrorText": seriesErrorText(f.LastError),
		"Ready": a.hardcoverReady() == nil, "Total": len(views), "ManyPages": f.Total > hcSeriesPageSize*hcSeriesPagesPerCheck, "PageSize": hcSeriesPageSize}, 200)
}

// seriesDiscoveryAction acknowledges, dismisses or restores results. It is a
// reader's note about a result: no book, queue, or Hardcover change.
func (a *App) seriesDiscoveryAction(w http.ResponseWriter, r *http.Request) error {
	sid, err := seriesIDParam(r, "sid")
	if err != nil {
		return err
	}
	action := r.FormValue("action")
	var from []string
	var to string
	switch action {
	case "ack":
		from, to = []string{"open"}, "acknowledged"
	case "dismiss":
		from, to = []string{"open", "acknowledged"}, "dismissed"
	case "restore":
		from, to = []string{"dismissed"}, "acknowledged"
	default:
		return bad("Choose an action.")
	}
	var ids []int64
	for _, raw := range r.Form["item"] {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	allNew := r.FormValue("all") == "new" && action == "ack"
	if len(ids) == 0 && !allNew {
		return bad("Choose at least one book.")
	}
	back := "/series/followed/" + strconv.FormatInt(sid, 10)
	err = a.store.Write(func(tx *sql.Tx) error {
		f, err := getFollow(tx, sid)
		if err != nil {
			return err
		}
		if f == nil {
			return conflict("You no longer follow this series.")
		}
		if allNew {
			views, err := a.discoveryViews(tx, sid)
			if err != nil {
				return err
			}
			for _, v := range views {
				if v.Category == catNew {
					ids = append(ids, v.HardcoverBookID)
				}
			}
		}
		now := canonicalTime(a.now())
		for _, id := range ids {
			if _, err := tx.Exec("UPDATE series_discoveries SET state=?,resolved_at=?,version=version+1 WHERE series_id=? AND hardcover_book_id=? AND state IN ("+placeholders(len(from))+")",
				append([]any{to, now, sid, id}, anys(from)...)...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return redirect(w, r, back+"?notice=series-discoveries&view="+url.QueryEscape(r.FormValue("view")))
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
func anys(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// seriesDiscoveryAddPreview previews adding chosen results. It reads saved
// data only, and the plan it stores is applied by the existing series plan.
func (a *App) seriesDiscoveryAddPreview(w http.ResponseWriter, r *http.Request) error {
	sid, err := seriesIDParam(r, "sid")
	if err != nil {
		return err
	}
	f, err := getFollow(a.store.DB, sid)
	if err != nil {
		return err
	}
	if f == nil {
		return conflict("You no longer follow this series.")
	}
	var chosen []int64
	for _, raw := range r.Form["item"] {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 && !slices.Contains(chosen, id) {
			chosen = append(chosen, id)
		}
	}
	views, err := a.discoveryViews(a.store.DB, sid)
	if err != nil {
		return err
	}
	local := f.LocalSeries
	if local == "" {
		local = f.Name
	}
	plan := hcSeriesPlan{SeriesID: sid, FollowedAt: f.FollowedAt, LocalSeries: local, SeriesName: f.Name}
	for _, v := range views {
		if !v.Offerable || !slices.Contains(chosen, v.HardcoverBookID) {
			continue
		}
		plan.Items = append(plan.Items, hcSeriesMember{HardcoverBookID: v.HardcoverBookID, Position: v.Position, Title: v.Title, Author: v.Author, Year: derefInt(v.Year), Pages: derefInt(v.Pages),
			Version: v.Version, ReleaseText: v.ReleaseText, Why: v.Why, Upcoming: v.Upcoming, Compilation: v.Compilation, Details: v.Details})
	}
	switch {
	case len(plan.Items) == 0:
		return bad("Choose at least one book that is not in your library.")
	case len(plan.Items) > hcSeriesAddLimit:
		return bad(fmt.Sprintf("Add at most %d books at a time.", hcSeriesAddLimit))
	}
	var id string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = savePlan(tx, "hc-series", Plan{HardcoverSeries: &plan})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/series/"+id)
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// applyDiscoveryAdd adds a previewed discovery plan inside the caller's
// transaction. It refuses anything that changed since the preview and treats
// a book that is already in the library as done, so a repeat adds nothing.
func (a *App) applyDiscoveryAdd(tx *sql.Tx, plan *hcSeriesPlan, at string) (added, skipped int, err error) {
	f, err := getFollow(tx, plan.SeriesID)
	if err != nil {
		return 0, 0, err
	}
	if f == nil || f.FollowedAt != plan.FollowedAt {
		return 0, 0, conflict("You no longer follow this series, or followed it again. Preview the books again.")
	}
	views, err := a.discoveryViews(tx, plan.SeriesID)
	if err != nil {
		return 0, 0, err
	}
	byID := map[int64]discoveryView{}
	for _, v := range views {
		byID[v.HardcoverBookID] = v
	}
	for _, it := range plan.Items {
		v, ok := byID[it.HardcoverBookID]
		switch {
		case !ok || v.Version != it.Version:
			return 0, 0, conflict("The list for this series changed since the preview. Preview the books again.")
		case v.Category == catLibrary:
			skipped++
			continue
		case !v.Offerable:
			return 0, 0, conflict("A book you chose can no longer be added (dismissed, removed, or no longer listed). Preview again.")
		}
		rec := Record{Book: Book{Title: it.Title, Author: it.Author, Status: "to-read", Series: plan.LocalSeries, SeriesNumber: it.Position, Source: "hardcover-series", Interest: 3, NeedsPlacement: true, Pages: positive(it.Pages), PublishedYear: positive(it.Year)}}
		res, err := importRecords(tx, []Record{rec})
		if err != nil {
			return 0, 0, err
		}
		if res.Added == 0 {
			skipped++
			continue
		}
		var bookID int64
		if err = tx.QueryRow("SELECT id FROM books WHERE identity=?", identity(it.Title, it.Author)).Scan(&bookID); err != nil {
			return 0, 0, err
		}
		if err = saveBookLink(tx, bookID, hcCandidate{BookID: it.HardcoverBookID}, at); err != nil {
			return 0, 0, err
		}
		if _, err = tx.Exec("UPDATE series_discoveries SET state='added',resolved_at=?,version=version+1 WHERE series_id=? AND hardcover_book_id=?", at, plan.SeriesID, it.HardcoverBookID); err != nil {
			return 0, 0, err
		}
		added++
	}
	return added, skipped, nil
}
