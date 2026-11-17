package app

import (
	"cmp"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"
)

// readingEntry is one finished reading. Readings are a separate stream from
// reading status, interest, queue order, and library events, and never change
// any of them.
type readingEntry struct {
	ID          int64    `json:"id"`
	BookID      int64    `json:"book_id"`
	StartedAt   *string  `json:"started_at,omitempty"`
	FinishedAt  *string  `json:"finished_at"` // YYYY-MM-DD, or nil when unknown
	Rating      *float64 `json:"rating"`
	Source      string   `json:"source"` // goodreads or manual
	ReviewChars int      `json:"review_chars"`
	Review      *string  `json:"review"`
	Fingerprint string   `json:"fingerprint"`
	// FinishedPrecision is 1 for a known day, 2 when only the month is known,
	// and 3 when only the year is; FinishedAt is then the first day of that
	// period. HardcoverReadID ties the row to one Hardcover read.
	FinishedPrecision int    `json:"finished_precision,omitempty"`
	HardcoverReadID   *int64 `json:"hardcover_read_id,omitempty"`
	// Title and Author are filled for the history page; they are not stored.
	Title  string `json:"title,omitempty"`
	Author string `json:"author,omitempty"`
}

// Readings added by hand get a random fingerprint with this prefix; any other
// fingerprint came from Goodreads, even after the reader edited the reading.
const manualFingerprint = "manual:"

// When is the finish date as the reader should see it: the day, only the month
// and year, or only the year, never a day that Hardcover did not give.
func (e readingEntry) When() string {
	if e.FinishedAt == nil || len(*e.FinishedAt) < 10 {
		return ""
	}
	t, err := time.Parse(time.DateOnly, (*e.FinishedAt)[:10])
	if err != nil {
		return *e.FinishedAt
	}
	switch e.FinishedPrecision {
	case 2:
		return t.Format("January 2006")
	case 3:
		return t.Format("2006")
	}
	return t.Format(time.DateOnly)
}

// AddedByHand reports whether the reader created this reading in the app.
func (e readingEntry) AddedByHand() bool { return strings.HasPrefix(e.Fingerprint, manualFingerprint) }

// readingFingerprint identifies a Goodreads reading by book and finish date.
// The rating is deliberately left out: re-rating a book on Goodreads updates
// the same reading rather than adding a second one.
func readingFingerprint(r Record) string {
	key := r.GoodreadsID
	if key == "" {
		key = identity(r.Title, r.Author)
	}
	sum := sha256.Sum256([]byte("goodreads|" + key + "|" + r.FinishedAt))
	return hex.EncodeToString(sum[:])
}

func nullable[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// logReading records the Goodreads reading in r for book id, returning
// whether it added a row and whether it changed anything.
//
// A reading with the same fingerprint is this record's own reading: its rating
// and review follow Goodreads while its source is still goodreads, and once
// the reader edits it in the app it becomes manual and an import leaves it alone. A
// reading of the same book on the same date that came from elsewhere (a
// merged book's record, or the reader) is already recorded and is left alone
// too; updating it would let two records fight over one reading. A dated reading fills in an undated
// one from an earlier import, and an undated reading adds nothing to a book
// that already has one. A new Date Read after a re-read adds a second row, so
// history grows even though Goodreads keeps only the latest date.
func logReading(q queryer, id int64, r Record, keepReviews bool) (added, changed bool, err error) {
	fp := readingFingerprint(r)
	finished, rating := nullable(r.FinishedAt), nullable(r.Rating)
	var review *string
	if keepReviews {
		review = nullable(r.Review)
	}
	var rowID int64
	var source string
	var own bool
	err = q.QueryRow("SELECT id,source,fingerprint=? FROM reading_log WHERE fingerprint=? OR (book_id=? AND finished_at IS ?) ORDER BY fingerprint=? DESC LIMIT 1", fp, fp, id, finished, fp).Scan(&rowID, &source, &own)
	if err == nil {
		if !own || source != "goodreads" {
			return false, false, nil
		}
		changed, err = execChanged(q, "UPDATE reading_log SET rating=?,review_chars=?,review=? WHERE id=? AND (rating IS NOT ? OR review_chars!=? OR review IS NOT ?)", rating, r.ReviewChars, review, rowID, rating, r.ReviewChars, review)
		return false, changed, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, false, err
	}
	if finished != nil {
		changed, err = execChanged(q, "UPDATE reading_log SET finished_at=?,fingerprint=?,rating=?,review_chars=?,review=? WHERE id=(SELECT MIN(id) FROM reading_log WHERE book_id=? AND source='goodreads' AND finished_at IS NULL)", finished, fp, rating, r.ReviewChars, review, id)
		if err != nil || changed {
			return false, changed, err
		}
	} else {
		var n int
		if err = q.QueryRow("SELECT count(*) FROM reading_log WHERE book_id=?", id).Scan(&n); err != nil || n > 0 {
			return false, false, err
		}
	}
	_, err = q.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,review_chars,review,fingerprint) VALUES(?,?,?,'goodreads',?,?,?)", id, finished, rating, r.ReviewChars, review, fp)
	return err == nil, err == nil, err
}

// logsHistory reports whether a record carries a finished reading: a Goodreads
// shelf entry (CSV or RSS) on the read shelf.
func logsHistory(r Record) bool {
	return r.Source == "goodreads" && r.Event == nil && r.Status == "read"
}

const readingColumns = "l.id,l.book_id,l.finished_at,l.rating,l.source,l.review_chars,l.review,l.fingerprint,l.finished_precision,l.hardcover_read_id,l.started_at,b.title,b.author"

func queryReadings(q queryer, tail string, args ...any) ([]readingEntry, error) {
	rows, err := q.Query("SELECT "+readingColumns+" FROM reading_log l JOIN books b ON b.id=l.book_id "+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []readingEntry{}
	for rows.Next() {
		var e readingEntry
		if err = rows.Scan(&e.ID, &e.BookID, &e.FinishedAt, &e.Rating, &e.Source, &e.ReviewChars, &e.Review, &e.Fingerprint, &e.FinishedPrecision, &e.HardcoverReadID, &e.StartedAt, &e.Title, &e.Author); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// readingOrder lists the newest readings first, undated ones last. A
// year-only date is stored as 1 January, which would sort it after every
// other reading that year, and sorting it at the end of the year would invent
// a December date. It is a bucket of its own instead: within its year, after
// the readings whose day or month is known, before any earlier year.
const readingOrder = " ORDER BY l.finished_at IS NULL, substr(l.finished_at,1,4) DESC, l.finished_precision=3, l.finished_at DESC, l.id DESC"

func bookReadings(q queryer, bookID int64) ([]readingEntry, error) {
	return queryReadings(q, "WHERE l.book_id=?"+readingOrder, bookID)
}

// loadReadingLog returns every reading in insertion order, for the export.
func loadReadingLog(q queryer) ([]readingEntry, error) {
	entries, err := queryReadings(q, "ORDER BY l.id")
	for i := range entries {
		entries[i].Title, entries[i].Author = "", ""
	}
	return entries, err
}

type yearCount struct {
	Year  string
	Books int
}
type tagCount struct {
	Tag   string
	Books int
}
type readingStats struct {
	Readings, Books, Undated, Rated int
	AverageRating, AveragePages     float64
	PerYear                         []yearCount
	MostPerYear                     int
	TopTags                         []tagCount
}

// historyStats summarizes the reading log. The rating average is over rated
// readings; the page average counts each finished book once; tags are the
// finished books' tags, compared case-insensitively.
func historyStats(q queryer) (readingStats, error) {
	var s readingStats
	var avgRating, avgPages sql.NullFloat64
	err := q.QueryRow(`SELECT count(*), count(DISTINCT book_id), count(*)-count(finished_at), count(rating), avg(rating),
    (SELECT avg(pages) FROM books WHERE pages IS NOT NULL AND id IN (SELECT book_id FROM reading_log)) FROM reading_log`).Scan(&s.Readings, &s.Books, &s.Undated, &s.Rated, &avgRating, &avgPages)
	if err != nil {
		return s, err
	}
	s.AverageRating, s.AveragePages = avgRating.Float64, avgPages.Float64
	rows, err := q.Query("SELECT substr(finished_at,1,4), count(*) FROM reading_log WHERE finished_at IS NOT NULL GROUP BY 1 ORDER BY 1 DESC")
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var y yearCount
		if err = rows.Scan(&y.Year, &y.Books); err != nil {
			rows.Close()
			return s, err
		}
		s.PerYear = append(s.PerYear, y)
		s.MostPerYear = max(s.MostPerYear, y.Books)
	}
	err = rows.Err()
	// The store has one connection: close these rows before the next query.
	rows.Close()
	if err != nil {
		return s, err
	}
	rows, err = q.Query("SELECT tags FROM books WHERE tags!='' AND id IN (SELECT book_id FROM reading_log)")
	if err != nil {
		return s, err
	}
	defer rows.Close()
	counts, names := map[string]int{}, map[string]string{}
	for rows.Next() {
		var tags string
		if err = rows.Scan(&tags); err != nil {
			return s, err
		}
		seen := map[string]bool{}
		for _, tag := range strings.Split(tags, ",") {
			tag = strings.TrimSpace(tag)
			key := strings.ToLower(tag)
			if tag == "" || seen[key] {
				continue
			}
			seen[key] = true
			counts[key]++
			if _, ok := names[key]; !ok {
				names[key] = tag
			}
		}
	}
	for key, n := range counts {
		s.TopTags = append(s.TopTags, tagCount{names[key], n})
	}
	slices.SortFunc(s.TopTags, func(a, b tagCount) int {
		return cmp.Or(cmp.Compare(b.Books, a.Books), cmp.Compare(strings.ToLower(a.Tag), strings.ToLower(b.Tag)))
	})
	s.TopTags = s.TopTags[:min(5, len(s.TopTags))]
	return s, rows.Err()
}
