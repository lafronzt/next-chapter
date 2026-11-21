package app

import (
	"bytes"
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

// exportFile is the JSON export, and what a restore accepts. schema_version
// is the database schema the export was written from; it doubles as the
// format version.
type exportFile struct {
	HardcoverLink *hardcoverLink `json:"hardcover_link,omitempty"`
	SchemaVersion int            `json:"schema_version"`
	ExportedAt    string         `json:"exported_at"`
	Preferences   *Preferences   `json:"preferences,omitempty"`
	// Sync is the retired Goodreads feed connection. Older exports carry it;
	// it is accepted and ignored.
	Sync    json.RawMessage `json:"sync,omitempty"`
	Books   []Book          `json:"books"`
	Events  []Event         `json:"events"`
	Aliases []alias         `json:"aliases"`
	// ReadingLog is the reading history, from schema version 6.
	ReadingLog []readingEntry `json:"reading_log"`
	// Notes are the reader's private notes, from schema version 13.
	Notes []bookNote `json:"notes"`
	// SavedViews are the reader's named list filters, from schema version 14.
	SavedViews []savedViewExport `json:"saved_views"`
	// ReadingAttempts and ProgressCheckpoints are the optional progress
	// history, from schema version 15.
	ReadingAttempts     []readingAttempt     `json:"reading_attempts"`
	ProgressCheckpoints []progressCheckpoint `json:"progress_checkpoints"`
	// EventCorrections are the reader's edits and voids of library events,
	// from schema version 16. The events themselves are exported as imported.
	EventCorrections []eventCorrection `json:"event_corrections"`
	// HardcoverBooks and HardcoverAliases are the links to Hardcover books and
	// the ids remembered for deleted or merged books, from schema version 18.
	// Older exports have neither. Hardcover book ids are global, so they are
	// restored, unlike the account link.
	HardcoverBooks   []hcBookLink `json:"hardcover_books,omitempty"`
	HardcoverAliases []hcAlias    `json:"hardcover_aliases,omitempty"`
	HardcoverSync    []hcSyncRow  `json:"hardcover_sync,omitempty"`
}

// savedViewExport is a saved view as exported.
type savedViewExport struct {
	Name       string         `json:"name"`
	Definition viewDefinition `json:"definition"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
}

const (
	// maxRestoreSchema is the newest export this build can restore. A test
	// fails when a migration is added, so the restore code is checked against
	// the new schema before this is raised.
	maxRestoreSchema = 29
	maxRestoreUpload = 10_000_000
	maxRestoreEvents = 100_000
	maxRestoreAlias  = 20_000
	maxRestoreSync   = 20_000
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
	Notes          int        `json:"notes"`
	SavedViews     int        `json:"saved_views"`
	Attempts       int        `json:"attempts"`
	Updates        int        `json:"updates"`
	Corrections    int        `json:"corrections"`
	AliasesKept    int        `json:"aliases_kept"`
	HasPreferences bool       `json:"has_preferences"`
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
	if len(f.Notes) > maxRestoreNotes {
		add("The export has %d notes; restore at most %d.", len(f.Notes), maxRestoreNotes)
		return problems
	}
	if len(f.ReadingAttempts) > maxRestoreAttempts || len(f.ProgressCheckpoints) > maxRestoreCheckpoints {
		add("The export has %d reading attempts and %d progress updates; restore at most %d and %d.", len(f.ReadingAttempts), len(f.ProgressCheckpoints), maxRestoreAttempts, maxRestoreCheckpoints)
		return problems
	}
	if len(f.EventCorrections) > maxRestoreEvents {
		add("The export has %d event corrections; restore at most %d.", len(f.EventCorrections), maxRestoreEvents)
		return problems
	}
	if len(f.SavedViews) > maxRestoreViews {
		add("The export has %d saved views; restore at most %d.", len(f.SavedViews), maxRestoreViews)
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
		b.Reason = noteText(b.Reason)
		// Loans are rebuilt from events. Only the reader's own hold-date
		// estimates, which events do not carry, are kept to reapply.
		kept := []Loan{}
		for _, l := range b.Loans {
			if l.Status == "hold" && l.ExpectedReady != "" && validFinishDate(l.ExpectedReady) {
				kept = append(kept, l)
			}
		}
		b.Loans = kept
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
	hcBooks, hcLocal := map[int64]bool{}, map[int64]bool{}
	for i, l := range f.HardcoverBooks {
		label := fmt.Sprintf("Hardcover link %d", i+1)
		if !ids[l.BookID] || hcLocal[l.BookID] {
			add("%s refers to a book that is not in the file, or repeats one.", label)
		}
		if l.HardcoverBookID < 1 || hcBooks[l.HardcoverBookID] {
			add("%s: the Hardcover book id is missing or repeated.", label)
		}
		if (l.EditionID != nil && *l.EditionID < 1) || len(l.Slug) > 200 || len(l.LinkedAt) > 64 {
			add("%s: the edition, slug, or time is not valid.", label)
		}
		hcBooks[l.HardcoverBookID], hcLocal[l.BookID] = true, true
	}
	if len(f.HardcoverSync) > maxRestoreSync {
		add("The export has %d Hardcover sync rows; restore at most %d.", len(f.HardcoverSync), maxRestoreSync)
	}
	for i, x := range f.HardcoverSync {
		if !ids[x.BookID] || !in(x.Field, []string{"status", "rating", "read", "progress", "dates"}) || !in(x.Direction, []string{"push", "pull"}) || len(x.Local) > 64 || len(x.Remote) > 64 || len(x.At) > 64 {
			add("Hardcover sync row %d is not valid.", i+1)
		}
	}
	if len(f.HardcoverAliases) > maxRestoreAlias {
		add("The export has %d Hardcover aliases; restore at most 20,000.", len(f.HardcoverAliases))
	}
	for i, x := range f.HardcoverAliases {
		if x.Identity == "" || len(x.Identity) > 2000 || x.HardcoverBookID < 1 {
			add("Hardcover alias %d: the identity or Hardcover book id is not valid.", i+1)
		}
	}
	readingFingerprints, hcReads := map[string]bool{}, map[int64]bool{}
	for i := range f.ReadingLog {
		e := &f.ReadingLog[i]
		label := fmt.Sprintf("Reading %d", i+1)
		if !ids[e.BookID] {
			add("%s refers to a book that is not in the file.", label)
		}
		if e.StartedAt != nil && !validFinishDate(*e.StartedAt) {
			add("%s: started_at must be YYYY-MM-DD.", label)
		}
		if err := checkReadingDates(e.StartedAt, e.FinishedAt, e.FinishedPrecision); err != nil {
			add("%s: the start must be on or before the finish.", label)
		}
		if e.FinishedAt != nil && !validFinishDate(*e.FinishedAt) {
			add("%s: finished_at must be YYYY-MM-DD.", label)
		}
		if e.Rating != nil && (*e.Rating < 0.25 || *e.Rating > 5 || *e.Rating*4 != math.Trunc(*e.Rating*4)) {
			add("%s: the rating must be 0.25 to 5 in quarter stars.", label)
		}
		if !in(e.Source, []string{"goodreads", "manual", "hardcover"}) {
			add("%s: the source must be goodreads, manual, or hardcover.", label)
		}
		if e.FinishedPrecision < 0 || e.FinishedPrecision > 3 || (e.HardcoverReadID != nil && *e.HardcoverReadID < 1) {
			add("%s: the finish precision or Hardcover read id is not valid.", label)
		}
		if e.HardcoverReadID != nil {
			if hcReads[*e.HardcoverReadID] {
				add("%s: the Hardcover read id is repeated.", label)
			}
			hcReads[*e.HardcoverReadID] = true
		}
		if e.ReviewFormat != "" && e.ReviewFormat != "plain" && e.ReviewFormat != "markdown" {
			add("%s: the review format is not valid.", label)
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
	noteIDs, perBook := map[int64]bool{}, map[int64]int{}
	for i := range f.Notes {
		n := &f.Notes[i]
		label := fmt.Sprintf("Note %d", i+1)
		if n.HardcoverJournalID < 0 || n.HardcoverUserID < 0 || n.HardcoverBookID < 0 ||
			(n.HardcoverJournalID == 0) != (n.HardcoverUserID == 0) || (n.HardcoverJournalID == 0) != (n.HardcoverBookID == 0) {
			add("%s: Hardcover share ids must all be positive or all absent.", label)
		}

		if !ids[n.BookID] {
			add("%s refers to a book that is not in the file.", label)
		}
		if n.ID < 1 || noteIDs[n.ID] {
			add("%s: the id must be a unique positive number.", label)
		}
		noteIDs[n.ID] = true
		if perBook[n.BookID]++; perBook[n.BookID] > maxNotesPerBook {
			add("%s: a book can have at most %d notes.", label, maxNotesPerBook)
		}
		// Exports from before schema version 28 have only plain notes.
		if n.Kind == "" {
			n.Kind = "note"
		}
		// Before version 29 there was no origin or exchange record.
		if n.Origin == "" {
			n.Origin = "local"
		}
		if n.Origin != "local" && n.Origin != "hardcover" {
			add("%s: the origin is not recognized.", label)
		}
		if n.BasePrivacy < 0 || n.BasePrivacy > 3 || (n.HardcoverJournalID == 0 && (n.BaseEntry != "" || n.BasePage != nil || n.BasePrivacy != 0 || n.SyncedAt != "")) || (n.BasePage != nil && (*n.BasePage < 1 || *n.BasePage > maxProgressPage)) || len([]rune(n.BaseEntry)) > maxNote+20 {
			add("%s: the Hardcover exchange record is not valid.", label)
		}
		n.Body, n.Quote = noteText(n.Body), noteText(n.Quote)
		if message := checkNote(n.Kind, n.Quote, n.Body, n.Page, n.Percent); message != "" {
			add("%s: %s", label, message)
		}
		created, err1 := parseTime(n.CreatedAt)
		updated, err2 := parseTime(n.UpdatedAt)
		if err1 != nil || err2 != nil || created == "" || updated == "" {
			add("%s: created_at and updated_at must be dates and times.", label)
		}
		n.CreatedAt, n.UpdatedAt = created, updated
	}
	eventBook := map[string]int64{}
	for _, e := range f.Events {
		eventBook[e.Fingerprint] = e.BookID
	}
	correctedFP := map[string]bool{}
	for i := range f.EventCorrections {
		c := &f.EventCorrections[i]
		label := fmt.Sprintf("Event correction %d", i+1)
		if book, ok := eventBook[c.Fingerprint]; !ok || book != c.BookID {
			add("%s refers to a library event that is not in the file or belongs to another book.", label)
		}
		if correctedFP[c.Fingerprint] {
			add("%s: the event already has a correction.", label)
		}
		correctedFP[c.Fingerprint] = true
		switch c.Action {
		case "void":
			c.Source, c.Format, c.Status, c.At, c.DueDate = "", "", "", "", ""
		case "edit":
			at, err := parseTime(c.At)
			if !in(c.Source, loanSources) || c.Format == "" || len([]rune(c.Format)) > 100 || !in(c.Status, libraryStates) || err != nil || at == "" || (c.DueDate != "" && !validFinishDate(c.DueDate)) {
				add("%s: the corrected values are not valid.", label)
			}
			c.At = at
		default:
			add("%s: the action must be edit or void.", label)
		}
		created, err1 := parseTime(c.CreatedAt)
		updated, err2 := parseTime(c.UpdatedAt)
		if err1 != nil || err2 != nil || created == "" || updated == "" {
			add("%s: created_at and updated_at must be dates and times.", label)
		}
		c.CreatedAt, c.UpdatedAt = created, updated
	}
	bookStatus := map[int64]string{}
	for _, b := range f.Books {
		bookStatus[b.ID] = b.Status
	}
	attemptBook, openAttempts := map[int64]int64{}, map[int64]bool{}
	for i := range f.ReadingAttempts {
		x := &f.ReadingAttempts[i]
		label := fmt.Sprintf("Reading attempt %d", i+1)
		if !ids[x.BookID] {
			add("%s refers to a book that is not in the file.", label)
		}
		if _, dup := attemptBook[x.ID]; x.ID < 1 || dup {
			add("%s: the id must be a unique positive number.", label)
		}
		attemptBook[x.ID] = x.BookID
		if (x.StartedOn != "" && !validFinishDate(x.StartedOn)) || (x.EndedOn != nil && !validFinishDate(*x.EndedOn)) {
			add("%s: dates must be YYYY-MM-DD.", label)
		}
		if x.EndedOn == nil {
			if openAttempts[x.BookID] || bookStatus[x.BookID] != "reading" {
				add("%s is still open, but its book is not being read or already has an open attempt.", label)
			}
			openAttempts[x.BookID] = true
		}
	}
	checkpointIDs, days := map[int64]bool{}, map[string]bool{}
	for i := range f.ProgressCheckpoints {
		c := &f.ProgressCheckpoints[i]
		label := fmt.Sprintf("Progress update %d", i+1)
		if book, ok := attemptBook[c.AttemptID]; !ok || book != c.BookID {
			add("%s refers to a reading attempt that is not in the file or belongs to another book.", label)
		}
		if c.ID < 1 || checkpointIDs[c.ID] {
			add("%s: the id must be a unique positive number.", label)
		}
		checkpointIDs[c.ID] = true
		key := fmt.Sprintf("%d|%s", c.AttemptID, c.Day)
		if !validFinishDate(c.Day) || days[key] {
			add("%s: the day must be a unique calendar date within its attempt.", label)
		}
		days[key] = true
		limit := maxProgressPage
		if c.Unit == "percent" {
			limit = 100
		} else if c.Unit == "seconds" {
			limit = maxProgressSeconds
		}
		if c.TotalSeconds != nil && (*c.TotalSeconds < 1 || *c.TotalSeconds > maxProgressSeconds) {
			add("%s: invalid audiobook duration.", label)
		}
		if c.Unit != "page" && c.Unit != "percent" && c.Unit != "seconds" || (c.Value != nil && (*c.Value < 0 || *c.Value > limit)) || (c.TotalPages != nil && (*c.TotalPages < 1 || *c.TotalPages > 99999)) {
			add("%s: the unit or value is out of range.", label)
		}
	}
	viewNames := map[string]bool{}
	for i := range f.SavedViews {
		v := &f.SavedViews[i]
		label := fmt.Sprintf("Saved view %d", i+1)
		name, err := cleanViewName(v.Name)
		if err != nil || name != v.Name {
			add("%s: the name must be 1 to %d characters without extra spaces.", label, maxViewName)
		}
		if viewNames[foldKey(v.Name)] {
			add("%s: the name is repeated.", label)
		}
		viewNames[foldKey(v.Name)] = true
		if message := v.Definition.check(); message != "" {
			add("%s: %s.", label, message)
		}
		created, err1 := parseTime(v.CreatedAt)
		updated, err2 := parseTime(v.UpdatedAt)
		if err1 != nil || err2 != nil || created == "" || updated == "" {
			add("%s: created_at and updated_at must be dates and times.", label)
		}
		v.CreatedAt, v.UpdatedAt = created, updated
	}
	if p := f.Preferences; p != nil {
		if message := checkPreferences(*p); message != "" {
			add("Preferences: %s", message)
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
// original order so loans follow the same newest-wins rule as imports,.
func restoreExport(q queryer, f exportFile) (restorePlan, error) {
	result := restorePlan{File: f, HasPreferences: f.Preferences != nil}
	empty, err := libraryEmpty(q)
	if err != nil {
		return result, err
	}
	if !empty {
		return result, errNotEmpty
	}
	// A restored library must be linked explicitly with the current token.
	if _, err = q.Exec("DELETE FROM hardcover_link; DELETE FROM meta WHERE key='hardcover_test'"); err != nil {
		return result, err
	}
	books := slices.Clone(f.Books)
	slices.SortStableFunc(books, func(a, b Book) int { return cmp.Or(cmp.Compare(a.Position, b.Position), cmp.Compare(a.ID, b.ID)) })
	insertBook, err := q.Prepare(`INSERT INTO books(title,author,identity,isbn,goodreads_id,pages,progress_page,progress_percent,progress_seconds,audio_seconds,tags,interest,reading_status,position,pinned,needs_placement,moved_by_hand,series,series_number,published_year,avg_rating,added_at,source,description,description_source,reason,owned)
    VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return result, err
	}
	defer insertBook.Close()
	newID := make(map[int64]int64, len(books))
	for _, b := range books {
		res, err := insertBook.Exec(b.Title, b.Author, b.Identity, b.ISBN, b.GoodreadsID, b.Pages, b.ProgressPage, b.ProgressPercent, b.ProgressSeconds, b.AudioSeconds, b.Tags, b.Interest, b.Status, b.Position, b.Pinned, b.NeedsPlacement, b.MovedByHand, b.Series, b.SeriesNumber, b.PublishedYear, b.AvgRating, b.AddedAt, b.Source, b.Description, b.DescriptionSource, b.Reason, b.Owned)
		if err != nil {
			return result, err
		}
		if newID[b.ID], err = res.LastInsertId(); err != nil {
			return result, err
		}
		if _, err = q.Exec("UPDATE books SET started_at=?,started_manual=? WHERE id=?", b.StartedAt, b.StartedManual, newID[b.ID]); err != nil {
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
	correctedBooks := map[int64]bool{}
	for _, c := range f.EventCorrections {
		if _, err = q.Exec("INSERT INTO event_corrections(fingerprint,book_id,action,source,format,status,at,due_date,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)", c.Fingerprint, newID[c.BookID], c.Action, c.Source, c.Format, c.Status, c.At, c.DueDate, c.CreatedAt, c.UpdatedAt); err != nil {
			return result, err
		}
		correctedBooks[newID[c.BookID]] = true
		result.Corrections++
	}
	for id := range correctedBooks {
		// Current loans follow the corrected events, not the originals.
		if err = rebuildLoans(q, id, id); err != nil {
			return result, err
		}
	}
	for _, b := range books {
		for _, l := range b.Loans {
			if _, err = q.Exec("UPDATE loans SET expected_ready=? WHERE book_id=? AND source=? AND format=? AND status='hold'", l.ExpectedReady, newID[b.ID], l.Source, l.Format); err != nil {
				return result, err
			}
		}
	}
	if err = q.QueryRow("SELECT count(*) FROM loans").Scan(&result.Loans); err != nil {
		return result, err
	}
	for _, l := range f.HardcoverBooks {
		if _, err = q.Exec("INSERT INTO book_hardcover(book_id,hardcover_book_id,hardcover_edition_id,slug,linked_at,progress_sync) VALUES(?,?,?,?,?,?)", newID[l.BookID], l.HardcoverBookID, l.EditionID, l.Slug, l.LinkedAt, l.ProgressSync); err != nil {
			return result, err
		}
	}
	for _, x := range f.HardcoverSync {
		if _, err = q.Exec("INSERT INTO hardcover_sync(book_id,field,local_value,remote_value,direction,at) VALUES(?,?,?,?,?,?)", newID[x.BookID], x.Field, x.Local, x.Remote, x.Direction, x.At); err != nil {
			return result, err
		}
	}
	for _, x := range f.HardcoverAliases {
		if _, err = q.Exec("INSERT OR IGNORE INTO hardcover_alias(identity,hardcover_book_id) VALUES(?,?)", x.Identity, x.HardcoverBookID); err != nil {
			return result, err
		}
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
		if !prefs.KeepReviews && e.ReviewFormat != "markdown" {
			review = nil
		}
		if _, err = q.Exec("INSERT INTO reading_log(book_id,started_at,finished_at,rating,source,review_chars,review,fingerprint,finished_precision,hardcover_read_id,review_format) VALUES(?,?,?,?,?,?,?,?,?,?,?)", newID[e.BookID], e.StartedAt, e.FinishedAt, e.Rating, e.Source, e.ReviewChars, review, e.Fingerprint, max(1, e.FinishedPrecision), e.HardcoverReadID, reviewFormat(e.ReviewFormat)); err != nil {
			return result, err
		}
		result.Readings++
	}
	notes := slices.Clone(f.Notes)
	slices.SortStableFunc(notes, func(a, b bookNote) int { return cmp.Compare(a.ID, b.ID) })
	for _, n := range notes {
		if _, err = q.Exec("INSERT INTO book_notes(book_id,kind,quote,body,page,percent,created_at,updated_at,hardcover_journal_id,hardcover_user_id,hardcover_book_id,origin,hc_base_entry,hc_base_page,hc_base_privacy,hc_synced_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", newID[n.BookID], n.Kind, n.Quote, n.Body, n.Page, n.Percent, n.CreatedAt, n.UpdatedAt, n.HardcoverJournalID, n.HardcoverUserID, n.HardcoverBookID, n.Origin, n.BaseEntry, n.BasePage, n.BasePrivacy, n.SyncedAt); err != nil {
			return result, err
		}
		result.Notes++
	}
	newAttempt := make(map[int64]int64, len(f.ReadingAttempts))
	attempts := slices.Clone(f.ReadingAttempts)
	slices.SortStableFunc(attempts, func(a, b readingAttempt) int { return cmp.Compare(a.ID, b.ID) })
	for _, x := range attempts {
		res, err := q.Exec("INSERT INTO reading_attempts(book_id,started_on,ended_on) VALUES(?,?,?)", newID[x.BookID], x.StartedOn, x.EndedOn)
		if err != nil {
			return result, err
		}
		if newAttempt[x.ID], err = res.LastInsertId(); err != nil {
			return result, err
		}
		result.Attempts++
	}
	updates := slices.Clone(f.ProgressCheckpoints)
	slices.SortStableFunc(updates, func(a, b progressCheckpoint) int { return cmp.Compare(a.ID, b.ID) })
	for _, c := range updates {
		if _, err = q.Exec("INSERT INTO progress_checkpoints(attempt_id,book_id,day,unit,value,total_pages,total_seconds) VALUES(?,?,?,?,?,?,?)", newAttempt[c.AttemptID], newID[c.BookID], c.Day, c.Unit, c.Value, c.TotalPages, c.TotalSeconds); err != nil {
			return result, err
		}
		result.Updates++
	}
	for _, v := range f.SavedViews {
		raw, _ := json.Marshal(v.Definition)
		if _, err = q.Exec("INSERT INTO saved_views(name,definition,created_at,updated_at) VALUES(?,?,?,?)", v.Name, string(raw), v.CreatedAt, v.UpdatedAt); err != nil {
			return result, err
		}
		result.SavedViews++
	}
	if f.Preferences != nil {
		if err = setMeta(q, "preferences", *f.Preferences); err != nil {
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
	f.Preferences = &prefs
	if f.HardcoverLink, err = loadHardcoverLink(tx); err != nil {
		return f, err
	}
	if f.SchemaVersion, _, err = schemaVersion(tx); err != nil {
		return f, err
	}
	if f.Aliases, err = loadAliases(tx); err != nil {
		return f, err
	}
	if f.HardcoverBooks, err = loadBookLinks(tx); err != nil {
		return f, err
	}
	if f.HardcoverAliases, err = loadHardcoverAliases(tx); err != nil {
		return f, err
	}
	if f.HardcoverSync, err = loadHardcoverSync(tx); err != nil {
		return f, err
	}
	if f.ReadingLog, err = loadReadingLog(tx); err != nil {
		return f, err
	}
	if f.Notes, err = loadAllNotes(tx); err != nil {
		return f, err
	}
	if f.EventCorrections, err = loadAllCorrections(tx); err != nil {
		return f, err
	}
	if f.ReadingAttempts, err = loadAllAttempts(tx); err != nil {
		return f, err
	}
	if f.ProgressCheckpoints, err = loadAllCheckpoints(tx); err != nil {
		return f, err
	}
	views, err := loadSavedViews(tx)
	if err != nil {
		return f, err
	}
	f.SavedViews = []savedViewExport{}
	for _, v := range views {
		f.SavedViews = append(f.SavedViews, savedViewExport{v.Name, v.Definition, v.CreatedAt, v.UpdatedAt})
	}
	return f, nil
}
