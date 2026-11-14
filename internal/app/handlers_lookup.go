package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
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

// lookupRow is one field of a proposed fill: what the book has, what the
// database found, and whether the field can be filled (it is empty today).
type lookupRow struct {
	Key, Label, Current, Found string
	Fill                       bool
}

func fillable(b Book, d BookDetails) []lookupRow {
	rows := []lookupRow{}
	add := func(key, label, current, found string) {
		if found != "" {
			rows = append(rows, lookupRow{key, label, current, found, current == ""})
		}
	}
	number := func(n *int) string {
		if n == nil {
			return ""
		}
		return strconv.Itoa(*n)
	}
	seriesNumber := func(n *float64) string {
		if n == nil {
			return ""
		}
		return ratingText(*n)
	}
	add("description", "Description", b.Description, d.Description)
	add("pages", "Pages", number(b.Pages), number(d.Pages))
	add("published_year", "Year first published", number(b.PublishedYear), number(d.PublishedYear))
	add("series", "Series", b.Series, d.Series)
	add("series_number", "Number in series", seriesNumber(b.SeriesNumber), seriesNumber(d.SeriesNumber))
	add("tags", "Tags", b.Tags, strings.Join(d.Tags, ", "))
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
		rows := fillable(b, p.Lookup.Found)
		canFill := slices.ContainsFunc(rows, func(r lookupRow) bool { return r.Fill })
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
		changed, err := fillBook(tx, b, p.Lookup.Found, r.Form["take"])
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

// fillBook writes the chosen fields of d into book b, each only where the book
// has nothing, and reports whether anything changed. Values are cleaned again
// here, so nothing that reaches the database skipped the checks.
func fillBook(q queryer, b Book, d BookDetails, take []string) (bool, error) {
	changed := false
	run := func(statement string, args ...any) error {
		c, err := execChanged(q, statement, args...)
		changed = changed || c
		return err
	}
	has := func(key string) bool { return slices.Contains(take, key) }
	if text := cleanText(d.Description, maxDescription); has("description") && text != "" {
		if err := run("UPDATE books SET description=?,description_source=? WHERE id=? AND description=''", text, cleanText(d.DescriptionSource, 100), b.ID); err != nil {
			return false, err
		}
	}
	if has("pages") && d.Pages != nil && *d.Pages >= 1 && *d.Pages <= 99999 {
		if err := run("UPDATE books SET pages=? WHERE id=? AND pages IS NULL", *d.Pages, b.ID); err != nil {
			return false, err
		}
	}
	if has("published_year") && d.PublishedYear != nil && parseYear(strconv.Itoa(*d.PublishedYear)) != nil {
		if err := run("UPDATE books SET published_year=? WHERE id=? AND published_year IS NULL", *d.PublishedYear, b.ID); err != nil {
			return false, err
		}
	}
	series := cleanText(d.Series, 200)
	if has("series") && series != "" {
		if err := run("UPDATE books SET series=? WHERE id=? AND series=''", series, b.ID); err != nil {
			return false, err
		}
	}
	if has("series_number") && d.SeriesNumber != nil && *d.SeriesNumber >= 0 && *d.SeriesNumber <= 9999 {
		// A number belongs to the series it was found with.
		if b.Series == "" && has("series") || b.Series != "" && foldKey(b.Series) == foldKey(series) {
			if err := run("UPDATE books SET series_number=? WHERE id=? AND series_number IS NULL AND series!=''", *d.SeriesNumber, b.ID); err != nil {
				return false, err
			}
		}
	}
	if has("tags") {
		if joined := strings.Join(addTags(nil, d.Tags...), ", "); joined != "" {
			if err := run("UPDATE books SET tags=? WHERE id=? AND tags=''", joined, b.ID); err != nil {
				return false, err
			}
		}
	}
	return changed, nil
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
				fillRecord(rec, res.details)
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

// fillRecord adds found details to an import record's empty fields.
func fillRecord(rec *Record, d BookDetails) {
	if rec.Description == "" {
		if rec.Description = cleanText(d.Description, maxDescription); rec.Description != "" {
			rec.DescriptionSource = cleanText(d.DescriptionSource, 100)
		}
	}
	if rec.Pages == nil && d.Pages != nil {
		rec.Pages = d.Pages
	}
	if rec.PublishedYear == nil && d.PublishedYear != nil {
		rec.PublishedYear = d.PublishedYear
	}
	if rec.Series == "" && d.Series != "" {
		rec.Series, rec.SeriesNumber = cleanText(d.Series, 200), d.SeriesNumber
	}
	if rec.Tags == "" && len(d.Tags) > 0 {
		rec.Tags = strings.Join(addTags(nil, d.Tags...), ", ")
	}
}
