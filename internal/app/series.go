package app

import (
	"cmp"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// seriesVolume is one local book in a series, as the dashboard lists it.
type seriesVolume struct {
	ID     int64
	Title  string
	Author string
	Status string
	Number *float64
	// Finished counts the readings in the reader's history. It is evidence
	// of a completed reading, separate from the book's current status, and
	// is the same however many times the book was re-read.
	Finished int
	// DuplicateNumber marks a number that more than one local book carries.
	DuplicateNumber bool
}

// Fractional reports a number such as 0.5 or 2.5, which is listed in order
// but is never a prerequisite for the next volume.
func (v seriesVolume) Fractional() bool {
	return v.Number != nil && *v.Number != math.Trunc(*v.Number)
}

// Done reports whether the reader has finished the volume: it is marked Read
// or its history has a finished reading and it is not being read right now.
func (v seriesVolume) Done() bool {
	return v.Status == "read" || (v.Finished > 0 && v.Status != "reading")
}

// seriesNext is the first known numbered volume the reader has not finished.
type seriesNext struct {
	Volume  seriesVolume
	Message string
	// Action is the existing book action offered: "start", "requeue", or ""
	// for a plain link to the book.
	Action string
}

// seriesGroup is the local books that share one series name, using the same
// case rule as ranking (foldKey).
type seriesGroup struct {
	Name       string
	Numbered   []seriesVolume
	Unnumbered []seriesVolume
	// Gaps say which whole numbers have no local entry between two known
	// ones. They never claim such a volume exists.
	Gaps []string
	Next *seriesNext
	// Counts are of books, each counted once: by current status, and how many
	// have a finished reading in their history.
	Read, Reading, Queued, SetAside, ToReview, EverFinished int
	Books                                                   int
}

type seriesRow struct {
	seriesVolume
	series string
}

// loadSeries groups every local book that has a series name. It reads once,
// so the cost does not grow with the number of series. Results are ordered by
// folded name, then name, so output is deterministic.
func loadSeries(q queryer) ([]seriesGroup, error) {
	rows, err := q.Query(`SELECT b.id,b.title,b.author,b.series,b.series_number,b.reading_status,
 (SELECT count(*) FROM reading_log r WHERE r.book_id=b.id)
 FROM books b WHERE trim(b.series)!='' ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string][]seriesRow{}
	for rows.Next() {
		var r seriesRow
		if err = rows.Scan(&r.ID, &r.Title, &r.Author, &r.series, &r.Number, &r.Status, &r.Finished); err != nil {
			return nil, err
		}
		key := foldKey(strings.TrimSpace(r.series))
		groups[key] = append(groups[key], r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]seriesGroup, 0, len(groups))
	for key, members := range groups {
		out = append(out, buildSeries(key, members))
	}
	slices.SortFunc(out, func(a, b seriesGroup) int {
		return cmp.Or(cmp.Compare(foldKey(a.Name), foldKey(b.Name)), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

func buildSeries(_ string, members []seriesRow) seriesGroup {
	// The name shown is the spelling of the book added first.
	g := seriesGroup{Name: strings.TrimSpace(members[0].series), Books: len(members)}
	counts := map[float64]int{}
	for _, m := range members {
		switch m.Status {
		case "read":
			g.Read++
		case "reading":
			g.Reading++
		case "to-read":
			g.Queued++
		case "abandoned", "archived":
			g.SetAside++
		case "unreviewed":
			g.ToReview++
		}
		if m.Finished > 0 {
			g.EverFinished++
		}
		if m.Number != nil {
			counts[*m.Number]++
		}
	}
	for _, m := range members {
		v := m.seriesVolume
		if v.Number == nil {
			g.Unnumbered = append(g.Unnumbered, v)
			continue
		}
		v.DuplicateNumber = counts[*v.Number] > 1
		g.Numbered = append(g.Numbered, v)
	}
	byTitle := func(a, b seriesVolume) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)), cmp.Compare(a.ID, b.ID))
	}
	slices.SortFunc(g.Numbered, func(a, b seriesVolume) int { return cmp.Or(cmp.Compare(*a.Number, *b.Number), byTitle(a, b)) })
	slices.SortFunc(g.Unnumbered, byTitle)
	g.Gaps = seriesGaps(g.Numbered)
	g.Next = nextVolume(g.Numbered)
	return g
}

// seriesGaps lists, in order, each stretch between two known whole numbers
// with no local entry. Fractional numbers are ignored; the wording only says
// what is missing from this library.
func seriesGaps(numbered []seriesVolume) []string {
	whole := []float64{}
	for _, v := range numbered {
		if !v.Fractional() && (len(whole) == 0 || whole[len(whole)-1] != *v.Number) {
			whole = append(whole, *v.Number)
		}
	}
	gaps := []string{}
	for i := 1; i < len(whole); i++ {
		if whole[i]-whole[i-1] > 1 {
			gaps = append(gaps, fmt.Sprintf("No local entry between #%s and #%s", formatNumber(whole[i-1]), formatNumber(whole[i])))
		}
	}
	return gaps
}

func formatNumber(n float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", n), "0"), ".")
}

// nextVolume finds the first known whole-numbered volume the reader has not
// finished. A volume that is set aside or waiting for review is not skipped:
// it is what comes next, and the message says so. Fractional volumes never
// block or become the next volume.
func nextVolume(numbered []seriesVolume) *seriesNext {
	for i := 0; i < len(numbered); {
		j := i
		for j < len(numbered) && *numbered[j].Number == *numbered[i].Number {
			j++
		}
		group := numbered[i:j]
		i = j
		if group[0].Fractional() {
			continue
		}
		var pick *seriesVolume
		done := false
		for k := range group {
			if group[k].Done() {
				done = true
			}
		}
		if done {
			continue
		}
		// With duplicate numbers, prefer the one being read, then queued.
		for _, status := range []string{"reading", "to-read", "unreviewed", "abandoned", "archived"} {
			for k := range group {
				if pick == nil && group[k].Status == status {
					pick = &group[k]
				}
			}
		}
		if pick == nil {
			pick = &group[0]
		}
		n := formatNumber(*pick.Number)
		next := &seriesNext{Volume: *pick}
		switch pick.Status {
		case "reading":
			next.Message = fmt.Sprintf("You are reading #%s now.", n)
		case "to-read":
			next.Message = fmt.Sprintf("#%s is in your queue.", n)
			next.Action = "start"
		case "abandoned", "archived":
			next.Message = fmt.Sprintf("#%s is set aside, and it comes before the later volumes you have. Decide whether to read it first.", n)
			next.Action = "requeue"
		case "unreviewed":
			next.Message = fmt.Sprintf("#%s is waiting in To review. Decide whether you want it before the later volumes.", n)
		default:
			next.Message = fmt.Sprintf("#%s is the next volume you have not finished.", n)
		}
		return next
	}
	return nil
}

// seriesPage shows the local series. Nothing here changes a book or the queue:
// the actions shown are the ordinary book actions, each its own form.
func (a *App) seriesPage(w http.ResponseWriter, r *http.Request) error {
	all, err := loadSeries(a.store.DB)
	if err != nil {
		return err
	}
	query := searchQuery(r.URL.Query().Get("q"))
	groups := all
	if query != "" {
		groups = nil
		for _, g := range all {
			if strings.Contains(strings.ToLower(g.Name), query) {
				groups = append(groups, g)
			}
		}
	}
	pages := newPager(r, len(groups), "/series", url.Values{"q": {query}})
	shown := []seriesGroup{}
	if len(groups) > 0 {
		shown = groups[pages.offset():min(pages.offset()+pages.PerPage, len(groups))]
	}
	var standalone int
	if err = a.store.DB.QueryRow("SELECT count(*) FROM books WHERE trim(series)=''").Scan(&standalone); err != nil {
		return err
	}
	return a.render(w, r, "series.html", page{"Title": pageTitle("Series", pages), "Groups": shown, "Pager": pages, "Query": query, "TotalSeries": len(all), "Standalone": standalone}, 200)
}
