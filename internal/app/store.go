package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"encoding/json"
	"errors"
	"modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
)

//go:embed schema.sql
var schema string

func init() {
	// nc_lower is strings.ToLower. Unlike SQLite's lower(), it folds non-ASCII
	// letters, so list searches match the same books for "élan" and "Élan".
	sqlite.MustRegisterDeterministicScalarFunction("nc_lower", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		switch v := args[0].(type) {
		case string:
			return strings.ToLower(v), nil
		case []byte:
			return strings.ToLower(string(v)), nil
		}
		return args[0], nil
	})
}

type Store struct{ DB *sql.DB }
type queryer interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
	Prepare(string) (*sql.Stmt, error)
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
