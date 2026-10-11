package app

import "strings"

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
	all, err := loadBooks(q)
	if err != nil {
		return err
	}
	ids := []int64{}
	for _, b := range queueOnly(all) {
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
	Book          Book
	Events, Loans int
}

func previewDelete(q queryer, id int64) (deletePreview, error) {
	b, err := bookByID(q, id)
	if err != nil {
		return deletePreview{}, err
	}
	p := deletePreview{Book: b, Loans: len(b.Loans)}
	err = q.QueryRow("SELECT count(*) FROM events WHERE book_id=?", id).Scan(&p.Events)
	return p, err
}

func deleteBook(q queryer, id int64) error {
	b, err := bookByID(q, id)
	if err != nil {
		return err
	}
	for _, statement := range []string{"DELETE FROM events WHERE book_id=?", "DELETE FROM loans WHERE book_id=?", "UPDATE book_aliases SET target_book_id=NULL WHERE target_book_id=?"} {
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
	ISBNConflict           bool
	Refusal                string
}

// mergeBooks folds source into target. The target keeps its title, author,
// status, interest, pin, and position; blank metadata is filled from the source.
// Without apply it only computes the preview, so both share one code path.
func mergeBooks(q queryer, sourceID, targetID int64, apply bool) (mergePreview, error) {
	if sourceID == targetID {
		return mergePreview{}, bad("Choose a different book to merge into.")
	}
	source, err := bookByID(q, sourceID)
	if err != nil {
		return mergePreview{}, err
	}
	target, err := bookByID(q, targetID)
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
	targetEvents, err := loadEvents(q, targetID, 0)
	if err != nil {
		return p, err
	}
	for _, e := range targetEvents {
		keys[key(e)], fingerprints[e.Fingerprint] = true, true
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
				_, err = q.Exec("DELETE FROM events WHERE id=?", e.ID)
			}
		} else {
			p.MoveEvents++
			keys[key(e)], fingerprints[moved] = true, true
			if apply {
				_, err = q.Exec("UPDATE events SET book_id=?,fingerprint=? WHERE id=?", targetID, moved, e.ID)
			}
		}
		if err != nil {
			return p, err
		}
	}
	if !apply {
		return p, nil
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
	if _, err = q.Exec("DELETE FROM books WHERE id=?", sourceID); err != nil {
		return p, err
	}
	_, err = q.Exec(`UPDATE books SET goodreads_id=CASE WHEN goodreads_id='' THEN ? ELSE goodreads_id END, isbn=CASE WHEN isbn='' THEN ? ELSE isbn END,
    pages=COALESCE(pages,?), series=CASE WHEN series='' THEN ? ELSE series END, series_number=COALESCE(series_number,?), tags=CASE WHEN tags='' THEN ? ELSE tags END WHERE id=?`,
		source.GoodreadsID, source.ISBN, source.Pages, source.Series, source.SeriesNumber, source.Tags, targetID)
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
	if _, err := q.Exec("DELETE FROM loans WHERE book_id IN (?,?)", sourceID, targetID); err != nil {
		return err
	}
	rows, err := q.Query("SELECT source,format,status,at,due_date FROM events WHERE book_id=? ORDER BY id", targetID)
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
	return nil
}
