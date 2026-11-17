package app

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func bookFields(r *http.Request) (Book, error) {
	b := Book{Title: strings.TrimSpace(r.FormValue("title")), Author: strings.TrimSpace(r.FormValue("author")), Tags: strings.TrimSpace(r.FormValue("tags")), Status: r.FormValue("reading_status"), Series: strings.TrimSpace(r.FormValue("series")), Description: cleanText(r.FormValue("description"), maxDescription), Reason: noteText(r.FormValue("reason")), Owned: r.FormValue("owned") != "", Source: "manual"}
	var err error
	if b.StartedAt, err = startDateField(r); err != nil {
		return b, err
	}
	// A non-numeric interest stays 0, which checkBook rejects.
	b.Interest, _ = strconv.Atoi(r.FormValue("interest"))
	if raw := strings.TrimSpace(r.FormValue("audio_duration")); raw != "" {
		n, err := parseAudioPosition(raw)
		if err != nil {
			return b, err
		}
		b.AudioSeconds = &n
	}
	if raw := r.FormValue("pages"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil {
			n = 0
		}
		b.Pages = &n
	}
	if raw := r.FormValue("series_number"); raw != "" {
		n, e := strconv.ParseFloat(raw, 64)
		if e != nil {
			n = math.NaN()
		}
		b.SeriesNumber = &n
	}
	if raw := strings.TrimSpace(r.FormValue("isbn")); raw != "" {
		if b.ISBN = cleanISBN(raw); b.ISBN == "" {
			return b, bad("Enter a 10 or 13 digit ISBN, or leave it empty.")
		}
	}
	if raw := strings.TrimSpace(r.FormValue("published_year")); raw != "" {
		if b.PublishedYear = parseYear(raw); b.PublishedYear == nil {
			return b, bad("Enter a valid year first published, or leave it empty.")
		}
	}
	if message := checkBook(b); message != "" {
		return b, bad(message)
	}
	return b, nil
}

// checkBook returns why a book's fields are out of bounds, or "". The book
// form and JSON restore share these limits.
func checkBook(b Book) string {
	if strings.TrimSpace(b.Title) == "" || strings.TrimSpace(b.Author) == "" || len([]rune(b.Title)) > 500 || len([]rune(b.Author)) > 300 {
		return "Enter a title and author within the field limits."
	}
	if !in(b.Status, readingStates) {
		return "Choose a reading status."
	}
	if b.Interest < 1 || b.Interest > 5 {
		return "Want-to-read must be between 1 and 5."
	}
	if b.Pages != nil && (*b.Pages < 1 || *b.Pages > 99999) {
		return "Pages must be between 1 and 99,999."
	}
	if b.StartedAt != nil && !validFinishDate(*b.StartedAt) {
		return "Enter a valid start date, or leave it blank."
	}
	if b.AudioSeconds != nil && (*b.AudioSeconds < 1 || *b.AudioSeconds > maxProgressSeconds) {
		return "Total audiobook duration must be greater than 0:00 and at most 9999:59:59."
	}
	count := 0
	for _, v := range []*int{b.ProgressPage, b.ProgressPercent, b.ProgressSeconds} {
		if v != nil {
			count++
		}
	}
	if count > 1 {
		return "Keep progress in one unit only."
	}
	if b.ProgressSeconds != nil && (*b.ProgressSeconds < 0 || *b.ProgressSeconds > maxProgressSeconds) {
		return "Audiobook position is out of range."
	}
	if n := b.SeriesNumber; n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 || *n > 9999) {
		return "Enter a valid series number."
	}
	if len([]rune(b.Description)) > maxDescription || len([]rune(b.DescriptionSource)) > 100 {
		return "The description is too long."
	}
	if len([]rune(b.Reason)) > maxReason {
		return fmt.Sprintf("Keep why you want to read it under %d characters.", maxReason)
	}
	if len([]rune(b.Tags)) > 500 || len([]rune(b.Series)) > 200 {
		return "Tags or series name are too long."
	}
	if (b.PublishedYear != nil && parseYear(strconv.Itoa(*b.PublishedYear)) == nil) || (b.AvgRating != nil && !(*b.AvgRating > 0 && *b.AvgRating <= 5)) {
		return "The publication year or average rating is out of range."
	}
	return ""
}

// validFinishDate accepts a YYYY-MM-DD date between years 1000 and 9999.
func validFinishDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Year() >= 1000
}

// preferenceFields reads the ranking preferences from the settings form.
func preferenceFields(r *http.Request) (Preferences, error) {
	weight, err := strconv.ParseFloat(r.FormValue("library_weight"), 64)
	if err != nil {
		weight = -1
	}
	p := Preferences{LibraryWeight: weight, Length: r.FormValue("length"), Mood: truncate(strings.TrimSpace(r.FormValue("mood")), 100), KeepReviews: r.FormValue("keep_reviews") != "", UseEstimates: r.FormValue("use_estimates") != "", BookLookup: r.FormValue("book_lookup") != "", Hardcover: r.FormValue("hardcover") != ""}
	if message := checkPreferences(p); message != "" {
		return Preferences{}, bad(message)
	}
	return p, nil
}

// checkPreferences returns why preferences are invalid, or "".
func checkPreferences(p Preferences) string {
	if p.LibraryWeight != 0 && p.LibraryWeight != 0.5 && p.LibraryWeight != 1 && p.LibraryWeight != 2 {
		return "Choose a library timing preference."
	}
	if !in(p.Length, []string{"any", "short", "long"}) {
		return "Choose a length preference."
	}
	if len([]rune(p.Mood)) > 100 {
		return "Keep the mood under 100 characters."
	}
	if p.ReadingGoal < 0 || p.ReadingGoal > maxReadingGoal {
		return "Choose a yearly reading goal from 1 to 1000 books, or none."
	}
	if p.HardcoverPrivacy < 0 || p.HardcoverPrivacy > 3 {
		return "Choose a Hardcover privacy setting."
	}
	return ""
}

func requestID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, bad("Choose a valid book.")
	}
	return id, nil
}
func current(q queryer, r *http.Request) error {
	rev, err := revision(q)
	if err != nil {
		return err
	}
	if r.FormValue("revision") != strconv.FormatInt(rev, 10) {
		return conflict("Your library changed since this page loaded. Reload before making another change.")
	}
	return nil
}

// listReturn is the book list page a row action was sent from, so the reader
// stays on that page. Anything but a local list URL falls back to "/".
func listReturn(r *http.Request) string {
	u, err := url.Parse(r.FormValue("return"))
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Path != "/" {
		return "/"
	}
	v := u.Query()
	v.Del("notice")
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}
