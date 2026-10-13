package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// pythonEraDB writes a v0.1 (schema version 1) library with non-canonical
// timestamps, a pending preview, and an undo snapshot, as the Python app left it.
func pythonEraDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{schema,
		"INSERT INTO meta VALUES ('revision', '7'), ('schema_version', '1')",
		`INSERT INTO books(id,title,author,identity,interest,reading_status,position,pinned,needs_placement,added_at,source) VALUES(1,'Legacy','Author','legacy|author',5,'to-read',0,1,0,'2026-10-01T00:00:00+00:00','nypl')`,
		`INSERT INTO events(book_id,source,format,status,activity,at,due_date,fingerprint) VALUES(1,'nypl','print','borrowed','Borrowed','2026-10-10T12:30:00+00:00','','python-fingerprint')`,
		`INSERT INTO loans VALUES(1,'nypl','print','borrowed','','2026-10-10T12:30:00+00:00')`,
		`INSERT INTO plans VALUES('old-plan','reorder','{}',7,1e12)`,
		`INSERT INTO undo_queue VALUES(1,'[]',7)`,
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path
}
func backups(t *testing.T, path string) []string {
	t.Helper()
	found, err := filepath.Glob(path + ".pre-v*.bak")
	if err != nil {
		t.Fatal(err)
	}
	return found
}
func versionOf(t *testing.T, path string) (int, bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, legacy, err := schemaVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	return v, legacy
}
func scalar[T any](t *testing.T, path, query string) T {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v T
	if err = db.QueryRow(query).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

func TestMigrateFreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.sqlite3")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if v, legacy := versionOf(t, path); v != latestVersion(migrations) || legacy {
		t.Fatalf("version %d legacy %v", v, legacy)
	}
	for _, table := range []string{"meta", "books", "events", "loans", "plans", "undo_queue", "login_attempts", "book_aliases", "login_failures"} {
		if n := scalar[int](t, path, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='"+table+"'"); n != 1 {
			t.Fatalf("missing table %s", table)
		}
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM meta WHERE key='go_data_version'"); n != 0 {
		t.Fatal("go_data_version written")
	}
	if rev := scalar[int](t, path, "SELECT value FROM meta WHERE key='revision'"); rev != 0 {
		t.Fatal(rev)
	}
	if b := backups(t, path); len(b) != 0 {
		t.Fatalf("new database was backed up: %v", b)
	}
	if mode := scalar[string](t, path, "PRAGMA journal_mode"); mode != "wal" {
		t.Fatal(mode)
	}
}

func TestMigratePythonEraDatabase(t *testing.T) {
	path := pythonEraDB(t)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, legacy := versionOf(t, path); v != latestVersion(migrations) || legacy {
		t.Fatalf("version %d legacy %v", v, legacy)
	}
	books, err := loadBooks(s.DB)
	if err != nil || len(books) != 1 || !books[0].Pinned || books[0].Interest != 5 {
		t.Fatalf("%v %v", books, err)
	}
	events, err := loadEvents(s.DB, 0, 0)
	if err != nil || len(events) != 1 || events[0].At != "2026-10-10T12:30:00.000000000+00:00" || books[0].Loans[0].ObservedAt != events[0].At {
		t.Fatalf("timestamps not canonical: %v %v", events, err)
	}
	for _, table := range []string{"plans", "undo_queue"} {
		if n := scalar[int](t, path, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatalf("%s not cleared", table)
		}
	}
	found := backups(t, path)
	if len(found) != 1 || found[0] != path+".pre-v2.bak" {
		t.Fatalf("backups %v", found)
	}
	info, err := os.Stat(found[0])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup mode %v %v", info, err)
	}
	// The backup is the untouched pre-upgrade library.
	if v, _ := versionOf(t, found[0]); v != 1 {
		t.Fatalf("backup version %d", v)
	}
	if at := scalar[string](t, found[0], "SELECT at FROM events"); at != "2026-10-10T12:30:00+00:00" {
		t.Fatal(at)
	}
	if n := scalar[int](t, found[0], "SELECT count(*) FROM plans"); n != 1 {
		t.Fatal("backup lost plans")
	}
}

func TestMigrateGoDataVersionIsVersionTwo(t *testing.T) {
	path := pythonEraDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// A database already upgraded by the Go app before the migration runner.
	if _, err = db.Exec("INSERT INTO meta VALUES('go_data_version','1')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if v, legacy := versionOf(t, path); v != 2 || !legacy {
		t.Fatalf("version %d legacy %v", v, legacy)
	}
	if err = migrateFile(path, migrations[:2]); err != nil {
		t.Fatal(err)
	}
	if v, legacy := versionOf(t, path); v != 2 || legacy {
		t.Fatalf("version %d legacy %v", v, legacy)
	}
	// Migration 2 did not run again: no rewrite, previews kept, no backup needed.
	if at := scalar[string](t, path, "SELECT at FROM events"); at != "2026-10-10T12:30:00+00:00" {
		t.Fatal("rewrote data")
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM plans"); n != 1 {
		t.Fatal("cleared plans")
	}
	if b := backups(t, path); len(b) != 0 {
		t.Fatalf("unneeded backup %v", b)
	}
	// A later pending migration does back it up.
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v3.bak" {
		t.Fatalf("backups %v", b)
	}
}

func TestMigrateInjectedVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	base := migrations[:2]
	if err := migrateFile(path, base); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO books(title,author,identity,position,added_at,source) VALUES('Kept','A','kept|a',0,'x','manual')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	failing := append(append([]migration{}, base...), migration{3, "fails", func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE half_done(x); UPDATE books SET title='changed'"); err != nil {
			return err
		}
		return errors.New("boom")
	}})
	for range 2 {
		if err = migrateFile(path, failing); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("expected failure, got %v", err)
		}
	}
	if v, _ := versionOf(t, path); v != 2 {
		t.Fatalf("version %d after failed migration", v)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM sqlite_master WHERE name='half_done'"); n != 0 {
		t.Fatal("partial migration committed")
	}
	if title := scalar[string](t, path, "SELECT title FROM books"); title != "Kept" {
		t.Fatal(title)
	}
	// Backups are kept and never overwritten.
	if b := backups(t, path); len(b) != 2 || b[0] != path+".pre-v3.1.bak" || b[1] != path+".pre-v3.bak" {
		t.Fatalf("backups %v", b)
	}

	working := append(append([]migration{}, base...), migration{3, "adds notes", func(tx *sql.Tx) error {
		_, err := tx.Exec("ALTER TABLE books ADD COLUMN notes TEXT NOT NULL DEFAULT ''")
		return err
	}})
	if err = migrateFile(path, working); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 3 {
		t.Fatal(v)
	}
	if title := scalar[string](t, path, "SELECT title||notes FROM books"); title != "Kept" {
		t.Fatal(title)
	}
	if b := backups(t, path); len(b) != 3 {
		t.Fatalf("backups %v", b)
	}
}

func TestMigrateRefusesNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite3")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = setMeta(s.DB, "schema_version", latestVersion(migrations)+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = OpenStore(path); err == nil || !strings.Contains(err.Error(), "newer than this app supports") {
		t.Fatalf("opened newer schema: %v", err)
	}
}

func TestMigrateSecondStartIsNoOp(t *testing.T) {
	path := pythonEraDB(t)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	watcher, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	conn, err := watcher.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var before, after int64
	if err = conn.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// data_version changes on this connection only when another one commits.
	if err = conn.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("second start wrote to the database")
	}
	if b := backups(t, path); len(b) != 1 {
		t.Fatalf("backups %v", b)
	}
}

func TestMigrateConcurrentStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.sqlite3")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			s, err := OpenStore(path)
			if err == nil {
				s.Close()
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := versionOf(t, path); v != latestVersion(migrations) {
		t.Fatal(v)
	}

	// Each pending step runs exactly once even when two processes race.
	path = filepath.Join(t.TempDir(), "counted.sqlite3")
	var applied atomic.Int32
	counted := []migration{migrations[0], {2, "counted", func(tx *sql.Tx) error {
		applied.Add(1)
		_, err := tx.Exec("CREATE TABLE counted(x)")
		return err
	}}}
	for i := range errs {
		wg.Go(func() { errs[i] = migrateFile(path, counted) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if applied.Load() != 1 {
		t.Fatalf("migration applied %d times", applied.Load())
	}
}

func TestMigrationTransactionTakesWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.sqlite3")
	if err := migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", migrationDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// A deferred BEGIN would take no lock until its first write.
	if _, err = other.Exec("INSERT INTO meta VALUES('probe','1')"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("write succeeded while migration transaction was open: %v", err)
	}
}

// Builds before the migration runner created login_failures at every start, so
// migration 4 must accept a database that already has it.
func TestMigrateLoginFailuresAlreadyPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:3]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"CREATE TABLE login_failures (client TEXT NOT NULL, at REAL NOT NULL)", "CREATE INDEX login_failures_client ON login_failures(client, at)", "INSERT INTO login_failures VALUES('10.0.0.1', 1)"} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if err = migrateFile(path, migrations[:4]); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 4 {
		t.Fatal(v)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM login_failures"); n != 1 {
		t.Fatal("lost login failures")
	}
}

// Migration 5 only adds indexes: a version 4 library keeps its data, gains the
// indexes, and is backed up first.
func TestMigrateIndexesFromVersionFour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:4]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	err = s.Write(func(tx *sql.Tx) error {
		_, err := importRecords(tx, []Record{{Book: Book{Title: "Kept", Author: "Writer", Status: "to-read", Source: "manual"}}, libbyLoan("Borrowed")})
		return err
	})
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 5 {
		t.Fatal(v)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('books_status_position','books_position','books_isbn','events_book','events_at')"); n != 5 {
		t.Fatalf("%d indexes", n)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM books") + scalar[int](t, path, "SELECT count(*) FROM events"); n != 3 {
		t.Fatal("lost data")
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v5.bak" {
		t.Fatalf("backups %v", b)
	}
}
