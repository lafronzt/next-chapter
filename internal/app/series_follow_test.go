package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// seriesFake is a synthetic Hardcover that answers the series check query
// from per-series member lists, with pagination and injectable failures.
type fakeMember struct {
	ID          int
	Title       string
	Author      string
	Pos         *float64
	Release     string
	Year        int
	Compilation bool
	Details     string
}

type fakeSeries struct {
	Name      string
	Slug      string
	Canonical int
	Members   []fakeMember
}

type seriesFake struct {
	mu       sync.Mutex
	series   map[int]*fakeSeries
	vars     []map[string]any
	queries  []string
	failFrom int  // fail offsets at or above this, when > 0
	failAll  bool // fail every request
	status   int  // HTTP status for failures; 500 when zero
}

func (f *seriesFake) handle(query string, vars map[string]any, _ int) (int, map[string]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
	if !strings.Contains(query, "NextChapterSeriesCheck") {
		return 500, nil, `{}`
	}
	f.vars = append(f.vars, vars)
	id, offset, limit := int(vars["id"].(float64)), int(vars["offset"].(float64)), int(vars["limit"].(float64))
	if f.failAll || f.failFrom > 0 && offset >= f.failFrom {
		st := f.status
		if st == 0 {
			st = 500
		}
		return st, map[string]string{"Retry-After": "30"}, `{}`
	}
	s := f.series[id]
	if s == nil {
		return 200, nil, `{"data":{"series_by_pk":null}}`
	}
	members := []map[string]any{}
	if s.Canonical == 0 {
		for i := offset; i < len(s.Members) && i < offset+limit; i++ {
			m := s.Members[i]
			var release any
			if m.Release != "" {
				release = m.Release
			}
			var pos any
			if m.Pos != nil {
				pos = *m.Pos
			}
			members = append(members, map[string]any{"position": pos, "details": m.Details, "compilation": m.Compilation,
				"book": map[string]any{"id": m.ID, "title": m.Title, "slug": fmt.Sprintf("book-%d", m.ID), "release_date": release, "release_year": m.Year, "pages": 300,
					"contributions": []map[string]any{{"author": map[string]any{"name": m.Author}}}}})
		}
	}
	var canonical any
	if s.Canonical > 0 {
		canonical = s.Canonical
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"series_by_pk": map[string]any{"id": id, "name": s.Name, "slug": s.Slug, "canonical_id": canonical, "books_count": len(s.Members), "book_series": members}}})
	return 200, nil, string(raw)
}

func (f *seriesFake) requests() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.queries) }

func members(n int, startID int) []fakeMember {
	out := make([]fakeMember, n)
	for i := range out {
		out[i] = fakeMember{ID: startID + i, Title: fmt.Sprintf("Volume %d", i+1), Author: "S. Author", Pos: fp(float64(i + 1)), Year: 2010 + i%10}
	}
	return out
}

// seriesApp makes an app whose local series "Echo" has two linked books that
// the cache says belong to the given Hardcover series ids.
func followApp(t *testing.T, f *seriesFake, cacheIDs map[int]string) (*App, *browser, []int64) {
	t.Helper()
	a, b, _ := newHCApp(t, f.handle)
	ids := addBooks(t, a, Book{Title: "Echo One", Author: "S. Author", Series: "Echo"}, Book{Title: "Queue Anchor", Author: "Z"})
	if _, err := a.store.DB.Exec("UPDATE books SET series_number=1 WHERE id=?", ids[0]); err != nil {
		t.Fatal(err)
	}
	link(t, a, ids[0], 1)
	for sid, name := range cacheIDs {
		if _, err := a.store.DB.Exec("INSERT INTO series_cache(source_book_id,series_id,series_name,series_total,hardcover_book_id,position,title,author,fetched_at) VALUES(?,?,?,?,?,?,?,?,?)", ids[0], sid, name, 3, 1, 1, "Echo One", "S. Author", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	b.request("GET", "/series", nil)
	return a, b, ids
}

func followSeries(t *testing.T, b *browser, group string, id int) {
	t.Helper()
	expect(t, b.form("/series/follow", url.Values{"group": {group}, "series": {fmt.Sprint(id)}}), 303)
}

func checkSeries(t *testing.T, a *App, b *browser, ids ...int) {
	t.Helper()
	refill(a)
	var v []string
	for _, id := range ids {
		v = append(v, fmt.Sprint(id))
	}
	rr := b.form("/series/check", url.Values{"series": v})
	if rr.Code != 303 {
		t.Fatalf("check: %d %s", rr.Code, rr.Body)
	}
}

func viewsOf(t *testing.T, a *App, sid int) map[string][]string {
	t.Helper()
	views, err := a.discoveryViews(a.store.DB, int64(sid))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, v := range views {
		out[v.Category] = append(out[v.Category], v.Title)
	}
	return out
}

func TestSeriesFollowNeedsNoNetworkAndShowsIDsForSameNames(t *testing.T) {
	f := &seriesFake{series: map[int]*fakeSeries{101: {Name: "Echo"}, 102: {Name: "Echo"}}}
	a, b, _ := followApp(t, f, map[int]string{101: "Echo", 102: "Echo"})
	page := b.request("GET", "/series", nil).Body.String()
	for _, want := range []string{"Follow Echo (series 101)", "Follow Echo (series 102)", "You are not following any series"} {
		if !strings.Contains(page, want) {
			t.Errorf("series page missing %q", want)
		}
	}
	confirm := b.request("GET", "/series/follow?group=Echo&series=102", nil).Body.String()
	if !strings.Contains(confirm, "Hardcover series number 102") || !strings.Contains(confirm, "reads nothing from Hardcover") {
		t.Fatal("confirmation page")
	}
	// A series that is not one of this group's choices cannot be followed.
	expect(t, b.form("/series/follow", url.Values{"group": {"Echo"}, "series": {"999"}}), 400)
	followSeries(t, b, "Echo", 102)
	if f.requests() != 0 {
		t.Fatal("following contacted Hardcover")
	}
	page = b.request("GET", "/series", nil).Body.String()
	if !strings.Contains(page, `href="/series/followed/102"`) || strings.Contains(page, "Follow Echo (series 102)") || !strings.Contains(page, "Follow Echo (series 101)") || f.requests() != 0 {
		t.Fatal("the follow is not shown, or the page read Hardcover")
	}
	// Unfollowing needs its confirmation and keeps every book.
	books := dbValue[int](t, a, "SELECT count(*) FROM books")
	expect(t, b.form("/series/followed/102/unfollow", nil), 400)
	expect(t, b.form("/series/followed/102/unfollow", url.Values{"confirm": {"unfollow"}}), 303)
	if dbValue[int](t, a, "SELECT count(*) FROM series_follows") != 0 || dbValue[int](t, a, "SELECT count(*) FROM books") != books {
		t.Fatal("unfollow changed the wrong data")
	}
}

func TestSeriesFirstCheckIsABaselineThenFindsNewBooks(t *testing.T) {
	s := &fakeSeries{Name: "Echo", Slug: "echo", Members: members(3, 1)}
	f := &seriesFake{series: map[int]*fakeSeries{101: s}}
	a, b, ids := followApp(t, f, map[int]string{101: "Echo"})
	followSeries(t, b, "Echo", 101)
	rev, queue := revisionOf(t, a), queueOrderOf(t, a)

	checkSeries(t, a, b, 101)
	got := viewsOf(t, a, 101)
	if len(got[catNew]) != 0 || len(got[catKnown]) != 2 || len(got[catLibrary]) != 1 {
		t.Fatalf("first check: %v", got)
	}
	fl, _ := getFollow(a.store.DB, 101)
	if fl.CompleteChecks != 1 || fl.Partial() || fl.LastCompleteAt == "" || fl.Slug != "echo" {
		t.Fatalf("%+v", fl)
	}
	// Only the id and paging were sent to Hardcover.
	for _, v := range f.vars {
		if len(v) != 3 || v["id"].(float64) != 101 {
			t.Fatalf("request variables: %v", v)
		}
	}

	// A book cataloged later, one announced, and one with no date at all.
	s.Members = append(s.Members,
		fakeMember{ID: 4, Title: "Volume 4", Author: "S. Author", Pos: fp(4), Release: "2020-05-05"},
		fakeMember{ID: 5, Title: "Volume 5", Author: "S. Author", Pos: fp(5), Release: "2099-01-01"},
		fakeMember{ID: 6, Title: "Undated", Author: "S. Author"})
	checkSeries(t, a, b, 101)
	got = viewsOf(t, a, 101)
	if fmt.Sprint(got[catNew]) != "[Volume 4 Undated]" || fmt.Sprint(got[catUpcoming]) != "[Volume 5]" || fmt.Sprint(got[catKnown]) != "[Volume 2 Volume 3]" {
		t.Fatalf("second check: %v", got)
	}
	// Repeating the check changes nothing and never forgets a discovery.
	before := rowsOf(t, a, "SELECT hardcover_book_id,state,version,baseline FROM series_discoveries ORDER BY 1")
	checkSeries(t, a, b, 101)
	checkSeries(t, a, b, 101)
	if after := rowsOf(t, a, "SELECT hardcover_book_id,state,version,baseline FROM series_discoveries ORDER BY 1"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("a repeated check changed results:\n%v\n%v", before, after)
	}
	html := b.request("GET", "/series/followed/101?view=upcoming", nil).Body.String()
	if !strings.Contains(html, "Future date listed") || !strings.Contains(html, "does not say how precise") {
		t.Fatal("the upcoming list does not explain the date")
	}
	html = b.request("GET", "/series/followed/101?view=new", nil).Body.String()
	if !strings.Contains(html, "when they were cataloged, not when they were published") || !strings.Contains(html, "No release date listed") {
		t.Fatal("the new list does not explain itself")
	}

	// Checks and page loads never touch the library.
	if revisionOf(t, a) != rev || queueOrderOf(t, a) != queue || dbValue[int](t, a, "SELECT count(*) FROM books") != 2 {
		t.Fatal("a check changed the library")
	}
	for _, q := range f.queries {
		if strings.Contains(q, "mutation") {
			t.Fatal("a check wrote to Hardcover")
		}
	}

	// New stays new until the reader acknowledges it; dismissed stays dismissed.
	expect(t, b.form("/series/followed/101/discoveries", url.Values{"action": {"dismiss"}, "item": {"6"}}), 303)
	expect(t, b.form("/series/followed/101/discoveries", url.Values{"action": {"ack"}, "all": {"new"}}), 303)
	checkSeries(t, a, b, 101)
	got = viewsOf(t, a, 101)
	if len(got[catNew]) != 0 || fmt.Sprint(got[catDismissed]) != "[Undated]" || fmt.Sprint(got[catKnown]) != "[Volume 2 Volume 3 Volume 4]" {
		t.Fatalf("after acknowledging: %v", got)
	}
	expect(t, b.form("/series/followed/101/discoveries", url.Values{"action": {"restore"}, "item": {"6"}}), 303)
	if got = viewsOf(t, a, 101); len(got[catDismissed]) != 0 || len(got[catKnown]) != 4 {
		t.Fatalf("after restoring: %v", got)
	}
	_ = ids
}

func TestSeriesLargePartialAndFailedChecks(t *testing.T) {
	s := &fakeSeries{Name: "Echo", Members: members(450, 1000)}
	f := &seriesFake{series: map[int]*fakeSeries{101: s}}
	a, b, _ := followApp(t, f, map[int]string{101: "Echo"})
	followSeries(t, b, "Echo", 101)
	checkSeries(t, a, b, 101)
	if f.requests() != 3 {
		t.Fatalf("450 members should take 3 requests, took %d", f.requests())
	}
	fl, _ := getFollow(a.store.DB, 101)
	if fl.CompleteChecks != 1 || dbValue[int](t, a, "SELECT count(*) FROM series_discoveries") != 450 {
		t.Fatalf("%+v", fl)
	}

	// More than one press can read: stop at the budget, label it partial, and
	// do not claim a baseline or mark anything missing.
	big := &fakeSeries{Name: "Big", Members: members(1100, 5000)}
	f.series[202] = big
	b.request("GET", "/series", nil)
	if _, err := a.store.DB.Exec("INSERT INTO series_follows(series_id,name,followed_at) VALUES(202,'Big','2026-01-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	f.queries = nil
	checkSeries(t, a, b, 202)
	fl, _ = getFollow(a.store.DB, 202)
	if f.requests() != hcSeriesPagesPerCheck || !fl.Partial() || fl.NextOffset != 1000 || fl.CompleteChecks != 0 {
		t.Fatalf("partial check: %d requests, %+v", f.requests(), fl)
	}
	html := b.request("GET", "/series/followed/202", nil).Body.String()
	if !strings.Contains(html, "Partial: the latest check read up to member 1000") || !strings.Contains(html, "Continue the check") || !strings.Contains(html, "No complete check yet") {
		t.Fatal("the partial state is not shown")
	}
	// Continuing finishes it, and only then is the baseline established.
	checkSeries(t, a, b, 202)
	fl, _ = getFollow(a.store.DB, 202)
	if fl.Partial() || fl.CompleteChecks != 1 || dbValue[int](t, a, "SELECT count(*) FROM series_discoveries WHERE series_id=202 AND baseline=1") != 1100 {
		t.Fatalf("continued check: %+v", fl)
	}

	// A page that fails keeps what earlier pages read, and nothing is deleted.
	s.Members = append(s.Members, members(10, 9000)...)
	f.failFrom, f.status = 200, 500
	before := dbValue[int](t, a, "SELECT count(*) FROM series_discoveries WHERE series_id=101")
	checkSeries(t, a, b, 101) // the first page succeeds, the second fails
	fl, _ = getFollow(a.store.DB, 101)
	if !fl.Partial() || fl.NextOffset != 200 || fl.LastError == "" || fl.CompleteChecks != 1 || dbValue[int](t, a, "SELECT count(*) FROM series_discoveries WHERE series_id=101") != before {
		t.Fatalf("failed page: %+v", fl)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM series_discoveries WHERE series_id=101 AND listed=0") != 0 {
		t.Fatal("an incomplete read marked members as no longer listed")
	}
	html = b.request("GET", "/series/followed/101", nil).Body.String()
	if !strings.Contains(html, "The last check stopped early") || !strings.Contains(html, "Partial:") {
		t.Fatal("the failure is not shown")
	}
	// A failure on the very first request records the failure and nothing else.
	f.failAll = true
	b.gateway = true
	refill(a)
	if rr := b.form("/series/check", url.Values{"series": {"202"}, "restart": {"1"}}); rr.Code < 400 {
		t.Fatalf("a total failure looked like success: %d", rr.Code)
	}
	if got := dbValue[int](t, a, "SELECT count(*) FROM series_discoveries WHERE series_id=202"); got != 1100 {
		t.Fatalf("a failed restart lost results: %d", got)
	}
}

func TestSeriesRateLimitAndMergedAndGone(t *testing.T) {
	f := &seriesFake{series: map[int]*fakeSeries{101: {Name: "Echo", Members: members(3, 1)}, 102: {Name: "Old Echo", Canonical: 101}}}
	a, b, _ := followApp(t, f, map[int]string{101: "Echo", 102: "Old Echo", 103: "Vanished"})
	followSeries(t, b, "Echo", 102)
	followSeries(t, b, "Echo", 103)
	checkSeries(t, a, b, 102, 103)
	merged, _ := getFollow(a.store.DB, 102)
	gone, _ := getFollow(a.store.DB, 103)
	if merged.CanonicalID != 101 || merged.CompleteChecks != 0 || !gone.Gone {
		t.Fatalf("%+v %+v", merged, gone)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM series_discoveries") != 0 {
		t.Fatal("a merged or missing series stored members")
	}
	html := b.request("GET", "/series/followed/102", nil).Body.String()
	if !strings.Contains(html, "merged this series into series number 101") {
		t.Fatal("the merge is not explained")
	}

	followSeries(t, b, "Echo", 101)
	f.failAll, f.status = true, 429
	b.gateway = true
	refill(a)
	rr := b.form("/series/check", url.Values{"series": {"101"}})
	if rr.Code < 400 {
		t.Fatalf("429: %d", rr.Code)
	}
	fl, _ := getFollow(a.store.DB, 101)
	if fl.LastError != "rate-limit" || fl.CompleteChecks != 0 {
		t.Fatalf("%+v", fl)
	}
}

func TestSeriesClassificationAndOrdering(t *testing.T) {
	f := &seriesFake{series: map[int]*fakeSeries{101: {Name: "Echo", Members: []fakeMember{
		{ID: 1, Title: "Echo One", Author: "S. Author", Pos: fp(1)},
		{ID: 2, Title: "Linked Elsewhere", Author: "S. Author", Pos: fp(2)},
		{ID: 3, Title: "Same Identity", Author: "S. Author", Pos: fp(3)},
		{ID: 4, Title: "Deleted Earlier", Author: "S. Author", Pos: fp(4)},
		{ID: 5, Title: "Merged Away", Author: "S. Author", Pos: fp(5)},
		{ID: 6, Title: "Queue Anchor", Author: "Someone Else", Pos: fp(6)},
		{ID: 7, Title: "Half Volume", Author: "S. Author", Pos: fp(2.5), Details: "2.5"},
		{ID: 8, Title: "Collection", Author: "S. Author", Pos: fp(1), Details: "1-3", Compilation: true},
		{ID: 9, Title: "No Number", Author: "S. Author"},
		{ID: 10, Title: "Alpha No Number", Author: "S. Author"},
	}}}}
	a, b, ids := followApp(t, f, map[int]string{101: "Echo"})
	followSeries(t, b, "Echo", 101)
	other := addBooks(t, a, Book{Title: "Elsewhere Title", Author: "S. Author"}, Book{Title: "Same Identity", Author: "S. Author"}, Book{Title: "Merge Target", Author: "S. Author"})
	link(t, a, other[0], 2)
	gone := addBooks(t, a, Book{Title: "Deleted Earlier", Author: "S. Author"})[0]
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, gone) }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO book_aliases(identity,target_book_id) VALUES(?,?)", identity("Merged Away", "S. Author"), other[2]); err != nil {
		t.Fatal(err)
	}
	checkSeries(t, a, b, 101)
	got := viewsOf(t, a, 101)
	want := map[string]string{
		catLibrary:  "[Echo One Linked Elsewhere Same Identity Merged Away]",
		catRemoved:  "[Deleted Earlier]",
		catPossible: "[Queue Anchor]",
	}
	for cat, w := range want {
		if fmt.Sprint(got[cat]) != w {
			t.Errorf("%s: got %v, want %s", cat, got[cat], w)
		}
	}
	// Fractional, duplicate, and missing numbers stay visible, in a fixed order.
	if fmt.Sprint(got[catKnown]) != "[Collection Half Volume Alpha No Number No Number]" {
		t.Fatalf("known: %v", got[catKnown])
	}
	html := b.request("GET", "/series/followed/101?view=known", nil).Body.String()
	for _, want := range []string{"Collection or omnibus", "In between", "#2.5", "#1 (1-3)", "no number"} {
		if !strings.Contains(html, want) {
			t.Errorf("known list missing %q", want)
		}
	}
	// An ambiguous match needs the reader's review and names the local book.
	html = b.request("GET", "/series/followed/101?view=possible", nil).Body.String()
	if !strings.Contains(html, "different author") || !strings.Contains(html, "Your book: “Queue Anchor”") {
		t.Fatal("the possible match is not explained")
	}
	if dbValue[int](t, a, "SELECT count(*) FROM books WHERE title='Merged Away' OR title='Deleted Earlier'") != 0 {
		t.Fatal("checking recreated a book")
	}
	_ = ids
}

func TestSeriesAddUsesThePreviewAndIsSafe(t *testing.T) {
	s := &fakeSeries{Name: "Echo", Members: members(3, 1)}
	f := &seriesFake{series: map[int]*fakeSeries{101: s}}
	a, b, _ := followApp(t, f, map[int]string{101: "Echo"})
	followSeries(t, b, "Echo", 101)
	checkSeries(t, a, b, 101)
	preview := func(items ...string) string {
		b.request("GET", "/series/followed/101", nil)
		return b.form("/series/followed/101/add", url.Values{"item": items}).Header().Get("Location")
	}
	expect(t, b.form("/series/followed/101/add", url.Values{}), 400)
	expect(t, b.form("/series/followed/101/add", url.Values{"item": {"1"}}), 400) // already in the library
	books, rev, queue := dbValue[int](t, a, "SELECT count(*) FROM books"), revisionOf(t, a), queueOrderOf(t, a)
	loc := preview("2", "3")
	page := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(page, "Volume 2") || !strings.Contains(page, "Needs placement") || !strings.Contains(page, "nothing is sent to Hardcover") || dbValue[int](t, a, "SELECT count(*) FROM books") != books || revisionOf(t, a) != rev {
		t.Fatal("the preview is wrong, or changed something")
	}
	expect(t, b.request("POST", loc, nil), 303)
	got := rowsOf(t, a, "SELECT title,source,needs_placement,reading_status,series,series_number FROM books WHERE source='hardcover-series' ORDER BY title")
	if fmt.Sprint(got) != "[[Volume 2 hardcover-series 1 to-read Echo 2] [Volume 3 hardcover-series 1 to-read Echo 3]]" {
		t.Fatalf("added: %v", got)
	}
	if dbValue[int](t, a, "SELECT hardcover_book_id FROM book_hardcover WHERE book_id=(SELECT id FROM books WHERE title='Volume 3')") != 3 {
		t.Fatal("not linked")
	}
	if !strings.HasPrefix(queueOrderOf(t, a), queue) {
		t.Fatalf("the queue was reordered: %s -> %s", queue, queueOrderOf(t, a))
	}
	if dbValue[int](t, a, "SELECT count(*) FROM series_discoveries WHERE state='added'") != 2 {
		t.Fatal("the results were not marked added")
	}
	// A repeat is refused and adds nothing.
	rr := b.request("POST", loc, nil)
	if rr.Code != 404 && rr.Code != 409 || dbValue[int](t, a, "SELECT count(*) FROM books") != books+2 {
		t.Fatalf("a repeat: %d", rr.Code)
	}
	for _, q := range f.queries {
		if strings.Contains(q, "mutation") {
			t.Fatal("adding wrote to Hardcover")
		}
	}

	// Stale: the library changed, the list changed, or the follow was replaced.
	s.Members = append(s.Members, fakeMember{ID: 4, Title: "Volume 4", Author: "S. Author", Pos: fp(4)}, fakeMember{ID: 5, Title: "Volume 5", Author: "S. Author", Pos: fp(5)}, fakeMember{ID: 6, Title: "Volume 6", Author: "S. Author", Pos: fp(6)})
	checkSeries(t, a, b, 101)
	loc = preview("4")
	if err := a.store.Write(func(tx *sql.Tx) error { return bump(tx) }); err != nil {
		t.Fatal(err)
	}
	if rr := b.request("POST", loc, nil); rr.Code != 409 {
		t.Fatalf("stale revision: %d", rr.Code)
	}
	loc = preview("4")
	s.Members[3].Title = "Volume Four Retitled"
	checkSeries(t, a, b, 101)
	if rr := b.request("POST", loc, nil); rr.Code != 409 {
		t.Fatalf("changed result: %d", rr.Code)
	}
	loc = preview("5")
	expect(t, b.form("/series/followed/101/unfollow", url.Values{"confirm": {"unfollow"}}), 303)
	if rr := b.request("POST", loc, nil); rr.Code != 409 && rr.Code != 404 {
		t.Fatalf("unfollowed: %d", rr.Code)
	}
	followSeries(t, b, "Echo", 101)
	if rr := b.request("POST", loc, nil); rr.Code != 409 && rr.Code != 404 {
		t.Fatalf("followed again: %d", rr.Code)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM books WHERE title IN ('Volume 4','Volume Four Retitled','Volume 5')") != 0 {
		t.Fatal("a stale plan added a book")
	}
}

func TestSeriesSameBookInTwoSeriesIsOneBook(t *testing.T) {
	shared := fakeMember{ID: 50, Title: "Crossover", Author: "S. Author", Pos: fp(1)}
	f := &seriesFake{series: map[int]*fakeSeries{101: {Name: "Echo", Members: []fakeMember{{ID: 1, Title: "Echo One", Author: "S. Author", Pos: fp(1)}, shared}}, 202: {Name: "Echo Universe", Members: []fakeMember{shared}}}}
	a, b, _ := followApp(t, f, map[int]string{101: "Echo", 202: "Echo Universe"})
	followSeries(t, b, "Echo", 101)
	followSeries(t, b, "Echo", 202)
	checkSeries(t, a, b, 101, 202)
	loc := b.form("/series/followed/101/add", url.Values{"item": {"50"}}).Header().Get("Location")
	expect(t, b.request("POST", loc, nil), 303)
	if fmt.Sprint(viewsOf(t, a, 202)[catLibrary]) != "[Crossover]" || fmt.Sprint(viewsOf(t, a, 101)[catLibrary]) != "[Crossover Echo One]" {
		t.Fatal("the added book is not recognised in both series")
	}
	if dbValue[int](t, a, "SELECT count(*) FROM books WHERE title='Crossover'") != 1 {
		t.Fatal("two copies")
	}
}

func TestSeriesDisconnectedStillShowsSavedResults(t *testing.T) {
	f := &seriesFake{series: map[int]*fakeSeries{101: {Name: "Echo", Members: members(3, 1)}}}
	a, b, _ := followApp(t, f, map[int]string{101: "Echo"})
	followSeries(t, b, "Echo", 101)
	checkSeries(t, a, b, 101)
	n := f.requests()
	a.hardcover = nil
	for _, path := range []string{"/series", "/series/followed/101"} {
		html := b.request("GET", path, nil).Body.String()
		if !strings.Contains(html, "checking is unavailable") || !strings.Contains(html, "Volume 2") && path != "/series" {
			t.Fatalf("%s: disconnected page", path)
		}
	}
	expect(t, b.form("/series/check", url.Values{"series": {"101"}}), 400)
	if f.requests() != n {
		t.Fatal("a disconnected app contacted Hardcover")
	}
	expect(t, b.form("/series/followed/101/discoveries", url.Values{"action": {"dismiss"}, "item": {"2"}}), 303)
}

func TestSeriesFollowsExportRestoreForgetAndMigration(t *testing.T) {
	s := &fakeSeries{Name: "Echo", Slug: "echo", Members: members(3, 1)}
	f := &seriesFake{series: map[int]*fakeSeries{101: s}}
	a, b, _ := followApp(t, f, map[int]string{101: "Echo"})
	followSeries(t, b, "Echo", 101)
	checkSeries(t, a, b, 101)
	expect(t, b.form("/series/followed/101/discoveries", url.Values{"action": {"dismiss"}, "item": {"3"}}), 303)
	raw := exportOf(t, a)
	var doc exportFile
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.SeriesFollows) != 1 || len(doc.SeriesDiscoveries) != 3 || doc.SchemaVersion != maxRestoreSchema {
		t.Fatal(err, len(doc.SeriesFollows), len(doc.SeriesDiscoveries), doc.SchemaVersion)
	}
	target, tb := noteApp(t)
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, raw)
	expect(t, rr, 303)
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	if got, want := normalizedExport(t, exportOf(t, target)), normalizedExport(t, raw); !equalJSON(got, want) {
		t.Fatal("follows did not round trip")
	}
	if dbValue[string](t, target, "SELECT state FROM series_discoveries WHERE hardcover_book_id=3") != "dismissed" {
		t.Fatal("a dismissal was lost")
	}

	// Invalid records are refused; an older export restores with no follows.
	for name, edit := range map[string]func(m map[string]any){
		"unknown series":   func(m map[string]any) { m["series_discoveries"].([]any)[0].(map[string]any)["series_id"] = 999 },
		"bad state":        func(m map[string]any) { m["series_discoveries"].([]any)[0].(map[string]any)["state"] = "maybe" },
		"bad release date": func(m map[string]any) { m["series_discoveries"].([]any)[0].(map[string]any)["release_date"] = "soon" },
		"repeated book": func(m map[string]any) {
			d := m["series_discoveries"].([]any)
			d[1].(map[string]any)["hardcover_book_id"] = d[0].(map[string]any)["hardcover_book_id"]
		},
		"no name": func(m map[string]any) { m["series_follows"].([]any)[0].(map[string]any)["name"] = "" },
	} {
		var m map[string]any
		json.Unmarshal(raw, &m)
		edit(m)
		body, _ := json.Marshal(m)
		_, tb2 := noteApp(t)
		expect(t, tb2.request("GET", "/imports", nil), 200)
		if rr := uploadRestore(tb2, body); rr.Code != 400 {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	delete(m, "series_follows")
	delete(m, "series_discoveries")
	m["schema_version"] = 27
	old, _ := json.Marshal(m)
	t3, tb3 := noteApp(t)
	expect(t, tb3.request("GET", "/imports", nil), 200)
	rr = uploadRestore(tb3, old)
	expect(t, rr, 303)
	expect(t, tb3.request("POST", rr.Header().Get("Location"), nil), 303)
	if dbValue[int](t, t3, "SELECT count(*) FROM series_follows") != 0 {
		t.Fatal("an older export invented follows")
	}

	// Forget Hardcover ids clears the ids and results; no book changes.
	books := dbValue[int](t, a, "SELECT count(*) FROM books")
	counts, err := countForget(a.store.DB)
	if err != nil || counts.Follows != 1 || counts.Discoveries != 3 {
		t.Fatal(counts, err)
	}
	if err := forgetIDs(a.store.DB); err != nil {
		t.Fatal(err)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM series_follows")+dbValue[int](t, a, "SELECT count(*) FROM series_discoveries") != 0 || dbValue[int](t, a, "SELECT count(*) FROM books") != books {
		t.Fatal("forget left follows, or touched books")
	}
}

func TestMigrateSeriesFollowsKeepsTheCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:27]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source,series) VALUES(1,'Echo One','A','echo one|a',0,'x','manual','Echo');
 INSERT INTO series_cache(source_book_id,series_id,series_name,series_total,hardcover_book_id,position,title,author,fetched_at) VALUES(1,101,'Echo',3,1,1,'Echo One','A','2026-01-01T00:00:00Z');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[int](t, path, "SELECT count(*) FROM series_cache") != 1 || scalar[int](t, path, "SELECT count(*) FROM series_follows") != 0 || scalar[int](t, path, "SELECT count(*) FROM series_discoveries") != 0 {
		t.Fatal("migration changed the cache or invented follows")
	}
	if got := backups(t, path); len(got) != 1 || got[0] != path+".pre-v28.bak" {
		t.Fatal(got)
	}
}

func TestSeriesChoicesSurviveUndoAndLocalDeletes(t *testing.T) {
	s := &fakeSeries{Name: "Echo", Members: members(3, 1)}
	f := &seriesFake{series: map[int]*fakeSeries{101: s}}
	a, b, ids := followApp(t, f, map[int]string{101: "Echo"})
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	followSeries(t, b, "Echo", 101)
	checkSeries(t, a, b, 101)
	before := queueOrderOf(t, a)

	// An unrelated queue change, then a dismissal, then Undo of the queue
	// change: the dismissal stays and Undo still works.
	b.request("GET", "/", nil)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/action", ids[0]), url.Values{"action": {"down"}}), 303)
	expect(t, b.form("/series/followed/101/discoveries", url.Values{"action": {"dismiss"}, "item": {"2"}}), 303)
	b.request("GET", "/", nil)
	expect(t, b.request("POST", "/undo", nil), 303)
	if queueOrderOf(t, a) != before {
		t.Fatalf("Undo no longer works after a dismissal: %s", queueOrderOf(t, a))
	}
	if dbValue[string](t, a, "SELECT state FROM series_discoveries WHERE hardcover_book_id=2") != "dismissed" {
		t.Fatal("Undo erased a dismissal")
	}

	// Deleting a local book leaves the follow and its results; the deleted
	// book is then remembered, not offered again.
	if err := a.store.Write(func(tx *sql.Tx) error { return deleteBook(tx, ids[0]) }); err != nil {
		t.Fatal(err)
	}
	checkSeries(t, a, b, 101)
	got := viewsOf(t, a, 101)
	if fmt.Sprint(got[catRemoved]) != "[Volume 1]" || dbValue[int](t, a, "SELECT count(*) FROM series_follows") != 1 || dbValue[int](t, a, "SELECT count(*) FROM series_discoveries") != 3 {
		t.Fatalf("after a local delete: %v", got)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM books WHERE title='Volume 1' OR title='Echo One'") != 0 {
		t.Fatal("a check recreated a deleted book")
	}
}
