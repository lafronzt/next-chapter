import json
import sqlite3
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from .imports import identity, fingerprint

SCHEMA = '''
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT OR IGNORE INTO meta VALUES ('revision', '0');
INSERT OR IGNORE INTO meta VALUES ('schema_version', '1');
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
'''


def now():
    return datetime.now(timezone.utc).isoformat()


class Store:
    def __init__(self, path):
        self.path = str(path)
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        with self.connect() as db:
            db.execute('PRAGMA journal_mode=WAL')
            db.executescript(SCHEMA)

    @contextmanager
    def connect(self, write=False):
        db = sqlite3.connect(self.path, timeout=15)
        db.row_factory = sqlite3.Row
        db.execute('PRAGMA foreign_keys=ON')
        try:
            if write:
                db.execute('BEGIN IMMEDIATE')
            yield db
            db.commit()
        except Exception:
            db.rollback()
            raise
        finally:
            db.close()


def revision(db):
    return int(db.execute("SELECT value FROM meta WHERE key='revision'").fetchone()[0])


def bump(db):
    db.execute("UPDATE meta SET value=CAST(value AS INTEGER)+1 WHERE key='revision'")


def get_meta(db, key, default=None):
    row = db.execute('SELECT value FROM meta WHERE key=?', (key,)).fetchone()
    return json.loads(row[0]) if row else default


def set_meta(db, key, value):
    db.execute('INSERT INTO meta VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value', (key, json.dumps(value)))


def books(db, queue_only=False):
    rows = [dict(row) for row in db.execute('SELECT * FROM books ORDER BY position, id')]
    loans = {}
    for row in db.execute('SELECT * FROM loans ORDER BY observed_at DESC'):
        loans.setdefault(row['book_id'], []).append(dict(row))
    for row in rows:
        row['loans'] = loans.get(row['id'], [])
    return [b for b in rows if b['reading_status'] == 'to-read'] if queue_only else rows


def reorder(db, ids):
    for index, bid in enumerate(ids):
        db.execute('UPDATE books SET position=? WHERE id=?', (index, bid))


def match(db, record):
    if record.get('goodreads_id'):
        found = db.execute('SELECT * FROM books WHERE goodreads_id=?', (record['goodreads_id'],)).fetchone()
        if found:
            return found
    if record.get('isbn'):
        found = db.execute('SELECT * FROM books WHERE isbn=?', (record['isbn'],)).fetchall()
        if len(found) == 1:
            return found[0]
    return db.execute('SELECT * FROM books WHERE identity=?', (identity(record['title'], record['author']),)).fetchone()


def import_records(db, records):
    added = events_added = 0
    for record in records:
        found = match(db, record)
        if not found:
            cursor = db.execute('''INSERT INTO books
                (title,author,identity,isbn,goodreads_id,pages,tags,reading_status,position,added_at,source)
                VALUES (?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(position),-1)+1 FROM books),?,?)''',
                (record['title'], record['author'], identity(record['title'], record['author']),
                 record.get('isbn', ''), record.get('goodreads_id', ''), record.get('pages'), record.get('tags', ''),
                 record.get('reading_status', 'to-read'), now(), record['source']))
            bid = cursor.lastrowid
            added += 1
        else:
            bid = found['id']
            # Once imported, the reader's local status and preferences are authoritative.
            for field in ('goodreads_id', 'isbn', 'pages'):
                if not found[field] and record.get(field):
                    db.execute(f'UPDATE books SET {field}=? WHERE id=?', (record[field], bid))
        event = record.get('event')
        if event:
            inserted = db.execute('''INSERT OR IGNORE INTO events
                (book_id,source,format,status,activity,at,due_date,fingerprint) VALUES (?,?,?,?,?,?,?,?)''',
                (bid, record['source'], event['format'], event['status'], event['activity'],
                 event['at'], event['due_date'], fingerprint(record))).rowcount
            events_added += inserted
            db.execute('''INSERT INTO loans VALUES (?,?,?,?,?,?)
                ON CONFLICT(book_id,source,format) DO UPDATE SET
                status=excluded.status,due_date=excluded.due_date,observed_at=excluded.observed_at
                WHERE excluded.observed_at > loans.observed_at OR
                (excluded.observed_at = loans.observed_at AND excluded.status = loans.status)''',
                (bid, record['source'], event['format'], event['status'], event['due_date'], event['at']))
    return added, events_added
