package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Note sharing is deliberately separate from status/progress sync. No note
// body leaves the server until its own confirmation is submitted.
type hcNotePlan struct {
	NoteID, BookID, UserID, HardcoverBookID int64
	Version                                 int
	Title, Entry                            string
	JournalID                               int64
	RemoteEntry                             string
	RemotePrivacy                           int
}

type hcJournal struct {
	ID      flexInt `json:"id"`
	Entry   string  `json:"entry"`
	Privacy int     `json:"privacy_setting_id"`
}

const hcJournalQuery = `query NextChapterNoteJournal($uid: Int!, $book: Int!, $id: bigint!) {
 reading_journals(where: {user_id: {_eq: $uid}, book_id: {_eq: $book}, id: {_eq: $id}, event: {_eq: "note"}}, limit: 1) {
 id entry privacy_setting_id
 }
}`

// Only called after confirmation: the entry text in the filter is shared data.
const hcNoteMatchQuery = `query NextChapterNoteMatch($uid: Int!, $book: Int!, $entry: String!) {
 reading_journals(where: {user_id: {_eq: $uid}, book_id: {_eq: $book}, entry: {_eq: $entry}, event: {_eq: "note"}}, limit: 2) {
 id entry privacy_setting_id
 }
}`

// Shapes verified against Hardcover's Updating Reading Journal guide, Oct 2026.
// Use field variables instead of depending on a generated input type name.
const hcInsertNoteMutation = `mutation NextChapterInsertNote($book: Int!, $entry: String!, $privacy: Int!) {
 insert_reading_journal(object: {book_id: $book, event: "note", entry: $entry, privacy_setting_id: $privacy, tags: []}) {
 errors reading_journal { id entry privacy_setting_id }
 }
}`
const hcUpdateNoteMutation = `mutation NextChapterUpdateNote($id: Int!, $entry: String!, $privacy: Int!) {
 update_reading_journal(id: $id, object: {entry: $entry, privacy_setting_id: $privacy}) {
 errors reading_journal { id entry privacy_setting_id }
 }
}`

func (c *hardcoverClient) noteJournal(ctx context.Context, p hcNotePlan, match bool) ([]hcJournal, error) {
	vars := map[string]any{"uid": p.UserID, "book": p.HardcoverBookID}
	query := hcJournalQuery
	if match {
		query, vars["entry"] = hcNoteMatchQuery, p.Entry
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
	err := q.QueryRow(`SELECT id,book_id,body,page,percent,version,created_at,updated_at,hardcover_journal_id,hardcover_user_id,hardcover_book_id FROM book_notes WHERE id=?`, id).Scan(&n.ID, &n.BookID, &n.Body, &n.Page, &n.Percent, &n.Version, &n.CreatedAt, &n.UpdatedAt, &n.HardcoverJournalID, &n.HardcoverUserID, &n.HardcoverBookID)
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
	p := hcNotePlan{NoteID: id, BookID: n.BookID, UserID: acct.UserID, HardcoverBookID: link.HardcoverBookID, Version: n.Version, Title: b.Title, Entry: n.Body}
	if where := n.Where(); where != "" {
		p.Entry += "\n\n(" + where + ")"
	}
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
		p.RemoteEntry, p.RemotePrivacy = entries[0].Entry, entries[0].Privacy
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
		return bad("Choose who can see this note and confirm sharing.")
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
		if len(entries) != 1 || entries[0].Entry != p.RemoteEntry || entries[0].Privacy != p.RemotePrivacy {
			return conflict("The note on Hardcover changed. Preview sharing again before replacing it.")
		}
	} else if len(entries) > 1 {
		return conflict("Hardcover has multiple notes with this text. Check them there before sharing again.")
	}
	journalID := p.JournalID
	// Recover a successful send whose response was lost; never add another copy.
	if len(entries) == 1 {
		journalID = int64(entries[0].ID)
	}
	if len(entries) != 1 || entries[0].Entry != p.Entry || entries[0].Privacy != privacy {
		query := hcInsertNoteMutation
		vars := map[string]any{"book": p.HardcoverBookID, "entry": p.Entry, "privacy": privacy}
		if journalID > 0 {
			query = hcUpdateNoteMutation
			delete(vars, "book")
			vars["id"] = journalID
		}
		var out struct {
			Insert hcNoteResult `json:"insert_reading_journal"`
			Update hcNoteResult `json:"update_reading_journal"`
		}
		if err := a.hardcover.do(r.Context(), query, vars, &out); err != nil {
			if !definiteRefusal(err) {
				return httpError{Status: http.StatusBadGateway, Message: "Hardcover did not confirm the note. It may have saved it. Share again to check for an existing copy before retrying; your local note is safe."}
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
		if _, err := tx.Exec("UPDATE book_notes SET hardcover_journal_id=?,hardcover_user_id=?,hardcover_book_id=? WHERE id=? AND book_id=?", journalID, p.UserID, p.HardcoverBookID, p.NoteID, p.BookID); err != nil {
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
