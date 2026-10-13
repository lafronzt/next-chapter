package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// listView is one tab of the book list. views is the only list of tabs; the
// index handler and templates both use it.
type listView struct {
	Key, Label, Status, Heading, Intro string
}

const defaultIntro = "Your list should follow your curiosity, not the date you added a book."

var views = []listView{
	{"queue", "Up next", "to-read", "A good book. Then another.", defaultIntro},
	{"reading", "Reading", "reading", "In good company.", defaultIntro},
	{"read", "Finished", "read", "Chapters completed.", defaultIntro},
	{"unreviewed", "To review", "unreviewed", "Borrowed, but not yet decided.", "Library loans arrive here so borrowing never counts as wanting to read. Tell us what each one means to you."},
	{"abandoned", "Set aside", "abandoned", "Not every book is for now.", defaultIntro},
	{"archived", "Archived", "archived", "Kept for the record.", defaultIntro},
	{"all", "All books", "", "Your books, together.", defaultIntro},
}

func findView(key string) listView {
	for _, v := range views {
		if v.Key == key {
			return v
		}
	}
	return views[0]
}
func searchQuery(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// matchesQuery is the list search: a lowercase substring of title, author, or tags.
func matchesQuery(b Book, query string) bool {
	return query == "" || strings.Contains(strings.ToLower(b.Title+" "+b.Author+" "+b.Tags), query)
}

// shows reports whether a book belongs in this view for a normalized query.
func (v listView) shows(b Book, query string) bool {
	return (v.Status == "" || b.Status == v.Status) && matchesQuery(b, query)
}
func (a *App) index(w http.ResponseWriter, r *http.Request) error {
	all, err := loadBooks(a.store.DB)
	if err != nil {
		return err
	}
	queue := queueOnly(all)
	view := findView(r.URL.Query().Get("view"))
	query := searchQuery(r.URL.Query().Get("q"))
	selected := []Book{}
	positions := map[int64]int{}
	pending, active := 0, 0
	for i, b := range queue {
		positions[b.ID] = i + 1
		if b.NeedsPlacement {
			pending++
		}
	}
	for _, b := range all {
		for _, l := range b.Loans {
			if l.Status == "borrowed" || l.Status == "ready" {
				active++
				break
			}
		}
		if view.shows(b, query) {
			selected = append(selected, b)
		}
	}
	var undoRev int64
	err = a.store.DB.QueryRow("SELECT revision FROM undo_queue WHERE id=1").Scan(&undoRev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	canUndo := err == nil && undoRev == state(r).Revision
	return a.render(w, r, "index.html", page{"Items": selected, "AllBooks": all, "Queue": queue, "View": view.Key, "ViewInfo": view, "Views": views, "Query": query, "Positions": positions, "Pending": pending, "ActiveLoans": active, "CanUndo": canUndo}, 200)
}
func (a *App) newBook(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodGet {
		return a.render(w, r, "book.html", page{"Book": Book{Interest: 3, Status: "to-read"}}, 200)
	}
	b, err := bookFields(r)
	if err != nil {
		return err
	}
	location := "/?notice=saved"
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		id, err := findBook(tx, Record{Book: b})
		if err != nil {
			return err
		}
		if id != 0 {
			return conflict("This book is already in your library. Edit its existing entry.")
		}
		// Adding a deleted book again by hand is an explicit choice that wins.
		if err = clearTombstones(tx, Record{Book: b}); err != nil {
			return err
		}
		if _, err = importRecords(tx, []Record{{Book: b}}); err != nil {
			return err
		}
		id, err = findBook(tx, Record{Book: b})
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE books SET interest=? WHERE id=?", b.Interest, id); err != nil {
			return err
		}
		if err = bump(tx); err != nil {
			return err
		}
		if b.Status == "to-read" {
			plan, err := makeSuggestion(tx, id)
			if err != nil {
				return err
			}
			location = "/plans/" + plan
		}
		return nil
	})
	if err != nil {
		return err
	}
	return redirect(w, r, location)
}
func (a *App) editBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		b, err := bookByID(a.store.DB, id)
		if err != nil {
			return err
		}
		events, err := loadEvents(a.store.DB, id, 500)
		if err != nil {
			return err
		}
		return a.render(w, r, "book.html", page{"Book": b, "Editing": true, "Events": events}, 200)
	}
	b, err := bookFields(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		old, err := bookByID(tx, id)
		if err != nil {
			return err
		}
		// Title and author must be unique among live books only.
		match, _, err := lookupBook(tx, Record{Book: b}, false)
		if err != nil {
			return err
		}
		if match != 0 && match != id {
			return conflict("Another book already has this title and author.")
		}
		pin, needs, position, err := statusChange(tx, old, b.Status)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE books SET title=?,author=?,identity=?,interest=?,reading_status=?,pages=?,tags=?,series=?,series_number=?,pinned=?,needs_placement=?,position=? WHERE id=?`, b.Title, b.Author, identity(b.Title, b.Author), b.Interest, b.Status, b.Pages, b.Tags, b.Series, b.SeriesNumber, pin, needs, position, id)
		if err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved", id))
}
func (a *App) bookAction(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		all, err := loadBooks(tx)
		if err != nil {
			return err
		}
		queue := queueOnly(all)
		idx := -1
		for i, b := range queue {
			if b.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			return bad("Choose a book in your reading queue.")
		}
		b := queue[idx]
		switch r.FormValue("action") {
		case "pin":
			_, err = tx.Exec("UPDATE books SET pinned=? WHERE id=?", !b.Pinned, id)
		case "placed":
			_, err = tx.Exec("UPDATE books SET needs_placement=0 WHERE id=?", id)
		case "up", "down":
			target := idx - 1
			if r.FormValue("action") == "down" {
				target = idx + 1
			}
			if target < 0 || target >= len(queue) || b.Pinned || queue[target].Pinned {
				return bad("This move would cross a pinned book or the end of the queue.")
			}
			queue[idx], queue[target] = queue[target], queue[idx]
			ids := []int64{}
			for _, b := range queue {
				ids = append(ids, b.ID)
			}
			err = reorder(tx, ids)
		default:
			return bad("Choose a valid action.")
		}
		if err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/")
}
func (a *App) loan(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	source, format, status, due := r.FormValue("source"), r.FormValue("format"), r.FormValue("status"), r.FormValue("due_date")
	if !in(source, []string{"nypl", "libby"}) || !in(format, []string{"print", "ebook", "audiobook", "digital"}) || !in(status, libraryStates) {
		return bad("Choose a library source, format, and activity.")
	}
	if due != "" {
		if _, err = time.Parse(time.DateOnly, due); err != nil {
			return bad("Enter a valid due date.")
		}
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		b, err := bookByID(tx, id)
		if err != nil {
			return err
		}
		b.Source = source
		event := &Event{Source: source, Format: format, Status: status, Activity: status, At: canonicalTime(time.Now()), DueDate: due}
		if _, err = importRecords(tx, []Record{{Book: b, Event: event}}); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=loan", id))
}
func bookByID(q queryer, id int64) (Book, error) {
	all, err := loadBooks(q)
	if err != nil {
		return Book{}, err
	}
	for _, b := range all {
		if b.ID == id {
			return b, nil
		}
	}
	return Book{}, sql.ErrNoRows
}
