package app

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
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
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if _, err := getBook(tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(?,?,?,'manual',?)", id, finished, rating, manualFingerprint+randomToken()); err != nil {
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
				return bad("Readings from Goodreads cannot be deleted here, because the next import would add them again. Edit the date or rating instead.")
			}
			_, err := tx.Exec("DELETE FROM reading_log WHERE id=?", rid)
			if err != nil {
				return err
			}
			return bump(tx)
		}
		// The fingerprint stays, so an import still recognizes an edited Goodreads
		// reading as the same one.
		changed, err := execChanged(tx, "UPDATE reading_log SET finished_at=?,rating=?,source='manual' WHERE id=? AND (finished_at IS NOT ? OR rating IS NOT ? OR source!='manual')", finished, rating, rid, finished, rating)
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
