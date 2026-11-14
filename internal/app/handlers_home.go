package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// dueSoonDays is how far ahead the home screen looks for loans coming due.
const dueSoonDays = 7

// dueSoon is one borrowed book coming due, for the home screen.
type dueSoon struct {
	Book Book
	Loan Loan
	Days int
}

// DaysText says how long is left, plainly: overdue is a fact, not an alarm.
func (d dueSoon) DaysText() string {
	switch {
	case d.Days < 0:
		return fmt.Sprintf("%d %s past the due date", -d.Days, plural(-d.Days, "day", "days"))
	case d.Days == 0:
		return "due today"
	}
	return fmt.Sprintf("%d %s left", d.Days, plural(d.Days, "day", "days"))
}

// homeData is the first page of the reading queue's extras: the next book,
// what is being read, and what is due back at the library.
type homeData struct {
	Next    *Book
	Reading []Book
	Due     []dueSoon
}

// loadHome reads the home screen with three queries and no per-row ones.
// Each query finishes before the next, because the store has one connection.
func loadHome(q queryer, today time.Time) (homeData, error) {
	var h homeData
	// Next up is the first queued book that is not waiting for placement, so
	// a just-added book never jumps to the top.
	next, err := queryBooks(q, "WHERE b.reading_status='to-read' AND b.needs_placement=0 ORDER BY b.position,b.id LIMIT 1")
	if err != nil {
		return h, err
	}
	if len(next) > 0 {
		h.Next = &next[0]
	}
	if h.Reading, err = queryBooks(q, "WHERE b.reading_status='reading' ORDER BY b.position,b.id"); err != nil {
		return h, err
	}
	limit := today.AddDate(0, 0, dueSoonDays).Format(time.DateOnly)
	rows, err := q.Query("SELECT book_id,source,format,status,due_date,observed_at FROM loans WHERE status='borrowed' AND due_date!='' AND due_date<=? ORDER BY due_date,book_id", limit)
	if err != nil {
		return h, err
	}
	var loans []Loan
	for rows.Next() {
		var l Loan
		if err = rows.Scan(&l.BookID, &l.Source, &l.Format, &l.Status, &l.DueDate, &l.ObservedAt); err != nil {
			rows.Close()
			return h, err
		}
		loans = append(loans, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(loans) == 0 {
		return h, err
	}
	ids := make([]int64, len(loans))
	for i, l := range loans {
		ids[i] = l.BookID
	}
	books, err := getBooks(q, ids)
	if err != nil {
		return h, err
	}
	midnight := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	for _, l := range loans {
		due, err := time.Parse(time.DateOnly, l.DueDate)
		if b, ok := books[l.BookID]; ok && err == nil {
			h.Due = append(h.Due, dueSoon{Book: b, Loan: l, Days: int(due.Sub(midnight).Hours() / 24)})
		}
	}
	return h, nil
}

// bookStatus starts or stops reading a book. It changes only that book's
// status (clearing a pin the way any status change does) and never moves
// another book.
func (a *App) bookStatus(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	action := r.FormValue("action")
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		b, err := getBook(tx, id)
		if err != nil {
			return err
		}
		before, err := snapshotBooks(tx, "id=?", id)
		if err != nil {
			return err
		}
		switch {
		case action == "start" && b.Status == "to-read":
			err = setStatus(tx, b, "reading")
		case action == "stop" && b.Status == "reading":
			err = setStatus(tx, b, "abandoned")
		case action == "requeue" && (b.Status == "abandoned" || b.Status == "archived"):
			err = setStatus(tx, b, "to-read")
		case action == "start" || action == "stop" || action == "requeue":
			return bad("That book is no longer in that state. Reload and try again.")
		default:
			return bad("Choose a valid action.")
		}
		if err != nil {
			return err
		}
		if err = bump(tx); err != nil {
			return err
		}
		return recordUndo(tx, noticeText(action, b.Title, 0), before)
	})
	if err != nil {
		return err
	}
	if r.FormValue("back") == "book" {
		return redirect(w, r, a.withNotice(fmt.Sprintf("/books/%d", id), action, id, 0))
	}
	return redirect(w, r, a.withNotice(listReturn(r), action, id, 0))
}

// finishBook shows and saves the Finished form: a finish date (today by
// default) and a rating. It sets the status to read and logs exactly one
// reading. A double submit fails the revision check instead of logging twice.
func (a *App) finishBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		b, err := getBook(a.store.DB, id)
		if err != nil {
			return err
		}
		return a.render(w, r, "finish.html", page{"Title": "Finished: " + b.Title, "Book": b, "Today": time.Now().Format(time.DateOnly), "Nav": "queue"}, 200)
	}
	finished, rating, err := readingFields(r)
	if err != nil {
		return err
	}
	if finished == nil {
		return bad("Enter the date you finished.")
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		b, err := getBook(tx, id)
		if err != nil {
			return err
		}
		if b.Status != "reading" && b.Status != "to-read" {
			return bad("Only a book you are reading, or have queued, can be marked finished here.")
		}
		if err = setStatus(tx, b, "read"); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(?,?,?,'manual',?)", id, finished, rating, manualFingerprint+randomToken()); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/?view=read&notice=finished")
}
