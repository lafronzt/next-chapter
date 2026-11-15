package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
)

const (
	// maxReason and maxNote bound the reader's private writing, in characters.
	maxReason = 500
	maxNote   = 5000
	// maxNotesPerBook keeps one book's page and a restore bounded.
	maxNotesPerBook  = 200
	maxRestoreNotes  = 50_000
	reasonMergeLabel = "Why I wanted to read this (from a merged entry)"
)

// bookNote is one dated private note. It is plain text; templates escape it.
// Page and Percent say where in the book it was written (at most one is set)
// and never change reading progress.
type bookNote struct {
	ID        int64  `json:"id"`
	BookID    int64  `json:"book_id"`
	Body      string `json:"body"`
	Page      *int   `json:"page,omitempty"`
	Percent   *int   `json:"percent,omitempty"`
	Version   int    `json:"-"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Edited reports whether the note was changed after it was written.
func (n bookNote) Edited() bool { return n.UpdatedAt != n.CreatedAt }

// Where describes the place in the book a note was written at.
func (n bookNote) Where() string {
	switch {
	case n.Page != nil:
		return fmt.Sprintf("page %d", *n.Page)
	case n.Percent != nil:
		return fmt.Sprintf("%d%%", *n.Percent)
	}
	return ""
}

// noteText normalizes the reader's writing without altering its content:
// line endings become \n, control characters other than newline and tab are
// dropped, and surrounding space is trimmed. Markup is kept as typed, because
// templates escape it; the text is never interpreted.
func noteText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	return strings.TrimSpace(s)
}

// checkNote returns why a note's fields are out of bounds, or "".
func checkNote(body string, page, percent *int) string {
	switch {
	case body == "":
		return "Write something in the note, or delete it."
	case len([]rune(body)) > maxNote:
		return fmt.Sprintf("Keep a note under %d characters.", maxNote)
	case page != nil && percent != nil:
		return "Give a page or a percentage for a note, not both."
	case page != nil && (*page < 1 || *page > maxProgressPage):
		return fmt.Sprintf("Enter a page from 1 to %d, or leave it blank.", maxProgressPage)
	case percent != nil && (*percent < 0 || *percent > 100):
		return "Enter a percentage from 0 to 100, or leave it blank."
	}
	return ""
}

// noteFields reads a note from a form.
func noteFields(r *http.Request) (body string, page, percent *int, err error) {
	body = noteText(r.FormValue("body"))
	for _, f := range []struct {
		name string
		dest **int
	}{{"page", &page}, {"percent", &percent}} {
		raw := strings.TrimSpace(r.FormValue(f.name))
		if raw == "" {
			continue
		}
		n, e := strconv.Atoi(raw)
		if e != nil {
			return "", nil, nil, bad("Enter the page or percentage as a whole number, or leave it blank.")
		}
		*f.dest = &n
	}
	if message := checkNote(body, page, percent); message != "" {
		return "", nil, nil, bad(message)
	}
	return body, page, percent, nil
}

func loadNotes(q queryer, bookID int64) ([]bookNote, error) {
	rows, err := q.Query("SELECT id,book_id,body,page,percent,version,created_at,updated_at FROM book_notes WHERE book_id=? ORDER BY created_at DESC,id DESC", bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []bookNote{}
	for rows.Next() {
		var n bookNote
		if err = rows.Scan(&n.ID, &n.BookID, &n.Body, &n.Page, &n.Percent, &n.Version, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

// loadAllNotes returns every note for an export, oldest first.
func loadAllNotes(q queryer) ([]bookNote, error) {
	rows, err := q.Query("SELECT id,book_id,body,page,percent,version,created_at,updated_at FROM book_notes ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []bookNote{}
	for rows.Next() {
		var n bookNote
		if err = rows.Scan(&n.ID, &n.BookID, &n.Body, &n.Page, &n.Percent, &n.Version, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

func countNotes(q queryer, bookID int64) (int, error) {
	var n int
	err := q.QueryRow("SELECT count(*) FROM book_notes WHERE book_id=?", bookID).Scan(&n)
	return n, err
}

// addNote saves a new note for a book of any status. Like progress, it changes
// only the note: no revision check or bump, so it never invalidates a preview
// or Undo, and an Undo never touches notes.
func (a *App) addNote(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	body, page, percent, err := noteFields(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if _, err := getBook(tx, id); err != nil {
			return err
		}
		n, err := countNotes(tx, id)
		if err != nil {
			return err
		}
		if n >= maxNotesPerBook {
			return bad(fmt.Sprintf("A book can have at most %d notes. Delete or combine some first.", maxNotesPerBook))
		}
		now := canonicalTime(a.now())
		_, err = tx.Exec("INSERT INTO book_notes(book_id,body,page,percent,created_at,updated_at) VALUES(?,?,?,?,?,?)", id, body, page, percent, now, now)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved#notes", id))
}

// editNote saves or deletes a note. The form carries the note's version, and a
// save or delete from a page older than the stored note is refused.
func (a *App) editNote(w http.ResponseWriter, r *http.Request) error {
	nid, err := strconv.ParseInt(r.PathValue("nid"), 10, 64)
	if err != nil || nid < 1 {
		return bad("Choose a valid note.")
	}
	version, err := strconv.Atoi(r.FormValue("version"))
	if err != nil {
		return bad("Reload the page and try again.")
	}
	action := r.FormValue("action")
	var body string
	var page, percent *int
	switch action {
	case "save":
		if body, page, percent, err = noteFields(r); err != nil {
			return err
		}
	case "delete":
	default:
		return bad("Choose a valid action.")
	}
	var bookID int64
	err = a.store.Write(func(tx *sql.Tx) error {
		var current int
		err := tx.QueryRow("SELECT book_id,version FROM book_notes WHERE id=?", nid).Scan(&bookID, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return conflict("This note was already deleted. Reload the page.")
		}
		if err != nil {
			return err
		}
		if current != version {
			return conflict("This note changed since the page loaded. Reload it before editing.")
		}
		if action == "delete" {
			_, err = tx.Exec("DELETE FROM book_notes WHERE id=?", nid)
			return err
		}
		_, err = tx.Exec("UPDATE book_notes SET body=?,page=?,percent=?,version=version+1,updated_at=? WHERE id=?", body, page, percent, canonicalTime(a.now()), nid)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved#notes", bookID))
}
