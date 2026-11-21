package app

import (
	"database/sql"
	"encoding/csv"
	"strings"
	"testing"
)

func TestISBN13Of(t *testing.T) {
	for in, want := range map[string]string{
		"0306406152":    "9780306406157", // leading zero
		"080442957X":    "9780804429573", // X check digit
		"9780306406157": "9780306406157",
		"0306406153":    "", // bad check digit
		"9780306406158": "",
		"12345":         "",
		"":              "",
	} {
		if got := isbn13Of(in); got != want {
			t.Errorf("isbn13Of(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExportRating(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		in   *float64
		want string
		note bool
	}{{nil, "", false}, {f(4), "4", false}, {f(4.25), "4.25", false}, {f(0.5), "0.5", false}, {f(4.3), "", true}, {f(5.25), "", true}, {f(5), "5", false}} {
		got, note := exportRating(c.in)
		if got != c.want || (note != "") != c.note {
			t.Errorf("exportRating(%v) = %q, %q", c.in, got, note)
		}
	}
}

func TestGoodreadsReviewHTML(t *testing.T) {
	got, err := goodreadsReviewHTML("# Title\n\nSo **bold** and *soft*, “café”.\n\n- one\n- two\n\n> quoted\n\n[site](https://example.com) `code`")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<b>Title</b>", "<b>bold</b>", "<i>soft</i>", "“café”", "<li>one</li>", "<blockquote>", `<a href="https://example.com">site</a>`, "<br><br>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, bad := range []string{"<em>", "<strong>", "<code>", "\n", "<script"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in %s", bad, got)
		}
	}
}

func goodreadsLibrary(t *testing.T) *App {
	t.Helper()
	a := testApp(t, true)
	add := func(title, isbn, status string) int64 {
		var id int64
		err := a.store.Write(func(tx *sql.Tx) error {
			res, err := tx.Exec("INSERT INTO books(title,author,identity,isbn,reading_status,interest,position,added_at,source,reason) VALUES(?,?,?,?,?,3,0,'2025-01-01','manual','private reason')", title, "Au, \"Thor\"", identity(title, "x"), isbn, status)
			if err != nil {
				return err
			}
			id, err = res.LastInsertId()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	read := add("Zed Read", "0140328726", "read")
	add("Alpha Reading", "080442957X", "reading")
	add("Mid Wanted", "9781861972712", "to-read")
	add("Dup One", "9780306406157", "to-read")
	add("Dup Two", "0306406152", "to-read")
	add("No ISBN", "", "read")
	add("Bad ISBN", "9780306406158", "read")
	add("Quit", "9780140328721", "abandoned")
	add("Shelved", "9780140328721", "archived")
	add("Undecided", "9780140328721", "unreviewed")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.store.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint,review,review_chars,review_format) VALUES(?,'2024-02-03',3,'manual','m1','old **review**',12,'markdown')", read)
	exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint,review,review_chars,review_format) VALUES(?,'2025-04-05',4,'manual','m2','',0,'markdown')", read)
	exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint,review,review_chars,review_format) VALUES(?,'2025-06-01',4.25,'goodreads','g1','imported text',13,'plain')", read)
	return a
}

func TestGoodreadsExportRowsAndExclusions(t *testing.T) {
	a := goodreadsLibrary(t)
	var before int64
	a.store.DB.QueryRow("SELECT value FROM meta WHERE key='revision'").Scan(&before)
	for _, reviews := range []bool{false, true} {
		var e goodreadsExport
		err := a.store.Write(func(tx *sql.Tx) (err error) { e, err = buildGoodreadsExport(tx, reviews); return })
		if err != nil {
			t.Fatal(err)
		}
		titles := []string{}
		for _, r := range e.Rows {
			titles = append(titles, r.Book.Title)
			if r.Shelf == "" || r.Fields[11] == "" {
				t.Errorf("%s has a blank shelf", r.Book.Title)
			}
		}
		if got := strings.Join(titles, "|"); got != "Alpha Reading|Mid Wanted|Zed Read" {
			t.Fatalf("rows %s", got)
		}
		reasons := map[string]string{}
		for _, x := range e.Excluded {
			reasons[x.Title] = x.Reason
		}
		for title, frag := range map[string]string{"No ISBN": "No ISBN", "Bad ISBN": "not valid", "Dup One": "Shares its ISBN", "Dup Two": "Shares its ISBN", "Quit": "abandoned", "Shelved": "archived", "Undecided": "unreviewed"} {
			if !strings.Contains(reasons[title], frag) {
				t.Errorf("%s: reason %q", title, reasons[title])
			}
		}
		zed := e.Rows[2].Fields
		// The newest reading by date is the 2025-06-01 import; its
		// quarter-star rating is passed through unchanged.
		if zed[3] != "4.25" || zed[9] != "2025-06-01" || zed[11] != "read" {
			t.Errorf("zed fields %q", zed)
		}
		// Only authored Markdown reviews count: the two newer readings
		// have an empty one and an imported plain-text one.
		if reviews && (!strings.Contains(zed[13], "<b>review</b>") || !strings.Contains(e.Rows[2].ReviewFrom, "2024-02-03")) {
			t.Errorf("review %q from %q", zed[13], e.Rows[2].ReviewFrom)
		}
		if !reviews && zed[13] != "" {
			t.Error("review exported without opt-in")
		}
		if e.Rows[2].Fields[2] != "0140328726" || e.Rows[0].Fields[2] != "080442957X" {
			t.Error("ISBN not preserved as text")
		}
	}
	var after int64
	a.store.DB.QueryRow("SELECT value FROM meta WHERE key='revision'").Scan(&after)
	if before != after {
		t.Fatal("export changed the revision")
	}
}

func TestGoodreadsDownload(t *testing.T) {
	a := goodreadsLibrary(t)
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/export/goodreads.csv?reviews=1", nil)
	expect(t, rr, 200)
	if ct := rr.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatal(ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "next-chapter-goodreads.csv") {
		t.Fatal(cd)
	}
	rows, err := csv.NewReader(rr.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || strings.Join(rows[0], ",") != strings.Join(goodreadsHeader, ",") {
		t.Fatalf("rows %v", rows)
	}
	if rows[1][1] != `Au, "Thor"` {
		t.Fatalf("author %q", rows[1][1])
	}
	if strings.Contains(rr.Body.String(), "private reason") {
		t.Fatal("private reason exported")
	}
	page := b.request("GET", "/export/goodreads?reviews=1", nil)
	expect(t, page, 200)
	for _, want := range []string{"3 books in the file", "7 left out", "Shares its ISBN", "/export/goodreads.csv?reviews=1"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("preview missing %q", want)
		}
	}
	empty := testApp(t, true)
	be := &browser{t: t, handler: empty.Handler()}
	if !strings.Contains(be.request("GET", "/export/goodreads", nil).Body.String(), "No books can be exported") {
		t.Error("no empty state")
	}
	rows, _ = csv.NewReader(strings.NewReader(be.request("GET", "/export/goodreads.csv", nil).Body.String())).ReadAll()
	if len(rows) != 1 {
		t.Fatalf("empty export has %d rows", len(rows))
	}
	// Authentication: a real (non-demo) app refuses an anonymous download.
	locked := testApp(t, false)
	if r := (&browser{t: t, handler: locked.Handler()}).request("GET", "/export/goodreads.csv", nil); r.Code == 200 {
		t.Fatalf("anonymous download: %d", r.Code)
	}
}
