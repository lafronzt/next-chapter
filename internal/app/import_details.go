package app

import (
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
)

// detailsPerPage is how many record decisions the details page shows.
const detailsPerPage = 50

// importDetails is one page of an import's record-by-record decisions, with
// counts over all of them.
type importDetails struct {
	// Counts are over every record: new, matched, skipped, noop (matched and
	// nothing to change), changes (matched with changes), ambiguous.
	Counts map[string]int
	// Total is the number of records the filter selects.
	Total int
	Rows  []recordDecision
}

func decisionMatches(d recordDecision, filter string) bool {
	switch filter {
	case "new", "matched", "skipped":
		return d.Outcome == filter
	case "noop":
		return d.NoOp()
	case "changes":
		return d.Outcome == "matched" && len(d.Changes) > 0
	case "ambiguous":
		return len(d.Ambiguous) > 0
	}
	return true
}

var detailFilters = []string{"new", "matched", "changes", "noop", "skipped", "ambiguous"}

// explainImport runs the import inside a savepoint that is rolled back, the
// way the preview does, and keeps only the decisions in the requested window.
// It runs the same function apply does, so what it says is what apply does.
func explainImport(q queryer, records []Record, filter string, offset, limit int) (importDetails, error) {
	d := importDetails{Counts: map[string]int{}}
	if _, err := q.Exec("SAVEPOINT import_explain"); err != nil {
		return d, err
	}
	seen := 0
	_, err := importRecordsExplained(q, records, func(x recordDecision) {
		d.Counts[x.Outcome]++
		if x.NoOp() {
			d.Counts["noop"]++
		}
		if x.Outcome == "matched" && len(x.Changes) > 0 {
			d.Counts["changes"]++
		}
		if len(x.Ambiguous) > 0 {
			d.Counts["ambiguous"]++
		}
		if !decisionMatches(x, filter) {
			return
		}
		if seen >= offset && seen < offset+limit {
			d.Rows = append(d.Rows, x)
		}
		seen++
	})
	d.Total = seen
	if _, e := q.Exec("ROLLBACK TO import_explain; RELEASE import_explain"); err == nil {
		err = e
	}
	return d, err
}

// importDetailsPage lists what an import preview would do to each record. It
// recomputes from the stored records and refuses a preview the library has
// moved on from, so it never explains something that would not happen.
func (a *App) importDetailsPage(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	kind, p, rev, err := readPlan(a.store.DB, id)
	if err != nil {
		return err
	}
	if kind != "import" {
		return sql.ErrNoRows
	}
	data := page{"Title": "Import details", "PlanID": id, "Plan": p, "Nav": "imports", "Filters": detailFilters}
	if rev != state(r).Revision {
		data["Stale"] = true
		return a.render(w, r, "import_details.html", data, 200)
	}
	filter := r.URL.Query().Get("show")
	if !in(filter, detailFilters) {
		filter = ""
	}
	run := func(page int) (importDetails, error) {
		var d importDetails
		err := a.store.Write(func(tx *sql.Tx) error {
			var err error
			d, err = explainImport(tx, p.Records, filter, (page-1)*detailsPerPage, detailsPerPage)
			return err
		})
		return d, err
	}
	pageNumber := 1
	if n, e := strconv.Atoi(r.URL.Query().Get("page")); e == nil && n > 1 {
		pageNumber = n
	}
	details, err := run(pageNumber)
	if err != nil {
		return err
	}
	// The list has its own page size, whatever per_page says.
	r2 := *r
	u := *r.URL
	q := u.Query()
	q.Set("per_page", strconv.Itoa(detailsPerPage))
	u.RawQuery = q.Encode()
	r2.URL = &u
	pages := newPager(&r2, details.Total, "/plans/"+id+"/details", url.Values{"show": {filter}})
	if pages.Page != pageNumber {
		if details, err = run(pages.Page); err != nil {
			return err
		}
	}
	data["Details"], data["Pager"], data["Filter"], data["Records"] = details, pages, filter, len(p.Records)
	return a.render(w, r, "import_details.html", data, 200)
}
