package app

import (
	"strings"
	"time"
)

// setAlias records that a book identity now points at target, or, with a nil
// target, that it was deleted and imports should ignore it.
func setAlias(q queryer, b Book, target *int64) error {
	_, err := q.Exec(`INSERT INTO book_aliases VALUES(?,?,?,?) ON CONFLICT(identity) DO UPDATE SET goodreads_id=excluded.goodreads_id,isbn=excluded.isbn,target_book_id=excluded.target_book_id`, b.Identity, b.GoodreadsID, b.ISBN, target)
	return err
}

// clearTombstones lets an explicit manual add win over an earlier deletion.
func clearTombstones(q queryer, r Record) error {
	for _, pair := range [][2]string{{"identity", identity(r.Title, r.Author)}, {"goodreads_id", r.GoodreadsID}, {"isbn", r.ISBN}} {
		if pair[1] == "" {
			continue
		}
		if _, err := q.Exec("DELETE FROM book_aliases WHERE target_book_id IS NULL AND "+pair[0]+"=?", pair[1]); err != nil {
			return err
		}
	}
	return nil
}

// countTombstoned reports how many records an import would skip as deleted.
func countTombstoned(q queryer, records []Record) (int, error) {
	var aliases int
	if err := q.QueryRow("SELECT count(*) FROM book_aliases WHERE target_book_id IS NULL").Scan(&aliases); err != nil || aliases == 0 {
		return 0, err
	}
	count := 0
	for _, r := range records {
		_, tombstoned, err := lookupBook(q, r, true)
		if err != nil {
			return 0, err
		}
		if tombstoned {
			count++
		}
	}
	return count, nil
}

// compactQueue renumbers to-read books 0..n-1 in their current order, matching
// reorder(); positions of books outside the queue are left alone.
func compactQueue(q queryer) error {
	queue, err := queueOrder(q)
	if err != nil {
		return err
	}
	ids := []int64{}
	for _, b := range queue {
		ids = append(ids, b.ID)
	}
	return reorder(q, ids)
}

type alias struct {
	Identity     string `json:"identity"`
	GoodreadsID  string `json:"goodreads_id"`
	ISBN         string `json:"isbn"`
	TargetBookID *int64 `json:"target_book_id"`
}

func loadAliases(q queryer) ([]alias, error) {
	rows, err := q.Query("SELECT identity,goodreads_id,isbn,target_book_id FROM book_aliases ORDER BY identity")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []alias{}
	for rows.Next() {
		var a alias
		if err = rows.Scan(&a.Identity, &a.GoodreadsID, &a.ISBN, &a.TargetBookID); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

type deletePreview struct {
	Book                           Book
	Events, Loans, Readings, Notes int
}

func previewDelete(q queryer, id int64) (deletePreview, error) {
	b, err := getBook(q, id)
	if err != nil {
		return deletePreview{}, err
	}
	p := deletePreview{Book: b, Loans: len(b.Loans)}
	err = q.QueryRow("SELECT (SELECT count(*) FROM events WHERE book_id=?), (SELECT count(*) FROM reading_log WHERE book_id=?), (SELECT count(*) FROM book_notes WHERE book_id=?)", id, id, id).Scan(&p.Events, &p.Readings, &p.Notes)
	return p, err
}

func deleteBook(q queryer, id int64) error {
	b, err := getBook(q, id)
	if err != nil {
		return err
	}
	for _, statement := range []string{"DELETE FROM events WHERE book_id=?", "DELETE FROM loans WHERE book_id=?", "DELETE FROM reading_log WHERE book_id=?", "DELETE FROM rating_estimates WHERE book_id=?", "DELETE FROM event_corrections WHERE book_id=?", "DELETE FROM book_notes WHERE book_id=?", "DELETE FROM progress_checkpoints WHERE book_id=?", "DELETE FROM reading_attempts WHERE book_id=?", "UPDATE book_aliases SET target_book_id=NULL WHERE target_book_id=?"} {
		if _, err = q.Exec(statement, id); err != nil {
			return err
		}
	}
	if err = setAlias(q, b, nil); err != nil {
		return err
	}
	if _, err = q.Exec("DELETE FROM books WHERE id=?", id); err != nil {
		return err
	}
	return compactQueue(q)
}

type mergePreview struct {
	Source, Target         Book
	Filled                 []string
	MoveEvents, Duplicates int
	// MoveReadings counts finished readings that move to the target;
	// SameReadings those dropped because the target has the same one.
	MoveReadings, SameReadings int
	// MoveNotes counts private notes that move to the target. ReasonKept is
	// true when both books have a different "why I want to read this": the
	// source's becomes a note on the target. ReasonFilled is true when the
	// target had none and takes the source's.
	// OwnedFilled is true when the merged book is owned and the kept one was
	// not, so the kept book becomes owned.
	OwnedFilled bool
	// MoveAttempts and MoveUpdates count reading attempts and progress
	// updates that move to the kept book.
	MoveAttempts, MoveUpdates int
	// MoveCorrections counts corrected or voided library events that move.
	MoveCorrections          int
	MoveNotes                int
	ReasonKept, ReasonFilled bool
	ISBNConflict             bool
	Refusal                  string
}

// mergeBooks folds source into target. The target keeps its title, author,
// status, interest, pin, and position; blank metadata is filled from the source.
// Without apply it only computes the preview, so both share one code path.
func mergeBooks(q queryer, sourceID, targetID int64, apply bool) (mergePreview, error) {
	if sourceID == targetID {
		return mergePreview{}, bad("Choose a different book to merge into.")
	}
	source, err := getBook(q, sourceID)
	if err != nil {
		return mergePreview{}, err
	}
	target, err := getBook(q, targetID)
	if err != nil {
		return mergePreview{}, err
	}
	p := mergePreview{Source: source, Target: target}
	if source.GoodreadsID != "" && target.GoodreadsID != "" && source.GoodreadsID != target.GoodreadsID {
		p.Refusal = "These books have different Goodreads IDs, so Goodreads treats them as separate books. They cannot be merged."
		if apply {
			return p, conflict(p.Refusal)
		}
		return p, nil
	}
	p.OwnedFilled = source.Owned && !target.Owned
	p.ISBNConflict = source.ISBN != "" && target.ISBN != "" && source.ISBN != target.ISBN
	for _, f := range []struct {
		label string
		fill  bool
	}{
		{"Goodreads ID", target.GoodreadsID == "" && source.GoodreadsID != ""},
		{"ISBN", target.ISBN == "" && source.ISBN != ""},
		{"Pages", target.Pages == nil && source.Pages != nil},
		{"Series", target.Series == "" && source.Series != ""},
		{"Number in series", target.SeriesNumber == nil && source.SeriesNumber != nil},
		{"Tags", target.Tags == "" && source.Tags != ""},
		{"Description", target.Description == "" && source.Description != ""},
	} {
		if f.fill {
			p.Filled = append(p.Filled, f.label)
		}
	}

	// Event fingerprints embed the book identity, so moved events are re-keyed to
	// the target's identity; that keeps re-imports of the source's rows idempotent.
	key := func(e Event) string {
		return strings.Join([]string{e.Source, e.Format, e.Status, e.At, e.DueDate}, "\x00")
	}
	keys, fingerprints := map[string]bool{}, map[string]bool{}
	keyOwner := map[string]string{} // event key to the fingerprint of the kept event
	targetEvents, err := loadEvents(q, targetID, 0)
	if err != nil {
		return p, err
	}
	for _, e := range targetEvents {
		keys[key(e)], fingerprints[e.Fingerprint] = true, true
		keyOwner[key(e)] = e.Fingerprint
	}
	sourceEvents, err := loadEvents(q, sourceID, 0)
	if err != nil {
		return p, err
	}
	for i := len(sourceEvents) - 1; i >= 0; i-- { // oldest first
		e := sourceEvents[i]
		moved := fingerprint(Record{Book: Book{Title: target.Title, Author: target.Author, Source: e.Source}, Event: &e})
		duplicate := keys[key(e)] || fingerprints[moved]
		if !duplicate {
			var n int
			if err = q.QueryRow("SELECT count(*) FROM events WHERE fingerprint=? AND id!=?", moved, e.ID).Scan(&n); err != nil {
				return p, err
			}
			duplicate = n > 0
		}
		if duplicate {
			p.Duplicates++
			if apply {
				// A correction on the dropped copy moves to the kept event when
				// that one has none, so a void or edit is not lost.
				survivor := keyOwner[key(e)]
				if survivor == "" {
					survivor = moved
				}
				if err = carryCorrection(q, e.Fingerprint, survivor, targetID); err == nil {
					_, err = q.Exec("DELETE FROM events WHERE id=?", e.ID)
				}
			}
		} else {
			p.MoveEvents++
			keys[key(e)], fingerprints[moved] = true, true
			keyOwner[key(e)] = moved
			if apply {
				if _, err = q.Exec("UPDATE event_corrections SET fingerprint=?,book_id=? WHERE fingerprint=?", moved, targetID, e.Fingerprint); err == nil {
					_, err = q.Exec("UPDATE events SET book_id=?,fingerprint=? WHERE id=?", targetID, moved, e.ID)
				}
			}
		}
		if err != nil {
			return p, err
		}
	}
	if err = q.QueryRow("SELECT count(*) FROM event_corrections WHERE book_id=?", sourceID).Scan(&p.MoveCorrections); err != nil {
		return p, err
	}
	if err = mergeReadings(q, &p, sourceID, targetID, apply); err != nil {
		return p, err
	}
	if err = mergeNotes(q, &p, source, target, apply); err != nil {
		return p, err
	}
	if err = mergeAttempts(q, &p, source, target, apply); err != nil || !apply {
		return p, err
	}

	if err = rebuildLoans(q, sourceID, targetID); err != nil {
		return p, err
	}
	if _, err = q.Exec("UPDATE book_aliases SET target_book_id=? WHERE target_book_id=?", targetID, sourceID); err != nil {
		return p, err
	}
	if err = setAlias(q, source, &targetID); err != nil {
		return p, err
	}
	// Delete the source before copying identifiers so the unique Goodreads ID
	// index never sees the same value twice.
	if _, err = q.Exec("DELETE FROM rating_estimates WHERE book_id=?", sourceID); err != nil {
		return p, err
	}
	if p.ReasonFilled {
		if _, err = q.Exec("UPDATE books SET reason=? WHERE id=?", source.Reason, targetID); err != nil {
			return p, err
		}
	}
	if _, err = q.Exec("DELETE FROM books WHERE id=?", sourceID); err != nil {
		return p, err
	}
	_, err = q.Exec(`UPDATE books SET goodreads_id=CASE WHEN goodreads_id='' THEN ? ELSE goodreads_id END, isbn=CASE WHEN isbn='' THEN ? ELSE isbn END,
    pages=COALESCE(pages,?), series=CASE WHEN series='' THEN ? ELSE series END, series_number=COALESCE(series_number,?), tags=CASE WHEN tags='' THEN ? ELSE tags END,
    description_source=CASE WHEN description='' THEN ? ELSE description_source END, description=CASE WHEN description='' THEN ? ELSE description END, owned=MAX(owned,?) WHERE id=?`,
		source.GoodreadsID, source.ISBN, source.Pages, source.Series, source.SeriesNumber, source.Tags, source.DescriptionSource, source.Description, source.Owned, targetID)
	if err != nil {
		return p, err
	}
	if source.Status == "to-read" {
		err = compactQueue(q)
	}
	return p, err
}

// rebuildLoans recomputes the target's current loans from its merged events in
// import order, using the same newest-wins rule as imports.
func rebuildLoans(q queryer, sourceID, targetID int64) error {
	// Remember hold dates the reader entered; events do not carry them.
	type heldDate struct{ source, format, day string }
	var held []heldDate
	hrows, err := q.Query("SELECT source,format,expected_ready FROM loans WHERE book_id IN (?,?) AND expected_ready!='' ORDER BY observed_at", sourceID, targetID)
	if err != nil {
		return err
	}
	for hrows.Next() {
		var h heldDate
		if err = hrows.Scan(&h.source, &h.format, &h.day); err != nil {
			hrows.Close()
			return err
		}
		held = append(held, h)
	}
	err = hrows.Err()
	hrows.Close()
	if err != nil {
		return err
	}
	if _, err := q.Exec("DELETE FROM loans WHERE book_id IN (?,?)", sourceID, targetID); err != nil {
		return err
	}
	// Events with the reader's corrections applied: a voided event is left out
	// and an edited one is read with its own values.
	rows, err := q.Query(`SELECT COALESCE(NULLIF(c.source,''),e.source),COALESCE(NULLIF(c.format,''),e.format),COALESCE(NULLIF(c.status,''),e.status),COALESCE(NULLIF(c.at,''),e.at),CASE WHEN c.action='edit' THEN c.due_date ELSE e.due_date END
FROM events e LEFT JOIN event_corrections c ON c.fingerprint=e.fingerprint WHERE e.book_id=? AND COALESCE(c.action,'')!='void' ORDER BY e.id`, targetID)
	if err != nil {
		return err
	}
	events := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Source, &e.Format, &e.Status, &e.At, &e.DueDate); err != nil {
			rows.Close()
			return err
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range events {
		if _, err = upsertLoan(q, targetID, e.Source, e); err != nil {
			return err
		}
	}
	for _, h := range held {
		if _, err = q.Exec("UPDATE loans SET expected_ready=? WHERE book_id=? AND source=? AND format=? AND status='hold'", h.day, targetID, h.source, h.format); err != nil {
			return err
		}
	}
	return nil
}

// mergeReadings moves the source's finished readings to the target. A reading
// on a date the target already has is the same reading and is dropped, as is
// an undated one when the target has any. Fingerprints stay: they identify
// the Goodreads record, which the merge alias now sends to the target.
func mergeReadings(q queryer, p *mergePreview, sourceID, targetID int64, apply bool) error {
	readings, err := bookReadings(q, sourceID)
	if err != nil {
		return err
	}
	have, err := bookReadings(q, targetID)
	if err != nil {
		return err
	}
	dates := map[string]bool{}
	for _, e := range have {
		if e.FinishedAt != nil {
			dates[*e.FinishedAt] = true
		}
	}
	for _, e := range readings {
		if len(have) > 0 && e.FinishedAt == nil || e.FinishedAt != nil && dates[*e.FinishedAt] {
			p.SameReadings++
			if apply {
				_, err = q.Exec("DELETE FROM reading_log WHERE id=?", e.ID)
			}
		} else {
			p.MoveReadings++
			if e.FinishedAt != nil {
				dates[*e.FinishedAt] = true
			}
			if apply {
				_, err = q.Exec("UPDATE reading_log SET book_id=? WHERE id=?", targetID, e.ID)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// mergeNotes moves every private note of the source to the target, identical
// text included, so none is lost. A differing reason for adding the book is
// kept as a note rather than overwritten.
func mergeNotes(q queryer, p *mergePreview, source, target Book, apply bool) error {
	n, err := countNotes(q, source.ID)
	if err != nil {
		return err
	}
	p.MoveNotes = n
	switch {
	case source.Reason == "" || source.Reason == target.Reason:
	case target.Reason == "":
		p.ReasonFilled = true
	default:
		p.ReasonKept = true
		p.MoveNotes++
	}
	if !apply {
		return nil
	}
	if _, err = q.Exec("UPDATE book_notes SET book_id=? WHERE book_id=?", target.ID, source.ID); err != nil {
		return err
	}
	if p.ReasonKept {
		_, err = q.Exec("INSERT INTO book_notes(book_id,body,created_at,updated_at) VALUES(?,?,?,?)", target.ID, reasonMergeLabel+":\n"+source.Reason, source.AddedAt, source.AddedAt)
	}
	return err
}

// mergeAttempts moves the source's reading attempts and progress updates to
// the target, keeping each update with its own attempt. Only one attempt can
// be open for a book that is being read: the source's open attempt is ended
// when the kept book is not being read, or already has one in progress.
func mergeAttempts(q queryer, p *mergePreview, source, target Book, apply bool) error {
	if err := q.QueryRow("SELECT (SELECT count(*) FROM reading_attempts WHERE book_id=?),(SELECT count(*) FROM progress_checkpoints WHERE book_id=?)", source.ID, source.ID).Scan(&p.MoveAttempts, &p.MoveUpdates); err != nil || !apply {
		return err
	}
	open, err := openAttemptID(q, target.ID)
	if err != nil {
		return err
	}
	if target.Status != "reading" || open != 0 {
		if _, err = q.Exec("UPDATE reading_attempts SET ended_on=? WHERE book_id=? AND ended_on IS NULL", midnight(time.Now()).Format(time.DateOnly), source.ID); err != nil {
			return err
		}
	}
	if _, err = q.Exec("UPDATE reading_attempts SET book_id=? WHERE book_id=?", target.ID, source.ID); err != nil {
		return err
	}
	_, err = q.Exec("UPDATE progress_checkpoints SET book_id=? WHERE book_id=?", target.ID, source.ID)
	return err
}

// carryCorrection moves a correction from a dropped duplicate event to the
// event that stays, unless that event has its own, which wins. Either way the
// dropped event's correction is gone.
func carryCorrection(q queryer, from, to string, targetID int64) error {
	var has int
	if err := q.QueryRow("SELECT count(*) FROM event_corrections WHERE fingerprint=?", to).Scan(&has); err != nil {
		return err
	}
	if has == 0 {
		_, err := q.Exec("UPDATE event_corrections SET fingerprint=?,book_id=? WHERE fingerprint=?", to, targetID, from)
		return err
	}
	_, err := q.Exec("DELETE FROM event_corrections WHERE fingerprint=?", from)
	return err
}
