package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *App) newBook(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodGet {
		return a.render(w, r, "book.html", page{"Title": "Add a book", "Book": Book{Interest: 3, Status: "to-read"}}, 200)
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
		// bookFields validated Interest, so the new book takes it directly.
		if _, err = importRecords(tx, []Record{{Book: b}}); err != nil {
			return err
		}
		id, err = findBook(tx, Record{Book: b})
		if err != nil {
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

// bookEventLimit caps the borrowing history on a book's page; the page says
// when there are more, and the export has them all.
const bookEventLimit = 200

// bookPageData loads what a book's pages show: the book, its readings,
// loans, events, and estimate, in a fixed handful of queries.
func (a *App) bookPageData(id int64) (page, error) {
	b, err := getBook(a.store.DB, id)
	if err != nil {
		return nil, err
	}
	events, err := loadEvents(a.store.DB, id, bookEventLimit)
	if err != nil {
		return nil, err
	}
	total, err := countEvents(a.store.DB, eventFilter{BookID: id})
	if err != nil {
		return nil, err
	}
	readings, err := bookReadings(a.store.DB, id)
	if err != nil {
		return nil, err
	}
	estimate, err := readEstimate(a.store.DB, id)
	if err != nil {
		return nil, err
	}
	lookupOn, err := lookupEnabled(a.store.DB)
	if err != nil {
		return nil, err
	}
	data := page{"Title": b.Title, "Book": b, "LookupOn": lookupOn, "Editing": true, "Events": events, "EventTotal": total, "Readings": readings, "Estimate": estimate}
	for _, e := range readings {
		if e.Rating != nil {
			data["Rated"] = true
		}
	}
	if estimate != nil && data["Rated"] != nil {
		if data["Accuracy"], err = loadAccuracy(a.store.DB, id); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// viewBook is the read-only page for a book: what it is, where it stands,
// the one obvious next step, and its readings, library activity, and estimate.
func (a *App) viewBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	data, err := a.bookPageData(id)
	if err != nil {
		return err
	}
	return a.render(w, r, "book_view.html", data, 200)
}

// editBook shows and saves the edit form, with the forms for readings and
// library events beneath it.
func (a *App) editBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		data, err := a.bookPageData(id)
		if err != nil {
			return err
		}
		return a.render(w, r, "book.html", data, 200)
	}
	b, err := bookFields(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		old, err := getBook(tx, id)
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
		_, err = tx.Exec(`UPDATE books SET title=?,author=?,identity=?,interest=?,reading_status=?,pages=?,tags=?,series=?,series_number=?,description_source=CASE WHEN description=? THEN description_source ELSE '' END,description=?,pinned=?,needs_placement=?,position=? WHERE id=?`, b.Title, b.Author, identity(b.Title, b.Author), b.Interest, b.Status, b.Pages, b.Tags, b.Series, b.SeriesNumber, b.Description, b.Description, pin, needs, position, id)
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
	// What the reader sees afterwards: the action key and any number. An
	// empty key means nothing changed, so there is nothing to say or undo.
	noticeKey, noticeArg := "", 0
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		queue, err := queueOrder(tx)
		if err != nil {
			return err
		}
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
		// Capture what this action can touch before it runs: the whole queue
		// for a move (positions shift), just this book otherwise.
		var before []bookState
		snapshot := func(all bool) error {
			var err error
			if all {
				before, err = snapshotBooks(tx, "reading_status='to-read'")
			} else {
				before, err = snapshotBooks(tx, "id=?", id)
			}
			return err
		}
		action := r.FormValue("action")
		switch action {
		case "pin":
			if err = snapshot(false); err != nil {
				return err
			}
			_, err = tx.Exec("UPDATE books SET pinned=? WHERE id=?", !b.Pinned, id)
			noticeKey = map[bool]string{true: "unpin", false: "pin"}[b.Pinned]
		case "placed":
			if err = snapshot(false); err != nil {
				return err
			}
			_, err = tx.Exec("UPDATE books SET needs_placement=0 WHERE id=?", id)
			noticeKey = "placed"
		case "up", "down":
			target := idx - 1
			if action == "down" {
				target = idx + 1
			}
			if target < 0 || target >= len(queue) || b.Pinned || queue[target].Pinned {
				return bad("This move would cross a pinned book or the end of the queue.")
			}
			if err = snapshot(true); err != nil {
				return err
			}
			if err = reorder(tx, moveEntry(queue, idx, target)); err != nil {
				return err
			}
			_, err = tx.Exec("UPDATE books SET moved_by_hand=1 WHERE id=?", id)
			noticeKey = action
		case "move":
			n, parseErr := strconv.Atoi(strings.TrimSpace(r.FormValue("position")))
			if parseErr != nil || n < 1 || n > len(queue) {
				return bad(fmt.Sprintf("Enter a queue position from 1 to %d.", len(queue)))
			}
			target := n - 1
			if target == idx {
				return nil
			}
			if b.Pinned {
				return bad("Pinned books stay in place. Unpin it to move it.")
			}
			lo, hi := min(idx, target), max(idx, target)
			for i := lo; i <= hi; i++ {
				if queue[i].Pinned {
					return bad(fmt.Sprintf("This move would cross the pinned book at #%d. Unpin it or choose a position on this side of it.", i+1))
				}
			}
			if err = snapshot(true); err != nil {
				return err
			}
			if err = reorder(tx, moveEntry(queue, idx, target)); err != nil {
				return err
			}
			_, err = tx.Exec("UPDATE books SET moved_by_hand=1 WHERE id=?", id)
			noticeKey, noticeArg = "move", n
		default:
			return bad("Choose a valid action.")
		}
		if err != nil {
			return err
		}
		if err = bump(tx); err != nil {
			return err
		}
		var title string
		if err = tx.QueryRow("SELECT title FROM books WHERE id=?", id).Scan(&title); err != nil {
			return err
		}
		return recordUndo(tx, noticeText(noticeKey, title, noticeArg), before)
	})
	if err != nil {
		return err
	}
	dest := listReturn(r)
	if noticeKey != "" {
		dest = a.withNotice(dest, noticeKey, id, noticeArg)
	}
	// Land on the row that changed, without scrolling from the top.
	return redirect(w, r, fmt.Sprintf("%s#book-%d", dest, id))
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
		b, err := getBook(tx, id)
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
