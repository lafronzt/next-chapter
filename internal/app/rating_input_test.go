package app

import (
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestRatingInputRoundTripsEveryQuarter(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{{Title: "Rated", Author: "Writer", Source: "goodreads", Status: "to-read", Interest: 3}})
	id := bookNamed(t, a, "Rated").ID
	b := &browser{t: t, handler: a.Handler()}
	for q := 1; q <= 20; q++ {
		v := strconv.FormatFloat(float64(q)/4, 'f', -1, 64)
		expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), url.Values{"rating": {v}}), 303)
	}
	expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), url.Values{"rating": {""}}), 303)
	rs := readingsOf(t, a, "Rated")
	if len(rs) != 21 {
		t.Fatalf("%d readings", len(rs))
	}
	seen := map[float64]bool{}
	unrated := 0
	for _, r := range rs {
		if r.Rating == nil {
			unrated++
			continue
		}
		seen[*r.Rating] = true
	}
	if len(seen) != 20 || unrated != 1 {
		t.Fatalf("seen %d quarter values, %d unrated", len(seen), unrated)
	}
	var zeros int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM reading_log WHERE rating=0").Scan(&zeros); err != nil || zeros != 0 {
		t.Fatalf("unrated stored as 0: %d %v", zeros, err)
	}
	// Export and restore keep every value.
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, exportOf(t, a))
	expect(t, rr, 303)
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	restored := map[float64]bool{}
	for _, r := range readingsOf(t, target, "Rated") {
		if r.Rating != nil {
			restored[*r.Rating] = true
		}
	}
	if len(restored) != 20 {
		t.Fatalf("restored %d", len(restored))
	}
	for _, bad := range []string{"0", "0.1", "5.25", "x"} {
		expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), url.Values{"rating": {bad}}), 400)
	}
}

func TestRatingInputIsAccessibleAndPreselected(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{finished("Rated", "2025-01-02", 4.25)})
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", fmt.Sprintf("/books/%d/edit", bookNamed(t, a, "Rated").ID), nil).Body.String()
	if n := strings.Count(body, `<fieldset class="rating-input">`); n != 2 {
		t.Fatalf("%d rating controls, want the edit and add forms", n)
	}
	if strings.Count(body, `<legend>Your rating`) != 2 || strings.Contains(body, `<select name="rating">`) {
		t.Fatal("legend or old select")
	}
	// Each option has an accessible name, and the saved value is checked once.
	if got := len(regexp.MustCompile(`<span class="sr-only">\d(\.\d+)? out of 5</span>`).FindAllString(body, -1)); got < 40 {
		t.Fatalf("%d named options", got)
	}
	if !strings.Contains(body, `value="4.25" checked`) || strings.Count(body, `value="4.25" checked`) != 1 {
		t.Fatal("saved rating not preselected")
	}
	if !strings.Contains(body, "currently 4.25 out of 5") || !strings.Contains(body, "currently unrated") {
		t.Fatal("current value not shown as text")
	}
}

func TestQuarterStarDisplayShowsPartialStar(t *testing.T) {
	t.Parallel()
	r := 0.25
	got := string(templateFuncs()["stars"].(func(*float64) template.HTML)(&r))
	if !strings.Contains(got, `star-part q1`) || strings.Contains(got, "★") {
		t.Fatal(got)
	}
}
