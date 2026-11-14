package app

import (
	"database/sql"
	"errors"
	"slices"
	"time"
)

// lookupBook matches a record to a local book: ISBN when the record has one,
// then normalized title+author, then Goodreads ID as a last resort for a book
// whose title or author was edited, each only when exactly one row matches. With aliases,
// the same keys are then tried against merged-away and deleted books; a merge
// alias returns its target and a deletion tombstone reports tombstoned.
func lookupBook(q queryer, r Record, aliases bool) (id int64, tombstoned bool, err error) {
	keys := [][2]string{{"isbn", r.ISBN}, {"identity", identity(r.Title, r.Author)}, {"goodreads_id", r.GoodreadsID}}
	if r.OriginalTitle != "" {
		// A book imported before series were read from titles is stored under
		// the full title; match it too.
		keys = slices.Insert(keys, 2, [2]string{"identity", identity(r.OriginalTitle, r.Author)})
	}
	for _, pair := range keys {
		if pair[1] == "" {
			continue
		}
		// ISBN matches are accepted only when they identify one local book.
		// The != '' term lets SQLite use the partial goodreads_id and isbn indexes.
		err := q.QueryRow("SELECT MIN(id) FROM books WHERE "+pair[0]+"=? AND "+pair[0]+"!='' HAVING count(*)=1", pair[1]).Scan(&id)
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
	// Advanced counts books already in the library that a Goodreads shelf
	// moved forward (to-read to reading or read), keyed by their new status.
	Advanced map[string]int
	// Rated counts added books whose interest came from a rating; RatingsKept
	// counts rated records for existing books, which keep their interest.
	Rated, RatingsKept int
	// Logged counts finished readings added to the reading history.
	Logged int
	// Moved names the queued books a Goodreads shelf moves forward.
	Moved   []bookMove
	Changed bool
}

// bookMove is a queued book an import moves to Reading or Read.
type bookMove struct {
	Title  string `json:"title"`
	Author string `json:"author"`
	To     string `json:"to"`
}

func (r importResult) upgradedTotal() int { return sum(r.Upgraded) }
func (r importResult) advancedTotal() int { return sum(r.Advanced) }
func sum(counts map[string]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

// advances reports whether a Goodreads shelf moves a book forward in its
// reading life. Shelves only ever move a book toward reading or read: a
// to-read row never pulls a finished or archived book back into the queue,
// and the reader's own choices (archiving, finishing by hand) stand.
func advances(from, to string) bool {
	switch to {
	case "reading":
		return from == "to-read"
	case "read":
		return from == "to-read" || from == "reading"
	}
	return false
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
	out.Upgraded, out.Advanced = map[string]int{}, map[string]int{}
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return out, err
	}
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
			// Several callers leave Interest at 0; the column requires 1-5.
			interest := 3
			if r.Interest >= 1 && r.Interest <= 5 {
				interest = r.Interest
			}
			if r.Found != nil {
				f := resolveDetails(fieldsOf(r.Book), *r.Found, importMode(r.LookupMode))
				r.Description, r.DescriptionSource, r.Pages, r.PublishedYear, r.Series, r.SeriesNumber, r.Tags = f.Description, f.DescriptionSource, f.Pages, f.Year, f.Series, f.SeriesNumber, f.Tags
			}
			result, err := q.Exec(`INSERT INTO books(title,author,identity,isbn,goodreads_id,pages,tags,interest,reading_status,position,series,series_number,published_year,avg_rating,added_at,source,description,description_source)
    VALUES(?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(position),-1)+1 FROM books),?,?,?,?,?,?,?,?)`, r.Title, r.Author, identity(r.Title, r.Author), r.ISBN, r.GoodreadsID, r.Pages, r.Tags, interest, r.Status, r.Series, r.SeriesNumber, r.PublishedYear, r.AvgRating, canonicalTime(time.Now()), r.Source, r.Description, r.DescriptionSource)
			if err != nil {
				return out, err
			}
			if id, err = result.LastInsertId(); err != nil {
				return out, err
			}
			out.Added++
			if r.Rating > 0 {
				out.Rated++
			}
			out.Changed = true
		} else {
			// Local choices are authoritative: a rating never changes the
			// interest of a book that is already here.
			if r.Rating > 0 {
				out.RatingsKept++
			}
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
			if r.PublishedYear != nil {
				changed, err := execChanged(q, "UPDATE books SET published_year=? WHERE id=? AND published_year IS NULL", *r.PublishedYear, id)
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
			}
			// A series read from the title fills a book that has none, and its
			// number only a book whose series matches; what the reader set stays.
			if r.Series != "" {
				changed, err := execChanged(q, "UPDATE books SET series=? WHERE id=? AND series=''", r.Series, id)
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
				if r.SeriesNumber != nil {
					changed, err = execChanged(q, "UPDATE books SET series_number=? WHERE id=? AND series_number IS NULL AND series=?", *r.SeriesNumber, id, r.Series)
					if err != nil {
						return out, err
					}
					out.Changed = out.Changed || changed
				}
			}
			// The Goodreads average drifts between polls. Keeping it current is
			// not a library change: it must not expire previews every hour.
			if r.AvgRating != nil {
				if _, err := q.Exec("UPDATE books SET avg_rating=? WHERE id=? AND avg_rating IS NOT ?", *r.AvgRating, id, *r.AvgRating); err != nil {
					return out, err
				}
			}
			if r.Found != nil {
				var b Book
				b, err = getBook(q, id)
				if err != nil {
					return out, err
				}
				changed, err := saveDetails(q, id, fieldsOf(b), resolveDetails(fieldsOf(b), *r.Found, importMode(r.LookupMode)))
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
			}
			if upgradesUnreviewed(r) {
				var old Book
				if err = q.QueryRow("SELECT id,reading_status,pinned,needs_placement,position,title,author FROM books WHERE id=?", id).Scan(&old.ID, &old.Status, &old.Pinned, &old.NeedsPlacement, &old.Position, &old.Title, &old.Author); err != nil {
					return out, err
				}
				switch {
				case old.Status == "unreviewed":
					if err = setStatus(q, old, r.Status); err != nil {
						return out, err
					}
					out.Upgraded[r.Status]++
					out.Changed = true
				case advances(old.Status, r.Status):
					if err = setStatus(q, old, r.Status); err != nil {
						return out, err
					}
					out.Advanced[r.Status]++
					out.Moved = append(out.Moved, bookMove{old.Title, old.Author, r.Status})
					out.Changed = true
				}
			}
		}
		if logsHistory(r) {
			added, changed, err := logReading(q, id, r, prefs.KeepReviews)
			if err != nil {
				return out, err
			}
			if added {
				out.Logged++
			}
			out.Changed = out.Changed || changed
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
