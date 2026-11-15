package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func grRecord(title, status string) Record {
	return Record{Book: Book{Title: title, Author: "Author", Status: status, Source: "goodreads", Interest: 3}}
}

func libEvent(title, source, format, status, at, due string) Record {
	r := Record{Book: Book{Title: title, Author: "Author", Status: "unreviewed", Source: source}}
	r.Event = &Event{Format: format, Status: status, Activity: status, At: at, DueDate: due}
	return r
}

func exec(t *testing.T, a *App, query string, args ...any) {
	t.Helper()
	if _, err := a.store.DB.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func explainAll(t *testing.T, a *App, records []Record) importDetails {
	t.Helper()
	var d importDetails
	err := a.store.Write(func(tx *sql.Tx) error {
		var err error
		d, err = explainImport(tx, records, "", 0, 10_000)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func decisionFor(t *testing.T, d importDetails, title string) recordDecision {
	t.Helper()
	for _, r := range d.Rows {
		if r.Title == title {
			return r
		}
	}
	t.Fatalf("no decision for %q", title)
	return recordDecision{}
}

// Every explanation comes from the same function apply runs, and applying
// does what was said.
func TestImportExplanationMatchesApply(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	pages := 300
	base := records("By ISBN", "By Title", "Edited Away", "Series Book (Saga, #2)", "Merged Away", "Merge Target", "Deleted One", "Queued Finish", "Ambig A", "Ambig B")
	base[0].ISBN = "9780306406157"
	base[2].GoodreadsID = "gr-77"
	base[7].Tags = "mine"
	addRecords(t, a, base)
	exec(t, a, "UPDATE books SET isbn='9780306406164' WHERE title IN ('Ambig A','Ambig B')")
	exec(t, a, "UPDATE books SET title='Edited Renamed',identity='edited renamed|author' WHERE title='Edited Away'")
	exec(t, a, "UPDATE books SET pages=200,tags='my tags',interest=5 WHERE title='By Title'")
	ids := bookIDs(t, a)
	if err := a.store.Write(func(tx *sql.Tx) error {
		if _, err := mergeBooks(tx, ids["Merged Away"], ids["Merge Target"], true); err != nil {
			return err
		}
		return deleteBook(tx, ids["Deleted One"])
	}); err != nil {
		t.Fatal(err)
	}

	in := []Record{
		func() Record { r := grRecord("Different Title", "to-read"); r.ISBN = "9780306406157"; return r }(),
		func() Record {
			r := grRecord("By Title", "to-read")
			r.Pages, r.Tags, r.Rating = &pages, "their tags", 4
			return r
		}(),
		func() Record { r := grRecord("Totally New Name", "to-read"); r.GoodreadsID = "gr-77"; return r }(),
		func() Record {
			r := grRecord("Series Book", "to-read")
			r.OriginalTitle, r.Series, r.SeriesNumber = "Series Book (Saga, #2)", "Saga", fp(2)
			return r
		}(),
		grRecord("Merged Away", "to-read"),
		grRecord("Deleted One", "to-read"),
		grRecord("Queued Finish", "read"),
		func() Record { r := grRecord("Ambig A", "to-read"); r.ISBN = "9780306406164"; return r }(),
		grRecord("Brand New", "to-read"),
		grRecord("Brand New Library Only", "to-read"),
	}
	before := countRows(t, a)
	d := explainAll(t, a, in)
	if after := countRows(t, a); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("explaining changed the library: %v -> %v", before, after)
	}
	check := func(title, outcome, key, via string) recordDecision {
		t.Helper()
		x := decisionFor(t, d, title)
		if x.Outcome != outcome || x.Key != key || x.Via != via {
			t.Errorf("%s: %+v", title, x)
		}
		return x
	}
	byISBN := check("Different Title", "matched", "ISBN", "")
	check("By Title", "matched", "title and author", "")
	check("Totally New Name", "matched", "Goodreads ID", "")
	check("Series Book", "matched", "original title", "")
	merged := check("Merged Away", "matched", "title and author", "merge alias")
	check("Deleted One", "skipped", "title and author", "deletion record")
	check("Brand New", "new", "", "")
	if merged.MatchedTitle != "Merge Target" || byISBN.MatchedTitle != "By ISBN" {
		t.Errorf("matched titles %q %q", merged.MatchedTitle, byISBN.MatchedTitle)
	}
	// An ISBN two books share is refused, not guessed; the title decides.
	amb := check("Ambig A", "matched", "title and author", "")
	if len(amb.Ambiguous) != 1 || !strings.Contains(amb.Ambiguous[0], "ISBN matches 2 books in your library, so it was not used") {
		t.Errorf("ambiguous: %v", amb.Ambiguous)
	}
	// Changes and preserved local values.
	byTitle := decisionFor(t, d, "By Title")
	if len(byTitle.Changes) != 0 || !byTitle.NoOp() {
		t.Errorf("By Title changes %v", byTitle.Changes)
	}
	for _, want := range []string{"want-to-read (5 of 5); the Goodreads rating is not applied", "page count (200)", "tags"} {
		if !strings.Contains(strings.Join(byTitle.Preserved, "|"), want) {
			t.Errorf("preserved lacks %q: %v", want, byTitle.Preserved)
		}
	}
	finish := decisionFor(t, d, "Queued Finish")
	if !strings.Contains(strings.Join(finish.Changes, "|"), "reading status Queue → Read") || !strings.Contains(strings.Join(finish.Changes, "|"), "finished reading added") {
		t.Errorf("finish changes %v", finish.Changes)
	}
	if !strings.Contains(strings.Join(decisionFor(t, d, "Series Book").Changes, "|"), "title cleaned of its series text") {
		t.Errorf("series: %v", decisionFor(t, d, "Series Book").Changes)
	}
	if d.Counts["new"] != 2 || d.Counts["skipped"] != 1 || d.Counts["matched"] != 7 || d.Counts["ambiguous"] != 1 || d.Counts["changes"] != 2 || d.Counts["noop"] != 5 {
		t.Errorf("counts %v", d.Counts)
	}

	// Apply: the explanation's totals are what happened.
	var result importResult
	if err := a.store.Write(func(tx *sql.Tx) (err error) { result, err = importRecords(tx, in); return }); err != nil {
		t.Fatal(err)
	}
	if result.Added != d.Counts["new"] || result.Skipped != d.Counts["skipped"] || result.Advanced["read"] != 1 || result.Logged != 1 {
		t.Fatalf("apply %+v vs explained %v", result, d.Counts)
	}
	if got := bookNamed(t, a, "Queued Finish"); got.Status != "read" {
		t.Fatal(got.Status)
	}
	if got := bookNamed(t, a, "By Title"); got.Pages == nil || *got.Pages != 200 || got.Tags != "my tags" || got.Interest != 5 {
		t.Fatalf("a preserved value changed: %+v", got)
	}
	if got := bookNamed(t, a, "Series Book"); got.Series != "Saga" {
		t.Fatal("the series was not filled in as explained")
	}
	if n := scalarDB[int](t, a, "SELECT count(*) FROM books WHERE title='Merged Away' OR title='Deleted One'"); n != 0 {
		t.Fatal("a merged or deleted book came back")
	}
	// The same file again changes nothing, and says so for every matched record.
	again := explainAll(t, a, in)
	for _, x := range again.Rows {
		if x.Outcome == "skipped" {
			continue
		}
		if !x.NoOp() && x.Outcome != "new" {
			t.Errorf("%s: %v would still change", x.Title, x.Changes)
		}
	}
	if again.Counts["new"] != 0 {
		t.Errorf("a repeat would add %d books", again.Counts["new"])
	}
	var second importResult
	if err := a.store.Write(func(tx *sql.Tx) (err error) { second, err = importRecords(tx, in); return }); err != nil {
		t.Fatal(err)
	}
	if second.Added != 0 || second.Changed {
		t.Fatalf("repeat import %+v", second)
	}
}

func TestImportDetailsPage(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	in := []Record{}
	for i := range 120 {
		in = append(in, grRecord(fmt.Sprintf("Book %03d", i), "to-read"))
	}
	addRecords(t, a, in[:10])
	var plan string
	if err := a.store.Write(func(tx *sql.Tx) (err error) {
		plan, err = savePlan(tx, "import", Plan{Records: in, Source: "goodreads"})
		return
	}); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/", nil), 200)
	if p := b.request("GET", "/plans/"+plan, nil).Body.String(); !strings.Contains(p, "See what happens to each record") {
		t.Fatal("the preview does not link to the details")
	}
	page := b.request("GET", "/plans/"+plan+"/details", nil).Body.String()
	if strings.Count(page, "<article") != detailsPerPage || !strings.Contains(page, "Matched") || !strings.Contains(page, "No change: this record adds nothing") || !strings.Contains(page, `rel="next"`) || !strings.Contains(page, ">110<") {
		t.Fatalf("page 1: %d records\n%s", strings.Count(page, "<article"), page)
	}
	if p := b.request("GET", "/plans/"+plan+"/details?page=3", nil).Body.String(); strings.Count(p, "<article") != 20 {
		t.Fatalf("last page has %d records", strings.Count(p, "<article"))
	}
	if p := b.request("GET", "/plans/"+plan+"/details?show=new&page=2", nil).Body.String(); strings.Count(p, "<article") != 50 || strings.Contains(p, "No change: this record") {
		t.Fatal("the new filter is wrong")
	}
	if p := b.request("GET", "/plans/"+plan+"/details?show=noop", nil).Body.String(); strings.Count(p, "<article") != 10 {
		t.Fatalf("no-op filter shows %d", strings.Count(p, "<article"))
	}
	if p := b.request("GET", "/plans/"+plan+"/details?page=99", nil).Body.String(); strings.Count(p, "<article") != 20 {
		t.Fatal("a page past the end is not clamped")
	}
	// Nothing was written, and an unrelated plan kind has no details.
	if countRows(t, a)["books"] != 10 {
		t.Fatal("details wrote books")
	}
	var other string
	a.store.Write(func(tx *sql.Tx) (err error) { other, err = makeSuggestion(tx, 0); return })
	expect(t, b.request("GET", "/plans/"+other+"/details", nil), 404)
	expect(t, b.request("GET", "/plans/nope/details", nil), 404)
	// Once the library changes the preview is refused, not explained wrongly.
	exec(t, a, "UPDATE meta SET value=CAST(value AS INTEGER)+1 WHERE key='revision'")
	expect(t, b.request("GET", "/", nil), 200)
	if p := b.request("GET", "/plans/"+plan+"/details", nil).Body.String(); !strings.Contains(p, "changed since this preview") || strings.Contains(p, "<article") {
		t.Fatal("a stale preview was explained")
	}
}

// correctionLibrary has one book with a Libby ebook loan borrowed on 1 Oct.
func correctionLibrary(t *testing.T) (*App, *browser, int64) {
	t.Helper()
	a := testApp(t, true)
	addRecords(t, a, records("Corrected"))
	addRecords(t, a, []Record{libEvent("Corrected", "libby", "ebook", "borrowed", "2026-10-01T00:00:00.000000000+00:00", "2026-10-20")})
	return a, &browser{t: t, handler: a.Handler()}, bookNamed(t, a, "Corrected").ID
}

func eventID(t *testing.T, a *App, status string) int64 {
	t.Helper()
	return scalarDB[int64](t, a, "SELECT id FROM events WHERE status='"+status+"' ORDER BY id LIMIT 1")
}

func loanOfBook(t *testing.T, a *App, id int64, source, format string) string {
	t.Helper()
	var status, due string
	err := a.store.DB.QueryRow("SELECT status,due_date FROM loans WHERE book_id=? AND source=? AND format=?", id, source, format).Scan(&status, &due)
	if err == sql.ErrNoRows {
		return "none"
	}
	if err != nil {
		t.Fatal(err)
	}
	return status + " " + due
}

// correct runs the preview and confirm steps, returning the preview page.
func correct(t *testing.T, b *browser, event int64, v url.Values, wantPreview int) string {
	t.Helper()
	rr := b.form(fmt.Sprintf("/events/%d/correct", event), v)
	expect(t, rr, wantPreview)
	if wantPreview != 303 {
		return ""
	}
	plan := rr.Header().Get("Location")
	page := b.request("GET", plan, nil).Body.String()
	expect(t, b.request("POST", plan, nil), 303)
	return page
}

func TestEventCorrectionsAndReplays(t *testing.T) {
	t.Parallel()
	a, b, id := correctionLibrary(t)
	original := libEvent("Corrected", "libby", "ebook", "borrowed", "2026-10-01T00:00:00.000000000+00:00", "2026-10-20")
	returned := libEvent("Corrected", "libby", "ebook", "returned", "2026-10-25T00:00:00.000000000+00:00", "")
	addRecords(t, a, []Record{returned})
	if got := loanOfBook(t, a, id, "libby", "ebook"); got != "returned " {
		t.Fatal(got)
	}
	exec(t, a, "UPDATE books SET reading_status='to-read'")
	queue, ratings := queueSnapshot(t, a), scalarDB[int](t, a, "SELECT count(*) FROM reading_log")
	borrowed, ret := eventID(t, a, "borrowed"), eventID(t, a, "returned")

	// The preview shows the event and the resulting loan state and changes
	// nothing; only confirming writes.
	rr := b.form(fmt.Sprintf("/events/%d/correct", ret), url.Values{"action": {"void"}})
	expect(t, rr, 303)
	if scalarDB[int](t, a, "SELECT count(*) FROM event_corrections") != 0 || loanOfBook(t, a, id, "libby", "ebook") != "returned " {
		t.Fatal("previewing changed the library")
	}
	plan := rr.Header().Get("Location")
	preview := b.request("GET", plan, nil).Body.String()
	for _, want := range []string{"Event as imported", "voided (ignored)", "Before: LIBBY ebook: returned", "After: LIBBY ebook: borrowed (observed 2026-10-01), due 2026-10-20", "expires any suggested order preview and Undo"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview lacks %q:\n%s", want, preview)
		}
	}
	expect(t, b.request("POST", plan, nil), 303)
	if got := loanOfBook(t, a, id, "libby", "ebook"); got != "borrowed 2026-10-20" {
		t.Fatalf("after voiding the return: %s", got)
	}
	// The imported event is untouched.
	if scalarDB[int](t, a, "SELECT count(*) FROM events") != 2 || scalarDB[string](t, a, fmt.Sprintf("SELECT status FROM events WHERE id=%d", ret)) != "returned" {
		t.Fatal("the original event changed")
	}
	// Replaying the file, in either order, neither recreates the void nor
	// undoes anything.
	for _, order := range [][]Record{{original, returned}, {returned, original}} {
		var res importResult
		if err := a.store.Write(func(tx *sql.Tx) (err error) { res, err = importRecords(tx, order); return }); err != nil {
			t.Fatal(err)
		}
		if res.Events != 0 || res.Changed {
			t.Fatalf("replay %+v", res)
		}
		if got := loanOfBook(t, a, id, "libby", "ebook"); got != "borrowed 2026-10-20" {
			t.Fatalf("a replay changed the loan: %s", got)
		}
	}
	if d := explainAll(t, a, []Record{returned}); !strings.Contains(strings.Join(decisionFor(t, d, "Corrected").Preserved, "|"), "your correction of this library event") {
		t.Fatal("the replay explanation does not mention the correction")
	}

	// Edit the borrow: a new due date and a new source/format.
	correct(t, b, borrowed, url.Values{"action": {"edit"}, "source": {"libby"}, "format": {"ebook"}, "status": {"borrowed"}, "date": {"2026-10-01"}, "due_date": {"2026-10-27"}}, 303)
	if got := loanOfBook(t, a, id, "libby", "ebook"); got != "borrowed 2026-10-27" {
		t.Fatal(got)
	}
	correct(t, b, borrowed, url.Values{"action": {"edit"}, "source": {"nypl"}, "format": {"print"}, "status": {"borrowed"}, "date": {"2026-10-02"}, "due_date": {"2026-11-02"}}, 303)
	if loanOfBook(t, a, id, "libby", "ebook") != "none" || loanOfBook(t, a, id, "nypl", "print") != "borrowed 2026-11-02" {
		t.Fatalf("corrected source and format: %s / %s", loanOfBook(t, a, id, "libby", "ebook"), loanOfBook(t, a, id, "nypl", "print"))
	}
	var res importResult
	a.store.Write(func(tx *sql.Tx) (err error) { res, err = importRecords(tx, []Record{original, returned}); return })
	if res.Events != 0 || loanOfBook(t, a, id, "nypl", "print") != "borrowed 2026-11-02" || loanOfBook(t, a, id, "libby", "ebook") != "none" {
		t.Fatalf("a replay undid the edit: %+v", res)
	}
	// A newer, different observation for the same source and format replaces
	// corrected state.
	addRecords(t, a, []Record{libEvent("Corrected", "nypl", "print", "returned", "2026-11-05T00:00:00.000000000+00:00", "")})
	if got := loanOfBook(t, a, id, "nypl", "print"); got != "returned " {
		t.Fatalf("a newer observation did not win: %s", got)
	}
	// Restoring the original brings it back, and the return stays voided.
	correct(t, b, borrowed, url.Values{"action": {"revert"}}, 303)
	if got := loanOfBook(t, a, id, "libby", "ebook"); got != "borrowed 2026-10-20" {
		t.Fatal(got)
	}
	correct(t, b, ret, url.Values{"action": {"revert"}}, 303)
	if got := loanOfBook(t, a, id, "libby", "ebook"); got != "returned " {
		t.Fatal(got)
	}
	// Correcting library events never touches reading, ratings, or the queue.
	if queueSnapshot(t, a) != queue || scalarDB[int](t, a, "SELECT count(*) FROM reading_log") != ratings {
		t.Fatal("a correction changed reading history or the queue")
	}
}

func TestEventCorrectionValidationAndStaleness(t *testing.T) {
	t.Parallel()
	a, b, id := correctionLibrary(t)
	ev := eventID(t, a, "borrowed")
	good := url.Values{"action": {"edit"}, "source": {"libby"}, "format": {"ebook"}, "status": {"borrowed"}, "date": {"2026-10-01"}, "due_date": {"2026-10-30"}}
	with := func(k, v string) url.Values {
		c := url.Values{}
		for key, vals := range good {
			c[key] = vals
		}
		c.Set(k, v)
		return c
	}
	for name, v := range map[string]url.Values{
		"bad source":   with("source", "bookstore"),
		"no format":    with("format", " "),
		"bad status":   with("status", "lost"),
		"bad date":     with("date", "tomorrow"),
		"bad due":      with("due_date", "soon"),
		"no change":    with("due_date", "2026-10-20"),
		"bad action":   with("action", "explode"),
		"revert none":  {"action": {"revert"}},
		"long format":  with("format", strings.Repeat("f", 101)),
		"missing book": nil,
	} {
		code := 400
		path := fmt.Sprintf("/events/%d/correct", ev)
		if name == "missing book" {
			path, code, v = "/events/9999/correct", 404, good
		}
		if rr := b.form(path, v); rr.Code != code {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM event_corrections") != 0 {
		t.Fatal("a rejected correction was saved")
	}
	expect(t, b.request("GET", fmt.Sprintf("/events/%d/correct", ev), nil), 200)
	expect(t, b.request("GET", "/events/9999/correct", nil), 404)
	// A preview goes stale when the library changes.
	rr := b.form(fmt.Sprintf("/events/%d/correct", ev), good)
	expect(t, rr, 303)
	plan := rr.Header().Get("Location")
	exec(t, a, "UPDATE meta SET value=CAST(value AS INTEGER)+1 WHERE key='revision'")
	if page := b.request("GET", plan, nil).Body.String(); !strings.Contains(page, "changed since this preview") {
		t.Fatal("the stale preview is not flagged")
	}
	expect(t, b.request("POST", plan, nil), 409)
	if scalarDB[int](t, a, "SELECT count(*) FROM event_corrections") != 0 {
		t.Fatal("a stale preview was applied")
	}
	// A plan that fails while applying changes nothing, loans included.
	var bad string
	fingerprintOf := scalarDB[string](t, a, fmt.Sprintf("SELECT fingerprint FROM events WHERE id=%d", ev))
	if err := a.store.Write(func(tx *sql.Tx) (err error) {
		bad, err = savePlan(tx, "correction", Plan{Correction: &correctionPlan{EventID: ev, Fingerprint: fingerprintOf, BookID: id, Input: correctionInput{Action: "explode"}}})
		return
	}); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, a)
	expect(t, b.request("POST", "/plans/"+bad, nil), 400)
	if after := countRows(t, a); fmt.Sprint(before) != fmt.Sprint(after) || loanOfBook(t, a, id, "libby", "ebook") != "borrowed 2026-10-20" {
		t.Fatal("a failed correction changed the library")
	}
	// An event that no longer exists is a conflict, not a crash.
	var gone string
	a.store.Write(func(tx *sql.Tx) (err error) {
		gone, err = savePlan(tx, "correction", Plan{Correction: &correctionPlan{EventID: 9999, Input: correctionInput{Action: "void"}}})
		return
	})
	expect(t, b.request("POST", "/plans/"+gone, nil), 409)
}

func TestCorrectionsFollowMergeDeleteAndRestore(t *testing.T) {
	t.Parallel()
	a, b, keep := correctionLibrary(t)
	addRecords(t, a, records("Fold In"))
	fold := bookNamed(t, a, "Fold In").ID
	// The folded book has a different event, and one the kept book also has.
	addRecords(t, a, []Record{
		libEvent("Fold In", "nypl", "print", "borrowed", "2026-10-03T00:00:00.000000000+00:00", "2026-10-24"),
		libEvent("Fold In", "libby", "ebook", "borrowed", "2026-10-01T00:00:00.000000000+00:00", "2026-10-20"),
	})
	foldNYPL := scalarDB[int64](t, a, fmt.Sprintf("SELECT id FROM events WHERE book_id=%d AND source='nypl'", fold))
	foldDup := scalarDB[int64](t, a, fmt.Sprintf("SELECT id FROM events WHERE book_id=%d AND source='libby'", fold))
	correct(t, b, foldNYPL, url.Values{"action": {"edit"}, "source": {"nypl"}, "format": {"print"}, "status": {"borrowed"}, "date": {"2026-10-03"}, "due_date": {"2026-11-15"}}, 303)
	// The correction on the dropped duplicate moves to the kept event, which has none.
	correct(t, b, foldDup, url.Values{"action": {"edit"}, "source": {"libby"}, "format": {"ebook"}, "status": {"borrowed"}, "date": {"2026-10-01"}, "due_date": {"2026-10-31"}}, 303)
	if got := loanOfBook(t, a, fold, "nypl", "print"); got != "borrowed 2026-11-15" {
		t.Fatal(got)
	}
	page := b.request("GET", fmt.Sprintf("/books/%d/merge?target=%d", fold, keep), nil).Body.String()
	if !strings.Contains(page, "Your corrections to library events move with those events") {
		t.Fatal("the merge preview does not mention corrections")
	}
	expect(t, b.form(fmt.Sprintf("/books/%d/merge", fold), url.Values{"target": {fmt.Sprint(keep)}}), 303)
	if got := loanOfBook(t, a, keep, "nypl", "print"); got != "borrowed 2026-11-15" {
		t.Fatalf("the moved event lost its correction: %s", got)
	}
	if got := loanOfBook(t, a, keep, "libby", "ebook"); got != "borrowed 2026-10-31" {
		t.Fatalf("the dropped duplicate's correction was lost: %s", got)
	}
	if n := scalarDB[int](t, a, "SELECT count(*) FROM event_corrections WHERE book_id="+fmt.Sprint(keep)); n != 2 {
		t.Fatalf("%d corrections on the kept book", n)
	}
	// A replay of the folded book's rows (through its alias) keeps the corrections.
	var res importResult
	a.store.Write(func(tx *sql.Tx) (err error) {
		res, err = importRecords(tx, []Record{libEvent("Fold In", "nypl", "print", "borrowed", "2026-10-03T00:00:00.000000000+00:00", "2026-10-24")})
		return
	})
	if got := loanOfBook(t, a, keep, "nypl", "print"); got != "borrowed 2026-11-15" || res.Added != 0 {
		t.Fatalf("a replay through the alias undid a correction: %s %+v", got, res)
	}

	// Export and restore carry corrections, and the restored loans follow them.
	exported := exportOf(t, a)
	var f exportFile
	if err := json.Unmarshal(exported, &f); err != nil || len(f.EventCorrections) != 2 {
		t.Fatalf("%v %d", err, len(f.EventCorrections))
	}
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, exported)
	expect(t, rr, 303)
	if p := tb.request("GET", rr.Header().Get("Location"), nil).Body.String(); !strings.Contains(p, "2 corrections to library events come along") {
		t.Fatal("the restore preview does not mention corrections")
	}
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	if !equalJSON(normalizedExport(t, exportOf(t, target)), normalizedExport(t, exported)) {
		t.Fatal("corrections did not round trip")
	}
	restored := bookNamed(t, target, "Corrected").ID
	if loanOfBook(t, target, restored, "nypl", "print") != "borrowed 2026-11-15" || loanOfBook(t, target, restored, "libby", "ebook") != "borrowed 2026-10-31" {
		t.Fatal("restored loans ignore the corrections")
	}
	// Invalid corrections are refused and nothing is written.
	for name, edit := range map[string]func(map[string]any){
		"unknown event": func(m map[string]any) { c(m, 0)["fingerprint"] = strings.Repeat("a", 64) },
		"wrong book":    func(m map[string]any) { c(m, 0)["book_id"] = 999 },
		"bad action":    func(m map[string]any) { c(m, 0)["action"] = "delete" },
		"bad status":    func(m map[string]any) { c(m, 0)["status"] = "lost" },
		"bad date":      func(m map[string]any) { c(m, 0)["at"] = "later" },
		"repeated":      func(m map[string]any) { c(m, 1)["fingerprint"] = c(m, 0)["fingerprint"] },
	} {
		var m map[string]any
		json.Unmarshal(exported, &m)
		edit(m)
		data, _ := json.Marshal(m)
		other := testApp(t, true)
		ob := &browser{t: t, handler: other.Handler()}
		expect(t, ob.request("GET", "/imports", nil), 200)
		if rr := uploadRestore(ob, data); rr.Code != 400 {
			t.Errorf("%s: %d", name, rr.Code)
		}
		if countRows(t, other)["books"] != 0 {
			t.Errorf("%s wrote books", name)
		}
	}
	// Deleting a book removes its corrections.
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, keep) }); err != nil {
		t.Fatal(err)
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM event_corrections") != 0 {
		t.Fatal("a deleted book left corrections")
	}
}

func c(m map[string]any, i int) map[string]any {
	return m["event_corrections"].([]any)[i].(map[string]any)
}

func TestCorrectedEventsShowInHistoryAndBookPage(t *testing.T) {
	t.Parallel()
	a, b, id := correctionLibrary(t)
	ev := eventID(t, a, "borrowed")
	correct(t, b, ev, url.Values{"action": {"edit"}, "source": {"libby"}, "format": {"ebook"}, "status": {"borrowed"}, "date": {"2026-10-01"}, "due_date": {"2026-10-31"}}, 303)
	for _, path := range []string{"/history", fmt.Sprintf("/books/%d", id)} {
		p := b.request("GET", path, nil).Body.String()
		if !strings.Contains(p, "/events/"+fmt.Sprint(ev)+"/correct") || !strings.Contains(p, "Corrected by you") {
			t.Fatalf("%s does not show the correction", path)
		}
	}
	page := b.request("GET", fmt.Sprintf("/events/%d/correct", ev), nil).Body.String()
	if !strings.Contains(page, "As imported:") || !strings.Contains(page, "Corrected by you:") || !strings.Contains(page, "Preview restoring the original") {
		t.Fatal("the correction page lacks the original and the correction")
	}
	correct(t, b, ev, url.Values{"action": {"void"}}, 303)
	if p := b.request("GET", "/history", nil).Body.String(); !strings.Contains(p, "Voided by you") {
		t.Fatal("history does not show a void")
	}
	if loanOfBook(t, a, id, "libby", "ebook") != "none" {
		t.Fatal("a voided event still sets a loan")
	}
}

func aliasLibrary(t *testing.T) (*App, *browser) {
	t.Helper()
	a := testApp(t, true)
	addRecords(t, a, records("Keeper", "Folded", "Removed"))
	ids := bookIDs(t, a)
	exec(t, a, "UPDATE books SET isbn='9780306406157' WHERE title='Folded'")
	if err := a.store.Write(func(tx *sql.Tx) error {
		if _, err := mergeBooks(tx, ids["Folded"], ids["Keeper"], true); err != nil {
			return err
		}
		return deleteBook(tx, ids["Removed"])
	}); err != nil {
		t.Fatal(err)
	}
	return a, &browser{t: t, handler: a.Handler()}
}

func TestAliasListPreviewAndRemoval(t *testing.T) {
	t.Parallel()
	a, b := aliasLibrary(t)
	page := b.request("GET", "/aliases", nil).Body.String()
	keeper := bookNamed(t, a, "Keeper").ID
	for _, want := range []string{"Merge redirect", "Deleted", "normalized title and author: folded|author", "ISBN 9780306406157", fmt.Sprintf(`href="/books/%d"`, keeper), "Imports that match are skipped", "removed|author"} {
		if !strings.Contains(page, want) {
			t.Fatalf("list lacks %q:\n%s", want, page)
		}
	}
	if p := b.request("GET", "/aliases?kind=deleted", nil).Body.String(); strings.Contains(p, "Merge redirect") || !strings.Contains(p, "removed|author") {
		t.Fatal("deleted filter")
	}
	if p := b.request("GET", "/aliases?kind=merge", nil).Body.String(); strings.Contains(p, "removed|author") || !strings.Contains(p, "folded|author") {
		t.Fatal("merge filter")
	}
	before := countRows(t, a)

	// Preview: explained, and nothing is removed.
	rr := b.form("/aliases/remove", url.Values{"identity": {"removed|author"}})
	expect(t, rr, 303)
	plan := rr.Header().Get("Location")
	preview := b.request("GET", plan, nil).Body.String()
	for _, want := range []string{"Deleted book", "the next import containing this book adds it again as a new entry", "Nothing is added now"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview lacks %q", want)
		}
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM book_aliases") != 2 || before["books"] != countRows(t, a)["books"] || before["revision"] != countRows(t, a)["revision"] {
		t.Fatal("previewing removed something")
	}
	mergePreview := b.request("GET", b.form("/aliases/remove", url.Values{"identity": {"folded|author"}}).Header().Get("Location"), nil).Body.String()
	if !strings.Contains(mergePreview, "they match it only if its own ISBN, title and author, or Goodreads ID still matches directly") || !strings.Contains(mergePreview, "“Keeper”") {
		t.Fatal("merge preview")
	}
	// Applying removes that one record, and revives nothing.
	books := scalarDB[int](t, a, "SELECT count(*) FROM books")
	expect(t, b.request("POST", plan, nil), 303)
	if scalarDB[int](t, a, "SELECT count(*) FROM book_aliases") != 1 || scalarDB[int](t, a, "SELECT count(*) FROM books") != books {
		t.Fatal("the removal did the wrong thing")
	}
	// Re-importing now treats the book as unknown and adds it; the merge
	// redirect is untouched.
	res := importResultFor(t, a, []Record{grRecord("Removed", "to-read"), grRecord("Folded", "to-read")})
	if res.Added != 1 || res.Skipped != 0 {
		t.Fatalf("after removing the deletion record: %+v", res)
	}
	if n := scalarDB[int](t, a, "SELECT count(*) FROM books WHERE title='Folded'"); n != 0 {
		t.Fatal("the merge redirect stopped working")
	}
	// Removing the redirect makes the old title a new entry.
	expect(t, b.request("POST", b.form("/aliases/remove", url.Values{"identity": {"folded|author"}}).Header().Get("Location"), nil), 303)
	if res = importResultFor(t, a, []Record{grRecord("Folded", "to-read")}); res.Added != 1 {
		t.Fatalf("after removing the redirect: %+v", res)
	}
	if scalarDB[int](t, a, "SELECT count(*) FROM book_aliases") != 0 {
		t.Fatal("aliases remain")
	}
}

func TestAliasRemovalStaleAndEdgeCases(t *testing.T) {
	t.Parallel()
	a, b := aliasLibrary(t)
	// A missing record is a conflict.
	expect(t, b.form("/aliases/remove", url.Values{"identity": {"nothing|here"}}), 409)
	expect(t, b.form("/aliases/remove", url.Values{}), 400)
	// A preview goes stale when the library changes.
	rr := b.form("/aliases/remove", url.Values{"identity": {"removed|author"}})
	plan := rr.Header().Get("Location")
	exec(t, a, "UPDATE meta SET value=CAST(value AS INTEGER)+1 WHERE key='revision'")
	expect(t, b.request("POST", plan, nil), 409)
	if scalarDB[int](t, a, "SELECT count(*) FROM book_aliases") != 2 {
		t.Fatal("a stale removal applied")
	}
	// The record changing under a fresh preview is refused too (without a revision change).
	expect(t, b.request("GET", "/", nil), 200)
	rr = b.form("/aliases/remove", url.Values{"identity": {"removed|author"}})
	plan = rr.Header().Get("Location")
	exec(t, a, "UPDATE book_aliases SET isbn='9780306406164' WHERE identity='removed|author'")
	expect(t, b.request("POST", plan, nil), 409)
	if scalarDB[int](t, a, "SELECT count(*) FROM book_aliases") != 2 {
		t.Fatal("a changed record was removed")
	}
	// A redirect whose target is gone is shown as such, and can be removed.
	exec(t, a, "PRAGMA foreign_keys=OFF")
	exec(t, a, "UPDATE book_aliases SET target_book_id=99999 WHERE identity='folded|author'")
	if p := b.request("GET", "/aliases", nil).Body.String(); !strings.Contains(p, "no longer exists") {
		t.Fatal("a missing target is not shown")
	}
	// Records sharing a key are called out.
	exec(t, a, "UPDATE book_aliases SET isbn='9780306406157' WHERE identity='removed|author'")
	exec(t, a, "UPDATE meta SET value=CAST(value AS INTEGER)+0 WHERE key='revision'")
	rr = b.form("/aliases/remove", url.Values{"identity": {"removed|author"}})
	if p := b.request("GET", rr.Header().Get("Location"), nil).Body.String(); !strings.Contains(p, "1 other remembered record share this ISBN") {
		t.Fatal("a shared key is not mentioned")
	}
	// Plain path checks: only authenticated access, no bulk route.
	locked := testApp(t, false)
	lb := &browser{t: t, handler: locked.Handler()}
	for _, p := range []string{"/aliases", "/events/1/correct", "/plans/x/details"} {
		if rr := lb.request("GET", p, nil); rr.Code != 303 || !strings.HasPrefix(rr.Header().Get("Location"), "/login") {
			t.Errorf("%s: %d", p, rr.Code)
		}
	}
	expect(t, b.request("POST", "/aliases/clear", url.Values{}), 404)
}

func TestMigrateEventCorrectionsFromVersionFifteen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:15]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source) VALUES(1,'Old','A','old|a',0,'x','manual');
INSERT INTO events(book_id,source,format,status,activity,at,due_date,fingerprint) VALUES(1,'libby','ebook','borrowed','borrowed','2026-10-01T00:00:00.000000000+00:00','2026-10-20','fp1');
INSERT INTO loans(book_id,source,format,status,due_date,observed_at) VALUES(1,'libby','ebook','borrowed','2026-10-20','2026-10-01T00:00:00.000000000+00:00')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 16 {
		t.Fatal(v)
	}
	if scalar[int](t, path, "SELECT count(*) FROM event_corrections") != 0 || scalar[int](t, path, "SELECT count(*) FROM events") != 1 || scalar[string](t, path, "SELECT due_date FROM loans") != "2026-10-20" {
		t.Fatal("existing events or loans changed")
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v16.bak" {
		t.Fatalf("backups %v", b)
	}
}
