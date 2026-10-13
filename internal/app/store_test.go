package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportReconciliationAndBackup(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	r := records("Book")[0]
	addRecords(t, a, []Record{r})
	r.Source = "libby"
	r.Status = "unreviewed"
	r.Event = &Event{Format: "ebook", Status: "returned", Activity: "Returned", At: "2026-10-10T00:00:00.000000000+00:00"}
	addRecords(t, a, []Record{r})
	err := a.store.Write(func(tx *sql.Tx) error {
		result, e := importRecords(tx, []Record{r})
		if result.Added != 0 || result.Events != 0 || result.Changed {
			t.Fatal("duplicate changed state")
		}
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Event = &Event{Format: "ebook", Status: "borrowed", Activity: "Borrowed", At: "2026-10-01T00:00:00.000000000+00:00"}
	addRecords(t, a, []Record{r})
	books, err := loadBooks(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || books[0].Status != "to-read" || books[0].Loans[0].Status != "returned" {
		t.Fatal(books)
	}
	data, err := a.store.Backup()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.sqlite3")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	events, err := loadEvents(restored.DB, 0, 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("%v %v", events, err)
	}
	// A failure later in a batch rolls back earlier inserts.
	err = a.store.Write(func(tx *sql.Tx) error {
		rs := records("Other", "Book")
		rs[0].GoodreadsID = "same"
		rs[1].GoodreadsID = "same"
		_, e := importRecords(tx, rs)
		if e != nil {
			return e
		}
		return fmt.Errorf("abort")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	books, _ = loadBooks(a.store.DB)
	if len(books) != 1 {
		t.Fatal("partial import")
	}
}
func TestLegacyMigration(t *testing.T) {
	t.Parallel()
	path := pythonEraDB(t)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	books, err := loadBooks(s.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || !books[0].Pinned || books[0].Interest != 5 || books[0].Loans[0].ObservedAt != "2026-10-10T12:30:00.000000000+00:00" {
		t.Fatal(books)
	}
	r := records("Legacy")[0]
	r.Source = "nypl"
	r.Event = &Event{Format: "print", Status: "borrowed", Activity: "Borrowed", At: books[0].Loans[0].ObservedAt}
	if result, err := importRecords(s.DB, []Record{r}); err != nil || result.Events != 0 || result.Changed {
		t.Fatalf("legacy duplicate: %+v %v", result, err)
	}
}

func TestBackupScrubsEphemeralState(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	old := signIn(t, a, "")
	addRecords(t, a, records("One", "Two", "Three"))
	r := records("Two")[0]
	r.Source, r.Event = "nypl", &Event{Format: "print", Status: "borrowed", Activity: "Borrowed", At: canonicalTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)), DueDate: "2026-10-20"}
	addRecords(t, a, []Record{r})
	err := a.store.Write(func(tx *sql.Tx) error {
		if _, err := makeSuggestion(tx, 0); err != nil {
			return err
		}
		if err := setMeta(tx, "preferences", Preferences{LibraryWeight: 2, Length: "long", Mood: "calm"}); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO undo_queue VALUES(1,'[]',0); INSERT INTO login_attempts VALUES(1)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	seedFailures(t, a, "192.0.2.1", 3)
	rev, err := revision(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.store.Backup()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "SQLite format 3\x00") {
		t.Fatal("backup is not an SQLite file")
	}
	path := filepath.Join(t.TempDir(), "copy.sqlite3")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		"SELECT count(*) FROM books":                                         3,
		"SELECT count(*) FROM events":                                        1,
		"SELECT count(*) FROM loans WHERE due_date='2026-10-20'":             1,
		"SELECT count(*) FROM plans":                                         0,
		"SELECT count(*) FROM undo_queue":                                    0,
		"SELECT count(*) FROM login_attempts":                                0,
		"SELECT count(*) FROM login_failures":                                0,
		"SELECT count(*) FROM meta WHERE key='session_epoch'":                0,
		"SELECT CAST(value AS INTEGER) FROM meta WHERE key='revision'":       int(rev),
		"SELECT CAST(value AS INTEGER) FROM meta WHERE key='schema_version'": latestVersion(migrations),
	} {
		if got := scalar[int](t, path, query); got != want {
			t.Errorf("%s = %d, want %d", query, got, want)
		}
	}
	// The original keeps its own state.
	if n := failureCount(t, a, "192.0.2.1"); n != 3 {
		t.Fatalf("backup cleared the live database: %d failures", n)
	}
	restored, err := New(Config{Database: path, Secret: strings.Repeat("s", 40), Password: "correct-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	prefs, err := readMeta(restored.store.DB, "preferences", defaultPreferences)
	if err != nil || prefs.Mood != "calm" {
		t.Fatalf("%+v", prefs)
	}
	// A cookie from the original instance does not sign in to the copy, even
	// with the same secret and password, but signing in again works.
	copied := &browser{t: t, handler: restored.Handler(), cookie: old.cookie}
	expect(t, copied.request("GET", "/", nil), 303)
	b := signIn(t, restored, "")
	if rr := b.request("GET", "/?view=all", nil); !strings.Contains(rr.Body.String(), "Three") {
		t.Fatal("restored library missing books")
	}
}
