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

const quoteSecret = "OKAPI-PRIVATE-QUOTE-4410"

func TestQuoteLifecycleKeepsProgressAndQueue(t *testing.T) {
	t.Parallel()
	a, b := noteApp(t)
	addRecords(t, a, records("Queued", "Other"))
	id := bookNamed(t, a, "Queued").ID
	path := fmt.Sprintf("/books/%d/notes", id)
	expect(t, b.request("GET", "/", nil), 200)
	rev := revisionOf(t, a)
	titles, pos := queueTitles(t, a)

	for name, v := range map[string]url.Values{
		"no passage":        {"kind": {"quote"}, "body": {"only commentary"}},
		"oversized":         {"kind": {"quote"}, "quote": {strings.Repeat("x", maxQuote+1)}},
		"both places":       {"kind": {"quote"}, "quote": {"q"}, "page": {"3"}, "percent": {"4"}},
		"note with quote":   {"kind": {"note"}, "quote": {"smuggled"}},
		"note without body": {"kind": {"note"}, "quote": {"q"}},
	} {
		if noteCount(t, a, id) != 0 {
			t.Fatalf("%s: saved", name)
		}
		// A note ignores the quote field; only the empty-body case must fail.
		rr := b.request("POST", path, v)
		if name == "note with quote" || name == "note without body" {
			if rr.Code != 400 {
				t.Fatalf("%s: %d", name, rr.Code)
			}
			continue
		}
		if rr.Code != 400 {
			t.Fatalf("%s: %d", name, rr.Code)
		}
	}

	expect(t, b.request("POST", path, url.Values{"kind": {"quote"}, "quote": {quoteSecret + "\r\nsecond line <b>ü</b>"}, "page": {"12"}}), 303)
	notes, err := loadNotes(a.store.DB, id)
	if err != nil || len(notes) != 1 {
		t.Fatal(notes, err)
	}
	n := notes[0]
	if !n.IsQuote() || n.Body != "" || !strings.Contains(n.Quote, "second line <b>ü</b>") || strings.Contains(n.Quote, "\r") || n.Where() != "page 12" {
		t.Fatalf("%+v", n)
	}
	html := b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String()
	if !strings.Contains(html, "<blockquote") || strings.Contains(html, "<b>ü</b>") || !strings.Contains(html, "&lt;b&gt;") {
		t.Fatal("quote not shown escaped as a blockquote")
	}

	// Edit adds commentary; the kind cannot be changed by the form.
	expect(t, b.request("POST", fmt.Sprintf("/notes/%d", n.ID), url.Values{"action": {"save"}, "version": {"1"}, "kind": {"note"}, "quote": {quoteSecret}, "body": {"my thoughts"}}), 303)
	n, _ = getNote(a.store.DB, n.ID)
	if !n.IsQuote() || n.Body != "my thoughts" || n.Quote != quoteSecret || n.Version != 2 {
		t.Fatalf("%+v", n)
	}
	// Clearing the passage of a quote is refused.
	expect(t, b.request("POST", fmt.Sprintf("/notes/%d", n.ID), url.Values{"action": {"save"}, "version": {"2"}, "quote": {" "}, "body": {"x"}}), 400)

	// Writing quotes never touches the library.
	if revisionOf(t, a) != rev {
		t.Fatal("revision changed")
	}
	if t2, p2 := queueTitles(t, a); !equalJSON(t2, titles) || !equalJSON(p2, pos) {
		t.Fatal("queue changed")
	}
	expect(t, b.request("POST", fmt.Sprintf("/notes/%d", n.ID), url.Values{"action": {"delete"}, "version": {"2"}}), 303)
	if noteCount(t, a, id) != 0 {
		t.Fatal("not deleted")
	}
}

func TestQuoteIsNeverSharedToHardcover(t *testing.T) {
	a, b, _, f := hcNoteApp(t)
	var bookID int64
	a.store.DB.QueryRow("SELECT book_id FROM book_notes LIMIT 1").Scan(&bookID)
	expect(t, b.form(bookPath(bookID)+"/notes", url.Values{"kind": {"quote"}, "quote": {quoteSecret}}), 303)
	var qid int64
	if err := a.store.DB.QueryRow("SELECT id FROM book_notes WHERE kind='quote'").Scan(&qid); err != nil {
		t.Fatal(err)
	}
	page := b.request("GET", bookPath(bookID), nil).Body.String()
	if strings.Contains(page, fmt.Sprintf(`action="/notes/%d/hardcover"`, qid)) {
		t.Fatal("a quote offers Hardcover sharing")
	}
	refill(a)
	rr := b.form(fmt.Sprintf("/notes/%d/hardcover", qid), url.Values{"version": {"1"}})
	expect(t, rr, 400)
	if f.calls != 0 {
		t.Fatal("quote contacted Hardcover")
	}
}

func TestQuotesExportRestoreAndOlderExports(t *testing.T) {
	t.Parallel()
	source, _ := noteApp(t)
	addRecords(t, source, records("Alpha"))
	id := bookIDs(t, source)["Alpha"]
	for _, q := range []struct{ kind, quote, body string }{{"note", "", "plain"}, {"quote", quoteSecret, "with comment"}, {"quote", quoteSecret, ""}} {
		if _, err := source.store.DB.Exec("INSERT INTO book_notes(book_id,kind,quote,body,created_at,updated_at) VALUES(?,?,?,?,'2026-10-01T00:00:00.000000000+00:00','2026-10-01T00:00:00.000000000+00:00')", id, q.kind, q.quote, q.body); err != nil {
			t.Fatal(err)
		}
	}
	original := exportOf(t, source)
	target, tb := noteApp(t)
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, original)
	expect(t, rr, 303)
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	if got, want := normalizedExport(t, exportOf(t, target)), normalizedExport(t, original); !equalJSON(got, want) {
		t.Fatal("quotes did not round trip")
	}
	notes, _ := loadAllNotes(target.store.DB)
	if len(notes) != 3 || notes[1].Kind != "quote" || notes[2].Body != "" || notes[2].Quote != quoteSecret {
		t.Fatalf("%+v", notes)
	}

	// An export from before kinds existed restores its notes as notes; a quote
	// with no passage, or a note carrying one, is refused.
	var raw map[string]any
	json.Unmarshal(original, &raw)
	list := raw["notes"].([]any)
	delete(list[0].(map[string]any), "kind")
	raw["schema_version"] = 27
	list = list[:1]
	raw["notes"] = list
	old, _ := json.Marshal(raw)
	t2, tb2 := noteApp(t)
	expect(t, tb2.request("GET", "/imports", nil), 200)
	rr = uploadRestore(tb2, old)
	expect(t, rr, 303)
	expect(t, tb2.request("POST", rr.Header().Get("Location"), nil), 303)
	if ns, _ := loadAllNotes(t2.store.DB); len(ns) != 1 || ns[0].Kind != "note" {
		t.Fatalf("%+v", ns)
	}
	for name, edit := range map[string]func(m map[string]any){
		"empty quote":     func(m map[string]any) { m["kind"], m["quote"] = "quote", "" },
		"note with quote": func(m map[string]any) { m["kind"], m["quote"] = "note", "x" },
		"unknown kind":    func(m map[string]any) { m["kind"] = "journal" },
	} {
		var bad map[string]any
		json.Unmarshal(original, &bad)
		edit(bad["notes"].([]any)[0].(map[string]any))
		body, _ := json.Marshal(bad)
		t3, tb3 := noteApp(t)
		_ = t3
		expect(t, tb3.request("GET", "/imports", nil), 200)
		if rr := uploadRestore(tb3, body); rr.Code != 400 {
			t.Fatalf("%s: %d", name, rr.Code)
		}
	}
}

func TestQuoteMergeKeepsSeparateEntries(t *testing.T) {
	t.Parallel()
	a, _ := noteApp(t)
	addRecords(t, a, records("Keep", "Fold"))
	ids := bookIDs(t, a)
	for _, id := range []int64{ids["Keep"], ids["Fold"]} {
		if _, err := a.store.DB.Exec("INSERT INTO book_notes(book_id,kind,quote,body,hardcover_journal_id,hardcover_user_id,hardcover_book_id,created_at,updated_at) VALUES(?,'quote',?,'',0,0,0,'2026-10-01T00:00:00.000000000+00:00','2026-10-01T00:00:00.000000000+00:00')", id, quoteSecret); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.store.DB.Exec("UPDATE book_notes SET book_id=? WHERE book_id=?", ids["Keep"], ids["Fold"]); err != nil {
		t.Fatal(err)
	}
	if noteCount(t, a, ids["Keep"]) != 2 {
		t.Fatal("identical quotes must stay separate entries")
	}
}

func TestMigrateJournalQuotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:27]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source) VALUES(1,'Note','A','note|a',0,'x','manual');
 INSERT INTO book_notes(id,book_id,body,percent,version,created_at,updated_at,hardcover_journal_id,hardcover_user_id,hardcover_book_id) VALUES(7,1,'Existing note',40,3,'c','u',99,5,6);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[string](t, path, "SELECT kind FROM book_notes") != "note" || scalar[string](t, path, "SELECT quote FROM book_notes") != "" ||
		scalar[string](t, path, "SELECT body FROM book_notes") != "Existing note" || scalar[int](t, path, "SELECT id FROM book_notes") != 7 ||
		scalar[int](t, path, "SELECT version FROM book_notes") != 3 || scalar[int](t, path, "SELECT hardcover_journal_id FROM book_notes") != 99 {
		t.Fatal("migration changed an existing note or its share link")
	}
	if got := backups(t, path); len(got) != 1 || got[0] != path+".pre-v28.bak" {
		t.Fatal(got)
	}
}
