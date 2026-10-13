package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func importOnce(t *testing.T, a *App, rs []Record) importResult {
	t.Helper()
	var summary importResult
	err := a.store.Write(func(tx *sql.Tx) error {
		var err error
		summary, err = importRecords(tx, rs)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return summary
}
func bookIDs(t *testing.T, a *App) map[string]int64 {
	t.Helper()
	all, err := loadBooks(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, b := range all {
		out[b.Title] = b.ID
	}
	return out
}
func queueTitles(t *testing.T, a *App) ([]string, []int) {
	t.Helper()
	all, err := loadBooks(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	titles, positions := []string{}, []int{}
	for _, b := range queueOnly(all) {
		titles = append(titles, b.Title)
		positions = append(positions, b.Position)
	}
	return titles, positions
}
func libbyEvent(title, status, at string) Record {
	r := Record{Book: Book{Title: title, Author: "Author", Status: "unreviewed", Source: "libby"}}
	r.Event = &Event{Format: "ebook", Status: status, Activity: status, At: at}
	return r
}

func TestDeleteBook(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("One", "Two", "Three", "Four"))
	borrowed := libbyEvent("Two", "borrowed", "2026-10-01T00:00:00.000000000+00:00")
	returned := libbyEvent("Two", "returned", "2026-10-05T00:00:00.000000000+00:00")
	addRecords(t, a, []Record{borrowed, returned})
	ids := bookIDs(t, a)
	b := browser{t: t, handler: a.Handler()}
	rr := b.request("GET", fmt.Sprintf("/books/%d/delete", ids["Two"]), nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "also removes 2 library events and 1 current loans") {
		t.Fatal(rr.Body.String())
	}
	stale := b.rev
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/action", ids["Three"]), url.Values{"action": {"pin"}}), 303)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/delete", ids["Two"]), url.Values{"revision": {stale}}), 409)
	if _, err := getBook(a.store.DB, ids["Two"]); err != nil {
		t.Fatal("stale delete removed the book")
	}
	expect(t, b.request("GET", fmt.Sprintf("/books/%d/delete", ids["Two"]), nil), 200)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/delete", ids["Two"]), nil), 303)

	if _, err := getBook(a.store.DB, ids["Two"]); err != sql.ErrNoRows {
		t.Fatal("book not deleted")
	}
	for _, table := range []string{"events", "loans"} {
		var n int
		if err := a.store.DB.QueryRow("SELECT count(*) FROM "+table+" WHERE book_id=?", ids["Two"]).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s left behind: %d %v", table, n, err)
		}
	}
	titles, positions := queueTitles(t, a)
	if !reflect.DeepEqual(titles, []string{"One", "Three", "Four"}) || !reflect.DeepEqual(positions, []int{0, 1, 2}) {
		t.Fatalf("%v %v", titles, positions)
	}

	// Re-importing never resurrects it, and doing so twice changes nothing.
	gr := records("Two")[0]
	for range 2 {
		if s := importOnce(t, a, []Record{gr, borrowed, returned}); s.Skipped != 3 || s.Added != 0 || s.Events != 0 || s.Changed {
			t.Fatalf("%+v", s)
		}
	}
	if _, ok := bookIDs(t, a)["Two"]; ok {
		t.Fatal("resurrected")
	}

	// The import preview and notice report the skipped rows.
	if n, err := countTombstoned(a.store.DB, []Record{gr, records("New")[0]}); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	expect(t, b.request("GET", "/", nil), 200)
	if err := a.store.Write(func(tx *sql.Tx) error {
		_, err := savePlan(tx, "import", Plan{Records: []Record{gr}, Source: "goodreads"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var plan string
	if err := a.store.DB.QueryRow("SELECT id FROM plans WHERE kind='import'").Scan(&plan); err != nil {
		t.Fatal(err)
	}
	rr = b.request("GET", "/plans/"+plan, nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "1 rows match books you deleted") {
		t.Fatal("preview missing skipped count")
	}
	rr = b.request("POST", "/plans/"+plan, nil)
	expect(t, rr, 303)
	rr = b.request("GET", rr.Header().Get("Location"), nil)
	if !strings.Contains(rr.Body.String(), "Skipped 1 rows for books you deleted") {
		t.Fatal("notice missing skipped count")
	}

	// Adding it again by hand wins over the tombstone.
	expect(t, b.request("POST", "/books/new", url.Values{"title": {"Two"}, "author": {"Author"}, "interest": {"3"}, "reading_status": {"read"}}), 303)
	if _, ok := bookIDs(t, a)["Two"]; !ok {
		t.Fatal("manual add blocked")
	}
	if s := importOnce(t, a, []Record{gr}); s.Skipped != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestMergeBooks(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("First", "The Book", "Last"))
	pages := 320
	source := Record{Book: Book{Title: "The Book: A Novel", Author: "Author", Status: "unreviewed", Source: "goodreads", GoodreadsID: "123", Pages: &pages, Tags: "fiction"}}
	addRecords(t, a, []Record{source})
	libby := []Record{
		libbyEvent("The Book: A Novel", "borrowed", "2026-09-01T00:00:00.000000000+00:00"),
		libbyEvent("The Book: A Novel", "returned", "2026-09-20T00:00:00.000000000+00:00"),
	}
	addRecords(t, a, libby)
	// The kept book already has the borrow, recorded under its own title.
	addRecords(t, a, []Record{libbyEvent("The Book", "borrowed", "2026-09-01T00:00:00.000000000+00:00")})
	ids := bookIDs(t, a)
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1,interest=5,needs_placement=0 WHERE id=?", ids["The Book"]); err != nil {
		t.Fatal(err)
	}
	before, err := getBook(a.store.DB, ids["The Book"])
	if err != nil {
		t.Fatal(err)
	}

	b := browser{t: t, handler: a.Handler()}
	rr := b.request("GET", fmt.Sprintf("/books/%d/merge?q=book", ids["The Book: A Novel"]), nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), fmt.Sprintf("merge?target=%d", ids["The Book"])) || strings.Contains(rr.Body.String(), fmt.Sprintf("merge?target=%d", ids["The Book: A Novel"])) {
		t.Fatal(rr.Body.String())
	}
	rr = b.request("GET", fmt.Sprintf("/books/%d/merge?target=%d", ids["The Book: A Novel"], ids["The Book"]), nil)
	expect(t, rr, 200)
	for _, want := range []string{"Library events moved: 1; 1 duplicates dropped", "Goodreads ID, Pages, Tags"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("preview missing %q: %s", want, rr.Body)
		}
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/merge", ids["The Book: A Novel"]), url.Values{"target": {fmt.Sprint(ids["The Book"])}}), 303)

	if _, ok := bookIDs(t, a)["The Book: A Novel"]; ok {
		t.Fatal("source not removed")
	}
	after, err := getBook(a.store.DB, ids["The Book"])
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "The Book" || after.Status != "to-read" || !after.Pinned || after.Interest != 5 || after.Position != before.Position {
		t.Fatalf("target changed: %+v", after)
	}
	if after.GoodreadsID != "123" || after.Pages == nil || *after.Pages != 320 || after.Tags != "fiction" {
		t.Fatalf("blank fields not filled: %+v", after)
	}
	if len(after.Loans) != 1 || after.Loans[0].Status != "returned" {
		t.Fatalf("loans not rebuilt: %+v", after.Loans)
	}
	titles, _ := queueTitles(t, a)
	if !reflect.DeepEqual(titles, []string{"First", "The Book", "Last"}) {
		t.Fatal(titles)
	}
	events, err := loadEvents(a.store.DB, ids["The Book"], 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("%v %v", events, err)
	}
	for _, e := range events {
		want := fingerprint(Record{Book: Book{Title: "The Book", Author: "Author", Source: e.Source}, Event: &e})
		if e.Fingerprint != want {
			t.Fatalf("fingerprint not re-keyed: %+v", e)
		}
	}

	// Re-importing the merged book's Libby and Goodreads rows adds nothing.
	for range 2 {
		if s := importOnce(t, a, append(libby, source)); s.Added != 0 || s.Events != 0 || s.Skipped != 0 || s.Changed {
			t.Fatalf("%+v", s)
		}
	}
	if n := len(bookIDs(t, a)); n != 3 {
		t.Fatalf("%d books", n)
	}
	// Adding the merged title by hand points at the kept book.
	expect(t, b.request("GET", "/books/new", nil), 200)
	expect(t, b.request("POST", "/books/new", url.Values{"title": {"The Book: A Novel"}, "author": {"Author"}, "interest": {"3"}, "reading_status": {"to-read"}}), 409)

	// Deleting the kept book tombstones its merged aliases too.
	expect(t, b.request("GET", fmt.Sprintf("/books/%d/delete", ids["The Book"]), nil), 200)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/delete", ids["The Book"]), nil), 303)
	if s := importOnce(t, a, append(libby, source)); s.Skipped != 3 || s.Added != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestMergeConflicts(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	rs := records("Kept", "Other", "Third")
	rs[0].GoodreadsID, rs[1].GoodreadsID = "1", "2"
	rs[0].ISBN, rs[2].ISBN = "9780000000001", "9780000000002"
	addRecords(t, a, rs)
	ids := bookIDs(t, a)
	b := browser{t: t, handler: a.Handler()}
	path := fmt.Sprintf("/books/%d/merge", ids["Other"])
	rr := b.request("GET", fmt.Sprintf("%s?target=%d", path, ids["Kept"]), nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "different Goodreads IDs") || !strings.Contains(rr.Body.String(), "disabled") {
		t.Fatal(rr.Body.String())
	}
	rr = b.request("POST", path, url.Values{"target": {fmt.Sprint(ids["Kept"])}})
	expect(t, rr, 409)
	if !strings.Contains(rr.Body.String(), "different Goodreads IDs") {
		t.Fatal(rr.Body.String())
	}
	expect(t, b.request("POST", path, url.Values{"target": {fmt.Sprint(ids["Other"])}}), 400)
	if len(bookIDs(t, a)) != 3 {
		t.Fatal("refused merge changed books")
	}

	// Different ISBNs are allowed, explained, and the target's ISBN is kept.
	path = fmt.Sprintf("/books/%d/merge", ids["Third"])
	rr = b.request("GET", fmt.Sprintf("%s?target=%d", path, ids["Kept"]), nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "different ISBNs") {
		t.Fatal(rr.Body.String())
	}
	expect(t, b.request("POST", path, url.Values{"target": {fmt.Sprint(ids["Kept"])}}), 303)
	kept, err := getBook(a.store.DB, ids["Kept"])
	if err != nil || kept.ISBN != "9780000000001" {
		t.Fatalf("%+v %v", kept, err)
	}
	// The merged book's ISBN now finds the kept book.
	if id, err := findBook(a.store.DB, Record{Book: Book{Title: "Unrelated", Author: "X", ISBN: "9780000000002"}}); err != nil || id != ids["Kept"] {
		t.Fatalf("%d %v", id, err)
	}
}

func TestAliasesInBackupAndExport(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("Merged", "Target", "Deleted"))
	ids := bookIDs(t, a)
	err := a.store.Write(func(tx *sql.Tx) error {
		if _, err := mergeBooks(tx, ids["Merged"], ids["Target"], true); err != nil {
			return err
		}
		return deleteBook(tx, ids["Deleted"])
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.store.Backup()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copy.sqlite3")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM book_aliases"); n != 2 {
		t.Fatalf("backup has %d aliases", n)
	}
	b := browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/export.json", nil)
	expect(t, rr, 200)
	var export struct {
		SchemaVersion int     `json:"schema_version"`
		Aliases       []alias `json:"aliases"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &export); err != nil {
		t.Fatal(err)
	}
	if export.SchemaVersion != latestVersion(migrations) || len(export.Aliases) != 2 {
		t.Fatalf("%+v", export)
	}
	for _, x := range export.Aliases {
		if (x.Identity == "merged|author") != (x.TargetBookID != nil && *x.TargetBookID == ids["Target"]) {
			t.Fatalf("%+v", x)
		}
	}
	rr = b.request("GET", "/settings", nil)
	if !strings.Contains(rr.Body.String(), fmt.Sprintf("schema version %d", latestVersion(migrations))) {
		t.Fatal("settings missing schema version")
	}
}
