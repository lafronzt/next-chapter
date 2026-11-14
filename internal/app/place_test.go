package app

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var candidatePattern = regexp.MustCompile(`name="candidate_id" value="(\d+)"`)

// placementLibrary has a queue of placed books, one pinned, a series, and
// several books waiting for placement.
func placementLibrary(t *testing.T) *App {
	t.Helper()
	a := testApp(t, true)
	book := func(title, tags string, interest int, series string, n float64) Record {
		r := Record{Book: Book{Title: title, Author: "Author " + title, Status: "to-read", Source: "manual", Interest: interest, Tags: tags}}
		if series != "" {
			r.Series, r.SeriesNumber = series, &n
		}
		return r
	}
	addRecords(t, a, []Record{
		book("Alpha", "x", 5, "", 0), book("Bravo", "y", 3, "", 0), book("Charlie", "x", 4, "", 0), book("Delta", "z", 2, "", 0),
	})
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0; UPDATE books SET pinned=1 WHERE title='Bravo'"); err != nil {
		t.Fatal(err)
	}
	addRecords(t, a, []Record{book("Saga 2", "q", 5, "Saga", 2), book("Saga 1", "q", 1, "Saga", 1), book("Echo", "x", 5, "", 0), book("Foxtrot", "z", 1, "", 0)})
	return a
}

func queueOrderOf(t *testing.T, a *App) string {
	t.Helper()
	titles, _ := queueTitles(t, a)
	return strings.Join(titles, ",")
}

func waitingCount(t *testing.T, a *App) int {
	t.Helper()
	var n int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM books WHERE reading_status='to-read' AND needs_placement=1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// drivePlacement places every waiting book with "Put it here", following the
// redirects as a reader would, and returns the final Location.
func drivePlacement(t *testing.T, b *browser, action string) string {
	t.Helper()
	loc := "/place"
	for i := 0; i < 50; i++ {
		rr := b.request("GET", loc, nil)
		if rr.Code == 303 {
			return rr.Header().Get("Location")
		}
		expect(t, rr, 200)
		body := rr.Body.String()
		m := candidatePattern.FindStringSubmatch(body)
		if m == nil {
			t.Fatal("no candidate on the page")
		}
		values := url.Values{"candidate_id": {m[1]}, "action": {action}}
		for _, key := range []string{"total", "placed", "kept", "skip"} {
			if v := regexp.MustCompile(`name="` + key + `" value="([^"]*)"`).FindStringSubmatch(body); v != nil {
				values.Set(key, v[1])
			}
		}
		rr = b.form("/place", values)
		expect(t, rr, 303)
		loc = rr.Header().Get("Location")
	}
	t.Fatal("placement never ended")
	return ""
}

func TestPlacingOneByOneMatchesApplyingSuggestPlans(t *testing.T) {
	t.Parallel()
	viaPlace, viaSuggest := placementLibrary(t), placementLibrary(t)
	if queueOrderOf(t, viaPlace) != queueOrderOf(t, viaSuggest) {
		t.Fatal("fixtures differ")
	}
	b := &browser{t: t, handler: viaPlace.Handler()}
	unplaced := queueOrderOf(t, viaPlace)
	end := drivePlacement(t, b, "here")
	if queueOrderOf(t, viaPlace) == unplaced {
		t.Fatal("placing moved nothing; the fixture does not exercise the engine")
	}
	if !strings.Contains(end, "notice=placement") || !strings.Contains(end, "placed=4") {
		t.Fatal(end)
	}
	s := &browser{t: t, handler: viaSuggest.Handler()}
	for waitingCount(t, viaSuggest) > 0 {
		var id int64
		if err := viaSuggest.store.DB.QueryRow("SELECT id FROM books WHERE reading_status='to-read' AND needs_placement=1 ORDER BY position,id").Scan(&id); err != nil {
			t.Fatal(err)
		}
		rr := s.form("/suggest", url.Values{"candidate_id": {strconv.FormatInt(id, 10)}})
		expect(t, rr, 303)
		plan := rr.Header().Get("Location")
		s.request("GET", plan, nil)
		expect(t, s.request("POST", plan, url.Values{}), 303)
	}
	if got, want := queueOrderOf(t, viaPlace), queueOrderOf(t, viaSuggest); got != want {
		t.Fatalf("/place gave %s, /suggest gave %s", got, want)
	}
	// Pinned and series constraints held throughout.
	order := strings.Split(queueOrderOf(t, viaPlace), ",")
	idx := map[string]int{}
	for i, title := range order {
		idx[title] = i
	}
	if idx["Bravo"] != 1 {
		t.Fatalf("pinned book moved: %s", order)
	}
	if idx["Saga 1"] > idx["Saga 2"] {
		t.Fatalf("series out of order: %s", order)
	}
	if waitingCount(t, viaPlace) != 0 {
		t.Fatal("books still waiting")
	}
}

func TestDecideLaterChangesNothingAndTheBannerStays(t *testing.T) {
	t.Parallel()
	a := placementLibrary(t)
	before, waiting := queueOrderOf(t, a), waitingCount(t, a)
	positions := positionsOf(t, a)
	b := &browser{t: t, handler: a.Handler()}
	if body := b.request("GET", "/", nil).Body.String(); !strings.Contains(body, "4 books need a spot.") {
		t.Fatal("banner missing")
	}
	end := drivePlacement(t, b, "later")
	if !strings.Contains(end, "skipped=4") || !strings.Contains(end, "placed=0") {
		t.Fatal(end)
	}
	if queueOrderOf(t, a) != before || positionsOf(t, a) != positions || waitingCount(t, a) != waiting {
		t.Fatal("deciding later changed the library")
	}
	body := b.request("GET", end, nil).Body.String()
	if !strings.Contains(body, "4 left for later") || !strings.Contains(body, "4 books need a spot.") {
		t.Fatal("notice or banner missing after skipping everything")
	}
}

func TestKeepAtTheEndClearsOnlyTheFlag(t *testing.T) {
	t.Parallel()
	a := placementLibrary(t)
	before := queueOrderOf(t, a)
	b := &browser{t: t, handler: a.Handler()}
	end := drivePlacement(t, b, "keep")
	if queueOrderOf(t, a) != before || waitingCount(t, a) != 0 || !strings.Contains(end, "kept=4") {
		t.Fatal(end, queueOrderOf(t, a))
	}
}

func TestPlacementWithAStaleRevisionLosesNothing(t *testing.T) {
	t.Parallel()
	a := placementLibrary(t)
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/place", nil).Body.String()
	id := candidatePattern.FindStringSubmatch(body)[1]
	before := queueOrderOf(t, a)
	// Another tab changes the library after this page loaded.
	expect(t, b.form("/books/1/action", url.Values{"action": {"pin"}}), 303)
	rr := b.request("POST", "/place", url.Values{"candidate_id": {id}, "action": {"here"}, "revision": {"0"}})
	expect(t, rr, 409)
	if !strings.Contains(rr.Body.String(), "Your library changed since this page loaded") {
		t.Fatal(rr.Body.String())
	}
	if queueOrderOf(t, a) != before || waitingCount(t, a) != 4 {
		t.Fatal("a stale placement changed the library")
	}
	expect(t, b.form("/place", url.Values{"candidate_id": {id}, "action": {"burn"}}), 400)
}

func TestPlacePageShowsProgressAndNeighbors(t *testing.T) {
	t.Parallel()
	a := placementLibrary(t)
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/place", nil).Body.String()
	for _, want := range []string{"Book 1 of 4", "Back to my queue", "Suggested spot: number", `class="candidate"`, "Put it here", "Keep it at the end", "Decide later"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// With nothing waiting, /place returns to the queue.
	for waitingCount(t, a) > 0 {
		a.store.DB.Exec("UPDATE books SET needs_placement=0")
	}
	expect(t, b.request("GET", "/place", nil), 303)
}
