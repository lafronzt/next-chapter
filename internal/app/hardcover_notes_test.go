package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

type journalFake struct {
	entries           []hcJournal
	calls, writes     int
	queries           []string
	ambiguous, refuse bool
}

func (f *journalFake) handle(query string, vars map[string]any, _ int) (int, map[string]string, string) {
	if !strings.Contains(query, "Note") {
		return 500, nil, `{}`
	}
	f.calls++
	f.queries = append(f.queries, query)
	if strings.Contains(query, "NextChapterNoteJournal") || strings.Contains(query, "NextChapterNoteMatch") {
		entries := []hcJournal{}
		for _, e := range f.entries {
			if id, ok := vars["id"].(float64); ok && int64(e.ID) != int64(id) {
				continue
			}
			if entry, ok := vars["entry"].(string); ok && e.Entry != entry {
				continue
			}
			entries = append(entries, e)
		}
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"reading_journals": entries}})
		return 200, nil, string(raw)
	}
	if strings.Contains(query, "NextChapterInsertNote") || strings.Contains(query, "NextChapterUpdateNote") {
		f.writes++
		key := "insert_reading_journal"
		if strings.Contains(query, "NextChapterUpdateNote") {
			key = "update_reading_journal"
		}
		if f.refuse {
			return 200, nil, fmt.Sprintf(`{"data":{%q:{"errors":["refused"],"reading_journal":null}}}`, key)
		}
		e := hcJournal{ID: 123, Entry: vars["entry"].(string), Privacy: int(vars["privacy"].(float64))}
		if id, ok := vars["id"].(float64); ok {
			e.ID = flexInt(id)
		}
		f.entries = []hcJournal{e}
		if f.ambiguous {
			f.ambiguous = false
			return 200, nil, `not json`
		}
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{key: map[string]any{"errors": []string{}, "reading_journal": e}}})
		return 200, nil, string(raw)
	}
	return 500, nil, `{}`
}
func hcNoteApp(t *testing.T) (*App, *browser, int64, *journalFake) {
	t.Helper()
	f := &journalFake{}
	a, b, _ := newHCApp(t, f.handle)
	linkAccount(t, a)
	id := addBooks(t, a, Book{Title: "Note Book", Author: "Author"})[0]
	if _, err := a.store.DB.Exec("INSERT INTO book_hardcover(book_id,hardcover_book_id,linked_at) VALUES(?,500,'2026-01-01T00:00:00Z')", id); err != nil {
		t.Fatal(err)
	}
	b.request("GET", bookPath(id), nil)
	expect(t, b.form(bookPath(id)+"/notes", url.Values{"body": {"A private observation <script>alert(1)</script>"}, "percent": {"35"}}), 303)
	notes, err := loadNotes(a.store.DB, id)
	if err != nil || len(notes) != 1 {
		t.Fatal(notes, err)
	}
	return a, b, notes[0].ID, f
}
func notePreview(t *testing.T, a *App, b *browser, nid int64) string {
	t.Helper()
	refill(a)
	n, err := getNote(a.store.DB, nid)
	if err != nil {
		t.Fatal(err)
	}
	b.request("GET", bookPath(n.BookID), nil)
	return locationOf(t, b.form(fmt.Sprintf("/notes/%d/hardcover", nid), url.Values{"version": {fmt.Sprint(n.Version)}}))
}
func shareNote(t *testing.T, a *App, b *browser, path, privacy string, extra ...string) {
	t.Helper()
	b.request("GET", path, nil)
	refill(a)
	v := url.Values{"confirm": {"share"}, "privacy": {privacy}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	expect(t, b.form(path, v), 303)
}
func TestHardcoverNoteExplicitShareAndUpdate(t *testing.T) {
	a, b, nid, f := hcNoteApp(t)
	rev := revisionOf(t, a)
	if f.calls != 0 {
		t.Fatal("saving a local note contacted Hardcover")
	}
	path := notePreview(t, a, b, nid)
	if f.calls != 0 {
		t.Fatal("unshared note preview contacted Hardcover")
	}
	html := b.request("GET", path, nil).Body.String()
	if !strings.Contains(html, `value="3" selected`) || !strings.Contains(html, "&lt;script&gt;") || strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("privacy or escaping missing")
	}
	// Invalid/unconfirmed requests must not even contact Hardcover.
	expect(t, b.form(path, url.Values{"privacy": {"1"}}), 400)
	if f.calls != 0 {
		t.Fatal("missing confirmation contacted Hardcover")
	}
	shareNote(t, a, b, path, "3")
	if f.writes != 1 || len(f.entries) != 1 || f.entries[0].Privacy != 3 || !strings.HasSuffix(f.entries[0].Entry, "(35%)") {
		t.Fatalf("%+v", f)
	}
	n, _ := getNote(a.store.DB, nid)
	if n.HardcoverJournalID != 123 || n.HardcoverBookID != 500 || n.HardcoverUserID == 0 || revisionOf(t, a) != rev {
		t.Fatalf("%+v", n)
	}
	// The consumed plan cannot write again.
	rr := b.form(path, url.Values{"confirm": {"share"}, "privacy": {"3"}})
	if rr.Code == 303 || f.writes != 1 {
		t.Fatal("double send")
	}
	before := f.calls
	expect(t, b.form(fmt.Sprintf("/notes/%d", nid), url.Values{"action": {"save"}, "version": {fmt.Sprint(n.Version)}, "body": {"Updated locally"}}), 303)
	if f.calls != before || strings.Contains(f.entries[0].Entry, "Updated") {
		t.Fatal("local edit shared itself")
	}
	path = notePreview(t, a, b, nid)
	shareNote(t, a, b, path, "2")
	if f.writes != 2 || f.entries[0].Privacy != 2 || f.entries[0].Entry != "Updated locally" || !strings.Contains(f.queries[len(f.queries)-1], "NextChapterUpdateNote") {
		t.Fatalf("%+v", f)
	}
	// Share identity survives export/restore without enabling automatic sharing.
	raw := exportOf(t, a)
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	tb.request("GET", "/", nil)
	restore := locationOf(t, uploadRestore(tb, raw))
	tb.request("GET", restore, nil)
	expect(t, tb.form(restore, nil), 303)
	notes, err := loadAllNotes(target.store.DB)
	if err != nil || len(notes) != 1 || notes[0].HardcoverJournalID != 123 {
		t.Fatal(notes, err)
	}
}
func TestHardcoverNoteStalePreviewAndRemoteConflict(t *testing.T) {
	a, b, nid, f := hcNoteApp(t)
	path := notePreview(t, a, b, nid)
	if _, err := a.store.DB.Exec("UPDATE book_notes SET version=version+1,body='Changed' WHERE id=?", nid); err != nil {
		t.Fatal(err)
	}
	expect(t, b.form(path, url.Values{"confirm": {"share"}, "privacy": {"1"}}), 409)
	if f.calls != 0 {
		t.Fatal("stale note was sent")
	}
	path = notePreview(t, a, b, nid)
	shareNote(t, a, b, path, "3")
	path = notePreview(t, a, b, nid)
	f.entries[0].Entry = "Edited on Hardcover"
	expect(t, b.form(path, url.Values{"confirm": {"share"}, "privacy": {"3"}}), 409)
	if f.writes != 1 {
		t.Fatal("overwrote unpreviewed remote edit")
	}
}
func TestHardcoverNoteAmbiguousSendIsNotDuplicated(t *testing.T) {
	a, b, nid, f := hcNoteApp(t)
	path := notePreview(t, a, b, nid)
	b.gateway = true
	f.ambiguous = true
	expect(t, b.form(path, url.Values{"confirm": {"share"}, "privacy": {"3"}}), 502)
	if f.writes != 1 {
		t.Fatal(f.writes)
	}
	refill(a)
	// Identical text alone is not proof: without the explicit link choice
	// nothing is adopted and nothing is written.
	b.gateway = false
	expect(t, b.form(path, url.Values{"confirm": {"share"}, "privacy": {"3"}}), 409)
	if n, _ := getNote(a.store.DB, nid); n.HardcoverJournalID != 0 || f.writes != 1 {
		t.Fatal("adopted a text match without consent")
	}
	refill(a)
	shareNote(t, a, b, path, "3", "link", "existing")
	n, _ := getNote(a.store.DB, nid)
	if f.writes != 1 || n.HardcoverJournalID != 123 {
		t.Fatalf("%d %+v", f.writes, n)
	}
}
func TestHardcoverNoteRefusalKeepsLocal(t *testing.T) {
	a, b, nid, f := hcNoteApp(t)
	path := notePreview(t, a, b, nid)
	b.gateway = true
	f.refuse = true
	rr := b.form(path, url.Values{"confirm": {"share"}, "privacy": {"3"}})
	if rr.Code < 400 {
		t.Fatal(rr.Code)
	}
	n, _ := getNote(a.store.DB, nid)
	if n.HardcoverJournalID != 0 || n.Body == "" {
		t.Fatalf("%+v", n)
	}
}

func TestHardcoverNoteBookLinkChangedAndForget(t *testing.T) {
	a, b, nid, f := hcNoteApp(t)
	path := notePreview(t, a, b, nid)
	if _, err := a.store.DB.Exec("UPDATE book_hardcover SET hardcover_book_id=501"); err != nil {
		t.Fatal(err)
	}
	expect(t, b.form(path, url.Values{"confirm": {"share"}, "privacy": {"1"}}), 409)
	if f.calls != 0 {
		t.Fatal("changed book link shared note")
	}
	if _, err := a.store.DB.Exec("UPDATE book_hardcover SET hardcover_book_id=500"); err != nil {
		t.Fatal(err)
	}
	path = notePreview(t, a, b, nid)
	shareNote(t, a, b, path, "1")
	if f.entries[0].Privacy != 1 {
		t.Fatal("explicit public choice not sent")
	}
	counts, err := countForget(a.store.DB)
	if err != nil || counts.NoteIDs != 1 {
		t.Fatal(counts, err)
	}
	if err := forgetIDs(a.store.DB); err != nil {
		t.Fatal(err)
	}
	n, err := getNote(a.store.DB, nid)
	if err != nil || n.Body == "" || n.HardcoverJournalID != 0 || n.HardcoverUserID != 0 || n.HardcoverBookID != 0 {
		t.Fatal(n, err)
	}
	if f.writes != 1 {
		t.Fatal("forgetting wrote to Hardcover")
	}
}
