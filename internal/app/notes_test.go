package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const noteSecret = "ZEBRA-PRIVATE-NOTE-7731"

func noteApp(t *testing.T) (*App, *browser) {
	t.Helper()
	a := testApp(t, true)
	return a, &browser{t: t, handler: a.Handler()}
}

func noteCount(t *testing.T, a *App, bookID int64) int {
	t.Helper()
	n, err := countNotes(a.store.DB, bookID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNoteLifecycle(t *testing.T) {
	t.Parallel()
	a, b := noteApp(t)
	rs := records("Queued", "Other")
	addRecords(t, a, rs)
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	id := bookNamed(t, a, "Queued").ID
	path := fmt.Sprintf("/books/%d/notes", id)
	expect(t, b.request("GET", "/", nil), 200)
	revBefore := revisionOf(t, a)
	titlesBefore, posBefore := queueTitles(t, a)

	// Validation: nothing is saved for any of these.
	for name, v := range map[string]url.Values{
		"empty":        {"body": {"   \n "}},
		"oversized":    {"body": {strings.Repeat("x", maxNote+1)}},
		"both places":  {"body": {"hi"}, "page": {"3"}, "percent": {"4"}},
		"page zero":    {"body": {"hi"}, "page": {"0"}},
		"page text":    {"body": {"hi"}, "page": {"x"}},
		"percent high": {"body": {"hi"}, "percent": {"101"}},
	} {
		if rr := b.request("POST", path, v); rr.Code != 400 {
			t.Fatalf("%s: status %d", name, rr.Code)
		}
	}
	if noteCount(t, a, id) != 0 {
		t.Fatal("a rejected note was saved")
	}
	// A missing book is a 404, and nothing is saved.
	expect(t, b.request("POST", "/books/9999/notes", url.Values{"body": {"hi"}}), 404)

	// A note keeps its markup as text and its line breaks, for any status.
	text := "<script>alert(1)</script>\r\nline two & <b>bold</b>"
	expect(t, b.request("POST", path, url.Values{"body": {text}, "page": {"42"}}), 303)
	notes, _ := loadNotes(a.store.DB, id)
	if len(notes) != 1 || notes[0].Body != "<script>alert(1)</script>\nline two & <b>bold</b>" || notes[0].Page == nil || *notes[0].Page != 42 {
		t.Fatalf("%+v", notes)
	}
	html := b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String()
	if strings.Contains(html, "<script>alert(1)") || strings.Contains(html, "<b>bold</b>") || !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(html, "at page 42") {
		t.Fatalf("note not escaped:\n%s", html)
	}

	// Editing needs the version the page showed.
	edit := fmt.Sprintf("/notes/%d", notes[0].ID)
	expect(t, b.request("POST", edit, url.Values{"action": {"save"}, "body": {"Second thought"}, "percent": {"10"}, "version": {"1"}}), 303)
	expect(t, b.request("POST", edit, url.Values{"action": {"save"}, "body": {"From a stale page"}, "version": {"1"}}), 409)
	expect(t, b.request("POST", edit, url.Values{"action": {"delete"}, "version": {"1"}}), 409)
	expect(t, b.request("POST", edit, url.Values{"action": {"save"}, "body": {""}, "version": {"2"}}), 400)
	expect(t, b.request("POST", edit, url.Values{"action": {"save"}, "body": {"x"}}), 400)
	expect(t, b.request("POST", edit, url.Values{"action": {"explode"}, "version": {"2"}}), 400)
	notes, _ = loadNotes(a.store.DB, id)
	if len(notes) != 1 || notes[0].Body != "Second thought" || notes[0].Page != nil || notes[0].Percent == nil || *notes[0].Percent != 10 || notes[0].Version != 2 {
		t.Fatalf("%+v", notes)
	}
	expect(t, b.request("POST", edit, url.Values{"action": {"delete"}, "version": {"2"}}), 303)
	expect(t, b.request("POST", edit, url.Values{"action": {"delete"}, "version": {"2"}}), 409)
	expect(t, b.request("POST", "/notes/9999", url.Values{"action": {"delete"}, "version": {"1"}}), 409)
	expect(t, b.request("POST", "/notes/abc", url.Values{"action": {"delete"}, "version": {"1"}}), 400)

	// Notes never change the queue, status, progress, or the library revision.
	if revisionOf(t, a) != revBefore {
		t.Fatal("a note changed the revision")
	}
	titles, pos := queueTitles(t, a)
	if strings.Join(titles, ",") != strings.Join(titlesBefore, ",") || fmt.Sprint(pos) != fmt.Sprint(posBefore) {
		t.Fatal("a note moved the queue")
	}
	if got := bookNamed(t, a, "Queued"); got.Status != "to-read" || got.ProgressPage != nil || got.ProgressPercent != nil {
		t.Fatalf("%+v", got)
	}
	// The cap per book.
	for range maxNotesPerBook {
		expect(t, b.request("POST", path, url.Values{"body": {"n"}}), 303)
	}
	expect(t, b.request("POST", path, url.Values{"body": {"one too many"}}), 400)
	// The empty state.
	other := bookNamed(t, a, "Other").ID
	if !strings.Contains(b.request("GET", fmt.Sprintf("/books/%d", other), nil).Body.String(), "No notes yet") {
		t.Fatal("no empty state")
	}
}

func TestReasonForAddingAndImportsKeepLocalWriting(t *testing.T) {
	t.Parallel()
	a, b := noteApp(t)
	rr := b.request("GET", "/books/new", nil)
	if !strings.Contains(rr.Body.String(), `name="reason"`) {
		t.Fatal("add form lacks the reason field")
	}
	expect(t, b.form("/books/new", bookForm("Reasoned", "unreviewed", "reason", "Recommended <b>by</b> a friend\r\nsecond line")), 303)
	book := bookNamed(t, a, "Reasoned")
	if book.Reason != "Recommended <b>by</b> a friend\nsecond line" {
		t.Fatalf("%q", book.Reason)
	}
	edit := bookForm("Reasoned", "unreviewed", "reason", "New reason")
	expect(t, b.form(fmt.Sprintf("/books/%d/edit", book.ID), edit), 303)
	if got := bookNamed(t, a, "Reasoned").Reason; got != "New reason" {
		t.Fatal(got)
	}
	expect(t, b.form(fmt.Sprintf("/books/%d/edit", book.ID), bookForm("Reasoned", "unreviewed", "reason", strings.Repeat("r", maxReason+1))), 400)
	if !strings.Contains(b.request("GET", fmt.Sprintf("/books/%d", book.ID), nil).Body.String(), "New reason") {
		t.Fatal("the book page does not show the reason")
	}
	// A note and the reason survive an import that updates the book, and a
	// lookup-style import never writes either.
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/notes", book.ID), url.Values{"body": {"keep me"}}), 303)
	changed := Record{Book: Book{Title: "Reasoned", Author: "Author", Status: "to-read", Source: "goodreads", Tags: "fresh, tags", Interest: 3}, LookedUp: true}
	addRecords(t, a, []Record{changed})
	got := bookNamed(t, a, "Reasoned")
	if got.Reason != "New reason" || noteCount(t, a, book.ID) != 1 {
		t.Fatalf("an import changed local writing: %+v", got)
	}
}

// Notes and the reason are absent from everything an AI request or a lookup
// carries: the payload, the exact-prompt preview, and the pages that show it.
func TestNotesNeverReachAIOrLookup(t *testing.T) {
	var bodies []string
	a, _ := aiApp(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		replyFor("anthropic", `{"estimate":4,"confidence":"high","reasons":["Fits."]}`)(w, r)
	})
	id := libraryFixture(t, a, 6)
	finished := bookNamed(t, a, "Finished 01")
	for _, bookID := range []int64{id, finished.ID} {
		if _, err := a.store.DB.Exec("UPDATE books SET reason=? WHERE id=?", "reason "+noteSecret, bookID); err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.DB.Exec("INSERT INTO book_notes(book_id,body,created_at,updated_at) VALUES(?,?,'x','x')", bookID, "note "+noteSecret); err != nil {
			t.Fatal(err)
		}
	}
	b := &browser{t: t, handler: a.Handler()}
	page := b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil).Body.String()
	if strings.Contains(page, noteSecret) {
		t.Fatal("the estimate page shows notes")
	}
	in, _ := loadEstimateInputs(a.store.DB, id)
	payload, err := estimateRequest(buildEstimatePayload(in.Book, in.History, in.Baseline, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{payload.System, payload.User} {
		if strings.Contains(text, noteSecret) {
			t.Fatal("the estimate prompt carries a note")
		}
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate/ai", id), url.Values{"fingerprint": {in.Fingerprint}, "reviews": {"1"}}), 303)
	// A suggested order sends books to the AI as well.
	expect(t, b.request("GET", "/", nil), 200)
	if rr := b.request("POST", "/suggest", nil); rr.Code == 303 {
		plan := rr.Header().Get("Location")
		if page := b.request("GET", plan, nil).Body.String(); strings.Contains(page, noteSecret) {
			t.Fatal("the suggestion preview shows notes")
		} else if fp := fingerprintField.FindStringSubmatch(page); fp != nil {
			b.request("POST", plan+"/ai", url.Values{"fingerprint": {fp[1]}})
		}
	}
	if len(bodies) == 0 {
		t.Fatal("no AI request was captured")
	}
	for i, body := range bodies {
		if strings.Contains(body, noteSecret) {
			t.Fatalf("AI request %d carries a note or reason", i)
		}
	}
	// A lookup sends only a title, an author, and an ISBN.
	var fields []string
	for f := range reflect.TypeFor[LookupQuery]().Fields() {
		fields = append(fields, f.Name)
	}
	if strings.Join(fields, ",") != "Title,Author,ISBN" {
		t.Fatalf("lookup fields changed: %v", fields)
	}
}

func TestNotesAcrossUndo(t *testing.T) {
	t.Parallel()
	a, b := noteApp(t)
	addRecords(t, a, records("A", "B", "C"))
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	ids := bookIDs(t, a)
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/action", ids["A"]), url.Values{"action": {"down"}}), 303)
	if titles, _ := queueTitles(t, a); strings.Join(titles, ",") != "B,A,C" {
		t.Fatal(titles)
	}
	// A note saved after the move neither blocks Undo nor is erased by it.
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/notes", ids["A"]), url.Values{"body": {"written after the move"}}), 303)
	if _, err := a.store.DB.Exec("UPDATE books SET reason='kept reason' WHERE id=?", ids["A"]); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("GET", "/", nil), 200)
	expect(t, b.request("POST", "/undo", nil), 303)
	if titles, _ := queueTitles(t, a); strings.Join(titles, ",") != "A,B,C" {
		t.Fatalf("Undo did not apply: %v", titles)
	}
	if noteCount(t, a, ids["A"]) != 1 || bookNamed(t, a, "A").Reason != "kept reason" {
		t.Fatal("Undo erased private writing")
	}
}

func TestNotesDeleteAndMerge(t *testing.T) {
	t.Parallel()
	a, b := noteApp(t)
	addRecords(t, a, records("Keep", "Fold", "Gone", "Plain"))
	ids := bookIDs(t, a)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.store.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE books SET reason='target reason' WHERE id=?", ids["Keep"])
	exec("UPDATE books SET reason='source reason' WHERE id=?", ids["Fold"])
	for _, n := range []struct {
		book int64
		body string
	}{{ids["Keep"], "same text"}, {ids["Fold"], "same text"}, {ids["Fold"], "only on fold"}, {ids["Gone"], "doomed"}} {
		exec("INSERT INTO book_notes(book_id,body,created_at,updated_at) VALUES(?,?,'2026-10-01T00:00:00.000000000+00:00','2026-10-01T00:00:00.000000000+00:00')", n.book, n.body)
	}
	// The merge preview says what happens to private writing.
	rr := b.request("GET", fmt.Sprintf("/books/%d/merge?target=%d", ids["Fold"], ids["Keep"]), nil)
	expect(t, rr, 200)
	for _, want := range []string{"Private notes moved: 3", "kept as a note"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("merge preview lacks %q:\n%s", want, rr.Body)
		}
	}
	if noteCount(t, a, ids["Fold"]) != 2 {
		t.Fatal("the preview moved notes")
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/merge", ids["Fold"]), url.Values{"target": {fmt.Sprint(ids["Keep"])}}), 303)
	notes, _ := loadNotes(a.store.DB, ids["Keep"])
	bodies := []string{}
	for _, n := range notes {
		bodies = append(bodies, n.Body)
	}
	joined := strings.Join(bodies, "|")
	if len(notes) != 4 || strings.Count(joined, "same text") != 2 || !strings.Contains(joined, "only on fold") || !strings.Contains(joined, "source reason") {
		t.Fatalf("%q", joined)
	}
	if got := bookNamed(t, a, "Keep").Reason; got != "target reason" {
		t.Fatal(got)
	}
	var orphans int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM book_notes WHERE book_id NOT IN (SELECT id FROM books)").Scan(&orphans); err != nil || orphans != 0 {
		t.Fatal(orphans, err)
	}
	// A target without a reason takes the source's.
	exec("UPDATE books SET reason='' WHERE id=?", ids["Keep"])
	exec("UPDATE books SET reason='filled' WHERE id=?", ids["Plain"])
	if err := a.store.Write(func(tx *sql.Tx) error { _, err := mergeBooks(tx, ids["Plain"], ids["Keep"], true); return err }); err != nil {
		t.Fatal(err)
	}
	if got := bookNamed(t, a, "Keep").Reason; got != "filled" {
		t.Fatal(got)
	}
	// Deleting a book removes its notes, and the preview counts them.
	rr = b.request("GET", fmt.Sprintf("/books/%d/delete", ids["Gone"]), nil)
	if !strings.Contains(rr.Body.String(), "1 private notes") {
		t.Fatal(rr.Body)
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/delete", ids["Gone"]), nil), 303)
	if err := a.store.DB.QueryRow("SELECT count(*) FROM book_notes WHERE body='doomed'").Scan(&orphans); err != nil || orphans != 0 {
		t.Fatal("a deleted book's note remained")
	}
}

func TestNotesExportRestore(t *testing.T) {
	t.Parallel()
	source, _ := noteApp(t)
	addRecords(t, source, records("Alpha", "Beta"))
	ids := bookIDs(t, source)
	if _, err := source.store.DB.Exec("UPDATE books SET reason='because' WHERE id=?", ids["Alpha"]); err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct {
		book int64
		body string
		page any
	}{{ids["Alpha"], "first", 7}, {ids["Beta"], "second\nline", nil}} {
		if _, err := source.store.DB.Exec("INSERT INTO book_notes(book_id,body,page,created_at,updated_at) VALUES(?,?,?,'2026-10-01T00:00:00.000000000+00:00','2026-10-02T00:00:00.000000000+00:00')", n.book, n.body, n.page); err != nil {
			t.Fatal(err)
		}
	}
	original := exportOf(t, source)
	var f exportFile
	if err := json.Unmarshal(original, &f); err != nil || len(f.Notes) != 2 || f.SchemaVersion != maxRestoreSchema {
		t.Fatal(err, len(f.Notes), f.SchemaVersion)
	}
	target, tb := noteApp(t)
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, original)
	expect(t, rr, 303)
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	if got, want := normalizedExport(t, exportOf(t, target)), normalizedExport(t, original); !equalJSON(got, want) {
		t.Fatal("notes did not round trip")
	}
	if bookNamed(t, target, "Alpha").Reason != "because" {
		t.Fatal("reason lost")
	}

	// An older export has neither notes nor reasons, and restores fine.
	var raw map[string]any
	if err := json.Unmarshal(original, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "notes")
	raw["schema_version"] = 12
	for _, book := range raw["books"].([]any) {
		delete(book.(map[string]any), "reason")
	}
	older, _ := json.Marshal(raw)
	fresh, fb := noteApp(t)
	expect(t, fb.request("GET", "/imports", nil), 200)
	rr = uploadRestore(fb, older)
	expect(t, rr, 303)
	expect(t, fb.request("POST", rr.Header().Get("Location"), nil), 303)
	if noteCount(t, fresh, bookNamed(t, fresh, "Alpha").ID) != 0 {
		t.Fatal("notes appeared from nowhere")
	}

	// Invalid notes are refused.
	for name, edit := range map[string]func(map[string]any){
		"unknown book": func(m map[string]any) { m["notes"].([]any)[0].(map[string]any)["book_id"] = 999 },
		"empty body":   func(m map[string]any) { m["notes"].([]any)[0].(map[string]any)["body"] = "  " },
		"both places":  func(m map[string]any) { m["notes"].([]any)[0].(map[string]any)["percent"] = 5 },
		"bad date":     func(m map[string]any) { m["notes"].([]any)[0].(map[string]any)["created_at"] = "later" },
		"duplicate id": func(m map[string]any) {
			m["notes"].([]any)[1].(map[string]any)["id"] = m["notes"].([]any)[0].(map[string]any)["id"]
		},
		"long reason": func(m map[string]any) {
			m["books"].([]any)[0].(map[string]any)["reason"] = strings.Repeat("r", maxReason+1)
		},
	} {
		var m map[string]any
		if err := json.Unmarshal(original, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		data, _ := json.Marshal(m)
		other, ob := noteApp(t)
		expect(t, ob.request("GET", "/imports", nil), 200)
		if rr := uploadRestore(ob, data); rr.Code != 400 {
			t.Fatalf("%s: status %d", name, rr.Code)
		}
		if n := countRows(t, other)["books"]; n != 0 {
			t.Fatalf("%s wrote books", name)
		}
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestMigratePrivateNotesFromVersionTwelve(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:12]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source) VALUES(1,'Old','A','old|a',0,'x','manual')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = migrateFile(path, migrations[:13]); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 13 {
		t.Fatal(v)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM books WHERE reason='' AND title='Old'"); n != 1 {
		t.Fatal("existing books changed")
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM book_notes"); n != 0 {
		t.Fatal(n)
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v13.bak" {
		t.Fatalf("backups %v", b)
	}
}
