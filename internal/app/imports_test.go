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

func TestCSVAndRSS(t *testing.T) {
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
	rss := `<rss><channel><item><book_title>A &amp; B</book_title><author_name>Person</author_name><book_id>42</book_id><book><num_pages>250</num_pages></book></item></channel></rss>`
	rows, err := ParseRSS([]byte(rss))
	if err != nil || len(rows) != 1 || rows[0].Title != "A & B" || rows[0].Pages == nil {
		t.Fatalf("%v %v", rows, err)
	}
	if _, err = ParseRSS([]byte(`<!DOCTYPE rss><rss/>`)); err == nil {
		t.Fatal("accepted DTD")
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

func TestParseRSSShapes(t *testing.T) {
	t.Parallel()
	item := func(inner string) string { return "<rss><channel><item>" + inner + "</item></channel></rss>" }
	rows, err := ParseRSS([]byte(item("<title>Fallback</title><author_name> W </author_name><num_pages>12</num_pages><isbn>0306406152</isbn>")))
	if err != nil || len(rows) != 1 || rows[0].Title != "Fallback" || rows[0].Author != "W" || *rows[0].Pages != 12 || rows[0].ISBN != "0306406152" || rows[0].Status != "to-read" || rows[0].Source != "goodreads" {
		t.Fatalf("%+v %v", rows, err)
	}
	rows, err = ParseRSS([]byte(item("<book_title>No author</book_title>")))
	if err != nil || len(rows) != 0 {
		t.Fatalf("%+v %v", rows, err)
	}
	rows, err = ParseRSS([]byte(item("<book_title>" + strings.Repeat("t", 600) + "</book_title><author_name>W</author_name>")))
	if err != nil || len([]rune(rows[0].Title)) != 500 {
		t.Fatalf("title not truncated: %v", err)
	}
	many := "<rss><channel>" + strings.Repeat("<item><book_title>T</book_title><author_name>W</author_name></item>", maxRows+5) + "</channel></rss>"
	if rows, err = ParseRSS([]byte(many)); err != nil || len(rows) != maxRows {
		t.Fatalf("%d rows %v", len(rows), err)
	}
	for _, bad := range []string{"<rss><channel>", "<feed/>", `<!ENTITY x "y"><rss/>`, "<rss>" + strings.Repeat(" ", maxUpload) + "</rss>"} {
		if _, err = ParseRSS([]byte(bad)); err == nil {
			t.Errorf("accepted %.40q", bad)
		}
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

func FuzzParseRSS(f *testing.F) {
	f.Add([]byte(testRSS))
	f.Add([]byte(`<rss><channel><item><book_title>A &amp; B</book_title><author_name>P</author_name><book><num_pages>250</num_pages></book></item></channel></rss>`))
	f.Add([]byte(`<rss><channel><item><title> </title><author_name>P</author_name></item><item/></channel></rss>`))
	f.Add([]byte(`<?xml version="1.0"?><rss><channel><item><book_title><![CDATA[<b>T</b>]]></book_title><author_name>A</author_name><isbn>12</isbn><num_pages>x</num_pages></item></channel></rss>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		rows, err := ParseRSS(data)
		if err != nil {
			return
		}
		for _, r := range rows {
			// Feed text is not validated as UTF-8 by encoding/xml, so only the
			// structural promises are checked here.
			if strings.TrimSpace(r.Title) == "" || strings.TrimSpace(r.Author) == "" || len([]rune(r.Title)) > 500 || len([]rune(r.Author)) > 300 {
				t.Fatalf("title/author out of bounds: %q / %q", r.Title, r.Author)
			}
			if r.Status != "to-read" || r.Source != "goodreads" || r.Interest < 1 || r.Interest > 5 || r.Event != nil {
				t.Fatalf("%+v", r)
			}
			if r.Pages != nil && (*r.Pages < 1 || *r.Pages > 99999) {
				t.Fatalf("pages %d", *r.Pages)
			}
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
