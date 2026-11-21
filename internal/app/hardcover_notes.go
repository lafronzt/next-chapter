package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Note and quote sharing is deliberately separate from status/progress sync.
// No writing leaves the server until its own confirmation is submitted.
type hcNotePlan struct {
	NoteID, BookID, UserID, HardcoverBookID int64
	Version                                 int
	Title, Entry, Event                     string
	// Page goes in the entry's metadata; only a quote has one.
	Page *int
	// CommentaryStays and PercentStays say what was left out of the payload.
	CommentaryStays, PercentStays bool
	JournalID                     int64
	RemoteEntry                   string
	RemotePrivacy                 int
	RemotePage                    *int
	RemoteMetadata                string
}

// hcJournal is one reading_journals row. Metadata is free-form JSON; the only
// key used is page (Hardcover's documented example for a quote).
type hcJournal struct {
	ID        flexInt         `json:"id"`
	Event     string          `json:"event,omitempty"`
	Entry     string          `json:"entry"`
	Privacy   int             `json:"privacy_setting_id"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt string          `json:"created_at,omitempty"`
}

// page reads the page from the entry's metadata. ok is false when a page is
// present but is not a whole number in range, so it cannot be used as a
// location.
func (j hcJournal) page() (page *int, ok bool) {
	var m map[string]json.RawMessage
	if len(j.Metadata) == 0 || json.Unmarshal(j.Metadata, &m) != nil {
		return nil, true
	}
	raw, present := m["page"]
	if !present || string(raw) == "null" {
		return nil, true
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || f != math.Trunc(f) || f < 1 || f > maxProgressPage {
		return nil, false
	}
	n := int(f)
	return &n, true
}

const hcJournalQuery = `query NextChapterNoteJournal($uid: Int!, $book: Int!, $id: bigint!) {
 reading_journals(where: {user_id: {_eq: $uid}, book_id: {_eq: $book}, id: {_eq: $id}}, limit: 1) {
 id event entry privacy_setting_id metadata
 }
}`

// Only called after confirmation: the entry text in the filter is shared data.
const hcNoteMatchQuery = `query NextChapterNoteMatch($uid: Int!, $book: Int!, $entry: String!, $event: String!) {
 reading_journals(where: {user_id: {_eq: $uid}, book_id: {_eq: $book}, entry: {_eq: $entry}, event: {_eq: $event}}, limit: 2) {
 id event entry privacy_setting_id metadata
 }
}`

// Shapes verified against Hardcover's Updating Reading Journal guide (the docs
// repository source, read 2026-11-21): event is "note" or "quote", privacy 1
// everyone, 2 followers, 3 only me, and metadata is free-form (the SDK shows
// {"page": 212} for a quote). Field variables avoid a generated type name.
func hcInsertJournalMutation(withMetadata bool) string {
	meta, decl := "", ""
	if withMetadata {
		meta, decl = ", metadata: $metadata", ", $metadata: jsonb"
	}
	return `mutation NextChapterInsertNote($book: Int!, $event: String!, $entry: String!, $privacy: Int!` + decl + `) {
 insert_reading_journal(object: {book_id: $book, event: $event, entry: $entry, privacy_setting_id: $privacy, tags: []` + meta + `}) {
 errors reading_journal { id event entry privacy_setting_id metadata }
 }
}`
}
func hcUpdateJournalMutation(withMetadata bool) string {
	meta, decl := "", ""
	if withMetadata {
		meta, decl = ", metadata: $metadata", ", $metadata: jsonb"
	}
	return `mutation NextChapterUpdateNote($id: Int!, $entry: String!, $privacy: Int!` + decl + `) {
 update_reading_journal(id: $id, object: {entry: $entry, privacy_setting_id: $privacy` + meta + `}) {
 errors reading_journal { id event entry privacy_setting_id metadata }
 }
}`
}

// pageMetadata is the metadata to send for a page: the remote object with only
// its page key set, so other keys Hardcover holds survive an update.
func pageMetadata(remote string, page *int) map[string]any {
	if page == nil {
		return nil
	}
	out := map[string]any{}
	_ = json.Unmarshal([]byte(remote), &out)
	if out == nil {
		out = map[string]any{}
	}
	out["page"] = *page
	return out
}

func (c *hardcoverClient) noteJournal(ctx context.Context, p hcNotePlan, match bool) ([]hcJournal, error) {
	vars := map[string]any{"uid": p.UserID, "book": p.HardcoverBookID}
	query := hcJournalQuery
	if match {
		query, vars["entry"], vars["event"] = hcNoteMatchQuery, p.Entry, p.Event
	} else {
		vars["id"] = p.JournalID
	}
	var out struct {
		Entries []hcJournal `json:"reading_journals"`
	}
	err := c.do(ctx, query, vars, &out)
	return out.Entries, err
}

func getNote(q queryer, id int64) (bookNote, error) {
	var n bookNote
	err := q.QueryRow("SELECT "+noteColumns+" FROM book_notes WHERE id=?", id).Scan(n.scan()...)
	return n, err
}

func (a *App) hardcoverNotePreview(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	id, err := strconv.ParseInt(r.PathValue("nid"), 10, 64)
	if err != nil || id < 1 {
		return bad("Choose a valid note.")
	}
	n, err := getNote(a.store.DB, id)
	if err != nil {
		return err
	}
	if r.FormValue("version") != strconv.Itoa(n.Version) {
		return conflict("This note changed. Reload it before sharing.")
	}
	b, err := getBook(a.store.DB, n.BookID)
	if err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, n.BookID)
	if err != nil {
		return err
	}
	acct, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	if link == nil || acct == nil {
		return bad("Link this book to Hardcover and test the connection first.")
	}
	p := hcNotePlan{NoteID: id, BookID: n.BookID, UserID: acct.UserID, HardcoverBookID: link.HardcoverBookID, Version: n.Version, Title: b.Title,
		Entry: n.PayloadEntry(), Event: n.Event(), Page: n.PayloadPage(),
		CommentaryStays: n.IsQuote() && n.Body != "", PercentStays: n.IsQuote() && n.Percent != nil}
	if n.HardcoverUserID == p.UserID && n.HardcoverBookID == p.HardcoverBookID {
		p.JournalID = n.HardcoverJournalID
	}
	if p.JournalID > 0 {
		a.hardcoverMu.Lock()
		entries, err := a.hardcover.noteJournal(r.Context(), p, false)
		a.hardcoverMu.Unlock()
		if err != nil {
			return hardcoverHTTPError(err)
		}
		if len(entries) != 1 {
			return conflict("The previously shared note is missing from Hardcover. Nothing was sent.")
		}
		p.RemoteEntry, p.RemotePrivacy, p.RemoteMetadata = entries[0].Entry, entries[0].Privacy, string(entries[0].Metadata)
		p.RemotePage, _ = entries[0].page()
	}
	var planID string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		planID, err = savePlan(tx, "hc-note", Plan{HardcoverNote: &p})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/notes/"+planID)
}

func (a *App) hardcoverNotePlan(w http.ResponseWriter, r *http.Request) error {
	planID := r.PathValue("id")
	kind, plan, _, err := readPlan(a.store.DB, planID)
	if err != nil {
		return err
	}
	if kind != "hc-note" || plan.HardcoverNote == nil {
		return sql.ErrNoRows
	}
	p := *plan.HardcoverNote
	if r.Method == http.MethodGet {
		return a.render(w, r, "hardcover_note.html", page{"Title": "Share a note", "Nav": "hardcover", "PlanID": planID, "Plan": p}, 200)
	}
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	privacy, err := strconv.Atoi(r.FormValue("privacy"))
	if err != nil || privacy < 1 || privacy > 3 || r.FormValue("confirm") != "share" {
		return bad("Choose who can see this entry and confirm sharing.")
	}
	// A smaller number is a wider audience. Never widen one by accident.
	if p.JournalID != 0 && privacy < p.RemotePrivacy && r.FormValue("widen") != "confirm" {
		return bad("That makes the entry visible to more people than it is now. Tick the box to confirm the wider audience, or choose the current one.")
	}
	a.hardcoverMu.Lock()
	defer a.hardcoverMu.Unlock()
	// Re-read inside the lock so a double submission cannot insert twice.
	kind, plan, _, err = readPlan(a.store.DB, planID)
	if err != nil {
		return err
	}
	if kind != "hc-note" || plan.HardcoverNote == nil {
		return sql.ErrNoRows
	}
	n, err := getNote(a.store.DB, p.NoteID)
	if err != nil {
		return err
	}
	acct, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, p.BookID)
	if err != nil {
		return err
	}
	if n.Version != p.Version || n.BookID != p.BookID || acct == nil || acct.UserID != p.UserID || link == nil || link.HardcoverBookID != p.HardcoverBookID {
		return conflict("The note, account, or book link changed. Preview sharing again.")
	}
	entries, err := a.hardcover.noteJournal(r.Context(), p, p.JournalID == 0)
	if err != nil {
		return hardcoverHTTPError(err)
	}
	if p.JournalID != 0 {
		if len(entries) != 1 || entries[0].Entry != p.RemoteEntry || entries[0].Privacy != p.RemotePrivacy || entries[0].Event != p.Event && entries[0].Event != "" {
			return conflict("The note on Hardcover changed. Preview sharing again before replacing it.")
		}
	} else if len(entries) > 1 {
		return conflict("Hardcover has multiple notes with this text. Check them there before sharing again.")
	}
	journalID := p.JournalID
	// Identical text is not proof of identity: another entry may be a different
	// note the reader wrote on purpose. A match is linked only when the reader
	// ticks "link", and linking writes nothing and leaves its privacy alone.
	linked := false
	if p.JournalID == 0 && len(entries) == 1 {
		if r.FormValue("link") != "existing" {
			return conflict("Hardcover already has an entry on this book with exactly this text. Nothing was sent. If it is this one (for example, an earlier send whose answer was lost), submit again with “Link to it” ticked; otherwise change the text first.")
		}
		journalID, linked = int64(entries[0].ID), true
	}
	basePrivacy, basePage, baseEntry := privacy, p.Page, p.Entry
	if linked {
		basePrivacy, baseEntry = entries[0].Privacy, entries[0].Entry
		basePage, _ = entries[0].page()
	} else {
		query, vars := hcInsertJournalMutation(p.Page != nil), map[string]any{"book": p.HardcoverBookID, "event": p.Event, "entry": p.Entry, "privacy": privacy}
		if journalID > 0 {
			query = hcUpdateJournalMutation(p.Page != nil)
			delete(vars, "book")
			delete(vars, "event")
			vars["id"] = journalID
		}
		if meta := pageMetadata(p.RemoteMetadata, p.Page); meta != nil {
			vars["metadata"] = meta
		}
		var out struct {
			Insert hcNoteResult `json:"insert_reading_journal"`
			Update hcNoteResult `json:"update_reading_journal"`
		}
		if err := a.hardcover.do(r.Context(), query, vars, &out); err != nil {
			if !definiteRefusal(err) {
				return httpError{Status: http.StatusBadGateway, Message: "Hardcover did not confirm the entry. It may have saved it. Check Hardcover, then share again: if it is there, tick “Link to it” instead of adding another copy. Your local writing is safe."}
			}
			return hardcoverHTTPError(err)
		}
		result := out.Insert
		if journalID > 0 {
			result = out.Update
		}
		if result.failed() {
			return hardcoverHTTPError(errHardcoverRefused)
		}
		journalID = int64(result.Journal.ID)
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		// Keep the identity of the copy even if a local edit happened during the send.
		if _, err := tx.Exec("UPDATE book_notes SET hardcover_journal_id=?,hardcover_user_id=?,hardcover_book_id=?,hc_base_entry=?,hc_base_page=?,hc_base_privacy=?,hc_synced_at=? WHERE id=? AND book_id=?", journalID, p.UserID, p.HardcoverBookID, baseEntry, basePage, basePrivacy, canonicalTime(a.now()), p.NoteID, p.BookID); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM plans WHERE id=?", planID)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=hc-note-sent#note-%d", p.BookID, p.NoteID))
}

type hcNoteResult struct {
	Errors  json.RawMessage `json:"errors"`
	Journal *hcJournal      `json:"reading_journal"`
}

func (r hcNoteResult) failed() bool {
	e := strings.TrimSpace(string(r.Errors))
	return r.Journal == nil || r.Journal.ID <= 0 || (e != "" && e != "null" && e != `""` && e != "[]" && e != "{}")
}
