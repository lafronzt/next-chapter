package app

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// The Goodreads export is read-only and reader-initiated: it builds a CSV in
// the layout of Goodreads' sample import file from what is already stored. The
// rules below are conservative where Goodreads' behavior is unverified (its
// help pages need a login), and every row or value that is left out is
// reported to the reader instead of dropped silently.
var goodreadsHeader = []string{"Title", "Author", "ISBN", "My Rating", "Average Rating", "Publisher", "Binding", "Year Published", "Original Publication Year", "Date Read", "Date Added", "Shelves", "Bookshelves", "My Review"}

// goodreadsShelves maps the reading statuses Goodreads has an exclusive shelf
// for. unreviewed (not yet decided), abandoned and archived have no honest
// equivalent: they are excluded rather than shown as read or wanted.
var goodreadsShelves = map[string]string{"read": "read", "reading": "currently-reading", "to-read": "to-read"}

type goodreadsRow struct {
	Book   Book
	ISBN   string
	Shelf  string
	Fields []string
	// ReviewFrom names the reading the review came from, empty without one.
	ReviewFrom string
	// Notes list values that were left blank on purpose.
	Notes []string
}

type goodreadsExclusion struct {
	BookID        int64
	Title, Author string
	Reason        string
}

type goodreadsExport struct {
	Rows           []goodreadsRow
	Excluded       []goodreadsExclusion
	IncludeReviews bool
}

// ReviewCount is the number of rows that carry a review.
func (e goodreadsExport) ReviewCount() int {
	n := 0
	for _, r := range e.Rows {
		if r.ReviewFrom != "" {
			n++
		}
	}
	return n
}

// NoteCount is the number of rows with a value left blank on purpose.
func (e goodreadsExport) NoteCount() int {
	n := 0
	for _, r := range e.Rows {
		if len(r.Notes) > 0 {
			n++
		}
	}
	return n
}

// isbn13Of returns the ISBN-13 form of a checksum-valid ISBN-10 or ISBN-13,
// or "" for anything else. Both forms of one edition compare equal this way.
func isbn13Of(isbn string) string {
	switch len(isbn) {
	case 10:
		sum := 0
		for i := range 10 {
			c := isbn[i]
			v := 0
			switch {
			case c >= '0' && c <= '9':
				v = int(c - '0')
			case c == 'X' && i == 9:
				v = 10
			default:
				return ""
			}
			sum += v * (10 - i)
		}
		if sum%11 != 0 {
			return ""
		}
		return "978" + isbn[:9] + string(rune('0'+isbn13Check("978"+isbn[:9])))
	case 13:
		for _, c := range isbn {
			if c < '0' || c > '9' {
				return ""
			}
		}
		if int(isbn[12]-'0') != isbn13Check(isbn[:12]) {
			return ""
		}
		return isbn
	}
	return ""
}

func isbn13Check(first12 string) int {
	sum := 0
	for i, c := range first12 {
		w := 1
		if i%2 == 1 {
			w = 3
		}
		sum += int(c-'0') * w
	}
	return (10 - sum%10) % 10
}

func exportRating(r *float64) (value, note string) {
	if r == nil || *r == 0 {
		return "", ""
	}
	if *r == math.Trunc(*r) && *r >= 1 && *r <= 5 {
		return strconv.Itoa(int(*r)), ""
	}
	return "", fmt.Sprintf("Your %s-star rating is not exported: Goodreads' support for fractional ratings is unverified, and rounding would change it.", strconv.FormatFloat(*r, 'f', -1, 64))
}

var (
	reviewTagReplacer = strings.NewReplacer("<em>", "<i>", "</em>", "</i>", "<strong>", "<b>", "</strong>", "</b>", "<hr>", "", "<code>", "", "</code>", "", "<pre>", "", "</pre>", "")
	reviewHeading     = regexp.MustCompile(`<h[1-6]>(.*?)</h[1-6]>`)
	reviewBreaks      = regexp.MustCompile(`(<br>)+(</blockquote>|</li>)`)
)

// goodreadsReviewHTML converts a Markdown review to the small HTML subset
// Goodreads reviews are believed to render: b, i, a, br, blockquote, and
// lists. The subset is unverified against a live import; anything else (code,
// headings, rules) is reduced to text, bold, or line breaks.
func goodreadsReviewHTML(markdown string) (string, error) {
	var out bytes.Buffer
	if err := reviewMarkdown.Convert([]byte(markdown), &out); err != nil {
		return "", err
	}
	s := reviewTagReplacer.Replace(out.String())
	s = reviewHeading.ReplaceAllString(s, "<b>$1</b><br>")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "<p>", ""), "</p>", "<br><br>")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", "")
	s = reviewBreaks.ReplaceAllString(s, "$2")
	for strings.HasSuffix(s, "<br>") {
		s = strings.TrimSuffix(s, "<br>")
	}
	return s, nil
}

// buildGoodreadsExport covers the whole library. Row order is by title, then
// author, then ID; it never reflects or changes the queue.
func buildGoodreadsExport(q queryer, includeReviews bool) (goodreadsExport, error) {
	e := goodreadsExport{IncludeReviews: includeReviews}
	books, err := queryBooks(q, "ORDER BY b.title COLLATE NOCASE,b.author COLLATE NOCASE,b.id")
	if err != nil {
		return e, err
	}
	logged, err := queryReadings(q, readingOrder)
	if err != nil {
		return e, err
	}
	readings := map[int64][]readingEntry{}
	for _, l := range logged {
		readings[l.BookID] = append(readings[l.BookID], l)
	}
	type candidate struct {
		book Book
		isbn string
	}
	byISBN := map[string][]Book{}
	var eligible []candidate
	exclude := func(b Book, reason string) {
		e.Excluded = append(e.Excluded, goodreadsExclusion{b.ID, b.Title, b.Author, reason})
	}
	for _, b := range books {
		if _, ok := goodreadsShelves[b.Status]; !ok {
			exclude(b, fmt.Sprintf("Status “%s” has no Goodreads shelf; it would have to be shown as read or wanted.", b.Status))
			continue
		}
		isbn := cleanISBN(b.ISBN)
		key := isbn13Of(isbn)
		switch {
		case strings.TrimSpace(b.ISBN) == "":
			exclude(b, "No ISBN; Goodreads requires one, and none is guessed.")
		case key == "":
			exclude(b, "The stored ISBN is not valid (wrong length or check digit).")
		default:
			byISBN[key] = append(byISBN[key], b)
			eligible = append(eligible, candidate{b, isbn})
		}
	}
	for _, c := range eligible {
		b := c.book
		if group := byISBN[isbn13Of(c.isbn)]; len(group) > 1 {
			others := []string{}
			for _, o := range group {
				if o.ID != b.ID {
					others = append(others, "“"+o.Title+"”")
				}
			}
			exclude(b, "Shares its ISBN with "+strings.Join(others, ", ")+"; Goodreads would treat them as one book. Merge or correct them first.")
			continue
		}
		row := goodreadsRow{Book: b, ISBN: c.isbn, Shelf: goodreadsShelves[b.Status]}
		own := readings[b.ID]
		var rating, dateRead, review string
		if len(own) > 0 {
			latest := own[0]
			var note string
			rating, note = exportRating(latest.Rating)
			if note != "" {
				row.Notes = append(row.Notes, note)
			}
			if row.Shelf == "read" && latest.FinishedAt != nil {
				if latest.FinishedPrecision <= 1 {
					dateRead = (*latest.FinishedAt)[:min(10, len(*latest.FinishedAt))]
				} else {
					row.Notes = append(row.Notes, "The finish date is only known to the month or year, so Date Read is left blank.")
				}
			}
			if includeReviews {
				for _, l := range own {
					if l.Review == nil || l.ReviewFormat != "markdown" || strings.TrimSpace(*l.Review) == "" {
						continue
					}
					if review, err = goodreadsReviewHTML(*l.Review); err != nil {
						return e, err
					}
					row.ReviewFrom = "your reading finished " + readingDateLabel(l)
					break
				}
			}
		}
		year := ""
		if b.PublishedYear != nil {
			year = strconv.Itoa(*b.PublishedYear)
		}
		row.Fields = []string{b.Title, b.Author, c.isbn, rating, "", "", "", "", year, dateRead, "", row.Shelf, "", review}
		e.Rows = append(e.Rows, row)
	}
	return e, nil
}

func readingDateLabel(l readingEntry) string {
	if l.FinishedAt == nil {
		return "on an unknown date"
	}
	switch l.FinishedPrecision {
	case 3:
		return "in " + (*l.FinishedAt)[:4]
	case 2:
		return "in " + (*l.FinishedAt)[:7]
	}
	return "on " + (*l.FinishedAt)[:10]
}

// WriteCSV writes the header and every row, failing without a partial result
// for the caller to send.
func (e goodreadsExport) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(goodreadsHeader); err != nil {
		return err
	}
	for _, r := range e.Rows {
		if err := cw.Write(r.Fields); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func goodreadsIncludeReviews(r *http.Request) bool { return r.URL.Query().Get("reviews") == "1" }

// goodreadsPreview lists what the CSV will and will not contain.
func (a *App) goodreadsPreview(w http.ResponseWriter, r *http.Request) error {
	var e goodreadsExport
	err := a.store.Write(func(tx *sql.Tx) error {
		var err error
		e, err = buildGoodreadsExport(tx, goodreadsIncludeReviews(r))
		return err
	})
	if err != nil {
		return err
	}
	return a.render(w, r, "goodreads_export.html", page{"Title": "Export for Goodreads", "Export": e}, 200)
}

// goodreadsDownload sends the CSV. The file is built completely first, so a
// failure never produces an apparently successful partial download.
func (a *App) goodreadsDownload(w http.ResponseWriter, r *http.Request) error {
	var buf bytes.Buffer
	err := a.store.Write(func(tx *sql.Tx) error {
		e, err := buildGoodreadsExport(tx, goodreadsIncludeReviews(r))
		if err != nil {
			return err
		}
		return e.WriteCSV(&buf)
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="next-chapter-goodreads.csv"`)
	_, err = w.Write(buf.Bytes())
	return err
}
