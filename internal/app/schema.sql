-- Schema version 1 baseline, applied by migration 1 in migrate.go. Do not edit:
-- change the schema by appending a migration instead.

CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS books (
 id INTEGER PRIMARY KEY, title TEXT NOT NULL, author TEXT NOT NULL, identity TEXT NOT NULL UNIQUE,
 isbn TEXT NOT NULL DEFAULT '', goodreads_id TEXT NOT NULL DEFAULT '', pages INTEGER,
 tags TEXT NOT NULL DEFAULT '', interest INTEGER NOT NULL DEFAULT 3 CHECK(interest BETWEEN 1 AND 5),
 reading_status TEXT NOT NULL DEFAULT 'to-read', position INTEGER NOT NULL,
 pinned INTEGER NOT NULL DEFAULT 0, needs_placement INTEGER NOT NULL DEFAULT 1,
 series TEXT NOT NULL DEFAULT '', series_number REAL, added_at TEXT NOT NULL, source TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS goodreads_id_unique ON books(goodreads_id) WHERE goodreads_id != '';
CREATE TABLE IF NOT EXISTS loans (
 book_id INTEGER NOT NULL REFERENCES books(id), source TEXT NOT NULL, format TEXT NOT NULL,
 status TEXT NOT NULL, due_date TEXT NOT NULL, observed_at TEXT NOT NULL,
 PRIMARY KEY(book_id, source, format)
);
CREATE TABLE IF NOT EXISTS events (
 id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books(id), source TEXT NOT NULL,
 format TEXT NOT NULL, status TEXT NOT NULL, activity TEXT NOT NULL, at TEXT NOT NULL,
 due_date TEXT NOT NULL, fingerprint TEXT UNIQUE NOT NULL
);
CREATE TABLE IF NOT EXISTS plans (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL, revision INTEGER NOT NULL, created_at REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS undo_queue (id INTEGER PRIMARY KEY CHECK(id=1), payload TEXT NOT NULL, revision INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS login_attempts (at REAL NOT NULL);
