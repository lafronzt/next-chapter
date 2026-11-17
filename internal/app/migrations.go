package app

import (
	"database/sql"
	"fmt"
)

// Append new migrations here; never edit one that has shipped.
var migrations = []migration{
	{1, "Python-era baseline schema", migrateBaseline},
	{2, "canonical Go timestamps", migrateGoTimestamps},
	{3, "book aliases for merges and deletions", migrateBookAliases},
	{4, "per-client login failures", migrateLoginFailures},
	{5, "indexes for paging and imports", migrateIndexes},
	{6, "reading history", migrateReadingLog},
	{7, "estimated ratings", migrateRatingEstimates},
	{8, "books moved by hand", migrateMovedByHand},
	{9, "quarter-star ratings", migrateQuarterStars},
	{10, "book descriptions", migrateDescriptions},
	{11, "reading progress and expected hold dates", migrateProgressAndHolds},
	{12, "reading progress as a percentage", migrateProgressPercent},
	{13, "private reading notes", migratePrivateNotes},
	{14, "ownership flag and saved library views", migrateSavedViews},
	{15, "reading attempts and dated progress updates", migrateProgressHistory},
	{16, "reader corrections to library events", migrateEventCorrections},
	{17, "Hardcover account link", migrateHardcoverLink},
	{18, "Hardcover book links", migrateHardcoverBookLinks},
	{19, "Hardcover readings: source, precision, read id", migrateHardcoverReadings},
	{20, "Hardcover sync history", migrateHardcoverSync},
	{21, "Hardcover progress opt-in and sync fields", migrateHardcoverProgress},
	{22, "Hardcover catalog cache", migrateHardcoverCatalog},
	{23, "audiobook time progress", migrateAudioProgress},
	{24, "explicit Hardcover note shares", migrateHardcoverNotes},
	{25, "audiobook duration", migrateAudioDuration},
	{26, "reading start dates", migrateReadingStartDates},
}

func migrateBaseline(tx *sql.Tx) error {
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT OR IGNORE INTO meta VALUES ('revision', '0')")
	return err
}

// Python-era (v0.1) libraries stored timestamps in several ISO 8601 shapes.
// Normalize them once so chronological comparisons stay consistent. Previews and
// undo snapshots from the old runtime are not portable and are cleared.
func migrateGoTimestamps(tx *sql.Tx) error {
	for _, table := range []string{"events", "loans"} {
		column := "at"
		if table == "loans" {
			column = "observed_at"
		}
		rows, err := tx.Query("SELECT rowid," + column + " FROM " + table)
		if err != nil {
			return err
		}
		type item struct {
			id int64
			at string
		}
		items := []item{}
		for rows.Next() {
			var x item
			if err = rows.Scan(&x.id, &x.at); err != nil {
				rows.Close()
				return err
			}
			items = append(items, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, x := range items {
			at, err := parseTime(x.at)
			if err != nil {
				return fmt.Errorf("migrate %s timestamp: %w", table, err)
			}
			if _, err = tx.Exec("UPDATE "+table+" SET "+column+"=? WHERE rowid=?", at, x.id); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec("DELETE FROM plans; DELETE FROM undo_queue")
	return err
}

// Aliases remember merged-away and deleted books so re-imports do not recreate
// them. A NULL target means "ignore this book".
func migrateBookAliases(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE book_aliases (
 identity TEXT PRIMARY KEY, goodreads_id TEXT NOT NULL DEFAULT '', isbn TEXT NOT NULL DEFAULT '', target_book_id INTEGER NULL
);
CREATE INDEX book_aliases_goodreads_id ON book_aliases(goodreads_id) WHERE goodreads_id != '';
CREATE INDEX book_aliases_isbn ON book_aliases(isbn) WHERE isbn != '';`)
	return err
}

// Failed logins per client; login_attempts is no longer used. IF NOT EXISTS
// because builds before the migration runner created this table at startup.
func migrateLoginFailures(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS login_failures (client TEXT NOT NULL, at REAL NOT NULL);
CREATE INDEX IF NOT EXISTS login_failures_client ON login_failures(client, at);`)
	return err
}

// Indexes for paging the book list and history and for matching imported
// rows. Without them each imported row scans every book for its position and
// match keys, so a 10,000-row import held the write lock for seconds.
func migrateIndexes(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE INDEX books_status_position ON books(reading_status, position, id);
CREATE INDEX books_position ON books(position, id);
CREATE INDEX books_isbn ON books(isbn) WHERE isbn != '';
CREATE INDEX events_book ON events(book_id, at);
CREATE INDEX events_at ON events(at, id);`)
	return err
}

// Reading history from Goodreads and manual entries, kept apart from library
// events. Existing Goodreads connections start following the "read" shelf.
func migrateReadingLog(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE reading_log (
 id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id),
 finished_at TEXT NULL, rating INTEGER NULL CHECK(rating BETWEEN 1 AND 5),
 source TEXT NOT NULL CHECK(source IN ('goodreads','manual')),
 review_chars INTEGER NOT NULL DEFAULT 0, review TEXT NULL, fingerprint TEXT NOT NULL UNIQUE
);
CREATE INDEX reading_log_book ON reading_log(book_id, finished_at);
CREATE INDEX reading_log_finished ON reading_log(finished_at, id);
ALTER TABLE books ADD COLUMN published_year INTEGER NULL;
ALTER TABLE books ADD COLUMN avg_rating REAL NULL;
UPDATE meta SET value=json_set(value, '$.read_shelf', 'read') WHERE key='sync' AND json_type(value, '$.read_shelf') IS NULL;`)
	return err
}

// Saved rating estimates, one per book. basis_fingerprint hashes the inputs,
// so an estimate is stale once the reader's rated history or the book
// changes. They are a cache: not exported, and safe to drop.
func migrateRatingEstimates(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE rating_estimates (
 book_id INTEGER PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
 basis_fingerprint TEXT NOT NULL,
 baseline REAL NULL CHECK(baseline BETWEEN 1 AND 5),
 ai REAL NULL CHECK(ai BETWEEN 1 AND 5),
 confidence TEXT NULL CHECK(confidence IN ('low','medium','high')),
 reasons TEXT NOT NULL DEFAULT '[]',
 model TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL
);`)
	return err
}

// moved_by_hand marks a queued book the reader moved with the arrows or Move
// to #, so suggestions can treat its place as the reader's choice. Applying a
// suggestion clears it; no existing book has been marked.
func migrateMovedByHand(tx *sql.Tx) error {
	_, err := tx.Exec("ALTER TABLE books ADD COLUMN moved_by_hand INTEGER NOT NULL DEFAULT 0")
	return err
}

// Goodreads ratings come in quarter stars, so reading_log.rating becomes a
// number from 0.25 to 5. SQLite cannot change a CHECK in place, so the table
// is rebuilt with its rows and indexes. Nothing references reading_log.
// Rollback: restore the pre-v9 backup the migration framework writes.
func migrateQuarterStars(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE reading_log_new (
 id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id),
 finished_at TEXT NULL, rating REAL NULL CHECK(rating BETWEEN 0.25 AND 5),
 source TEXT NOT NULL CHECK(source IN ('goodreads','manual')),
 review_chars INTEGER NOT NULL DEFAULT 0, review TEXT NULL, fingerprint TEXT NOT NULL UNIQUE
);
INSERT INTO reading_log_new SELECT id,book_id,finished_at,rating,source,review_chars,review,fingerprint FROM reading_log;
DROP TABLE reading_log;
ALTER TABLE reading_log_new RENAME TO reading_log;
CREATE INDEX reading_log_book ON reading_log(book_id, finished_at);
CREATE INDEX reading_log_finished ON reading_log(finished_at, id);`)
	return err
}

// Descriptions come from the reader or from an opt-in lookup in a global book
// database (see lookup.go). description_source names the database, and is
// empty for text the reader wrote or an import supplied. Both default to
// empty, so existing books are unchanged.
//
// Backup: the migration framework writes <database>.pre-v10.bak before
// applying this. Rollback: stop the app, put that file back as the database,
// and run the previous release; this release's exports restore only into this
// release or newer, so export from the older release after rolling back.
func migrateDescriptions(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE books ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE books ADD COLUMN description_source TEXT NOT NULL DEFAULT '';`)
	return err
}

// progress_page is the page the reader has reached in a book they are
// reading, and expected_ready the reader's own estimate (YYYY-MM-DD) of when
// a library hold will be ready. Both are optional and default to empty, so
// existing books and loans are unchanged. The yearly reading goal lives in the
// preferences, not in the schema.
//
// Backup: the migration framework writes <database>.pre-v11.bak before
// applying this. Rollback: stop the app, put that file back as the database,
// and run the previous release; export from the older release after rolling
// back, because this release's exports carry fields the older one ignores.
func migrateProgressAndHolds(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE books ADD COLUMN progress_page INTEGER NULL;
ALTER TABLE loans ADD COLUMN expected_ready TEXT NOT NULL DEFAULT '';`)
	return err
}

// progress_percent is how far along the reader is in a book they track as a
// percentage instead of a page, such as an audiobook. A book has at most one
// of progress_page and progress_percent. It defaults to empty, so existing
// books are unchanged.
//
// Backup: the migration framework writes <database>.pre-v12.bak before
// applying this. Rollback: stop the app, put that file back as the database,
// and run the previous release.
func migrateProgressPercent(tx *sql.Tx) error {
	_, err := tx.Exec("ALTER TABLE books ADD COLUMN progress_percent INTEGER NULL CHECK(progress_percent BETWEEN 0 AND 100)")
	return err
}

// books.reason is the reader's private "why I want to read this", and
// book_notes holds their dated notes about a book. Both are local writing:
// imports and lookups never touch them and no AI request carries them. version
// counts saves, so an edit made from a stale page is refused. Existing books
// get an empty reason and no notes.
//
// Backup: the migration framework writes <database>.pre-v13.bak before
// applying this. Rollback: stop the app, put that file back as the database,
// and run the previous release; notes written since are lost, so download an
// export first if you want to keep them.
func migratePrivateNotes(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE books ADD COLUMN reason TEXT NOT NULL DEFAULT '';
CREATE TABLE book_notes (
 id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id),
 body TEXT NOT NULL, page INTEGER NULL, percent INTEGER NULL CHECK(percent BETWEEN 0 AND 100),
 version INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX book_notes_book ON book_notes(book_id, created_at, id);`)
	return err
}

// books.owned is the reader's own flag that they have a copy (default 0), and
// saved_views holds named, versioned filter definitions for the All books
// list. A saved view only selects and sorts books; it never moves them.
//
// Backup: the migration framework writes <database>.pre-v14.bak before
// applying this. Rollback: stop the app, put that file back as the database,
// and run the previous release; ownership flags and saved views made since
// are lost, so download an export first if you want to keep them.
func migrateSavedViews(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE books ADD COLUMN owned INTEGER NOT NULL DEFAULT 0 CHECK(owned IN (0,1));
CREATE TABLE saved_views (
 id INTEGER PRIMARY KEY, name TEXT NOT NULL, definition TEXT NOT NULL,
 version INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX saved_views_name ON saved_views(name COLLATE NOCASE);`)
	return err
}

// reading_attempts has one row per time a book is read (a reread is a new
// attempt), with the date it was started (empty when unknown, as for a book
// already being read before this version) and ended. progress_checkpoints are
// the dated progress updates saved while the reader keeps progress history
// (off by default, a preference). A checkpoint with a NULL value records that
// progress was cleared. Nothing is created for existing books, and no start
// date or checkpoint is invented for them.
//
// Backup: the migration framework writes <database>.pre-v15.bak before
// applying this. Rollback: stop the app, put that file back as the database,
// and run the previous release; attempts and updates made since are lost, so
// download an export first if you want to keep them.
func migrateProgressHistory(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE reading_attempts (
 id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id),
 started_on TEXT NOT NULL DEFAULT '', ended_on TEXT NULL
);
CREATE INDEX reading_attempts_book ON reading_attempts(book_id, id);
CREATE TABLE progress_checkpoints (
 id INTEGER PRIMARY KEY, attempt_id INTEGER NOT NULL REFERENCES reading_attempts(id),
 book_id INTEGER NOT NULL REFERENCES books(id), day TEXT NOT NULL,
 unit TEXT NOT NULL CHECK(unit IN ('page','percent')), value INTEGER NULL CHECK(value IS NULL OR value >= 0),
 total_pages INTEGER NULL, version INTEGER NOT NULL DEFAULT 1, UNIQUE(attempt_id, day)
);
CREATE INDEX progress_checkpoints_book ON progress_checkpoints(book_id, day);`)
	return err
}

// event_corrections records the reader's edit or void of a library event,
// apart from the event itself, which keeps the observation as it was
// imported. It is keyed by the event's fingerprint, the identity that replays
// of the same CSV and merges follow, so a replay can neither undo a
// correction nor bring a voided event back. A void keeps empty values. No
// existing event is changed.
//
// Backup: the migration framework writes <database>.pre-v16.bak before
// applying this. Rollback: stop the app, put that file back as the database,
// and run the previous release; corrections made since are lost, so download
// an export first if you want to keep them.
func migrateEventCorrections(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE event_corrections (
 fingerprint TEXT PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id),
 action TEXT NOT NULL CHECK(action IN ('edit','void')),
 source TEXT NOT NULL DEFAULT '', format TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '',
 at TEXT NOT NULL DEFAULT '', due_date TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX event_corrections_book ON event_corrections(book_id);`)
	return err
}

// No books change. Backup: <database>.pre-v17.bak. Rollback: stop the app,
// restore that backup and remove stray WAL/SHM files before the previous image.
func migrateHardcoverLink(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE hardcover_link (
 id INTEGER PRIMARY KEY CHECK(id=1), hardcover_user_id INTEGER NOT NULL,
 username TEXT NOT NULL, linked_at TEXT NOT NULL
 );`)
	return err
}

// book_hardcover links a local book to its Hardcover book (and edition when
// known). It is a side table, not columns on books, so ranking, listing and
// plan code that read books never see it. The unique hardcover_book_id keeps
// two local books from claiming one Hardcover book. hardcover_alias remembers
// the Hardcover id of a deleted or merged book under its identity, so a later
// pull cannot recreate or relink it wrongly. Existing books are unlinked.
//
// Backup: <database>.pre-v18.bak. Rollback: stop the app, restore that file,
// remove stray WAL/SHM files, and start the previous image; links made since
// are lost, and an older image refuses to open this schema.
func migrateHardcoverBookLinks(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE book_hardcover (
 book_id INTEGER PRIMARY KEY REFERENCES books(id), hardcover_book_id INTEGER NOT NULL UNIQUE,
 hardcover_edition_id INTEGER NULL, slug TEXT NOT NULL DEFAULT '', linked_at TEXT NOT NULL
);
CREATE TABLE hardcover_alias (identity TEXT PRIMARY KEY, hardcover_book_id INTEGER NOT NULL);
CREATE INDEX hardcover_alias_book ON hardcover_alias(hardcover_book_id);`)
	return err
}

// reading_log gains a third source, 'hardcover', a finished_precision (1 day,
// 2 month, 3 year) so a month-only or year-only date is never shown as a day,
// and hardcover_read_id, which ties a row to one Hardcover read so a repeat
// pull updates it instead of adding a duplicate. A date with precision 2 or 3
// is stored as the first day of its month or year; the precision says the rest
// is unknown. SQLite cannot change a CHECK in place, so the table is rebuilt
// with its rows and indexes, as migration 9 did. Existing rows keep precision 1.
//
// Backup: <database>.pre-v19.bak. Rollback: stop the app, restore that file,
// remove stray WAL/SHM files, and start the previous image; readings from
// Hardcover added since are lost.
func migrateHardcoverReadings(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE reading_log_new (
 id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id),
 finished_at TEXT NULL, rating REAL NULL CHECK(rating BETWEEN 0.25 AND 5),
 source TEXT NOT NULL CHECK(source IN ('goodreads','manual','hardcover')),
 review_chars INTEGER NOT NULL DEFAULT 0, review TEXT NULL, fingerprint TEXT NOT NULL UNIQUE,
 finished_precision INTEGER NOT NULL DEFAULT 1 CHECK(finished_precision IN (1,2,3)),
 hardcover_read_id INTEGER NULL
);
INSERT INTO reading_log_new(id,book_id,finished_at,rating,source,review_chars,review,fingerprint) SELECT id,book_id,finished_at,rating,source,review_chars,review,fingerprint FROM reading_log;
DROP TABLE reading_log;
ALTER TABLE reading_log_new RENAME TO reading_log;
CREATE INDEX reading_log_book ON reading_log(book_id, finished_at);
CREATE INDEX reading_log_finished ON reading_log(finished_at, id);
CREATE UNIQUE INDEX reading_log_hardcover_read ON reading_log(hardcover_read_id) WHERE hardcover_read_id IS NOT NULL;`)
	return err
}

// hardcover_sync records what was sent to Hardcover, so a later pull can tell
// "Hardcover changed" from "we changed it". Fields are status, rating, and
// read (the id of a read the Hardcover server created for a status change).
// History per book is capped and is cleared when a book is unlinked, relinked,
// merged away, or deleted. Existing books have none.
//
// Backup: <database>.pre-v20.bak. Rollback: stop the app, restore that file,
// remove stray WAL/SHM files, and start the previous image; the history is
// lost, and an older image refuses to open this schema.
func migrateHardcoverSync(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE hardcover_sync (
 id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id),
 field TEXT NOT NULL CHECK(field IN ('status','rating','read')),
 local_value TEXT NOT NULL DEFAULT '', remote_value TEXT NOT NULL DEFAULT '',
 direction TEXT NOT NULL CHECK(direction IN ('push','pull')), at TEXT NOT NULL
);
CREATE INDEX hardcover_sync_book ON hardcover_sync(book_id, id);`)
	return err
}

// book_hardcover gains progress_sync, the reader's per-book opt-in to send
// reading progress to Hardcover (off for every existing link), and the sync
// history accepts the fields progress and dates. SQLite cannot change a CHECK,
// so hardcover_sync is rebuilt with its rows.
//
// Backup: <database>.pre-v21.bak. Rollback: stop the app, restore that file,
// remove stray WAL/SHM files, and start the previous image; opt-ins and
// progress history made since are lost, and an older image refuses to open
// this schema.
func migrateHardcoverProgress(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE book_hardcover ADD COLUMN progress_sync INTEGER NOT NULL DEFAULT 0 CHECK(progress_sync IN (0,1));
CREATE TABLE hardcover_sync_new (
 id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id),
 field TEXT NOT NULL CHECK(field IN ('status','rating','read','progress','dates')),
 local_value TEXT NOT NULL DEFAULT '', remote_value TEXT NOT NULL DEFAULT '',
 direction TEXT NOT NULL CHECK(direction IN ('push','pull')), at TEXT NOT NULL
);
INSERT INTO hardcover_sync_new SELECT id,book_id,field,local_value,remote_value,direction,at FROM hardcover_sync;
DROP TABLE hardcover_sync;
ALTER TABLE hardcover_sync_new RENAME TO hardcover_sync;
CREATE INDEX hardcover_sync_book ON hardcover_sync(book_id, id);`)
	return err
}

// hardcover_catalog and series_cache cache what a reader-requested refresh of a
// linked book read from Hardcover's catalog: a community average, a cover URL
// (never the image), and the ordered members of the book's series. They are
// caches of remote data: nothing else reads them for ranking, they are left out
// of exports, and dropping them loses nothing. Existing books have none.
//
// Backup: <database>.pre-v22.bak. Rollback: stop the app, restore that file,
// remove stray WAL/SHM files, and start the previous image; the caches are
// simply rebuilt by refreshing again, and an older image refuses to open this
// schema.
func migrateHardcoverCatalog(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE hardcover_catalog (
 book_id INTEGER PRIMARY KEY REFERENCES books(id), rating REAL NULL CHECK(rating BETWEEN 0 AND 5),
 ratings_count INTEGER NOT NULL DEFAULT 0, image_url TEXT NOT NULL DEFAULT '', fetched_at TEXT NOT NULL
);
CREATE TABLE series_cache (
 source_book_id INTEGER NOT NULL REFERENCES books(id), series_id INTEGER NOT NULL, series_name TEXT NOT NULL,
 series_total INTEGER NOT NULL DEFAULT 0, hardcover_book_id INTEGER NOT NULL, position REAL NULL,
 title TEXT NOT NULL, author TEXT NOT NULL DEFAULT '', year INTEGER NULL, pages INTEGER NULL, fetched_at TEXT NOT NULL,
 PRIMARY KEY(source_book_id, series_id, hardcover_book_id)
);`)
	return err
}

// migrateAudioProgress keeps existing positions and checkpoint versions intact.
func migrateAudioProgress(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE books ADD COLUMN progress_seconds INTEGER NULL CHECK(progress_seconds BETWEEN 0 AND 35999999);
CREATE TABLE progress_checkpoints_audio (
 id INTEGER PRIMARY KEY, attempt_id INTEGER NOT NULL REFERENCES reading_attempts(id),
 book_id INTEGER NOT NULL REFERENCES books(id), day TEXT NOT NULL,
 unit TEXT NOT NULL CHECK(unit IN ('page','percent','seconds')), value INTEGER NULL CHECK(value IS NULL OR value >= 0),
 total_pages INTEGER NULL, version INTEGER NOT NULL DEFAULT 1, UNIQUE(attempt_id, day)
);
INSERT INTO progress_checkpoints_audio SELECT * FROM progress_checkpoints;
DROP TABLE progress_checkpoints;
ALTER TABLE progress_checkpoints_audio RENAME TO progress_checkpoints;
CREATE INDEX progress_checkpoints_book ON progress_checkpoints(book_id, day);`)
	return err
}

func migrateHardcoverNotes(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE book_notes ADD COLUMN hardcover_journal_id INTEGER NOT NULL DEFAULT 0 CHECK(hardcover_journal_id >= 0);
ALTER TABLE book_notes ADD COLUMN hardcover_user_id INTEGER NOT NULL DEFAULT 0 CHECK(hardcover_user_id >= 0);
ALTER TABLE book_notes ADD COLUMN hardcover_book_id INTEGER NOT NULL DEFAULT 0 CHECK(hardcover_book_id >= 0);`)
	return err
}

func migrateAudioDuration(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE books ADD COLUMN audio_seconds INTEGER CHECK(audio_seconds BETWEEN 1 AND 35999999);
 ALTER TABLE progress_checkpoints ADD COLUMN total_seconds INTEGER CHECK(total_seconds BETWEEN 1 AND 35999999);`)
	return err
}

// Existing books/readings keep an unknown start; no historical dates are invented.
func migrateReadingStartDates(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE books ADD COLUMN started_at TEXT NULL;
 ALTER TABLE books ADD COLUMN started_manual INTEGER NOT NULL DEFAULT 0 CHECK(started_manual IN (0,1));
 ALTER TABLE reading_log ADD COLUMN started_at TEXT NULL;
 DELETE FROM plans; DELETE FROM undo_queue;`)
	return err
}
