package app

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// readingHistory pages through finished readings, newest first, with stats
// over the whole history.
// readingFields reads a finish date and rating; both may be left blank.
func readingFields(r *http.Request) (finished *string, rating *float64, err error) {
	if raw := strings.TrimSpace(r.FormValue("finished_at")); raw != "" {
		if !validFinishDate(raw) {
			return nil, nil, bad("Enter the finish date as a calendar date, or leave it blank.")
		}
		finished = &raw
	}
	if raw := r.FormValue("rating"); raw != "" {
		n, e := strconv.ParseFloat(raw, 64)
		if e != nil || n < 0.25 || n > 5 || n*4 != math.Trunc(n*4) {
			return nil, nil, bad("Choose a rating from 0.25 to 5 in quarter stars, or leave it unrated.")
		}
		rating = &n
	}
	return finished, rating, nil
}

// addReading logs a reading by hand. Manual readings are never changed by an import.
func (a *App) addReading(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	finished, rating, err := readingFields(r)
	if err != nil {
		return err
	}
	started, err := startDateField(r)
	if err != nil {
		return err
	}
	if err = checkReadingDates(started, finished, 1); err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if _, err := getBook(tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO reading_log(book_id,started_at,finished_at,rating,source,fingerprint) VALUES(?,?,?,?,'manual',?)", id, started, finished, rating, manualFingerprint+randomToken()); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=reading#reading", id))
}

// editReading saves the reader's own date and rating for a reading, which
// makes it manual so later imports leave it alone, or deletes one added by hand.
// A Goodreads reading cannot be deleted: the next import would bring it back.
func (a *App) editReading(w http.ResponseWriter, r *http.Request) error {
	rid, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil || rid < 1 {
		return bad("Choose a valid reading.")
	}
	action := r.FormValue("action")
	var finished *string
	var rating *float64
	switch action {
	case "save":
		if finished, rating, err = readingFields(r); err != nil {
			return err
		}
	case "delete":
	default:
		return bad("Choose a valid action.")
	}
	var bookID int64
	started, err := startDateField(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var fp string
		if err := tx.QueryRow("SELECT book_id,fingerprint FROM reading_log WHERE id=?", rid).Scan(&bookID, &fp); err != nil {
			return err
		}
		if action == "delete" {
			if !(readingEntry{Fingerprint: fp}).AddedByHand() {
				return bad("Readings from Goodreads or Hardcover cannot be deleted here, because the next import would add them again. Edit the date or rating instead.")
			}
			_, err := tx.Exec("DELETE FROM reading_log WHERE id=?", rid)
			if err != nil {
				return err
			}
			return bump(tx)
		}

		var previousStart *string
		var previousFinish *string
		var precision int
		if err := tx.QueryRow("SELECT started_at,finished_at,finished_precision FROM reading_log WHERE id=?", rid).Scan(&previousStart, &previousFinish, &precision); err != nil {
			return err
		}
		if !r.Form.Has("started_at") {
			started = previousStart
		}
		if previousFinish == nil || finished == nil || *previousFinish != *finished {
			precision = 1
		}
		if err := checkReadingDates(started, finished, precision); err != nil {
			return err
		}
		// The fingerprint stays, so an import still recognizes an edited Goodreads
		// reading as the same one.
		changed, err := execChanged(tx, "UPDATE reading_log SET started_at=?,finished_precision=CASE WHEN finished_at IS NOT ? THEN 1 ELSE finished_precision END,finished_at=?,rating=?,source='manual' WHERE id=? AND (finished_at IS NOT ? OR rating IS NOT ? OR started_at IS NOT ? OR source!='manual')", started, finished, finished, rating, rid, finished, rating, started)
		if err != nil || !changed {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=reading#reading", bookID))
}

// startDateField deliberately accepts only a known calendar day.
func startDateField(r *http.Request) (*string, error) {
	raw := strings.TrimSpace(r.FormValue("started_at"))
	if raw == "" {
		return nil, nil
	}
	if !validFinishDate(raw) {
		return nil, bad("Enter the start date as a calendar date, or leave it blank.")
	}
	return &raw, nil
}

func checkReadingDates(started, finished *string, precision int) error {
	if started == nil || finished == nil {
		return nil
	}
	end, err := time.Parse(time.DateOnly, *finished)
	if err != nil {
		return bad("Enter a valid finish date.")
	}
	switch precision {
	case 2:
		end = end.AddDate(0, 1, 0).AddDate(0, 0, -1)
	case 3:
		end = time.Date(end.Year(), 12, 31, 0, 0, 0, 0, time.UTC)
	}
	if *started > end.Format(time.DateOnly) {
		return bad("The start date must be on or before the finish date.")
	}
	return nil
}

// saveStartDate corrects just the active reading's date without resaving book
// details. A local edit, including a clear, is protected from later imports.
func (a *App) saveStartDate(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	started, err := startDateField(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		b, err := getBook(tx, id)
		if err != nil {
			return err
		}
		if b.Status != "reading" {
			return bad("Only a book you are reading has a current start date to save.")
		}
		if err = saveCurrentStart(tx, id, started, true); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved", id))
}
