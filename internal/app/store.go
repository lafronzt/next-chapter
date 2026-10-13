package app

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
)

//go:embed schema.sql
var schema string

type Store struct{ DB *sql.DB }
type queryer interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := migrateFile(path, migrations); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(15000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Write(fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func revision(q queryer) (int64, error) {
	var value int64
	err := q.QueryRow("SELECT CAST(value AS INTEGER) FROM meta WHERE key='revision'").Scan(&value)
	return value, err
}
func bump(q queryer) error {
	_, err := q.Exec("UPDATE meta SET value=CAST(value AS INTEGER)+1 WHERE key='revision'")
	return err
}
func readMeta[T any](q queryer, key string, fallback T) (T, error) {
	var raw string
	err := q.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return fallback, err
	}
	var result T
	err = json.Unmarshal([]byte(raw), &result)
	return result, err
}
func setMeta(q queryer, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = q.Exec("INSERT INTO meta VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, string(raw))
	return err
}
func loadBooks(q queryer) ([]Book, error) {
	rows, err := q.Query(`SELECT id,title,author,identity,isbn,goodreads_id,pages,tags,interest,reading_status,position,pinned,needs_placement,series,series_number,added_at,source FROM books ORDER BY position,id`)
	if err != nil {
		return nil, err
	}
	result := []Book{}
	lookup := map[int64]int{}
	for rows.Next() {
		var b Book
		err = rows.Scan(&b.ID, &b.Title, &b.Author, &b.Identity, &b.ISBN, &b.GoodreadsID, &b.Pages, &b.Tags, &b.Interest, &b.Status, &b.Position, &b.Pinned, &b.NeedsPlacement, &b.Series, &b.SeriesNumber, &b.AddedAt, &b.Source)
		if err != nil {
			rows.Close()
			return nil, err
		}
		b.Loans = []Loan{}
		lookup[b.ID] = len(result)
		result = append(result, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = q.Query("SELECT book_id,source,format,status,due_date,observed_at FROM loans ORDER BY observed_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Loan
		if err = rows.Scan(&l.BookID, &l.Source, &l.Format, &l.Status, &l.DueDate, &l.ObservedAt); err != nil {
			return nil, err
		}
		if i, ok := lookup[l.BookID]; ok {
			result[i].Loans = append(result[i].Loans, l)
		}
	}
	return result, rows.Err()
}
func loadEvents(q queryer, bid int64, limit int) ([]Event, error) {
	statement := `SELECT e.id,e.book_id,e.source,e.format,e.status,e.activity,e.at,e.due_date,e.fingerprint,b.title,b.author FROM events e JOIN books b ON b.id=e.book_id`
	args := []any{}
	if bid != 0 {
		statement += " WHERE e.book_id=?"
		args = append(args, bid)
	}
	statement += " ORDER BY e.at DESC,e.id DESC"
	if limit > 0 {
		statement += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := q.Query(statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.ID, &e.BookID, &e.Source, &e.Format, &e.Status, &e.Activity, &e.At, &e.DueDate, &e.Fingerprint, &e.Title, &e.Author); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

func reorder(q queryer, ids []int64) error {
	for pos, id := range ids {
		if _, err := q.Exec("UPDATE books SET position=? WHERE id=?", pos, id); err != nil {
			return err
		}
	}
	return nil
}

// VACUUM INTO takes a consistent SQLite snapshot including committed WAL pages.
func (s *Store) Backup() (data []byte, err error) {
	dir, err := os.MkdirTemp("", "next-chapter-backup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "next-chapter.sqlite3")
	if _, err = s.DB.Exec("VACUUM INTO ?", path); err != nil {
		return nil, err
	}
	copyDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Restoring starts a new session epoch, so cookies issued by this instance
	// are not accepted by a copy restored elsewhere.
	_, err = copyDB.Exec("DELETE FROM plans; DELETE FROM login_attempts; DELETE FROM login_failures; DELETE FROM meta WHERE key='session_epoch'")
	closeErr := copyDB.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return os.ReadFile(path)
}
