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
