package app

import (
	"bytes"
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

// exportFile is the JSON export, and what a restore accepts. schema_version
// is the database schema the export was written from; it doubles as the
// format version.
type exportFile struct {
	SchemaVersion int          `json:"schema_version"`
	ExportedAt    string       `json:"exported_at"`
	Preferences   *Preferences `json:"preferences,omitempty"`
	Sync          *Connection  `json:"sync,omitempty"`
	Books         []Book       `json:"books"`
	Events        []Event      `json:"events"`
	Aliases       []alias      `json:"aliases"`
	// ReadingLog is the reading history, from schema version 6.
	ReadingLog []readingEntry `json:"reading_log"`
}

const (
	// maxRestoreSchema is the newest export this build can restore. A test
	// fails when a migration is added, so the restore code is checked against
	// the new schema before this is raised.
	maxRestoreSchema = 8
	maxRestoreUpload = 10_000_000
	maxRestoreEvents = 100_000
	maxRestoreAlias  = 20_000
	maxRestoreLog    = 100_000
	maxProblemsShown = 30
)

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// restorePlan is a validated export together with what restoring it would do.
type restorePlan struct {
	File           exportFile `json:"file"`
	Queue          int        `json:"queue"`
	Other          int        `json:"other"`
	Events         int        `json:"events"`
	Loans          int        `json:"loans"`
	Aliases        int        `json:"aliases"`
	Readings       int        `json:"readings"`
	AliasesKept    int        `json:"aliases_kept"`
	HasPreferences bool       `json:"has_preferences"`
	HasSync        bool       `json:"has_sync"`
	SyncWasOn      bool       `json:"sync_was_on"`
}

// decodeExport reads an export strictly. Problems with individual records are
// returned as a list; a file that cannot be read at all is a bad() error.
func decodeExport(data []byte) (exportFile, []string, error) {
	var f exportFile
	if len(data) > maxRestoreUpload {
		return f, nil, bad("Restore files must be at most 10 MB.")
	}
	// Check the version leniently first, so a newer export is refused for its
	// version rather than for the fields it added.
	var head struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&head); err != nil {
		return f, nil, jsonProblem(err)
	}
	if head.SchemaVersion == nil {
		return f, nil, bad("This file has no schema_version, so it is not a Next Chapter JSON export.")
	}
	if v := *head.SchemaVersion; v < 1 || v > maxRestoreSchema {
		return f, nil, bad(fmt.Sprintf("This export has schema version %d. This version of Next Chapter restores schema versions 1 to %d; update the app to restore newer exports.", v, maxRestoreSchema))
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, nil, jsonProblem(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return f, nil, bad("The file has extra data after the export.")
	}
	return f, validateExport(&f), nil
}

func jsonProblem(err error) error {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return bad(fmt.Sprintf("The file is not valid JSON (near byte %d).", syntax.Offset))
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return bad(fmt.Sprintf("The field %q has the wrong type.", typeErr.Field))
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return bad("The file ends before the export is complete.")
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return bad(fmt.Sprintf("The file has an unknown field %s. Only unchanged Next Chapter JSON exports can be restored.", field))
	}
	return bad("The file is not a Next Chapter JSON export.")
}

// validateExport checks every record against the limits imports and forms
// use, and normalizes what the database derives itself: identities are
// recomputed and timestamps made canonical. Incoming IDs are only used to
// connect records within the file.
func validateExport(f *exportFile) []string {
	problems := []string{}
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if len(f.Books) == 0 {
		add("The export has no books.")
	}
	if len(f.Books) > maxRows {
		add("The export has %d books; restore at most 10,000.", len(f.Books))
		return problems
	}
	if len(f.Events) > maxRestoreEvents {
		add("The export has %d library events; restore at most 100,000.", len(f.Events))
		return problems
	}
	if len(f.ReadingLog) > maxRestoreLog {
		add("The export has %d readings; restore at most 100,000.", len(f.ReadingLog))
		return problems
	}
	if len(f.Aliases) > maxRestoreAlias {
		add("The export has %d aliases; restore at most 20,000.", len(f.Aliases))
		return problems
	}
	ids, identities, goodreads := map[int64]bool{}, map[string]int{}, map[string]int{}
	for i := range f.Books {
		b := &f.Books[i]
		label := fmt.Sprintf("Book %d (%q)", i+1, truncate(b.Title, 60))
		b.Title, b.Author, b.Tags, b.Series = strings.TrimSpace(b.Title), strings.TrimSpace(b.Author), strings.TrimSpace(b.Tags), strings.TrimSpace(b.Series)
		if message := checkBook(*b); message != "" {
			add("%s: %s", label, message)
		}
		if b.ID < 1 || ids[b.ID] {
			add("%s: the id must be a unique positive number.", label)
		}
		ids[b.ID] = true
		b.Identity = identity(b.Title, b.Author)
		if j, ok := identities[b.Identity]; ok {
			add("%s has the same title and author as book %d.", label, j+1)
		}
		identities[b.Identity] = i
		if b.GoodreadsID != "" {
			if j, ok := goodreads[b.GoodreadsID]; ok {
				add("%s has the same Goodreads ID as book %d.", label, j+1)
			}
			goodreads[b.GoodreadsID] = i
		}
		if len(b.GoodreadsID) > 50 || (b.ISBN != "" && cleanISBN(b.ISBN) != b.ISBN) {
			add("%s: the Goodreads ID or ISBN is not valid.", label)
		}
		if b.Position < 0 || b.Position > 1<<30 {
			add("%s: the position is out of range.", label)
		}
		if b.Source == "" || len([]rune(b.Source)) > 50 {
			add("%s: the source must be 1 to 50 characters.", label)
		}
		at, err := parseTime(b.AddedAt)
		if err != nil || at == "" {
			add("%s: added_at must be a date and time.", label)
		}
		b.AddedAt = at
		// Loans are rebuilt from events.
		b.Loans = nil
	}
	fingerprints, eventIDs := map[string]bool{}, map[int64]bool{}
	for i := range f.Events {
		e := &f.Events[i]
		label := fmt.Sprintf("Library event %d", i+1)
		if !ids[e.BookID] {
			add("%s refers to a book that is not in the file.", label)
		}
		if e.ID < 1 || eventIDs[e.ID] {
			add("%s: the id must be a unique positive number.", label)
		}
		eventIDs[e.ID] = true
		if e.Source == "" || len([]rune(e.Source)) > 50 || e.Format == "" || len([]rune(e.Format)) > 100 || len([]rune(e.Activity)) > 100 {
			add("%s: source, format, or activity is missing or too long.", label)
		}
		if !in(e.Status, libraryStates) {
			add("%s: unknown status %q.", label, truncate(e.Status, 40))
		}
		at, err := parseTime(e.At)
		if err != nil || at == "" {
			add("%s: at must be a date and time.", label)
		}
		e.At = at
		if e.DueDate != "" {
			if _, err := time.Parse(time.DateOnly, e.DueDate); err != nil {
				add("%s: due_date must be YYYY-MM-DD.", label)
			}
		}
		// A fingerprint hashes the title the event was first imported under,
		// so it cannot be recomputed after a rename or merge; it must be well
		// formed and unique, and future imports deduplicate against it.
		if !fingerprintPattern.MatchString(e.Fingerprint) || fingerprints[e.Fingerprint] {
			add("%s: the fingerprint is missing, malformed, or repeated.", label)
		}
		fingerprints[e.Fingerprint] = true
		e.Title, e.Author = "", ""
	}
	seen := map[string]bool{}
	for i, x := range f.Aliases {
		label := fmt.Sprintf("Alias %d", i+1)
		if x.Identity == "" || len(x.Identity) > 2000 || seen[x.Identity] {
			add("%s: the identity is missing, too long, or repeated.", label)
		}
		seen[x.Identity] = true
		if len(x.GoodreadsID) > 50 || (x.ISBN != "" && cleanISBN(x.ISBN) != x.ISBN) {
			add("%s: the Goodreads ID or ISBN is not valid.", label)
		}
		if x.TargetBookID != nil && !ids[*x.TargetBookID] {
			add("%s refers to a book that is not in the file.", label)
		}
	}
	readingFingerprints := map[string]bool{}
	for i := range f.ReadingLog {
		e := &f.ReadingLog[i]
		label := fmt.Sprintf("Reading %d", i+1)
		if !ids[e.BookID] {
			add("%s refers to a book that is not in the file.", label)
		}
		if e.FinishedAt != nil && !validFinishDate(*e.FinishedAt) {
			add("%s: finished_at must be YYYY-MM-DD.", label)
		}
		if e.Rating != nil && (*e.Rating < 1 || *e.Rating > 5) {
			add("%s: the rating must be 1 to 5.", label)
		}
		if !in(e.Source, []string{"goodreads", "manual"}) {
			add("%s: the source must be goodreads or manual.", label)
		}
		if e.ReviewChars < 0 || (e.Review != nil && len([]rune(*e.Review)) > maxReview) {
			add("%s: the review is too long.", label)
		}
		if e.Fingerprint == "" || len(e.Fingerprint) > 100 || readingFingerprints[e.Fingerprint] {
			add("%s: the fingerprint is missing, too long, or repeated.", label)
		}
		readingFingerprints[e.Fingerprint] = true
		e.Title, e.Author = "", ""
	}
	if p := f.Preferences; p != nil {
		if message := checkPreferences(*p); message != "" {
			add("Preferences: %s", message)
		}
	}
	if c := f.Sync; c != nil {
		if c.Shelf == "" {
			c.Shelf = "to-read"
		}
		// Before version 6 there was no read shelf; start following "read",
		// as the migration does for existing connections.
		if f.SchemaVersion < 6 && c.ReadShelf == "" {
			c.ReadShelf = "read"
		}
		if err := validateConnection(Connection{UserID: c.UserID, Shelf: c.Shelf, ReadShelf: c.ReadShelf}); err != nil {
			add("Goodreads settings: %s.", err)
		}
	}
	return problems
}

// libraryEmpty reports whether there are no books to restore over.
func libraryEmpty(q queryer) (bool, error) {
	var n int
	err := q.QueryRow("SELECT count(*) FROM books").Scan(&n)
	return n == 0, err
}

var errNotEmpty = conflict("Restoring needs an empty library, and this one has books. Use a new instance, or download a backup and start from an empty database. Merging an export into an existing library is not supported.")

// restoreExport writes a validated export into an empty library. Books get new
// IDs in their original order, events are remapped and replayed in their
// original order so loans follow the same newest-wins rule as imports, and a
// Goodreads connection is restored with polling off.
func restoreExport(q queryer, f exportFile) (restorePlan, error) {
	result := restorePlan{File: f, HasPreferences: f.Preferences != nil, HasSync: f.Sync != nil}
	empty, err := libraryEmpty(q)
	if err != nil {
		return result, err
	}
	if !empty {
		return result, errNotEmpty
	}
	books := slices.Clone(f.Books)
	slices.SortStableFunc(books, func(a, b Book) int { return cmp.Or(cmp.Compare(a.Position, b.Position), cmp.Compare(a.ID, b.ID)) })
	insertBook, err := q.Prepare(`INSERT INTO books(title,author,identity,isbn,goodreads_id,pages,tags,interest,reading_status,position,pinned,needs_placement,moved_by_hand,series,series_number,published_year,avg_rating,added_at,source)
    VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return result, err
	}
	defer insertBook.Close()
	newID := make(map[int64]int64, len(books))
	for _, b := range books {
		res, err := insertBook.Exec(b.Title, b.Author, b.Identity, b.ISBN, b.GoodreadsID, b.Pages, b.Tags, b.Interest, b.Status, b.Position, b.Pinned, b.NeedsPlacement, b.MovedByHand, b.Series, b.SeriesNumber, b.PublishedYear, b.AvgRating, b.AddedAt, b.Source)
		if err != nil {
			return result, err
		}
		if newID[b.ID], err = res.LastInsertId(); err != nil {
			return result, err
		}
		if b.Status == "to-read" {
			result.Queue++
		} else {
			result.Other++
		}
	}
	events := slices.Clone(f.Events)
	slices.SortStableFunc(events, func(a, b Event) int { return cmp.Compare(a.ID, b.ID) })
	insertEvent, err := q.Prepare("INSERT INTO events(book_id,source,format,status,activity,at,due_date,fingerprint) VALUES(?,?,?,?,?,?,?,?)")
	if err != nil {
		return result, err
	}
	defer insertEvent.Close()
	for _, e := range events {
		id := newID[e.BookID]
		if _, err = insertEvent.Exec(id, e.Source, e.Format, e.Status, e.Activity, e.At, e.DueDate, e.Fingerprint); err != nil {
			return result, err
		}
		if _, err = upsertLoan(q, id, e.Source, e); err != nil {
			return result, err
		}
		result.Events++
	}
	if err = q.QueryRow("SELECT count(*) FROM loans").Scan(&result.Loans); err != nil {
		return result, err
	}
	for _, x := range f.Aliases {
		var target *int64
		if x.TargetBookID != nil {
			id := newID[*x.TargetBookID]
			target = &id
		}
		// Tombstones already here (books deleted from this empty library)
		// are local decisions and win.
		added, err := execChanged(q, "INSERT INTO book_aliases VALUES(?,?,?,?) ON CONFLICT(identity) DO NOTHING", x.Identity, x.GoodreadsID, x.ISBN, target)
		if err != nil {
			return result, err
		}
		if added {
			result.Aliases++
		} else {
			result.AliasesKept++
		}
	}
	// Review text is kept only if the restored (or, without any, the local)
	// preferences opt in.
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return result, err
	}
	if f.Preferences != nil {
		prefs = *f.Preferences
	}
	readings := slices.Clone(f.ReadingLog)
	slices.SortStableFunc(readings, func(a, b readingEntry) int { return cmp.Compare(a.ID, b.ID) })
	for _, e := range readings {
		review := e.Review
		if !prefs.KeepReviews {
			review = nil
		}
		if _, err = q.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,review_chars,review,fingerprint) VALUES(?,?,?,?,?,?,?)", newID[e.BookID], e.FinishedAt, e.Rating, e.Source, e.ReviewChars, review, e.Fingerprint); err != nil {
			return result, err
		}
		result.Readings++
	}
	if f.Preferences != nil {
		if err = setMeta(q, "preferences", *f.Preferences); err != nil {
			return result, err
		}
	}
	if f.Sync != nil {
		conn := *f.Sync
		result.SyncWasOn, conn.Enabled = conn.Enabled, false
		if err = setMeta(q, "sync", conn); err != nil {
			return result, err
		}
	}
	return result, nil
}

// previewRestore runs a restore inside a savepoint and rolls it back, so the
// preview reports exactly what applying it would do, including database
// constraints the validation missed.
func previewRestore(q queryer, f exportFile) (restorePlan, error) {
	if _, err := q.Exec("SAVEPOINT restore_preview"); err != nil {
		return restorePlan{}, err
	}
	result, err := restoreExport(q, f)
	if _, e := q.Exec("ROLLBACK TO restore_preview; RELEASE restore_preview"); err == nil {
		err = e
	}
	return result, err
}

// exportLibrary builds the JSON export.
func exportLibrary(tx *sql.Tx) (exportFile, error) {
	f := exportFile{ExportedAt: canonicalTime(time.Now())}
	var err error
	if f.Books, err = loadBooks(tx); err != nil {
		return f, err
	}
	if f.Events, err = loadEvents(tx, 0, 0); err != nil {
		return f, err
	}
	prefs, err := readMeta(tx, "preferences", defaultPreferences)
	if err != nil {
		return f, err
	}
	conn, err := readMeta(tx, "sync", defaultConnection)
	if err != nil {
		return f, err
	}
	f.Preferences, f.Sync = &prefs, &conn
	if f.SchemaVersion, _, err = schemaVersion(tx); err != nil {
		return f, err
	}
	if f.Aliases, err = loadAliases(tx); err != nil {
		return f, err
	}
	f.ReadingLog, err = loadReadingLog(tx)
	return f, err
}
