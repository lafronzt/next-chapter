package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxUpload = 2_000_000
const maxRows = 10_000

func normalized(s string) string {
	s = cases.Fold().String(norm.NFKC.String(s))
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			return r
		}
		return -1
	}, s)
}
func identity(title, author string) string { return normalized(title) + "|" + normalized(author) }
func cleanISBN(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		if r == 'X' || r == 'x' {
			return 'X'
		}
		return -1
	}, s)
	if len(s) != 10 && len(s) != 13 {
		return ""
	}
	return s
}
func canonicalTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000+00:00") }
func parseTime(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) == 10 || len(value) == 13 {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			if len(value) == 13 {
				return canonicalTime(time.UnixMilli(n)), nil
			}
			return canonicalTime(time.Unix(n, 0)), nil
		}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", time.DateOnly, "2006/01/02", "01/02/2006", "1/2/2006", "January 2, 2006", "02 Jan 2006", time.RFC1123Z, time.RFC1123, time.RFC822Z} {
		// Offsets can carry year 1 or 9999 across the edge, where the
		// canonical form stops being four-digit RFC 3339.
		if t, err := time.Parse(layout, value); err == nil && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999 {
			return canonicalTime(t), nil
		}
	}
	return "", fmt.Errorf("unrecognized date %q; use YYYY-MM-DD or ISO 8601", truncate(value, 50))
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
func field(row map[string]string, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(row[normalized(name)]); v != "" {
			return v
		}
	}
	return ""
}
func activityStatus(s string) (string, error) {
	status := map[string]string{"borrowed": "borrowed", "loan": "borrowed", "loans": "borrowed", "checkout": "borrowed", "checkedout": "borrowed", "renewed": "borrowed", "renewal": "borrowed", "renew": "borrowed", "hold": "hold", "holds": "hold", "placedonhold": "hold", "requested": "hold", "ready": "ready", "readyforpickup": "ready", "available": "available", "returned": "returned", "return": "returned", "cancelled": "cancelled", "canceled": "cancelled"}[normalized(s)]
	if status == "" {
		return "", fmt.Errorf("unknown library activity %q; use borrowed, hold, ready, returned, or cancelled", truncate(s, 40))
	}
	return status, nil
}

func ParseCSV(data []byte, source string) ([]Record, []string, error) {
	if !in(source, []string{"goodreads", "libby", "nypl"}) {
		return nil, nil, fmt.Errorf("choose a supported import source")
	}
	if len(data) > maxUpload {
		return nil, nil, fmt.Errorf("CSV files must be at most 2 MB")
	}
	if !utf8.Valid(data) {
		return nil, nil, fmt.Errorf("save the file as UTF-8 CSV before importing")
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff")))
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("CSV needs a header row with Title and Author")
	}
	keys := []string{}
	seen := map[string]bool{}
	for _, h := range headers {
		key := normalized(h)
		if seen[key] {
			return nil, nil, fmt.Errorf("duplicate CSV column %q", h)
		}
		seen[key] = true
		keys = append(keys, key)
	}
	if !seen["title"] {
		return nil, nil, fmt.Errorf("CSV needs a Title column; see the downloadable examples")
	}
	records := []Record{}
	errors := []string{}
	for rowNumber := 2; ; rowNumber++ {
		values, e := reader.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, nil, fmt.Errorf("invalid CSV near row %d: %w", rowNumber, e)
		}
		if rowNumber > maxRows+1 {
			return nil, nil, fmt.Errorf("import at most 10,000 rows at a time")
		}
		row := map[string]string{}
		for i, v := range values {
			if i < len(keys) {
				row[keys[i]] = v
			}
		}
		r, e := parseRecord(row, source)
		if e != nil {
			errors = append(errors, fmt.Sprintf("Row %d: %s", rowNumber, e))
		} else {
			records = append(records, r)
		}
	}
	if len(records) == 0 && len(errors) == 0 {
		return nil, nil, fmt.Errorf("the CSV contains no book rows")
	}
	return records, errors, nil
}
func parseRecord(row map[string]string, source string) (Record, error) {
	title, author := field(row, "title"), field(row, "author", "authors", "author name", "creator")
	if title == "" || author == "" || len([]rune(title)) > 500 || len([]rune(author)) > 300 {
		return Record{}, fmt.Errorf("Title (up to 500 characters) and Author (up to 300) are required")
	}
	b := Book{Title: title, Author: author, ISBN: cleanISBN(field(row, "isbn13", "isbn")), Source: source, Status: "to-read", Interest: 3, NeedsPlacement: true}
	if n, e := strconv.Atoi(field(row, "number of pages", "pages")); e == nil && n > 0 && n < 100000 {
		b.Pages = &n
	}
	r := Record{Book: b}
	if source == "goodreads" {
		r.GoodreadsID = field(row, "book id")
		r.Tags = truncate(field(row, "bookshelves"), 500)
		switch field(row, "exclusive shelf") {
		case "read":
			r.Status = "read"
		case "currently-reading":
			r.Status = "reading"
		case "to-read", "":
		default:
			r.Status = "archived"
		}
	} else {
		r.Status = "unreviewed"
		activity := field(row, "activity", "action", "status")
		status, e := activityStatus(activity)
		if e != nil {
			return r, e
		}
		at, e := parseTime(field(row, "timestamp", "date", "event date", "updated at"))
		if e != nil {
			return r, e
		}
		if at == "" {
			return r, fmt.Errorf("a Timestamp or Date is required for history reconciliation")
		}
		due, e := parseTime(field(row, "due date", "due"))
		if e != nil {
			return r, e
		}
		if due != "" {
			due = due[:10]
		}
		format := truncate(field(row, "format", "media", "media type"), 100)
		if format == "" {
			format = "digital"
			if source == "nypl" {
				format = "print"
			}
		}
		r.Event = &Event{Status: status, Activity: truncate(activity, 100), At: at, Format: format, DueDate: due}
	}
	return r, nil
}
func ParseRSS(data []byte) ([]Record, error) {
	upper := bytes.ToUpper(data)
	if len(data) > maxUpload || bytes.Contains(upper, []byte("<!DOCTYPE")) || bytes.Contains(upper, []byte("<!ENTITY")) {
		return nil, fmt.Errorf("unsupported or oversized Goodreads feed")
	}
	var feed struct {
		XMLName xml.Name `xml:"rss"`
		Items   []struct {
			Title       string `xml:"title"`
			BookTitle   string `xml:"book_title"`
			Author      string `xml:"author_name"`
			ID          string `xml:"book_id"`
			ISBN        string `xml:"isbn"`
			Pages       string `xml:"num_pages"`
			NestedPages string `xml:"book>num_pages"`
		} `xml:"channel>item"`
	}
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("Goodreads returned an unreadable RSS feed; try CSV import")
	}
	records := []Record{}
	for i, item := range feed.Items {
		if i >= maxRows {
			break
		}
		title := strings.TrimSpace(item.BookTitle)
		if title == "" {
			title = strings.TrimSpace(item.Title)
		}
		author := strings.TrimSpace(item.Author)
		if title == "" || author == "" {
			continue
		}
		r := Record{Book: Book{Title: truncate(title, 500), Author: truncate(author, 300), GoodreadsID: strings.TrimSpace(item.ID), ISBN: cleanISBN(item.ISBN), Source: "goodreads", Status: "to-read", Interest: 3, NeedsPlacement: true}}
		pages := item.Pages
		if pages == "" {
			pages = item.NestedPages
		}
		if n, e := strconv.Atoi(pages); e == nil && n > 0 && n < 100000 {
			r.Pages = &n
		}
		records = append(records, r)
	}
	return records, nil
}
func fingerprint(r Record) string {
	e := r.Event
	raw := strings.Join([]string{r.Source, identity(r.Title, r.Author), e.Format, e.Status, e.At, e.DueDate}, "|")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
