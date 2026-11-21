package app

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const journalSecret = "NARWHAL-JOURNAL-TEXT-9921"

func jEntry(id int, event, entry string, privacy int, meta string) hcJournal {
	j := hcJournal{ID: flexInt(id), Event: event, Entry: entry, Privacy: privacy, CreatedAt: "2026-05-01T10:00:00"}
	if meta != "" {
		j.Metadata = json.RawMessage(meta)
	}
	return j
}

func journalApp(t *testing.T) (*App, *browser, *journalFake, int64) {
	t.Helper()
	f := &journalFake{multi: true}
	a, b, _ := newHCApp(t, f.handle)
	linkAccount(t, a)
	id := addBooks(t, a, Book{Title: "Journal Book", Author: "Author"})[0]
	if _, err := a.store.DB.Exec("INSERT INTO book_hardcover(book_id,hardcover_book_id,linked_at) VALUES(?,500,'2026-01-01T00:00:00Z')", id); err != nil {
		t.Fatal(err)
	}
	b.request("GET", bookPath(id), nil)
	return a, b, f, id
}

// localNote inserts an entry; journalID 0 leaves it unlinked.
func localNote(t *testing.T, a *App, bookID int64, kind, text, body string, page any, journalID int, basePrivacy int, base string) int64 {
	t.Helper()
	quote, b := "", text
	if kind == "quote" {
		quote, b = text, body
	}
	user, hcBook := 0, 0
	if journalID > 0 {
		user, hcBook = 123, 500
	}
	res, err := a.store.DB.Exec(`INSERT INTO book_notes(book_id,kind,quote,body,page,created_at,updated_at,hardcover_journal_id,hardcover_user_id,hardcover_book_id,hc_base_entry,hc_base_page,hc_base_privacy,hc_synced_at)
 VALUES(?,?,?,?,?,'2026-10-01T00:00:00.000000000+00:00','2026-10-01T00:00:00.000000000+00:00',?,?,?,?,?,?,?)`,
		bookID, kind, quote, b, page, journalID, user, hcBook, base, page, basePrivacy, map[bool]string{true: "2026-10-02T00:00:00.000000000+00:00"}[basePrivacy != 0])
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func checkJournal(t *testing.T, a *App, b *browser, bookID int64) string {
	t.Helper()
	refill(a)
	b.request("GET", bookPath(bookID), nil)
	return locationOf(t, b.form(fmt.Sprintf("/books/%d/journal/check", bookID), nil))
}

func applyJournal(t *testing.T, a *App, b *browser, path string, v url.Values) *httptest.ResponseRecorder {
	t.Helper()
	b.request("GET", path, nil)
	refill(a)
	return b.form(path, v)
}

func notesOf(t *testing.T, a *App, bookID int64) []bookNote {
	t.Helper()
	n, err := loadNotes(a.store.DB, bookID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJournalCheckImportsOnlyChosenAndIgnoresAutomaticEvents(t *testing.T) {
	a, b, f, id := journalApp(t)
	logs := captureLog(a)
	f.entries = []hcJournal{
		jEntry(11, "note", "Remote note "+journalSecret, 3, ""),
		jEntry(12, "quote", "A remote quote", 2, `{"page": 42}`),
		jEntry(13, "status_read", "", 3, ""),
		jEntry(14, "rated", "", 3, ""),
	}
	rev := revisionOf(t, a)
	path := checkJournal(t, a, b, id)
	if f.writes != 0 || len(notesOf(t, a, id)) != 0 {
		t.Fatal("a check wrote something")
	}
	if v := f.listVars[0]; v["uid"].(float64) != 123 || v["book"].(float64) != 500 || v["limit"].(float64) != 100 {
		t.Fatalf("%v", v)
	}
	html := b.request("GET", path, nil).Body.String()
	for _, want := range []string{"Remote note " + journalSecret, "A remote quote", "Written by Hardcover itself (status, progress, ratings, reviews) and never imported: 2", `value="leave" checked`, "On Hardcover, not here"} {
		if !strings.Contains(html, want) {
			t.Errorf("preview missing %q", want)
		}
	}
	// Applying with nothing selected changes nothing.
	expect(t, applyJournal(t, a, b, path, url.Values{}), 200)
	if len(notesOf(t, a, id)) != 0 {
		t.Fatal("unselected entries were imported")
	}
	path = checkJournal(t, a, b, id)
	rr := applyJournal(t, a, b, path, url.Values{"act_r12": {"import"}})
	expect(t, rr, 200)
	notes := notesOf(t, a, id)
	if len(notes) != 1 {
		t.Fatalf("%+v", notes)
	}
	n := notes[0]
	if !n.IsQuote() || n.Quote != "A remote quote" || n.Page == nil || *n.Page != 42 || n.Origin != "hardcover" || n.HardcoverJournalID != 12 || n.HardcoverUserID != 123 || n.HardcoverBookID != 500 || n.BasePrivacy != 2 || n.ChangedHere() {
		t.Fatalf("%+v", n)
	}
	if n.CreatedAt[:10] != "2026-05-01" {
		t.Fatalf("created %s", n.CreatedAt)
	}
	// The consumed plan cannot apply again; a new check shows it in sync.
	if rr := b.form(path, url.Values{"act_r12": {"import"}}); rr.Code != 404 {
		t.Fatalf("replay: %d", rr.Code)
	}
	path = checkJournal(t, a, b, id)
	html = b.request("GET", path, nil).Body.String()
	if strings.Contains(html, "A remote quote") || !strings.Contains(html, "In sync: 1") {
		t.Fatal("an imported entry was offered again")
	}
	expect(t, applyJournal(t, a, b, path, url.Values{"act_r11": {"import"}}), 200)
	if len(notesOf(t, a, id)) != 2 {
		t.Fatal("not two entries")
	}
	// None of that wrote to Hardcover, touched progress, or moved the revision.
	if f.writes != 0 || revisionOf(t, a) != rev {
		t.Fatal("writes or a revision change")
	}
	if strings.Contains(logs.String(), journalSecret) || strings.Contains(logs.String(), "A remote quote") {
		t.Fatal("entry text reached the logs")
	}
}

func TestJournalCheckPaginationAndFailure(t *testing.T) {
	a, b, f, id := journalApp(t)
	for i := 1; i <= 250; i++ {
		f.entries = append(f.entries, jEntry(i, "status_read", "", 3, ""))
	}
	path := checkJournal(t, a, b, id)
	if f.listCalls != 3 || !strings.Contains(b.request("GET", path, nil).Body.String(), "Read 250 entries") {
		t.Fatalf("calls %d", f.listCalls)
	}
	// More than a check reads: stop at the budget and do not infer deletions.
	localNote(t, a, id, "note", "Shared earlier", "", nil, 999, 3, "Shared earlier")
	for i := 251; i <= 600; i++ {
		f.entries = append(f.entries, jEntry(i, "status_read", "", 3, ""))
	}
	f.listCalls = 0
	path = checkJournal(t, a, b, id)
	html := b.request("GET", path, nil).Body.String()
	if f.listCalls != hcJournalPages || !strings.Contains(html, "more entries than one check reads") || strings.Contains(html, "Missing on Hardcover") {
		t.Fatalf("calls %d", f.listCalls)
	}
	// A failed page fails the check: no preview, and nothing looks missing.
	f.entries, f.listFail = f.entries[:150], 100
	refill(a)
	b.gateway = true
	rr := b.form(fmt.Sprintf("/books/%d/journal/check", id), nil)
	if rr.Code < 400 {
		t.Fatalf("a failed read produced a preview: %d", rr.Code)
	}
}

func TestJournalCheckStates(t *testing.T) {
	a, b, f, id := journalApp(t)
	both := localNote(t, a, id, "quote", "Quote local edit", "", nil, 21, 3, "Quote base")
	remoteOnlyPrivacy := localNote(t, a, id, "note", "Privacy base text", "", nil, 22, 3, "Privacy base text")
	localOnly := localNote(t, a, id, "note", "Local edit only", "", nil, 23, 3, "Local base")
	missing := localNote(t, a, id, "note", "Was shared", "", nil, 24, 3, "Was shared")
	f.entries = []hcJournal{
		jEntry(21, "quote", "Quote remote edit", 3, ""),
		jEntry(22, "note", "Privacy base text", 1, ""),
		jEntry(23, "note", "Local base", 3, ""),
	}
	path := checkJournal(t, a, b, id)
	html := b.request("GET", path, nil).Body.String()
	for _, want := range []string{"Changed in both places", "Changed on Hardcover", "Changed here", "Missing on Hardcover", "Preview sending mine…", `form="send-` + fmt.Sprint(both) + `"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	// With nothing chosen every entry is untouched.
	before := notesOf(t, a, id)
	expect(t, applyJournal(t, a, b, path, url.Values{}), 200)
	if after := notesOf(t, a, id); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatal("an unselected entry changed")
	}
	// A choice that does not belong to the state is refused.
	path = checkJournal(t, a, b, id)
	rr := applyJournal(t, a, b, path, url.Values{fmt.Sprintf("act_l%d", localOnly): {"remote"}, "act_r23": {"remote"}})
	if !strings.Contains(rr.Body.String(), "that choice does not apply") {
		t.Fatal("accepted a choice that does not apply")
	}
	// Use remote on both-changed and privacy-only changes; unlink the missing one.
	path = checkJournal(t, a, b, id)
	rr = applyJournal(t, a, b, path, url.Values{"act_r21": {"remote"}, "act_r22": {"remote"}, fmt.Sprintf("act_l%d", missing): {"unlink"}})
	expect(t, rr, 200)
	n := func(id int64) bookNote { x, _ := getNote(a.store.DB, id); return x }
	if x := n(both); x.Quote != "Quote remote edit" || x.BaseEntry != "Quote remote edit" || x.Version != 2 || x.ChangedHere() {
		t.Fatalf("%+v", x)
	}
	if x := n(remoteOnlyPrivacy); x.BasePrivacy != 1 || x.Body != "Privacy base text" {
		t.Fatalf("%+v", x)
	}
	if x := n(missing); x.Body != "Was shared" || x.Linked() || x.BasePrivacy != 0 {
		t.Fatalf("an unlinked entry lost its text or link: %+v", x)
	}
	if !n(localOnly).ChangedHere() && n(localOnly).Body != "Local edit only" {
		t.Fatal("local edit lost")
	}
	if f.writes != 0 {
		t.Fatal("applying wrote to Hardcover")
	}
	if len(notesOf(t, a, id)) != 4 {
		t.Fatal("an entry was deleted")
	}
}

func TestJournalApplySkipsWhatChangedAfterPreview(t *testing.T) {
	a, b, f, id := journalApp(t)
	linked := localNote(t, a, id, "note", "Base", "", nil, 31, 3, "Base")
	f.entries = []hcJournal{jEntry(31, "note", "Remote new", 3, ""), jEntry(32, "note", "Remote only", 3, "")}
	path := checkJournal(t, a, b, id)
	// Hardcover changes, then a local edit lands, before the reader applies.
	f.entries[0].Entry = "Remote newer still"
	rr := applyJournal(t, a, b, path, url.Values{"act_r31": {"remote"}, "act_r32": {"import"}})
	if !strings.Contains(rr.Body.String(), "changed on Hardcover after the preview") {
		t.Fatal("applied over a remote change")
	}
	if x, _ := getNote(a.store.DB, linked); x.Body != "Base" {
		t.Fatal("local text changed")
	}
	path = checkJournal(t, a, b, id)
	if _, err := a.store.DB.Exec("UPDATE book_notes SET body='Edited meanwhile',version=version+1 WHERE id=?", linked); err != nil {
		t.Fatal(err)
	}
	rr = applyJournal(t, a, b, path, url.Values{"act_r31": {"remote"}})
	if !strings.Contains(rr.Body.String(), "local entry changed after the preview") {
		t.Fatal("overwrote a local edit made after the preview")
	}
	if x, _ := getNote(a.store.DB, linked); x.Body != "Edited meanwhile" {
		t.Fatal("local edit lost")
	}
}

func TestJournalIdenticalTextIsNeverIdentity(t *testing.T) {
	a, b, f, id := journalApp(t)
	first := localNote(t, a, id, "quote", "Same words", "", nil, 0, 0, "")
	second := localNote(t, a, id, "quote", "Same words", "", nil, 0, 0, "")
	f.entries = []hcJournal{jEntry(41, "quote", "Same words", 3, "")}
	path := checkJournal(t, a, b, id)
	html := b.request("GET", path, nil).Body.String()
	if !strings.Contains(html, fmt.Sprintf(`value="link:%d"`, first)) || !strings.Contains(html, fmt.Sprintf(`value="link:%d"`, second)) {
		t.Fatal("both identical entries should be offered")
	}
	// Leaving it alone links nothing; linking names exactly one entry.
	expect(t, applyJournal(t, a, b, path, url.Values{}), 200)
	for _, n := range notesOf(t, a, id) {
		if n.Linked() {
			t.Fatal("text equality linked an entry")
		}
	}
	path = checkJournal(t, a, b, id)
	expect(t, applyJournal(t, a, b, path, url.Values{"act_r41": {fmt.Sprintf("link:%d", second)}}), 200)
	if x, _ := getNote(a.store.DB, first); x.Linked() {
		t.Fatal("the other identical entry was linked")
	}
	if x, _ := getNote(a.store.DB, second); x.HardcoverJournalID != 41 || x.BasePrivacy != 3 {
		t.Fatalf("%+v", x)
	}
	if f.writes != 0 || len(notesOf(t, a, id)) != 2 {
		t.Fatal("linking wrote or added a copy")
	}
	// A crafted choice naming an entry that was not offered is refused.
	f.entries = append(f.entries, jEntry(42, "quote", "Same words", 3, ""))
	path = checkJournal(t, a, b, id)
	rr := applyJournal(t, a, b, path, url.Values{"act_r42": {"link:99999"}})
	if !strings.Contains(rr.Body.String(), "not one of the entries offered") {
		t.Fatal("accepted a link to an entry that was not offered")
	}
}

func TestJournalUnsupportedEntriesAreExplainedNotCoerced(t *testing.T) {
	a, b, f, id := journalApp(t)
	f.entries = []hcJournal{
		jEntry(51, "quote", strings.Repeat("x", maxQuote+1), 3, ""),
		jEntry(52, "quote", "Bad page", 3, `{"page": "abc"}`),
		jEntry(53, "note", "Odd audience", 9, ""),
		jEntry(54, "note", "   ", 3, ""),
		jEntry(55, "note", "Fractional page", 3, `{"page": 3.5}`),
		jEntry(56, "note", "Fine", 3, `{}`),
	}
	path := checkJournal(t, a, b, id)
	html := b.request("GET", path, nil).Body.String()
	for _, want := range []string{"longer than the 5000 characters", "page number is not a usable", "audience setting is not one", "It has no text."} {
		if !strings.Contains(html, want) {
			t.Errorf("missing explanation %q", want)
		}
	}
	rr := applyJournal(t, a, b, path, url.Values{"act_r51": {"import"}, "act_r52": {"import"}, "act_r56": {"import"}})
	expect(t, rr, 200)
	notes := notesOf(t, a, id)
	if len(notes) != 1 || notes[0].Body != "Fine" {
		t.Fatalf("%+v", notes)
	}
}

func TestJournalCheckNeedsLinkAndSwitch(t *testing.T) {
	a, b, f, id := journalApp(t)
	if _, err := a.store.DB.Exec("DELETE FROM book_hardcover"); err != nil {
		t.Fatal(err)
	}
	refill(a)
	b.request("GET", bookPath(id), nil)
	expect(t, b.form(fmt.Sprintf("/books/%d/journal/check", id), nil), 400)
	if f.calls != 0 {
		t.Fatal("an unlinked book contacted Hardcover")
	}
	// Account switch: a plan from another account is refused.
	if _, err := a.store.DB.Exec("INSERT INTO book_hardcover(book_id,hardcover_book_id,linked_at) VALUES(?,500,'2026-01-01T00:00:00Z')", id); err != nil {
		t.Fatal(err)
	}
	f.entries = []hcJournal{jEntry(61, "note", "Mine", 3, "")}
	path := checkJournal(t, a, b, id)
	if _, err := a.store.DB.Exec("UPDATE hardcover_link SET hardcover_user_id=999"); err != nil {
		t.Fatal(err)
	}
	expect(t, applyJournal(t, a, b, path, url.Values{"act_r61": {"import"}}), 409)
	if len(notesOf(t, a, id)) != 0 {
		t.Fatal("applied after an account switch")
	}
}

func TestJournalBaselineExportRestoreAndForget(t *testing.T) {
	a, _, f, id := journalApp(t)
	linked := localNote(t, a, id, "quote", "Kept", "mine", 7, 77, 2, "Kept")
	if _, err := a.store.DB.Exec("UPDATE book_notes SET origin='hardcover' WHERE id=?", linked); err != nil {
		t.Fatal(err)
	}
	raw := exportOf(t, a)
	target, tb := noteApp(t)
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, raw)
	expect(t, rr, 303)
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	notes, _ := loadAllNotes(target.store.DB)
	if len(notes) != 1 || notes[0].Origin != "hardcover" || notes[0].BasePrivacy != 2 || notes[0].BaseEntry != "Kept" || notes[0].BasePage == nil || *notes[0].BasePage != 7 || notes[0].Body != "mine" {
		t.Fatalf("%+v", notes)
	}
	// A base without a link, a bad privacy, or an unknown origin is refused.
	for name, edit := range map[string]func(m map[string]any){
		"base without link": func(m map[string]any) {
			m["hardcover_journal_id"], m["hardcover_user_id"], m["hardcover_book_id"] = 0, 0, 0
		},
		"bad privacy": func(m map[string]any) { m["hardcover_base_privacy"] = 9 },
		"bad origin":  func(m map[string]any) { m["origin"] = "elsewhere" },
	} {
		var doc map[string]any
		json.Unmarshal(raw, &doc)
		edit(doc["notes"].([]any)[0].(map[string]any))
		body, _ := json.Marshal(doc)
		_, tb2 := noteApp(t)
		expect(t, tb2.request("GET", "/imports", nil), 200)
		if rr := uploadRestore(tb2, body); rr.Code != 400 {
			t.Fatalf("%s: %d", name, rr.Code)
		}
	}
	if err := forgetIDs(a.store.DB); err != nil {
		t.Fatal(err)
	}
	x, _ := getNote(a.store.DB, linked)
	if x.Linked() || x.BaseEntry != "" || x.BasePrivacy != 0 || x.BasePage != nil || x.SyncedAt != "" || x.Body != "mine" || x.Quote != "Kept" {
		t.Fatalf("forget left sync metadata or lost writing: %+v", x)
	}
	if f.writes != 0 {
		t.Fatal("forget wrote to Hardcover")
	}
}
