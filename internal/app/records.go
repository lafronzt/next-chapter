package app

import (
	"database/sql"
	"errors"
	"time"
)

// lookupBook matches a record to a local book: Goodreads ID, then ISBN, then
// normalized title+author, each only when exactly one row matches. With aliases,
// the same keys are then tried against merged-away and deleted books; a merge
// alias returns its target and a deletion tombstone reports tombstoned.
func lookupBook(q queryer, r Record, aliases bool) (id int64, tombstoned bool, err error) {
	keys := [][2]string{{"goodreads_id", r.GoodreadsID}, {"isbn", r.ISBN}, {"identity", identity(r.Title, r.Author)}}
	for _, pair := range keys {
		if pair[1] == "" {
			continue
		}
		// ISBN matches are accepted only when they identify one local book.
		err := q.QueryRow("SELECT MIN(id) FROM books WHERE "+pair[0]+"=? HAVING count(*)=1", pair[1]).Scan(&id)
		if err == nil {
			return id, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	if !aliases {
		return 0, false, nil
	}
	for _, pair := range keys {
		if pair[1] == "" {
			continue
		}
		var target sql.NullInt64
		var live int
		err := q.QueryRow("SELECT MIN(a.target_book_id),count(b.id) FROM book_aliases a LEFT JOIN books b ON b.id=a.target_book_id WHERE a."+pair[0]+"=? HAVING count(*)=1", pair[1]).Scan(&target, &live)
		if err == nil {
			if target.Valid && live == 1 {
				return target.Int64, false, nil
			}
			return 0, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	return 0, false, nil
}

// findBook returns the live book a record refers to, following merge aliases,
// or 0 when there is none (including deleted books).
func findBook(q queryer, r Record) (int64, error) {
	id, _, err := lookupBook(q, r, true)
	return id, err
}

type importResult struct {
	Added, Events int
	// Skipped counts records for books the reader deleted.
	Skipped int
	// Upgraded counts books that left unreviewed, keyed by their new status.
	Upgraded map[string]int
	Changed  bool
}

func (r importResult) upgradedTotal() int {
	total := 0
	for _, n := range r.Upgraded {
		total += n
	}
	return total
}

// upgradesUnreviewed reports whether a record may move a matching unreviewed
// book to the record's status. Only a Goodreads shelf (CSV or RSS) expresses
// reading intent; library events, manual entries, and the demo never do.
func upgradesUnreviewed(r Record) bool {
	return r.Source == "goodreads" && r.Event == nil && in(r.Status, []string{"to-read", "reading", "read", "archived"})
}

// statusChange returns the pin, placement, and position a book gets when its
// reading status changes: the pin is cleared, and a book entering the queue
// is appended and marked Needs placement.
func statusChange(q queryer, old Book, status string) (pinned, needs bool, position int, err error) {
	if old.Status == status {
		return old.Pinned, old.NeedsPlacement, old.Position, nil
	}
	position = old.Position
	if status == "to-read" {
		err = q.QueryRow("SELECT COALESCE(MAX(position),-1)+1 FROM books").Scan(&position)
	}
	return false, status == "to-read", position, err
}

// setStatus changes only a book's reading status and the fields that follow
// from it.
func setStatus(q queryer, old Book, status string) error {
	pinned, needs, position, err := statusChange(q, old, status)
	if err != nil {
		return err
	}
	_, err = q.Exec("UPDATE books SET reading_status=?,pinned=?,needs_placement=?,position=? WHERE id=?", status, pinned, needs, position, old.ID)
	return err
}

// previewImport runs an import inside a savepoint and rolls it back, so a
// preview reports exactly what applying it would change.
func previewImport(q queryer, records []Record) (importResult, error) {
	if _, err := q.Exec("SAVEPOINT import_preview"); err != nil {
		return importResult{}, err
	}
	result, err := importRecords(q, records)
	if _, e := q.Exec("ROLLBACK TO import_preview; RELEASE import_preview"); err == nil {
		err = e
	}
	return result, err
}
func importRecords(q queryer, records []Record) (out importResult, err error) {
	out.Upgraded = map[string]int{}
	for _, r := range records {
		id, tombstoned, err := lookupBook(q, r, true)
		if err != nil {
			return out, err
		}
		if tombstoned {
			// The reader deleted this book; re-imports must not resurrect it.
			out.Skipped++
			continue
		}
		if id == 0 {
			result, err := q.Exec(`INSERT INTO books(title,author,identity,isbn,goodreads_id,pages,tags,interest,reading_status,position,series,series_number,added_at,source)
    VALUES(?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(position),-1)+1 FROM books),?,?,?,?)`, r.Title, r.Author, identity(r.Title, r.Author), r.ISBN, r.GoodreadsID, r.Pages, r.Tags, 3, r.Status, r.Series, r.SeriesNumber, canonicalTime(time.Now()), r.Source)
			if err != nil {
				return out, err
			}
			if id, err = result.LastInsertId(); err != nil {
				return out, err
			}
			out.Added++
			out.Changed = true
		} else {
			for _, pair := range [][2]string{{"goodreads_id", r.GoodreadsID}, {"isbn", r.ISBN}} {
				if pair[1] != "" {
					changed, err := execChanged(q, "UPDATE books SET "+pair[0]+"=? WHERE id=? AND "+pair[0]+"=''", pair[1], id)
					if err != nil {
						return out, err
					}
					out.Changed = out.Changed || changed
				}
			}
			if r.Pages != nil {
				changed, err := execChanged(q, "UPDATE books SET pages=? WHERE id=? AND pages IS NULL", *r.Pages, id)
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
			}
			if upgradesUnreviewed(r) {
				var old Book
				if err = q.QueryRow("SELECT id,reading_status,pinned,needs_placement,position FROM books WHERE id=?", id).Scan(&old.ID, &old.Status, &old.Pinned, &old.NeedsPlacement, &old.Position); err != nil {
					return out, err
				}
				if old.Status == "unreviewed" {
					if err = setStatus(q, old, r.Status); err != nil {
						return out, err
					}
					out.Upgraded[r.Status]++
					out.Changed = true
				}
			}
		}
		if r.Event != nil {
			e := r.Event
			var existing int
			if err = q.QueryRow(`SELECT count(*) FROM events WHERE fingerprint=? OR (book_id=? AND source=? AND format=? AND status=? AND at=? AND due_date=?)`, fingerprint(r), id, r.Source, e.Format, e.Status, e.At, e.DueDate).Scan(&existing); err != nil {
				return out, err
			}
			if existing == 0 {
				if _, err = q.Exec(`INSERT INTO events(book_id,source,format,status,activity,at,due_date,fingerprint) VALUES(?,?,?,?,?,?,?,?)`, id, r.Source, e.Format, e.Status, e.Activity, e.At, e.DueDate, fingerprint(r)); err != nil {
					return out, err
				}
				out.Events++
				out.Changed = true
			}
			// Identical reimports do not invalidate queue previews or undo.
			changed, err := upsertLoan(q, id, r.Source, *e)
			if err != nil {
				return out, err
			}
			out.Changed = out.Changed || changed
		}
	}
	return out, nil
}
func execChanged(q queryer, statement string, args ...any) (bool, error) {
	res, err := q.Exec(statement, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// upsertLoan keeps the newest known state per book, source, and format: a later
// observation wins, and at the same time only a changed due date for the same
// status updates it, so the first of several conflicting same-time events stays.
func upsertLoan(q queryer, bookID int64, source string, e Event) (bool, error) {
	return execChanged(q, `INSERT INTO loans VALUES(?,?,?,?,?,?) ON CONFLICT(book_id,source,format) DO UPDATE SET status=excluded.status,due_date=excluded.due_date,observed_at=excluded.observed_at
    WHERE excluded.observed_at>loans.observed_at OR (excluded.observed_at=loans.observed_at AND excluded.status=loans.status AND excluded.due_date!=loans.due_date)`, bookID, source, e.Format, e.Status, e.DueDate, e.At)
}
