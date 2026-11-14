package app

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
)

// readingFixture logs readings directly: (finished, rating) pairs, with nil
// rating meaning unrated and "" meaning undated.
func readingFixture(t *testing.T, a *App, rows [][2]any) {
	t.Helper()
	for i, row := range rows {
		title := fmt.Sprintf("Book %02d", i)
		addRecords(t, a, records(title))
		id := bookNamed(t, a, title).ID
		var finished, rating any
		if s := row[0].(string); s != "" {
			finished = s
		}
		rating = row[1]
		if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(?,?,?,'manual',?)", id, finished, rating, fmt.Sprintf("manual:t%d", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestYearSectionsAndAveragesWithQuarterStars(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	readingFixture(t, a, [][2]any{{"2025-03-01", 4.25}, {"2025-07-09", 3.75}, {"2025-08-01", nil}, {"2024-01-02", 5.0}, {"", 2.5}})
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/reading", nil).Body.String()
	positions := []int{strings.Index(body, `id="year-2025-s"`), strings.Index(body, `id="year-2024-s"`), strings.Index(body, `id="year-none-s"`)}
	if positions[0] < 0 || positions[0] > positions[1] || positions[1] > positions[2] {
		t.Fatalf("year order %v", positions)
	}
	// 2025 averages the two rated readings (4.25 and 3.75 = 4.00), not the unrated one.
	if !strings.Contains(body, "3 readings · averaging 4.00★ over 2 rated") || !strings.Contains(body, "1 reading · averaging 5.00★ over 1 rated") {
		t.Fatal("per-year summary")
	}
	if !strings.Contains(body, "5 books across 2 years, averaging 3.9★") {
		t.Fatalf("summary sentence: %s", regexp.MustCompile(`reading-summary">[^<]*`).FindString(body))
	}
	if !strings.Contains(body, "finished March") {
		t.Fatal("finish month")
	}
}

func TestRatingDistributionSumsToRatedReadings(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	rows := [][2]any{{"2025-01-01", 0.25}, {"2025-01-02", 1.0}, {"2025-01-03", 3.25}, {"2025-01-04", 4.25}, {"2025-01-05", 4.5}, {"2025-01-06", 4.75}, {"2025-01-07", 5.0}, {"2025-01-08", nil}, {"2025-01-09", nil}}
	readingFixture(t, a, rows)
	d, err := loadDistribution(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, bk := range d.Buckets {
		total += bk.Count
		q := 0
		for _, n := range bk.Quarters {
			q += n
		}
		if q != bk.Count {
			t.Errorf("%d★ quarters %v do not sum to %d", bk.Stars, bk.Quarters, bk.Count)
		}
	}
	if total != 7 || d.Rated != 7 || d.Unrated != 2 {
		t.Fatalf("%+v", d)
	}
	if d.Buckets[0].Count != 2 || d.Buckets[4].Count != 4 || d.Buckets[2].Count != 0 || d.Buckets[3].Count != 1 {
		t.Fatalf("%+v", d.Buckets)
	}
	// 4.25, 4.5, 4.75, and 5 all fall in the 5★ bucket: (4, 5].
	if d.Buckets[4].Quarters != [4]int{1, 1, 1, 1} {
		t.Fatalf("%v", d.Buckets[4].Quarters)
	}
	body := (&browser{t: t, handler: a.Handler()}).request("GET", "/reading", nil).Body.String()
	if !strings.Contains(body, "<th scope=\"row\">Unrated</th><td></td><td>2</td>") {
		t.Fatal("unrated row")
	}
	// The average ignores unrated readings: (0.25+1+3.25+4.25+4.5+4.75+5)/7.
	stats, _ := historyStats(a.store.DB)
	if want := (0.25 + 1 + 3.25 + 4.25 + 4.5 + 4.75 + 5) / 7; math.Abs(stats.AverageRating-want) > 1e-9 {
		t.Fatalf("%v", stats.AverageRating)
	}
}

func TestReadingFiltersCombineAndPage(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	rows := [][2]any{}
	for i := 0; i < 12; i++ {
		rows = append(rows, [2]any{fmt.Sprintf("2025-%02d-01", i+1), 4.0})
	}
	rows = append(rows, [2]any{"2025-06-15", 2.0}, [2]any{"2025-07-15", nil}, [2]any{"2024-05-05", 5.0}, [2]any{"", nil})
	readingFixture(t, a, rows)
	b := &browser{t: t, handler: a.Handler()}
	count := func(path string) int {
		return strings.Count(b.request("GET", path, nil).Body.String(), `<li>
      <a class="book-cover`)
	}
	cases := map[string]int{
		"/reading?year=2025":                  14,
		"/reading?year=2025&min=4":            12,
		"/reading?min=4.5":                    1,
		"/reading?unrated=1":                  2,
		"/reading?year=2025&unrated=1":        1,
		"/reading?year=undated":               1,
		"/reading?year=2024&min=3":            1,
		"/reading?min=5&unrated=1":            0,
		"/reading?year=bogus&min=7&unrated=x": 16,
	}
	for path, want := range cases {
		if got := count(path); got != want {
			t.Errorf("%s: %d readings, want %d", path, got, want)
		}
	}
	// Paging keeps the filters.
	body := b.request("GET", "/reading?year=2025&min=4&per_page=5", nil).Body.String()
	next := regexp.MustCompile(`href="([^"]*)" rel="next"`).FindStringSubmatch(body)
	if next == nil || !strings.Contains(next[1], "year=2025") || !strings.Contains(next[1], "min=4") {
		t.Fatalf("%v", next)
	}
	page2 := b.request("GET", strings.ReplaceAll(next[1], "&amp;", "&"), nil).Body.String()
	if strings.Count(page2, `<li>
      <a class="book-cover`) != 5 || !strings.Contains(page2, "(continued)") {
		t.Fatal("second filtered page")
	}
}

func TestReviewTextNeverRendersOnTheHistoryPageWithoutOptIn(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	readingFixture(t, a, [][2]any{{"2025-03-01", 4.0}})
	if _, err := a.store.DB.Exec("UPDATE reading_log SET review='A SECRET REVIEW', review_chars=15"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	if body := b.request("GET", "/reading", nil).Body.String(); strings.Contains(body, "A SECRET REVIEW") {
		t.Fatal("review text rendered on the history page")
	}
}
