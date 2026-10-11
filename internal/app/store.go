package app

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	for _, stmt := range []string{"PRAGMA busy_timeout=15000", "PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", schema} {
		if _, err = db.Exec(stmt); err != nil {
			return fail(err)
		}
	}
	s := &Store{db}
	// Existing v0.1 libraries retain their schema and content. Normalize timestamps
	// once so chronological comparisons remain consistent across both runtimes.
	err = s.Write(func(tx *sql.Tx) error {
		version, e := readMeta(tx, "go_data_version", 0)
		if e != nil {
			return e
		}
		if version >= 1 {
			return nil
		}
		for _, table := range []string{"events", "loans"} {
			column := "at"
			if table == "loans" {
				column = "observed_at"
			}
			rows, e := tx.Query("SELECT rowid," + column + " FROM " + table)
			if e != nil {
				return e
			}
			type item struct {
				id int64
				at string
			}
			items := []item{}
			for rows.Next() {
				var x item
				if e = rows.Scan(&x.id, &x.at); e != nil {
					rows.Close()
					return e
				}
				items = append(items, x)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			for _, x := range items {
				at, e := parseTime(x.at)
				if e != nil {
					return fmt.Errorf("migrate %s timestamp: %w", table, e)
				}
				if _, e = tx.Exec("UPDATE "+table+" SET "+column+"=? WHERE rowid=?", at, x.id); e != nil {
					return e
				}
			}
		}
		if _, e = tx.Exec("DELETE FROM plans; DELETE FROM undo_queue"); e != nil {
			return e
		}
		return setMeta(tx, "go_data_version", 1)
	})
	if err != nil {
		return fail(err)
	}
	return s, nil
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
func findBook(q queryer, r Record) (int64, error) {
	var id int64
	for _, pair := range [][2]string{{"goodreads_id", r.GoodreadsID}, {"isbn", r.ISBN}, {"identity", identity(r.Title, r.Author)}} {
		if pair[1] == "" {
			continue
		}
		// ISBN matches are accepted only when they identify one local book.
		err := q.QueryRow("SELECT MIN(id) FROM books WHERE "+pair[0]+"=? HAVING count(*)=1", pair[1]).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	return 0, nil
}
func importRecords(q queryer, records []Record) (added, eventCount int, changed bool, err error) {
	for _, r := range records {
		id, e := findBook(q, r)
		if e != nil {
			return 0, 0, false, e
		}
		if id == 0 {
			result, e := q.Exec(`INSERT INTO books(title,author,identity,isbn,goodreads_id,pages,tags,interest,reading_status,position,series,series_number,added_at,source)
    VALUES(?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(position),-1)+1 FROM books),?,?,?,?)`, r.Title, r.Author, identity(r.Title, r.Author), r.ISBN, r.GoodreadsID, r.Pages, r.Tags, 3, r.Status, r.Series, r.SeriesNumber, canonicalTime(time.Now()), r.Source)
			if e != nil {
				return 0, 0, false, e
			}
			id, e = result.LastInsertId()
			if e != nil {
				return 0, 0, false, e
			}
			added++
			changed = true
		} else {
			for _, pair := range [][2]string{{"goodreads_id", r.GoodreadsID}, {"isbn", r.ISBN}} {
				if pair[1] != "" {
					res, e := q.Exec("UPDATE books SET "+pair[0]+"=? WHERE id=? AND "+pair[0]+"=''", pair[1], id)
					if e != nil {
						return 0, 0, false, e
					}
					n, e := res.RowsAffected()
					if e != nil {
						return 0, 0, false, e
					}
					changed = changed || n > 0
				}
			}
			if r.Pages != nil {
				res, e := q.Exec("UPDATE books SET pages=? WHERE id=? AND pages IS NULL", *r.Pages, id)
				if e != nil {
					return 0, 0, false, e
				}
				n, e := res.RowsAffected()
				if e != nil {
					return 0, 0, false, e
				}
				changed = changed || n > 0
			}
		}
		if r.Event != nil {
			e := r.Event
			var existing int
			if err = q.QueryRow(`SELECT count(*) FROM events WHERE fingerprint=? OR (book_id=? AND source=? AND format=? AND status=? AND at=? AND due_date=?)`, fingerprint(r), id, r.Source, e.Format, e.Status, e.At, e.DueDate).Scan(&existing); err != nil {
				return 0, 0, false, err
			}
			if existing == 0 {
				if _, err = q.Exec(`INSERT INTO events(book_id,source,format,status,activity,at,due_date,fingerprint) VALUES(?,?,?,?,?,?,?,?)`, id, r.Source, e.Format, e.Status, e.Activity, e.At, e.DueDate, fingerprint(r)); err != nil {
					return 0, 0, false, err
				}
				eventCount++
				changed = true
			}
			// Identical reimports do not invalidate queue previews or undo.
			res, err := q.Exec(`INSERT INTO loans VALUES(?,?,?,?,?,?) ON CONFLICT(book_id,source,format) DO UPDATE SET status=excluded.status,due_date=excluded.due_date,observed_at=excluded.observed_at
    WHERE excluded.observed_at>loans.observed_at OR (excluded.observed_at=loans.observed_at AND excluded.status=loans.status AND excluded.due_date!=loans.due_date)`, id, r.Source, e.Format, e.Status, e.DueDate, e.At)
			if err != nil {
				return 0, 0, false, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, 0, false, err
			}
			changed = changed || n > 0
		}
	}
	return added, eventCount, changed, nil
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
	_, err = copyDB.Exec("DELETE FROM plans; DELETE FROM login_attempts")
	closeErr := copyDB.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return os.ReadFile(path)
}
