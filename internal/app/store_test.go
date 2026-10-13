package app

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestImportReconciliationAndBackup(t *testing.T) {
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
