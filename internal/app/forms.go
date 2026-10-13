package app

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func bookFields(r *http.Request) (Book, error) {
	b := Book{Title: strings.TrimSpace(r.FormValue("title")), Author: strings.TrimSpace(r.FormValue("author")), Tags: strings.TrimSpace(r.FormValue("tags")), Status: r.FormValue("reading_status"), Series: strings.TrimSpace(r.FormValue("series")), Source: "manual"}
	// A non-numeric interest stays 0, which checkBook rejects.
	b.Interest, _ = strconv.Atoi(r.FormValue("interest"))
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
		return "Interest must be between 1 and 5."
	}
	if b.Pages != nil && (*b.Pages < 1 || *b.Pages > 99999) {
		return "Pages must be between 1 and 99,999."
	}
	if n := b.SeriesNumber; n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 || *n > 9999) {
		return "Enter a valid series number."
	}
	if len([]rune(b.Tags)) > 500 || len([]rune(b.Series)) > 200 {
		return "Tags or series name are too long."
	}
	return ""
}

// preferenceFields reads the ranking preferences from the settings form.
func preferenceFields(r *http.Request) (Preferences, error) {
	weight, err := strconv.ParseFloat(r.FormValue("library_weight"), 64)
	if err != nil {
		weight = -1
	}
	p := Preferences{weight, r.FormValue("length"), truncate(strings.TrimSpace(r.FormValue("mood")), 100)}
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
	return ""
}

// connectionFields reads the Goodreads feed connection from the settings form.
func connectionFields(r *http.Request) (Connection, error) {
	conn := Connection{strings.TrimSpace(r.FormValue("user_id")), strings.TrimSpace(r.FormValue("shelf")), r.FormValue("enabled") != ""}
	if err := validateConnection(conn); err != nil {
		return Connection{}, bad(err.Error())
	}
	return conn, nil
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
