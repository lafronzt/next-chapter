package app

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// matchInfo is how a record matched a local book: which key, and whether
// through an alias. lookupBook and the import explanations share it, so what
// a preview says is what applying does.
type matchInfo struct {
	ID         int64
	Tombstoned bool
	// Key names the key that matched: "ISBN", "title and author", "original
	// title", or "Goodreads ID". Via is "" for a live book, "merge alias" for
	// a redirect to another book, or "deletion record" for a deleted book.
	Key, Via string
	// Ambiguous lists keys that were not used because they match more than
	// one book or alias. Only filled when explaining.
	Ambiguous []string
}

// matchBook finds the local book a record refers to: ISBN when the record has
// one, then normalized title+author, then Goodreads ID as a last resort for a
// book whose title or author was edited, each only when exactly one row
// matches. With aliases, the same keys are then tried against merged-away and
// deleted books; a merge alias returns its target and a deletion tombstone
// reports Tombstoned. explain also reports keys skipped for being ambiguous.
func matchBook(q queryer, r Record, aliases, explain bool) (matchInfo, error) {
	var m matchInfo
	type matchKey struct{ column, value, label string }
	keys := []matchKey{{"isbn", r.ISBN, "ISBN"}, {"identity", identity(r.Title, r.Author), "title and author"}, {"goodreads_id", r.GoodreadsID, "Goodreads ID"}}
	if r.OriginalTitle != "" {
		// A book imported before series were read from titles is stored under
		// the full title; match it too.
		keys = slices.Insert(keys, 2, matchKey{"identity", identity(r.OriginalTitle, r.Author), "original title"})
	}
	ambiguous := func(table, column, value, label string) error {
		if !explain {
			return nil
		}
		var n int
		if err := q.QueryRow("SELECT count(*) FROM "+table+" WHERE "+column+"=? AND "+column+"!=''", value).Scan(&n); err != nil {
			return err
		}
		if n > 1 {
			m.Ambiguous = append(m.Ambiguous, fmt.Sprintf("%s matches %d %s, so it was not used", label, n, map[string]string{"books": "books in your library", "book_aliases": "remembered merges or deletions"}[table]))
		}
		return nil
	}
	for _, pair := range keys {
		if pair.value == "" {
			continue
		}
		// ISBN matches are accepted only when they identify one local book.
		// The != '' term lets SQLite use the partial goodreads_id and isbn indexes.
		err := q.QueryRow("SELECT MIN(id) FROM books WHERE "+pair.column+"=? AND "+pair.column+"!='' HAVING count(*)=1", pair.value).Scan(&m.ID)
		if err == nil {
			m.Key = pair.label
			return m, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return matchInfo{}, err
		}
		if err = ambiguous("books", pair.column, pair.value, pair.label); err != nil {
			return matchInfo{}, err
		}
	}
	if !aliases {
		return m, nil
	}
	for _, pair := range keys {
		if pair.value == "" {
			continue
		}
		var target sql.NullInt64
		var live int
		err := q.QueryRow("SELECT MIN(a.target_book_id),count(b.id) FROM book_aliases a LEFT JOIN books b ON b.id=a.target_book_id WHERE a."+pair.column+"=? HAVING count(*)=1", pair.value).Scan(&target, &live)
		if err == nil {
			m.Key = pair.label
			if target.Valid && live == 1 {
				m.ID, m.Via = target.Int64, "merge alias"
				return m, nil
			}
			m.Tombstoned, m.Via = true, "deletion record"
			return m, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return matchInfo{}, err
		}
		if err = ambiguous("book_aliases", pair.column, pair.value, pair.label+" (remembered)"); err != nil {
			return matchInfo{}, err
		}
	}
	return m, nil
}

// lookupBook is matchBook without the explanation.
func lookupBook(q queryer, r Record, aliases bool) (id int64, tombstoned bool, err error) {
	m, err := matchBook(q, r, aliases, false)
	return m.ID, m.Tombstoned, err
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
	if err == nil && status == "read" {
		// A finished book has no page to resume at.
		_, err = q.Exec("UPDATE books SET progress_page=NULL,progress_percent=NULL WHERE id=?", old.ID)
	}
	if err != nil {
		return err
	}
	return noteStatusChange(q, old.ID, old.Status, status, midnight(time.Now()), false)
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
func importRecords(q queryer, records []Record) (importResult, error) {
	return importRecordsExplained(q, records, nil)
}

// importRecordsExplained is importRecords, telling onRecord what it decided
// for each record when onRecord is not nil. Preview and apply both run this
// one function, so an explanation cannot drift from what is done.
func importRecordsExplained(q queryer, records []Record, onRecord func(recordDecision)) (out importResult, err error) {
	out.Upgraded, out.Advanced = map[string]int{}, map[string]int{}
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return out, err
	}
	for index, r := range records {
		d := recordDecision{Index: index, Title: r.Title, Author: r.Author}
		m, err := matchBook(q, r, true, onRecord != nil)
		if err != nil {
			return out, err
		}
		id, tombstoned := m.ID, m.Tombstoned
		d.Key, d.Via, d.Ambiguous = m.Key, m.Via, m.Ambiguous
		if tombstoned {
			// The reader deleted this book; re-imports must not resurrect it.
			out.Skipped++
			d.Outcome = "skipped"
			if onRecord != nil {
				onRecord(d)
			}
			continue
		}
		if id == 0 {
			d.Outcome = "new"
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
			d.change("added as a new book (" + statusLabel(r.Status) + ")")
		} else {
			d.Outcome, d.MatchedID = "matched", id
			var local Book
			if onRecord != nil {
				if local, err = getBook(q, id); err != nil {
					return out, err
				}
				d.MatchedTitle = local.Title
			}
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
					if changed {
						d.change(map[string]string{"goodreads_id": "Goodreads ID", "isbn": "ISBN"}[pair[0]] + " filled in")
					}
				}
			}
			if r.Pages != nil {
				changed, err := execChanged(q, "UPDATE books SET pages=? WHERE id=? AND pages IS NULL", *r.Pages, id)
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
				if changed {
					d.change("page count filled in")
				}
			}
			if r.PublishedYear != nil {
				changed, err := execChanged(q, "UPDATE books SET published_year=? WHERE id=? AND published_year IS NULL", *r.PublishedYear, id)
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
				if changed {
					d.change("publication year filled in")
				}
			}
			// A book stored under the full "Title (Series, #2)" takes the clean
			// title. Only a title the reader has not edited is renamed (editing
			// changes the identity), and never onto another book's identity.
			if r.OriginalTitle != "" {
				clean := identity(r.Title, r.Author)
				changed, err := execChanged(q, "UPDATE books SET title=?,identity=? WHERE id=? AND identity=? AND NOT EXISTS (SELECT 1 FROM books WHERE identity=? AND id!=?)", r.Title, clean, id, identity(r.OriginalTitle, r.Author), clean, id)
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
				if changed {
					d.change("title cleaned of its series text")
				}
			}
			// A series read from the title fills a book that has none, and its
			// number only a book whose series matches; what the reader set stays.
			if r.Series != "" {
				changed, err := execChanged(q, "UPDATE books SET series=? WHERE id=? AND series=''", r.Series, id)
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
				if changed {
					d.change("series filled in")
				}
				if r.SeriesNumber != nil {
					changed, err = execChanged(q, "UPDATE books SET series_number=? WHERE id=? AND series_number IS NULL AND series=?", *r.SeriesNumber, id, r.Series)
					if err != nil {
						return out, err
					}
					out.Changed = out.Changed || changed
					if changed {
						d.change("number in series filled in")
					}
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
				if changed {
					d.change("details from a book lookup saved")
				}
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
					d.change("moved out of To review to " + statusLabel(r.Status) + " (the Goodreads shelf)")
				case advances(old.Status, r.Status):
					if err = setStatus(q, old, r.Status); err != nil {
						return out, err
					}
					out.Advanced[r.Status]++
					out.Moved = append(out.Moved, bookMove{old.Title, old.Author, r.Status})
					out.Changed = true
					d.change("reading status " + statusLabel(old.Status) + " → " + statusLabel(r.Status) + " (the Goodreads shelf)")
				}
			}
			if onRecord != nil {
				d.Preserved = preservedFields(local, r, d)
			}
		}
		if logsHistory(r) {
			added, changed, err := logReading(q, id, r, prefs.KeepReviews)
			if err != nil {
				return out, err
			}
			if added {
				out.Logged++
				d.change("finished reading added to your reading history")
			} else if changed {
				d.change("a finished reading in your history updated (date or rating from Goodreads)")
			}
			out.Changed = out.Changed || changed
		}
		if r.Event != nil {
			e := r.Event
			var existingFP string
			err = q.QueryRow(`SELECT fingerprint FROM events WHERE fingerprint=? OR (book_id=? AND source=? AND format=? AND status=? AND at=? AND due_date=?) ORDER BY fingerprint=? DESC LIMIT 1`, fingerprint(r), id, r.Source, e.Format, e.Status, e.At, e.DueDate, fingerprint(r)).Scan(&existingFP)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return out, err
			}
			corrected := false
			if existingFP != "" {
				// The reader corrected or voided this recorded event. Replaying
				// the original observation must neither undo that nor bring a
				// voided event back, so the loan is left as the corrections have it.
				if err = q.QueryRow("SELECT count(*) FROM event_corrections WHERE fingerprint=?", existingFP).Scan(&corrected); err != nil {
					return out, err
				}
			}
			if existingFP == "" {
				if _, err = q.Exec(`INSERT INTO events(book_id,source,format,status,activity,at,due_date,fingerprint) VALUES(?,?,?,?,?,?,?,?)`, id, r.Source, e.Format, e.Status, e.Activity, e.At, e.DueDate, fingerprint(r)); err != nil {
					return out, err
				}
				out.Events++
				out.Changed = true
				d.change(fmt.Sprintf("library event recorded (%s %s %s)", strings.ToUpper(r.Source), e.Format, e.Status))
			}
			if corrected {
				d.Preserved = append(d.Preserved, "your correction of this library event (the replayed observation is not applied)")
			} else {
				// Identical reimports do not invalidate queue previews or undo.
				changed, err := upsertLoan(q, id, r.Source, *e)
				if err != nil {
					return out, err
				}
				out.Changed = out.Changed || changed
				if changed {
					d.change(fmt.Sprintf("current %s %s loan state updated to %s", strings.ToUpper(r.Source), e.Format, e.Status))
				}
			}
		}
		if onRecord != nil {
			onRecord(d)
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
	return execChanged(q, `INSERT INTO loans(book_id,source,format,status,due_date,observed_at) VALUES(?,?,?,?,?,?) ON CONFLICT(book_id,source,format) DO UPDATE SET status=excluded.status,due_date=excluded.due_date,observed_at=excluded.observed_at,
    expected_ready=CASE WHEN excluded.status='hold' AND loans.status='hold' THEN loans.expected_ready ELSE '' END
    WHERE excluded.observed_at>loans.observed_at OR (excluded.observed_at=loans.observed_at AND excluded.status=loans.status AND excluded.due_date!=loans.due_date)`, bookID, source, e.Format, e.Status, e.DueDate, e.At)
}

// recordDecision is what an import decided for one record, for the details
// page: how it matched, what it changes, and which local values it leaves.
type recordDecision struct {
	Index         int
	Title, Author string
	// Outcome is "new", "matched", or "skipped" (a deleted book).
	Outcome string
	// Key and Via say how a match was made (see matchInfo); Ambiguous lists
	// keys that were refused because they were not unique.
	Key, Via     string
	Ambiguous    []string
	MatchedID    int64
	MatchedTitle string
	Changes      []string
	Preserved    []string
}

func (d *recordDecision) change(text string) { d.Changes = append(d.Changes, text) }

// NoOp is true for a matched record that changes nothing.
func (d recordDecision) NoOp() bool { return d.Outcome == "matched" && len(d.Changes) == 0 }

// preservedFields lists the local values a matched record leaves alone while
// it differs from what the file says. Imports never overwrite these.
func preservedFields(local Book, r Record, d recordDecision) []string {
	var out []string
	if r.Rating > 0 {
		out = append(out, "want-to-read ("+fmt.Sprint(local.Interest)+" of 5); the Goodreads rating is not applied")
	}
	statusMoved := false
	for _, c := range d.Changes {
		if strings.HasPrefix(c, "reading status") || strings.HasPrefix(c, "moved out of To review") {
			statusMoved = true
		}
	}
	if r.Event == nil && r.Status != "" && r.Status != local.Status && !statusMoved {
		out = append(out, "reading status ("+statusLabel(local.Status)+"); the file says "+statusLabel(r.Status))
	}
	if r.Pages != nil && local.Pages != nil && *r.Pages != *local.Pages {
		out = append(out, fmt.Sprintf("page count (%d)", *local.Pages))
	}
	if r.Tags != "" && local.Tags != r.Tags {
		out = append(out, "tags")
	}
	if r.Series != "" && local.Series != "" && local.Series != r.Series {
		out = append(out, "series ("+local.Series+")")
	}
	if local.Title != r.Title || local.Author != r.Author {
		out = append(out, "title and author as you have them")
	}
	return out
}
