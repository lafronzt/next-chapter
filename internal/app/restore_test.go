package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func uploadRestore(b *browser, data []byte) *httptest.ResponseRecorder {
	b.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("csrf", b.csrf)
	writer.WriteField("revision", b.rev)
	part, _ := writer.CreateFormFile("file", "next-chapter.json")
	part.Write(data)
	writer.Close()
	req := httptest.NewRequest("POST", "/imports/restore", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return b.serve(req)
}

func exportOf(t *testing.T, a *App) []byte {
	t.Helper()
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/export.json", nil)
	expect(t, rr, 200)
	return rr.Body.Bytes()
}

// normalizedExport replaces database IDs with list positions and drops the
// export time, so exports of equivalent libraries compare equal.
func normalizedExport(t *testing.T, raw []byte) exportFile {
	t.Helper()
	var f exportFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	index := map[int64]int64{}
	for i := range f.Books {
		index[f.Books[i].ID] = int64(i)
		f.Books[i].ID = int64(i)
	}
	for i := range f.Books {
		for j := range f.Books[i].Loans {
			f.Books[i].Loans[j].BookID = index[f.Books[i].Loans[j].BookID]
		}
	}
	for i := range f.Events {
		f.Events[i].ID, f.Events[i].BookID = 0, index[f.Events[i].BookID]
	}
	for i := range f.ReadingLog {
		f.ReadingLog[i].ID, f.ReadingLog[i].BookID = 0, index[f.ReadingLog[i].BookID]
	}
	for i := range f.Aliases {
		if id := f.Aliases[i].TargetBookID; id != nil {
			n := index[*id]
			f.Aliases[i].TargetBookID = &n
		}
	}
	f.ExportedAt = ""
	return f
}

// richLibrary has pins, Needs placement, series, every reading status, loans
// with several events, a merge alias, a deletion tombstone, preferences, and
// Goodreads polling turned on.
func richLibrary(t *testing.T) *App {
	t.Helper()
	a := testApp(t, true)
	one, two, pages := 1.0, 2.5, 320
	rs := records("Queue A", "Queue B", "Queue C", "Merged Away", "Deleted")
	rs[0].Series, rs[0].SeriesNumber, rs[0].Pages, rs[0].Tags, rs[0].ISBN = "Saga", &one, &pages, "calm, hopeful", "9780306406157"
	rs[1].Series, rs[1].SeriesNumber, rs[1].GoodreadsID, rs[1].Rating, rs[1].Interest = "saga", &two, "123", 5, 5
	rs = append(rs, shelf("Finished", "read"), shelf("Now Reading", "reading"), shelf("Dropped", "abandoned"))
	addRecords(t, a, rs)
	addRecords(t, a, []Record{libbyLoan("Queue C"), libbyLoan("Library Only")})
	ret := libbyEvent("Queue C", "returned", "2026-10-05T00:00:00.000000000+00:00")
	ret.Event.Format = "ebook"
	nypl := libbyEvent("Queue B", "borrowed", "2026-10-02T09:30:00.000000000+00:00")
	nypl.Source, nypl.Event.Format, nypl.Event.DueDate = "nypl", "print", "2026-10-23"
	addRecords(t, a, []Record{ret, nypl})
	ids := bookIDs(t, a)
	err := a.store.Write(func(tx *sql.Tx) error {
		if _, err := mergeBooks(tx, ids["Merged Away"], ids["Queue A"], true); err != nil {
			return err
		}
		if err := deleteBook(tx, ids["Deleted"]); err != nil {
			return err
		}
		// Queue order differs from insertion order, with a pin and a
		// placement flag.
		if err := reorder(tx, []int64{ids["Queue C"], ids["Queue A"], ids["Queue B"]}); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE books SET pinned=1, needs_placement=0 WHERE id=?; UPDATE books SET needs_placement=0 WHERE id=?", ids["Queue A"], ids["Queue C"]); err != nil {
			return err
		}
		if err := setMeta(tx, "preferences", Preferences{LibraryWeight: 0.5, Length: "short", Mood: "hopeful"}); err != nil {
			return err
		}
		return setMeta(tx, "sync", Connection{UserID: "42", Shelf: "favorites", Enabled: true})
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func countRows(t *testing.T, a *App) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"books", "events", "loans", "plans", "book_aliases"} {
		var n int
		if err := a.store.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	rev, err := revision(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	out["revision"] = int(rev)
	return out
}

func TestRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	source := richLibrary(t)
	original := exportOf(t, source)
	target := testApp(t, true)
	b := &browser{t: t, handler: target.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	rr := uploadRestore(b, original)
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	if n := countRows(t, target)["books"]; n != 0 {
		t.Fatalf("preview wrote %d books", n)
	}
	rr = b.request("GET", planURL, nil)
	expect(t, rr, 200)
	for _, text := range []string{"3 books in your queue and 4 other books, with 4 library events and 3 current loans", "2 merge and deletion aliases", "polling off (it was on when exported)", "Queue C", "Restore library"} {
		if !strings.Contains(rr.Body.String(), text) {
			t.Fatalf("preview missing %q:\n%s", text, rr.Body)
		}
	}
	rr = b.request("POST", planURL, nil)
	expect(t, rr, 303)
	if rr.Header().Get("Location") != "/?notice=restored-sync" {
		t.Fatal(rr.Header())
	}
	if !strings.Contains(b.request("GET", "/?notice=restored-sync", nil).Body.String(), "Goodreads polling is off") {
		t.Fatal("notice missing")
	}
	restored := exportOf(t, target)
	want, got := normalizedExport(t, original), normalizedExport(t, restored)
	if !want.Sync.Enabled || got.Sync.Enabled {
		t.Fatalf("polling: exported %v, restored %v", want.Sync.Enabled, got.Sync.Enabled)
	}
	want.Sync.Enabled = false
	if !reflect.DeepEqual(want, got) {
		w, _ := json.MarshalIndent(want, "", " ")
		g, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("round trip differs\nwant %s\ngot  %s", w, g)
	}
	// Spot-check what the comparison covers.
	titles, _ := queueTitles(t, target)
	if strings.Join(titles, ",") != "Queue C,Queue A,Queue B" || !bookNamed(t, target, "Queue A").Pinned || !bookNamed(t, target, "Queue B").NeedsPlacement || bookNamed(t, target, "Queue B").Interest != 5 {
		t.Fatalf("queue %v", titles)
	}
	if loans := bookNamed(t, target, "Queue C").Loans; len(loans) != 1 || loans[0].Status != "returned" {
		t.Fatalf("loans %+v", loans)
	}
	// Restored aliases still steer imports: the merged book updates its
	// target and the deleted one stays deleted.
	result := importResultFor(t, target, records("Merged Away", "Deleted"))
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("%+v", result)
	}
	// Re-importing an event that came with the export is a duplicate.
	if result = importResultFor(t, target, []Record{ret("Queue C")}); result.Events != 0 || result.Changed {
		t.Fatalf("%+v", result)
	}
	// Restoring again is refused now that the library has books.
	expect(t, b.request("GET", "/imports", nil), 200)
	before := countRows(t, target)
	rr = uploadRestore(b, original)
	expect(t, rr, 409)
	if !strings.Contains(rr.Body.String(), "needs an empty library") {
		t.Fatal(rr.Body)
	}
	if after := countRows(t, target); !reflect.DeepEqual(before, after) {
		t.Fatalf("%v -> %v", before, after)
	}
}

func ret(title string) Record {
	r := libbyEvent(title, "returned", "2026-10-05T00:00:00.000000000+00:00")
	r.Event.Format = "ebook"
	return r
}

func TestRestoreRefusedWhenLibraryFillsBeforeApply(t *testing.T) {
	t.Parallel()
	original := exportOf(t, richLibrary(t))
	target := testApp(t, true)
	b := &browser{t: t, handler: target.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	rr := uploadRestore(b, original)
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	// A change that does not move the revision still cannot slip a book in.
	addRecords(t, target, records("Sneaked In"))
	expect(t, b.request("GET", planURL, nil), 200)
	expect(t, b.request("POST", planURL, nil), 409)
	if n := countRows(t, target)["books"]; n != 1 {
		t.Fatalf("%d books", n)
	}
}

func TestRestoreStaleRevision(t *testing.T) {
	t.Parallel()
	original := exportOf(t, richLibrary(t))
	target := testApp(t, true)
	b := &browser{t: t, handler: target.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	rr := uploadRestore(b, original)
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	expect(t, b.form("/settings", url.Values{"library_weight": {"1"}, "length": {"any"}, "shelf": {"to-read"}}), 303)
	expect(t, b.request("GET", planURL, nil), 200)
	rr = b.request("POST", planURL, nil)
	expect(t, rr, 409)
	if !strings.Contains(rr.Body.String(), "Generate a fresh preview") || countRows(t, target)["books"] != 0 {
		t.Fatal(rr.Body)
	}
	// An upload against an old revision is refused before it is read.
	expect(t, b.request("GET", "/imports", nil), 200)
	b.rev = "0"
	expect(t, uploadRestore(b, original), 409)
}

// editExport decodes an export into generic JSON, applies change, and encodes it.
func editExport(t *testing.T, raw []byte, change func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRestoreRejectsInvalidFiles(t *testing.T) {
	t.Parallel()
	original := exportOf(t, richLibrary(t))
	book := func(m map[string]any, i int) map[string]any { return m["books"].([]any)[i].(map[string]any) }
	event := func(m map[string]any, i int) map[string]any { return m["events"].([]any)[i].(map[string]any) }
	edit := func(change func(map[string]any)) []byte { return editExport(t, original, change) }
	cases := []struct {
		name    string
		data    []byte
		status  int
		message string
	}{
		{"not JSON", []byte("{not json"), 400, "not valid JSON"},
		{"truncated", original[:len(original)/2], 400, "ends before the export is complete"},
		{"a CSV", []byte("Title,Author\nA,B\n"), 400, "not valid JSON"},
		{"an array", []byte("[]"), 400, "not a Next Chapter JSON export"},
		{"trailing data", append(append([]byte{}, original...), []byte(` {}`)...), 400, "extra data"},
		{"no version", edit(func(m map[string]any) { delete(m, "schema_version") }), 400, "no schema_version"},
		{"newer version", edit(func(m map[string]any) { m["schema_version"] = maxRestoreSchema + 1 }), 400, fmt.Sprintf("restores schema versions 1 to %d", maxRestoreSchema)},
		{"version zero", edit(func(m map[string]any) { m["schema_version"] = 0 }), 400, "schema version 0"},
		{"unknown top-level key", edit(func(m map[string]any) { m["plans"] = []any{} }), 400, `unknown field "plans"`},
		{"unknown book key", edit(func(m map[string]any) { book(m, 0)["rating"] = 5 }), 400, `unknown field "rating"`},
		{"wrong type", edit(func(m map[string]any) { book(m, 0)["pages"] = "many" }), 400, "wrong type"},
		{"no books", edit(func(m map[string]any) { m["books"], m["events"], m["aliases"] = []any{}, []any{}, []any{} }), 400, "no books"},
		{"interest out of range", edit(func(m map[string]any) { book(m, 0)["interest"] = 9 }), 400, "Interest must be between 1 and 5"},
		{"pages out of range", edit(func(m map[string]any) { book(m, 0)["pages"] = 0 }), 400, "Pages must be between"},
		{"series number", edit(func(m map[string]any) { book(m, 0)["series_number"] = 10000 }), 400, "valid series number"},
		{"long title", edit(func(m map[string]any) { book(m, 0)["title"] = strings.Repeat("t", 501) }), 400, "within the field limits"},
		{"unknown status", edit(func(m map[string]any) { book(m, 0)["reading_status"] = "finished" }), 400, "Choose a reading status"},
		{"bad added_at", edit(func(m map[string]any) { book(m, 0)["added_at"] = "last week" }), 400, "added_at must be"},
		{"bad ISBN", edit(func(m map[string]any) { book(m, 0)["isbn"] = "12345" }), 400, "ISBN is not valid"},
		{"duplicate identity", edit(func(m map[string]any) {
			book(m, 1)["title"], book(m, 1)["author"] = book(m, 0)["title"], book(m, 0)["author"]
		}), 400, "same title and author"},
		{"duplicate book id", edit(func(m map[string]any) { book(m, 1)["id"] = book(m, 0)["id"] }), 400, "unique positive"},
		{"duplicate Goodreads ID", edit(func(m map[string]any) { book(m, 0)["goodreads_id"] = "123" }), 400, "same Goodreads ID"},
		{"event for a missing book", edit(func(m map[string]any) { event(m, 0)["book_id"] = 999999 }), 400, "not in the file"},
		{"unknown event status", edit(func(m map[string]any) { event(m, 0)["status"] = "lost" }), 400, `unknown status "lost"`},
		{"bad due date", edit(func(m map[string]any) { event(m, 0)["due_date"] = "2026-02-30" }), 400, "due_date must be YYYY-MM-DD"},
		{"bad event time", edit(func(m map[string]any) { event(m, 0)["at"] = "" }), 400, "at must be a date"},
		{"bad fingerprint", edit(func(m map[string]any) { event(m, 0)["fingerprint"] = "abc" }), 400, "fingerprint"},
		{"repeated fingerprint", edit(func(m map[string]any) { event(m, 1)["fingerprint"] = event(m, 0)["fingerprint"] }), 400, "fingerprint"},
		{"alias to a missing book", edit(func(m map[string]any) {
			for _, x := range m["aliases"].([]any) {
				x.(map[string]any)["target_book_id"] = 999999
			}
		}), 400, "not in the file"},
		{"bad preferences", edit(func(m map[string]any) { m["preferences"].(map[string]any)["length"] = "medium" }), 400, "length preference"},
		{"bad Goodreads settings", edit(func(m map[string]any) { m["sync"].(map[string]any)["user_id"] = "me" }), 400, "numeric Goodreads user ID"},
		{"oversized", append(append([]byte{}, original...), bytes.Repeat([]byte(" "), maxRestoreUpload)...), 413, "at most 10 MB"},
	}
	target := testApp(t, true)
	b := &browser{t: t, handler: target.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	before := countRows(t, target)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b.t = t
			expect(t, b.request("GET", "/imports", nil), 200)
			rr := uploadRestore(b, c.data)
			expect(t, rr, c.status)
			if !strings.Contains(rr.Body.String(), template.HTMLEscapeString(c.message)) {
				t.Fatalf("missing %q:\n%s", c.message, rr.Body)
			}
			if after := countRows(t, target); !reflect.DeepEqual(before, after) {
				t.Fatalf("database changed: %v -> %v", before, after)
			}
		})
	}
	// The unchanged export is accepted, so each case failed for its own reason.
	b.t = t
	expect(t, b.request("GET", "/imports", nil), 200)
	expect(t, uploadRestore(b, original), 303)
}

// An export from before the migration framework has schema_version 1, no
// aliases, and no sync settings.
func TestRestoreVersionOneExport(t *testing.T) {
	t.Parallel()
	original := editExport(t, exportOf(t, richLibrary(t)), func(m map[string]any) {
		m["schema_version"] = 1
		delete(m, "aliases")
		delete(m, "sync")
		delete(m, "preferences")
	})
	target := testApp(t, true)
	b := &browser{t: t, handler: target.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	rr := uploadRestore(b, original)
	expect(t, rr, 303)
	expect(t, b.request("GET", rr.Header().Get("Location"), nil), 200)
	rr = b.request("POST", rr.Header().Get("Location"), nil)
	expect(t, rr, 303)
	if rr.Header().Get("Location") != "/?notice=restored" {
		t.Fatal(rr.Header())
	}
	if rows := countRows(t, target); rows["books"] != 7 || rows["book_aliases"] != 0 {
		t.Fatal(rows)
	}
	if conn, _ := readMeta(target.store.DB, "sync", Connection{}); conn != (Connection{}) {
		t.Fatalf("%+v", conn)
	}
}

// The restore route takes larger files than every other form.
func TestRestoreUploadLimit(t *testing.T) {
	t.Parallel()
	original := exportOf(t, richLibrary(t))
	padded := append(bytes.Repeat([]byte(" "), 5_000_000), original...)
	target := testApp(t, true)
	b := &browser{t: t, handler: target.Handler()}
	expect(t, b.request("GET", "/imports", nil), 200)
	expect(t, uploadRestore(b, padded), 303)
	expect(t, b.request("GET", "/imports", nil), 200)
	expect(t, uploadCSV(b, "goodreads", "Title,Author\nA,"+strings.Repeat("B", maxUpload)), 413)
	expect(t, b.request("GET", "/settings", nil), 200)
	expect(t, b.request("POST", "/settings", url.Values{"mood": {strings.Repeat("m", maxUpload)}}), 413)
}

func TestRestoreSchemaVersionIsReviewed(t *testing.T) {
	t.Parallel()
	if latest := latestVersion(migrations); maxRestoreSchema != latest {
		t.Fatalf("migration %d was added: check that restore.go handles its schema, then set maxRestoreSchema to %d", latest, latest)
	}
}

// A plan kind the apply code does not know must fail, never reorder.
func TestUnknownPlanKindIsNotApplied(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("A", "B"))
	var id string
	err := a.store.Write(func(tx *sql.Tx) error {
		var err error
		id, err = savePlan(tx, "mystery", Plan{Rows: []Suggestion{{ID: 2}, {ID: 1}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/settings", nil), 200)
	req := httptest.NewRequest("POST", "/plans/"+id, strings.NewReader(url.Values{"csrf": {b.csrf}, "revision": {b.rev}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(b.cookie)
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Fatalf("status %d", rr.Code)
	}
	if titles, _ := queueTitles(t, a); strings.Join(titles, "") != "AB" {
		t.Fatal(titles)
	}
}
