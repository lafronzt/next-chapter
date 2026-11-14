package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCSV(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"goodreads", "libby", "nypl"} {
		t.Run(source, func(t *testing.T) {
			data, err := assets.ReadFile("web/examples/" + source + ".csv")
			if err != nil {
				t.Fatal(err)
			}
			rows, problems, err := ParseCSV(data, source)
			if err != nil || len(problems) != 0 || len(rows) == 0 {
				t.Fatalf("%v %v %v", rows, problems, err)
			}
		})
	}
	_, problems, err := ParseCSV([]byte("Title,Author,Timestamp,Activity\nBook,Person,2026-01-01,unknown\n"), "libby")
	if err != nil || len(problems) != 1 {
		t.Fatalf("%v %v", problems, err)
	}
	for _, data := range []string{"Title,Title\nA,B", "Bad,Header\nA,B", strings.Repeat("x", maxUpload+1)} {
		if _, _, err := ParseCSV([]byte(data), "goodreads"); err == nil {
			t.Fatal("accepted invalid CSV")
		}
	}
}

func TestParseTime(t *testing.T) {
	t.Parallel()
	const noon = "2026-10-10T12:00:00.000000000+00:00"
	const midnight = "2026-10-10T00:00:00.000000000+00:00"
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"1791633600", noon},    // epoch seconds
		{"1791633600000", noon}, // epoch milliseconds
		{"1791633600123", "2026-10-10T12:00:00.123000000+00:00"}, // milliseconds keep their fraction
		{"2026-10-10T12:00:00Z", noon},                           // RFC 3339
		{"2026-10-10T08:00:00-04:00", noon},                      // RFC 3339 with an offset
		{"2026-10-10T13:00:00.5+01:00", "2026-10-10T12:00:00.500000000+00:00"},
		{"2026-10-10T12:00:00", noon}, // naive: read as UTC
		{"2026-10-10 12:00:00", noon}, // naive with a space
		{" 2026-10-10 ", midnight},    // date only, trimmed
		{"2026/10/10", midnight},
		{"10/10/2026", midnight}, // US month/day/year
		{"1/2/2026", "2026-01-02T00:00:00.000000000+00:00"},
		{"October 10, 2026", midnight},
		{"10 Oct 2026", midnight},
		{"Sat, 10 Oct 2026 12:00:00 GMT", noon},       // RFC 1123
		{"Sat, 10 Oct 2026 14:00:00 +0200", noon},     // RFC 1123 with a numeric zone
		{"10 Oct 26 14:00 +0200", noon},               // RFC 822 with a numeric zone
		{"2026-10-10T12:00:00.000000000+00:00", noon}, // canonical is a fixed point
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseTime(c.in)
			if err != nil || got != c.want {
				t.Fatalf("parseTime(%q) = %q, %v; want %q", c.in, got, err, c.want)
			}
		})
	}
	for _, in := range []string{"yesterday", "2026-13-01", "13/45/2026", "123456789", "17916336001", "2026-10-10T25:00:00Z", "9999-12-31T23:59:59-01:00", "0001-01-01T00:00:00+01:00", strings.Repeat("9", 80)} {
		got, err := parseTime(in)
		if err == nil {
			t.Errorf("parseTime(%q) = %q, want an error", in, got)
		} else if len(err.Error()) > 120 {
			t.Errorf("error for %q is not truncated: %v", in, err)
		}
	}
}

func goodreadsRows(n int) string {
	var b strings.Builder
	b.WriteString("Book Id,Title,Author,Exclusive Shelf\n")
	for i := range n {
		fmt.Fprintf(&b, "%d,Book %d,Writer,to-read\n", i+1, i)
	}
	return b.String()
}

func TestParseCSVLimits(t *testing.T) {
	t.Parallel()
	rows, problems, err := ParseCSV([]byte(goodreadsRows(maxRows)), "goodreads")
	if err != nil || len(problems) != 0 || len(rows) != maxRows {
		t.Fatalf("10,000 rows: %d %v %v", len(rows), problems, err)
	}
	if _, _, err = ParseCSV([]byte(goodreadsRows(maxRows+1)), "goodreads"); err == nil || !strings.Contains(err.Error(), "at most 10,000 rows") {
		t.Fatalf("10,001 rows: %v", err)
	}
	// Exactly maxUpload bytes is accepted; one more is not.
	head := "Title,Author,Notes\nBook,Writer,"
	exact := head + strings.Repeat("x", maxUpload-len(head))
	if rows, _, err = ParseCSV([]byte(exact), "goodreads"); err != nil || len(rows) != 1 {
		t.Fatalf("2 MB: %v", err)
	}
	if _, _, err = ParseCSV([]byte(exact+"x"), "goodreads"); err == nil || !strings.Contains(err.Error(), "at most 2 MB") {
		t.Fatalf("2 MB + 1: %v", err)
	}
}

func TestParseCSVShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, source, data string
		rows, problems     int
		err                string
	}{
		{"BOM before the header", "goodreads", "\ufeffTitle,Author\nBook,Writer\n", 1, 0, ""},
		{"header names are normalized", "goodreads", "TITLE ,author name\nBook,Writer\n", 1, 0, ""},
		{"CRLF line endings", "goodreads", "Title,Author\r\nBook,Writer\r\n", 1, 0, ""},
		{"short row misses its author", "goodreads", "Title,Author\nBook\nOther,Writer\n", 1, 1, ""},
		{"long row ignores extra cells", "goodreads", "Title,Author\nBook,Writer,extra,cells\n", 1, 0, ""},
		{"title over 500 characters", "goodreads", "Title,Author\n" + strings.Repeat("t", 501) + ",Writer\n", 0, 1, ""},
		{"author over 300 characters", "goodreads", "Title,Author\nBook," + strings.Repeat("a", 301) + "\n", 0, 1, ""},
		{"Latin-1 bytes", "goodreads", "Title,Author\nCaf\xe9,Writer\n", 0, 0, "UTF-8"},
		{"duplicate header", "goodreads", "Title,Author,Title\nA,B,C\n", 0, 0, "duplicate CSV column"},
		{"duplicate after normalizing", "goodreads", "Title,Author,ti-tle\nA,B,C\n", 0, 0, "duplicate CSV column"},
		{"no Title column", "goodreads", "Name,Author\nA,B\n", 0, 0, "needs a Title column"},
		{"header only", "goodreads", "Title,Author\n", 0, 0, "no book rows"},
		{"empty file", "goodreads", "", 0, 0, "header row"},
		{"bare quote", "goodreads", "Title,Author\n\"Book,Writer\n", 0, 0, "invalid CSV near row 2"},
		{"unknown source", "kindle", "Title,Author\nA,B\n", 0, 0, "supported import source"},
		{"library row without a date", "libby", "Title,Author,Activity\nA,B,Borrowed\n", 0, 1, ""},
		{"library row with a bad date", "nypl", "Title,Author,Activity,Date\nA,B,Borrowed,someday\n", 0, 1, ""},
		{"library row with a bad due date", "nypl", "Title,Author,Activity,Date,Due Date\nA,B,Borrowed,2026-10-01,later\n", 0, 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows, problems, err := ParseCSV([]byte(c.data), c.source)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("error %v, want %q", err, c.err)
				}
				return
			}
			if err != nil || len(rows) != c.rows || len(problems) != c.problems {
				t.Fatalf("%d rows %q %v", len(rows), problems, err)
			}
		})
	}
}

func TestParseCSVFields(t *testing.T) {
	t.Parallel()
	rows, _, err := ParseCSV([]byte("Book Id,Title,Author,ISBN13,Number of Pages,Bookshelves,Exclusive Shelf\n"+
		"1,Read,W,\"=\"\"978-0-306-40615-7\"\"\",320,\"fiction, hopeful\",read\n"+
		"2,Reading,W,123,0,,currently-reading\n"+
		"3,Custom,W,,100000,,favorites\n"), "goodreads")
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	r := rows[0]
	if r.Status != "read" || r.ISBN != "9780306406157" || r.Pages == nil || *r.Pages != 320 || r.Tags != "fiction, hopeful" || r.GoodreadsID != "1" || r.Interest != 3 || !r.NeedsPlacement {
		t.Fatalf("%+v", r)
	}
	if rows[1].Status != "reading" || rows[1].ISBN != "" || rows[1].Pages != nil || rows[2].Status != "archived" || rows[2].Pages != nil {
		t.Fatalf("%+v %+v", rows[1], rows[2])
	}
	rows, _, err = ParseCSV([]byte("Title,Creator,Activity,Date,Due Date\nA,B,Renewed,2026-10-01T09:00:00-04:00,2026-10-20\n"), "nypl")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	e := rows[0].Event
	if rows[0].Status != "unreviewed" || e.Status != "borrowed" || e.Activity != "Renewed" || e.Format != "print" || e.At != "2026-10-01T13:00:00.000000000+00:00" || e.DueDate != "2026-10-20" {
		t.Fatalf("%+v %+v", rows[0], e)
	}
}

// fuzzSeeds adds the bundled examples and some awkward shapes to a corpus.
func fuzzSeeds(f *testing.F) []string {
	f.Helper()
	paths, err := filepath.Glob("../../examples/*.csv")
	if err != nil || len(paths) == 0 {
		f.Fatalf("examples: %v %v", paths, err)
	}
	seeds := []string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		seeds = append(seeds, string(data))
	}
	return append(seeds, "\ufeffTitle,Author\nA,B\n", "Title,Author\n\"A\"\"\",B\n", "Title\nA\n", "Title,Author,Timestamp,Activity\nA,B,1760097600000,hold\n", ",,,\n,,,\n", "Title,Author,Number of Pages\nA,B,-1\n")
}

var dueDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// checkRecord asserts what every parsed record promises the importer.
func checkRecord(t *testing.T, r Record) {
	t.Helper()
	title, author := []rune(r.Title), []rune(r.Author)
	if len(title) == 0 || len(title) > 500 || len(author) == 0 || len(author) > 300 || strings.TrimSpace(r.Title) == "" || strings.TrimSpace(r.Author) == "" {
		t.Fatalf("title/author out of bounds: %q / %q", r.Title, r.Author)
	}
	if !utf8.ValidString(r.Title) || !utf8.ValidString(r.Author) || len([]rune(r.Tags)) > 500 {
		t.Fatalf("invalid text: %+v", r)
	}
	if !in(r.Status, readingStates) || r.Interest < 1 || r.Interest > 5 {
		t.Fatalf("status %q interest %d", r.Status, r.Interest)
	}
	if r.Pages != nil && (*r.Pages < 1 || *r.Pages > 99999) {
		t.Fatalf("pages %d", *r.Pages)
	}
	if r.ISBN != "" && len(r.ISBN) != 10 && len(r.ISBN) != 13 {
		t.Fatalf("isbn %q", r.ISBN)
	}
	if e := r.Event; e != nil {
		if !in(e.Status, libraryStates) || e.Format == "" || len([]rune(e.Format)) > 100 || len([]rune(e.Activity)) > 100 {
			t.Fatalf("event %+v", e)
		}
		if again, err := parseTime(e.At); err != nil || again != e.At || e.At == "" {
			t.Fatalf("event time %q is not canonical: %q %v", e.At, again, err)
		}
		if e.DueDate != "" && !dueDatePattern.MatchString(e.DueDate) {
			t.Fatalf("due date %q", e.DueDate)
		}
	}
}

func FuzzParseCSV(f *testing.F) {
	sources := []string{"goodreads", "libby", "nypl"}
	for i, seed := range fuzzSeeds(f) {
		f.Add([]byte(seed), uint8(i))
	}
	f.Fuzz(func(t *testing.T, data []byte, source uint8) {
		rows, problems, err := ParseCSV(data, sources[int(source)%len(sources)])
		if err != nil {
			if rows != nil || problems != nil {
				t.Fatal("rows returned with an error")
			}
			return
		}
		if len(rows)+len(problems) == 0 || len(rows)+len(problems) > maxRows {
			t.Fatalf("%d rows, %d problems", len(rows), len(problems))
		}
		for _, r := range rows {
			checkRecord(t, r)
		}
	})
}

func FuzzParseTime(f *testing.F) {
	for _, seed := range []string{"", "1791633600", "1791633600000", "2026-10-10T08:00:00-04:00", "2026-10-10 12:00:00", "10/10/2026", "Sat, 10 Oct 2026 12:00:00 GMT", "0001-01-01T00:00:00+01:00", "9999-12-31T23:59:59-01:00", "2026-10-10T12:00:00.000000000+00:00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got, err := parseTime(value)
		if err != nil || got == "" {
			return
		}
		if _, err := time.Parse(time.RFC3339Nano, got); err != nil {
			t.Fatalf("parseTime(%q) = %q, which is not RFC 3339: %v", value, got, err)
		}
		if again, err := parseTime(got); err != nil || again != got {
			t.Fatalf("parseTime(%q) = %q, but that parses to %q, %v", value, got, again, err)
		}
	})
}

func TestGoodreadsRatingSeedsInterest(t *testing.T) {
	t.Parallel()
	csv := "Book Id,Title,Author,My Rating,Average Rating\n" +
		"1,Loved,W,5,1.20\n2,Unrated,W,0,4.90\n3,Blank,W,,4.90\n4,Seven,W,7,4.90\n5,Word,W,abc,4.90\n6,Half,W,3.5,4.90\n7,Liked,W, 2 ,4.90\n"
	rows, problems, err := ParseCSV([]byte(csv), "goodreads")
	if err != nil || len(problems) != 0 || len(rows) != 7 {
		t.Fatal(rows, problems, err)
	}
	for i, want := range []struct {
		interest, rating int
		ignored          bool
	}{{5, 5, false}, {3, 0, false}, {3, 0, false}, {3, 0, true}, {3, 0, true}, {3, 0, true}, {2, 2, false}} {
		r := rows[i]
		if r.Interest != want.interest || r.Rating != want.rating || r.RatingIgnored != want.ignored {
			t.Errorf("%s: interest %d rating %d ignored %v", r.Title, r.Interest, r.Rating, r.RatingIgnored)
		}
	}
	// Average Rating alone is never the reader's opinion.
	rows, _, _ = ParseCSV([]byte("Title,Author,Average Rating\nA,B,5\n"), "goodreads")
	if rows[0].Interest != 3 || rows[0].Rating != 0 {
		t.Fatalf("%+v", rows[0])
	}
	// Library exports have no ratings.
	rows, _, _ = ParseCSV([]byte("Title,Author,My Rating,Activity,Date\nA,B,5,Borrowed,2026-10-01\n"), "libby")
	if rows[0].Interest != 3 || rows[0].Rating != 0 {
		t.Fatalf("%+v", rows[0])
	}
}

func TestImportUsesRatingOnlyForNewBooks(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	rated := func(title string, rating int) Record {
		r := shelf(title, "to-read")
		r.Rating, r.Interest = rating, rating
		return r
	}
	result := importResultFor(t, a, []Record{rated("Rated", 5), shelf("Unrated", "to-read"), rated("Read", 4)})
	if result.Added != 3 || result.Rated != 2 || result.RatingsKept != 0 {
		t.Fatalf("%+v", result)
	}
	if bookNamed(t, a, "Rated").Interest != 5 || bookNamed(t, a, "Unrated").Interest != 3 {
		t.Fatal("rating not used for a new book")
	}
	// The reader changes their mind; a later import must not undo that.
	if _, err := a.store.DB.Exec("UPDATE books SET interest=1 WHERE title='Rated'"); err != nil {
		t.Fatal(err)
	}
	result = importResultFor(t, a, []Record{rated("Rated", 5), rated("Unrated", 4)})
	if result.Added != 0 || result.Rated != 0 || result.RatingsKept != 2 || result.Changed {
		t.Fatalf("%+v", result)
	}
	if bookNamed(t, a, "Rated").Interest != 1 || bookNamed(t, a, "Unrated").Interest != 3 {
		t.Fatal("re-import changed a local interest")
	}
	// Records built without an interest (the demo, loans) still satisfy the
	// column's 1-5 check.
	importResultFor(t, a, []Record{{Book: Book{Title: "Zero", Author: "W", Status: "to-read", Source: "demo"}}, {Book: Book{Title: "Huge", Author: "W", Status: "to-read", Source: "demo", Interest: 9}}})
	if bookNamed(t, a, "Zero").Interest != 3 || bookNamed(t, a, "Huge").Interest != 3 {
		t.Fatal("out-of-range interest not clamped")
	}
}

func TestStoredPlanKeepsRatings(t *testing.T) {
	t.Parallel()
	rows, _, err := ParseCSV([]byte("Title,Author,My Rating\nA,W,4\nB,W,0\n"), "goodreads")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodePlan(Plan{Records: rows})
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodePlan(nil, "reorder", raw)
	if err != nil || len(p.Records) != 2 {
		t.Fatal(p, err)
	}
	if p.Records[0].Interest != 4 || p.Records[0].Rating != 4 || p.Records[1].Interest != 3 || p.Records[1].Rating != 0 {
		t.Fatalf("%+v %+v", p.Records[0], p.Records[1])
	}
}

// A Goodreads export lists the newest additions first; the queue should
// follow the shelf instead: positions first, then Date Added.
func TestGoodreadsImportKeepsShelfOrder(t *testing.T) {
	t.Parallel()
	csv := "Book Id,Title,Author,Date Added,Bookshelves,Bookshelves with positions,Exclusive Shelf\n" +
		"1,Newest,Writer,2025/09/01,,,to-read\n" +
		"2,Second,Writer,2025/08/01,\"fantasy, to-read\",\"fantasy (#1), to-read (#2)\",to-read\n" +
		"3,Read Book,Writer,2025/07/01,,read (#1),read\n" +
		"4,First,Writer,2025/06/01,,to-read (#1),to-read\n" +
		"5,Oldest,Writer,2025/01/01,,,to-read\n" +
		"6,Undated,Writer,,,,to-read\n"
	rows, problems, err := ParseCSV([]byte(csv), "goodreads")
	if err != nil || len(problems) != 0 {
		t.Fatal(problems, err)
	}
	titles := []string{}
	for _, r := range rows {
		titles = append(titles, r.Title)
	}
	// "Read Book" has a position on its own shelf, which does not compare
	// with the to-read shelf's.
	if got, want := strings.Join(titles, ", "), "First, Second, Undated, Oldest, Read Book, Newest"; got != want {
		t.Fatalf("order %s, want %s", got, want)
	}
	a := testApp(t, true)
	addRecords(t, a, rows)
	queue, err := loadQueue(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	titles = titles[:0]
	for _, b := range queue {
		titles = append(titles, b.Title)
	}
	if got, want := strings.Join(titles, ", "), "First, Second, Undated, Oldest, Newest"; got != want {
		t.Fatalf("queue %s, want %s", got, want)
	}
}

func TestShelfPosition(t *testing.T) {
	t.Parallel()
	for cell, want := range map[string]int{"to-read (#12)": 12, "fantasy (#3), to-read (#40)": 40, "To-Read (#5)": 5, "fantasy (#3)": 0, "": 0, "to-read (#0)": 0, "to-read": 0, "to-read-later (#4)": 0} {
		if got := shelfPosition(cell, "to-read"); got != want {
			t.Errorf("%q: %d, want %d", cell, got, want)
		}
	}
}

func TestGoodreadsReimportMatchesByISBNThenTitle(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	first := "Book Id,Title,Author,ISBN13,Exclusive Shelf\n" +
		"1,With ISBN,Writer,\"=\"\"9780306406157\"\"\",to-read\n" +
		"2,No ISBN,Writer,,to-read\n"
	// A newer export: Goodreads picked a new Book Id for one, retitled
	// nothing, and the first book's title changed but its ISBN did not.
	second := "Book Id,Title,Author,ISBN13,Exclusive Shelf\n" +
		"90,With ISBN (Series #1),Writer,\"=\"\"9780306406157\"\"\",to-read\n" +
		"91,No ISBN,Writer,,to-read\n" +
		"92,Fresh,Writer,,to-read\n"
	for _, data := range []string{first, second} {
		rows, problems, err := ParseCSV([]byte(data), "goodreads")
		if err != nil || len(problems) != 0 {
			t.Fatal(problems, err)
		}
		addRecords(t, a, rows)
	}
	books, err := loadBooks(a.store.DB)
	if err != nil || len(books) != 3 {
		t.Fatalf("%d books, want 3: %+v %v", len(books), books, err)
	}
}

func TestGoodreadsReimportAdvancesQueuedBooks(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	header := "Book Id,Title,Author,Date Read,Exclusive Shelf,Bookshelves with positions\n"
	rows, _, err := ParseCSV([]byte(header+
		"1,Done Soon,W,,to-read,to-read (#1)\n"+
		"2,Started,W,,to-read,to-read (#2)\n"+
		"3,Stays,W,,to-read,to-read (#3)\n"+
		"4,Archived Here,W,,to-read,to-read (#4)\n"+
		"5,Finished Here,W,2025/01/01,read,\n"), "goodreads")
	if err != nil {
		t.Fatal(err)
	}
	addRecords(t, a, rows)
	for _, title := range []string{"Archived Here"} {
		if _, err := a.store.DB.Exec("UPDATE books SET reading_status='archived' WHERE title=?", title); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err = ParseCSV([]byte(header+
		"1,Done Soon,W,2026/11/20,read,\n"+
		"2,Started,W,,currently-reading,\n"+
		"3,Stays,W,,to-read,to-read (#1)\n"+
		"4,Archived Here,W,,to-read,to-read (#2)\n"+
		"5,Finished Here,W,2025/01/01,to-read,to-read (#3)\n"), "goodreads")
	if err != nil {
		t.Fatal(err)
	}
	result := importResultFor(t, a, rows)
	if result.Advanced["read"] != 1 || result.Advanced["reading"] != 1 || result.advancedTotal() != 2 {
		t.Fatalf("%+v", result)
	}
	for title, want := range map[string]string{"Done Soon": "read", "Started": "reading", "Stays": "to-read", "Archived Here": "archived", "Finished Here": "read"} {
		if got := bookNamed(t, a, title).Status; got != want {
			t.Errorf("%s: %s, want %s", title, got, want)
		}
	}
	if n := len(readingsOf(t, a, "Done Soon")); n != 1 {
		t.Errorf("Done Soon has %d readings", n)
	}
	// Importing it again changes nothing.
	if again := importResultFor(t, a, rows); again.Changed {
		t.Fatalf("%+v", again)
	}
}
