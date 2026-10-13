package app

import (
	"math"
	"net/http"
	"strconv"
	"strings"
)

func bookFields(r *http.Request) (Book, error) {
	b := Book{Title: strings.TrimSpace(r.FormValue("title")), Author: strings.TrimSpace(r.FormValue("author")), Tags: strings.TrimSpace(r.FormValue("tags")), Status: r.FormValue("reading_status"), Series: strings.TrimSpace(r.FormValue("series")), Source: "manual"}
	if b.Title == "" || b.Author == "" || len([]rune(b.Title)) > 500 || len([]rune(b.Author)) > 300 {
		return b, bad("Enter a title and author within the field limits.")
	}
	if !in(b.Status, readingStates) {
		return b, bad("Choose a reading status.")
	}
	var err error
	b.Interest, err = strconv.Atoi(r.FormValue("interest"))
	if err != nil || b.Interest < 1 || b.Interest > 5 {
		return b, bad("Interest must be between 1 and 5.")
	}
	if raw := r.FormValue("pages"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 99999 {
			return b, bad("Pages must be between 1 and 99,999.")
		}
		b.Pages = &n
	}
	if raw := r.FormValue("series_number"); raw != "" {
		n, e := strconv.ParseFloat(raw, 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9999 {
			return b, bad("Enter a valid series number.")
		}
		b.SeriesNumber = &n
	}
	if len([]rune(b.Tags)) > 500 || len([]rune(b.Series)) > 200 {
		return b, bad("Tags or series name are too long.")
	}
	return b, nil
}

// preferenceFields reads the ranking preferences from the settings form.
func preferenceFields(r *http.Request) (Preferences, error) {
	weight, err := strconv.ParseFloat(r.FormValue("library_weight"), 64)
	if err != nil || (weight != 0 && weight != 0.5 && weight != 1 && weight != 2) {
		return Preferences{}, bad("Choose a library timing preference.")
	}
	length := r.FormValue("length")
	if !in(length, []string{"any", "short", "long"}) {
		return Preferences{}, bad("Choose a length preference.")
	}
	return Preferences{weight, length, truncate(strings.TrimSpace(r.FormValue("mood")), 100)}, nil
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
