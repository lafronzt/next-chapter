package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A lookupPlan is one looked-up book waiting for the reader to choose which
// fields to fill. Query is what was sent.
type lookupPlan struct {
	BookID int64       `json:"book_id"`
	Query  LookupQuery `json:"query"`
	Found  BookDetails `json:"found"`
}

var errLookupOff = httpError{Status: http.StatusBadRequest, Message: "Book lookup is off. Turn it on in Preferences, under Privacy."}

func lookupEnabled(q queryer) (bool, error) {
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	return prefs.BookLookup, err
}

// firstErr returns err when set, else fallback.
func firstErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// What to do with a field the book already has. A field the book has not
// filled is taken unless the reader leaves it empty (modeSkip).
const (
	modeKeep   = "keep"   // keep my value
	modeTheirs = "theirs" // replace it with the database's
	modeMerge  = "merge"  // combine the two: text is appended, tags are joined
	modeSkip   = "skip"   // leave an empty field empty
)

// detailFields are the book fields a lookup can fill or change.
type detailFields struct {
	Description, DescriptionSource string
	Pages, Year                    *int
	Series                         string
	SeriesNumber                   *float64
	Tags                           string
}

func fieldsOf(b Book) detailFields {
	return detailFields{b.Description, b.DescriptionSource, b.Pages, b.PublishedYear, b.Series, b.SeriesNumber, b.Tags}
}

// act decides what a mode means for one field: "leave", "take", or "merge".
func act(empty bool, mode string) string {
	if empty {
		if mode == modeSkip {
			return "leave"
		}
		return "take"
	}
	switch mode {
	case modeTheirs:
		return "take"
	case modeMerge:
		return "merge"
	}
	return "leave"
}

// resolveDetails applies what a lookup found to a book's current fields as the
// reader chose, field by field, and returns the result. It cleans every found
// value again, so nothing reaches the database unchecked. A found value that is
// missing changes nothing. It is pure, so a second run with the same choices
// gives the same result.
func resolveDetails(cur detailFields, d BookDetails, mode func(key string) string) detailFields {
	out := cur
	if theirs := cleanText(d.Description, maxDescription); theirs != "" {
		source := cleanText(d.DescriptionSource, 100)
		switch act(cur.Description == "", mode("description")) {
		case "take":
			out.Description, out.DescriptionSource = theirs, source
		case "merge":
			if !strings.Contains(foldKey(cur.Description), foldKey(theirs)) && len([]rune(cur.Description)) < maxDescription-2 {
				out.Description = cleanText(cur.Description+"\n\n"+theirs, maxDescription)
				owner := "You"
				if cur.DescriptionSource != "" {
					owner = cur.DescriptionSource
				}
				if source != "" {
					out.DescriptionSource = truncate(owner+" and "+source, 100)
				}
			}
		}
	}
	if d.Pages != nil && *d.Pages >= 1 && *d.Pages <= 99999 && act(cur.Pages == nil, mode("pages")) == "take" {
		out.Pages = d.Pages
	}
	if d.PublishedYear != nil && parseYear(strconv.Itoa(*d.PublishedYear)) != nil && act(cur.Year == nil, mode("published_year")) == "take" {
		out.Year = d.PublishedYear
	}
	series := cleanText(d.Series, 200)
	if series != "" && act(cur.Series == "", mode("series")) == "take" {
		out.Series = series
	}
	// A number belongs to the series it was found with.
	if n := d.SeriesNumber; n != nil && *n >= 0 && *n <= 9999 && series != "" && foldKey(out.Series) == foldKey(series) && act(cur.SeriesNumber == nil, mode("series_number")) == "take" {
		out.SeriesNumber = n
	}
	if theirs := addTags(nil, d.Tags...); len(theirs) > 0 {
		switch act(cur.Tags == "", mode("tags")) {
		case "take":
			out.Tags = strings.Join(theirs, ", ")
		case "merge":
			out.Tags = joinTags(cur.Tags, theirs)
		}
	}
	return out
}

// joinTags appends tags the reader does not already have, keeping theirs first
// and the result within the 500 characters the tags field allows.
func joinTags(mine string, theirs []string) string {
	have := map[string]bool{}
	list := []string{}
	for _, t := range strings.Split(mine, ",") {
		if t = strings.TrimSpace(t); t != "" {
			have[foldKey(t)] = true
			list = append(list, t)
		}
	}
	for _, t := range theirs {
		if joined := strings.Join(append(list, t), ", "); !have[foldKey(t)] && len(joined) <= 500 {
			have[foldKey(t)] = true
			list = append(list, t)
		}
	}
	return strings.Join(list, ", ")
}

// saveDetails writes the fields that changed and reports whether any did.
func saveDetails(q queryer, id int64, before, after detailFields) (bool, error) {
	if reflect.DeepEqual(before, after) {
		return false, nil
	}
	_, err := q.Exec("UPDATE books SET description=?,description_source=?,pages=?,published_year=?,series=?,series_number=?,tags=? WHERE id=?",
		after.Description, after.DescriptionSource, after.Pages, after.Year, after.Series, after.SeriesNumber, after.Tags, id)
	return err == nil, err
}

// lookupOption is one choice for a field; lookupRow is a field's current value,
// the found value, and the choices for it. A row with no options is the same
// in both places.
type lookupOption struct {
	Value, Label string
	Checked      bool
}
type lookupRow struct {
	Key, Label, Current, Found string
	Options                    []lookupOption
}

func lookupRows(b Book, d BookDetails) []lookupRow {
	num := func(n *int) string {
		if n == nil {
			return ""
		}
		return strconv.Itoa(*n)
	}
	ser := func(n *float64) string {
		if n == nil {
			return ""
		}
		return ratingText(*n)
	}
	cur, found := fieldsOf(b), resolveDetails(detailFields{}, d, func(string) string { return modeTheirs })
	type pair struct {
		key, label, mine, theirs string
		mergeable                bool
	}
	pairs := []pair{
		{"description", "Description", cur.Description, found.Description, true},
		{"pages", "Pages", num(cur.Pages), num(found.Pages), false},
		{"published_year", "Year first published", num(cur.Year), num(found.Year), false},
		{"series", "Series", cur.Series, found.Series, false},
		{"series_number", "Number in series", ser(cur.SeriesNumber), ser(found.SeriesNumber), false},
		{"tags", "Tags", cur.Tags, found.Tags, true},
	}
	rows := []lookupRow{}
	for _, p := range pairs {
		if p.theirs == "" {
			continue
		}
		row := lookupRow{Key: p.key, Label: p.label, Current: p.mine, Found: p.theirs}
		switch {
		case p.mine == "":
			row.Options = []lookupOption{{modeTheirs, "Use theirs", true}, {modeSkip, "Leave empty", false}}
		case foldKey(p.mine) == foldKey(p.theirs):
		default:
			row.Options = []lookupOption{{modeKeep, "Keep mine", true}, {modeTheirs, "Use theirs", false}}
			if p.mergeable {
				row.Options = append(row.Options, lookupOption{modeMerge, "Merge both", false})
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// lookupDetails runs one lookup for one book, after the reader asks. The
// network request happens outside the database write.
func (a *App) lookupDetails(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if on, err := lookupEnabled(a.store.DB); err != nil || !on {
		return firstErr(err, errLookupOff)
	}
	b, err := getBook(a.store.DB, id)
	if err != nil {
		return err
	}
	q := LookupQuery{Title: b.Title, Author: b.Author, ISBN: b.ISBN}
	started := time.Now()
	found, err := a.finder.Find(r.Context(), q)
	a.logLookup(r, id, found, err, time.Since(started))
	if err != nil {
		// A book the databases do not know, or cannot tell apart, is a normal
		// answer, not a server fault; only a failed request is a bad gateway.
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, errLookupNotFound):
			status = http.StatusNotFound
		case errors.Is(err, errLookupAmbiguous):
			status = http.StatusConflict
		}
		return httpError{Status: status, Message: lookupMessage(err)}
	}
	var plan string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if _, err := getBook(tx, id); err != nil {
			return err
		}
		plan, err = savePlan(tx, "lookup", Plan{Lookup: &lookupPlan{BookID: id, Query: q, Found: found}})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/lookups/"+plan)
}

// logLookup records the outcome of one lookup. It logs the book's ID, never
// its title, author, or ISBN: those are the reader's library data. A failed
// request is a warning with the database's own error (for example "Google
// Books: the database answered with status 429"); not finding the book is
// ordinary and logged at info.
func (a *App) logLookup(r *http.Request, bookID int64, found BookDetails, err error, took time.Duration) {
	attrs := []any{"request_id", state(r).ID, "book_id", bookID, "ms", took.Milliseconds()}
	switch {
	case err == nil:
		a.logger.Info("book lookup", append(attrs, "outcome", "found", "sources", strings.Join(found.Sources, ","), "description", found.Description != "", "pages", found.Pages != nil, "year", found.PublishedYear != nil)...)
	case errors.Is(err, errLookupNotFound):
		a.logger.Info("book lookup", append(attrs, "outcome", "not_found")...)
	case errors.Is(err, errLookupAmbiguous):
		a.logger.Info("book lookup", append(attrs, "outcome", "ambiguous")...)
	default:
		a.logger.Warn("book lookup failed", append(attrs, "outcome", "error", "err", err)...)
	}
}

// lookupPage shows what a lookup found beside what the book has, and saves the
// chosen fields on POST. Only empty fields can be filled.
func (a *App) lookupPage(w http.ResponseWriter, r *http.Request) error {
	planID := r.PathValue("plan")
	if r.Method == http.MethodGet {
		kind, p, rev, err := readPlan(a.store.DB, planID)
		if err != nil || kind != "lookup" || p.Lookup == nil {
			return firstErr(err, sql.ErrNoRows)
		}
		b, err := getBook(a.store.DB, p.Lookup.BookID)
		if err != nil {
			return err
		}
		rows := lookupRows(b, p.Lookup.Found)
		canFill := slices.ContainsFunc(rows, func(r lookupRow) bool { return len(r.Options) > 0 })
		return a.render(w, r, "lookup.html", page{"Title": "Details for " + b.Title, "Book": b, "Rows": rows, "Found": p.Lookup.Found, "Sent": p.Lookup.Query.sent(), "PlanID": planID, "Stale": rev != state(r).Revision, "CanFill": canFill}, 200)
	}
	var bookID int64
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		kind, p, rev, err := readPlan(tx, planID)
		if err != nil || kind != "lookup" || p.Lookup == nil {
			return firstErr(err, sql.ErrNoRows)
		}
		if now, err := revision(tx); err != nil || now != rev {
			return firstErr(err, conflict("Your library changed. Look the book up again."))
		}
		bookID = p.Lookup.BookID
		b, err := getBook(tx, bookID)
		if err != nil {
			return err
		}
		// Only the choices the page offered count; anything else keeps the book as it is.
		offered := map[string][]lookupOption{}
		for _, row := range lookupRows(b, p.Lookup.Found) {
			offered[row.Key] = row.Options
		}
		choose := func(key string) string {
			v := r.FormValue("mode_" + key)
			if slices.ContainsFunc(offered[key], func(o lookupOption) bool { return o.Value == v }) {
				return v
			}
			return modeKeep
		}
		changed, err := saveDetails(tx, bookID, fieldsOf(b), resolveDetails(fieldsOf(b), p.Lookup.Found, choose))
		if err != nil || !changed {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved#details", bookID))
}

// needsDetails reports whether looking a record up could add anything: it is
// a new book, or a matching book still lacks a description, pages, year, or
// tags. Books the reader deleted are never looked up.
func needsDetails(q queryer, rec Record) (bool, error) {
	id, tombstoned, err := lookupBook(q, rec, true)
	if err != nil || tombstoned {
		return false, err
	}
	if id == 0 {
		return true, nil
	}
	b, err := getBook(q, id)
	if err != nil {
		return false, err
	}
	return b.Description == "" || b.Pages == nil || b.PublishedYear == nil || b.Tags == "", nil
}

func lookupKey(r Record) string { return identity(r.Title, r.Author) }

// lookupImport looks up details for the next few books of an import preview,
// after the reader asks, and adds what it finds to the preview's records
// where those fields are empty. Nothing is saved to the library until the
// reader imports.
func (a *App) lookupImport(w http.ResponseWriter, r *http.Request) error {
	planID := r.PathValue("id")
	mode := r.FormValue("mode")
	if mode == "" {
		mode = modeKeep
	}
	if !in(mode, []string{modeKeep, modeTheirs, modeMerge}) {
		return bad("Choose whether to keep your values, use theirs, or merge them.")
	}
	if on, err := lookupEnabled(a.store.DB); err != nil || !on {
		return firstErr(err, errLookupOff)
	}
	kind, p, _, err := readPlan(a.store.DB, planID)
	if err != nil {
		return err
	}
	if kind != "import" {
		return bad("Only an import preview can look up its books.")
	}
	started := time.Now()
	targets := []LookupQuery{}
	seen := map[string]bool{}
	for _, rec := range p.Records {
		key := lookupKey(rec)
		if rec.LookedUp || seen[key] || len(targets) >= lookupBatchLimit {
			continue
		}
		seen[key] = true
		need, err := needsDetails(a.store.DB, rec)
		if err != nil {
			return err
		}
		if need {
			targets = append(targets, LookupQuery{Title: rec.Title, Author: rec.Author, ISBN: rec.ISBN})
		}
	}
	type outcome struct {
		details BookDetails
		err     error
	}
	results := make([]outcome, len(targets))
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	slot := make(chan struct{}, 4)
	for i, q := range targets {
		wg.Add(1)
		slot <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slot }()
			results[i].details, results[i].err = a.finder.Find(ctx, q)
		}()
	}
	wg.Wait()
	failed, notFound := 0, 0
	for _, res := range results {
		switch {
		case res.err == nil:
		case errors.Is(res.err, errLookupNotFound), errors.Is(res.err, errLookupAmbiguous):
			notFound++
		default:
			failed++
			a.logger.Warn("book lookup failed", "request_id", state(r).ID, "plan", "import", "err", res.err)
		}
	}
	a.logger.Info("import book lookup", "request_id", state(r).ID, "books", len(targets), "found", len(targets)-failed-notFound, "not_found", notFound, "failed", failed, "ms", time.Since(started).Milliseconds())
	byKey := map[string]outcome{}
	for i, q := range targets {
		// A book the databases do not know is settled; a failed request is not,
		// so the reader can try those again.
		if e := results[i].err; e == nil || errors.Is(e, errLookupNotFound) || errors.Is(e, errLookupAmbiguous) {
			byKey[identity(q.Title, q.Author)] = results[i]
		}
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		kind, p, rev, err := readPlan(tx, planID)
		if err != nil || kind != "import" {
			return firstErr(err, sql.ErrNoRows)
		}
		if now, err := revision(tx); err != nil || now != rev {
			return firstErr(err, conflict("Your library changed. Generate a fresh preview before looking up details."))
		}
		p.LookupMode = mode
		counted := map[string]bool{}
		for i := range p.Records {
			rec := &p.Records[i]
			key := lookupKey(*rec)
			res, ok := byKey[key]
			if !ok || rec.LookedUp {
				continue
			}
			rec.LookedUp = true
			if !counted[key] {
				counted[key] = true
				p.LookupAttempted++
				if res.err == nil {
					p.LookupFound++
				}
			}
			if res.err == nil {
				found := res.details
				rec.Found = &found
			}
		}
		raw, err := encodePlan(p)
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE plans SET payload=? WHERE id=?", raw, planID)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+planID+"#lookup")
}

// importMode is the import preview's choice for every field: empty fields are
// filled, and a field the book already has is kept, replaced, or merged.
func importMode(mode string) func(string) string {
	return func(string) string { return mode }
}
