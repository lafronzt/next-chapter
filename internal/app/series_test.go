package app

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func fp(f float64) *float64 { return &f }

// seriesApp adds books as (title, series, number, status) and gives some of
// them finished readings.
type seriesBook struct {
	title, series string
	number        *float64
	status        string
	readings      int
}

func seriesLibrary(t *testing.T, books []seriesBook) *App {
	t.Helper()
	a := testApp(t, true)
	rs := []Record{}
	for _, b := range books {
		rs = append(rs, Record{Book: Book{Title: b.title, Author: "Author", Series: b.series, SeriesNumber: b.number, Status: b.status, Source: "manual", Interest: 3}})
	}
	addRecords(t, a, rs)
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	ids := bookIDs(t, a)
	for _, b := range books {
		for i := range b.readings {
			if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(?,?,NULL,'manual',?)", ids[b.title], fmt.Sprintf("2025-0%d-01", i+1), fmt.Sprintf("manual:%s:%d", b.title, i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return a
}

func groupNamed(t *testing.T, a *App, name string) seriesGroup {
	t.Helper()
	groups, err := loadSeries(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if foldKey(g.Name) == foldKey(name) {
			return g
		}
	}
	t.Fatalf("no series %q in %d groups", name, len(groups))
	return seriesGroup{}
}

func volumeTitles(vs []seriesVolume) string {
	out := []string{}
	for _, v := range vs {
		out = append(out, v.Title)
	}
	return strings.Join(out, ",")
}

func TestSeriesGroupingMatchesRanking(t *testing.T) {
	t.Parallel()
	a := seriesLibrary(t, []seriesBook{
		{"Wizard", "Earthsea", fp(1), "read", 0},
		{"Tombs", "EARTHSEA", fp(2), "to-read", 0},
		{"Shore", "earthsea", fp(3), "to-read", 0},
		{"Élan One", "Élan Saga", fp(1), "to-read", 0},
		{"Élan Two", "éLAN SAGA", fp(2), "to-read", 0},
		{"Blank", "   ", fp(1), "to-read", 0},
		{"Standalone", "", nil, "to-read", 0},
		{"Other", "Other", fp(1), "to-read", 0},
	})
	groups, err := loadSeries(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, g := range groups {
		names = append(names, g.Name)
	}
	if strings.Join(names, ",") != "Earthsea,Other,Élan Saga" {
		t.Fatalf("groups %v", names)
	}
	// The dashboard's groups are exactly the sets ranking treats as one series.
	books, _ := loadBooks(a.store.DB)
	deps := seriesPrerequisites(books)
	byID := map[int64]Book{}
	for _, b := range books {
		byID[b.ID] = b
	}
	for _, g := range groups {
		in := map[int64]bool{}
		for _, v := range g.Numbered {
			in[v.ID] = true
		}
		for _, v := range g.Numbered {
			for other, isDep := range deps[v.ID] {
				if isDep && !in[other] {
					t.Fatalf("ranking links %d to %d across groups", v.ID, other)
				}
			}
		}
	}
	if g := groupNamed(t, a, "earthsea"); g.Name != "Earthsea" || volumeTitles(g.Numbered) != "Wizard,Tombs,Shore" {
		t.Fatalf("%+v", g)
	}
	// One query, however many series there are, and identical results twice.
	c := &countingDB{queryer: a.store.DB}
	first, _ := loadSeries(c)
	if c.n != 1 {
		t.Fatalf("%d queries", c.n)
	}
	second, _ := loadSeries(a.store.DB)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("not deterministic")
	}
}

func TestSeriesOrderingDuplicatesAndStatus(t *testing.T) {
	t.Parallel()
	a := seriesLibrary(t, []seriesBook{
		{"Ten", "S", fp(10), "to-read", 0},
		{"Two", "S", fp(2), "to-read", 0},
		{"One", "S", fp(1), "read", 3}, // three readings, still one book
		{"Half", "S", fp(1.5), "to-read", 0},
		{"Dup A", "S", fp(2), "unreviewed", 0},
		{"Mystery", "S", nil, "to-read", 0},
		{"Another Mystery", "S", nil, "abandoned", 0},
		{"Old Friend", "S", fp(4), "to-read", 2}, // queued again, finished before
	})
	g := groupNamed(t, a, "S")
	if got := volumeTitles(g.Numbered); got != "One,Half,Dup A,Two,Old Friend,Ten" {
		t.Fatalf("numbered %s", got)
	}
	if got := volumeTitles(g.Unnumbered); got != "Another Mystery,Mystery" {
		t.Fatalf("unnumbered %s", got)
	}
	dup := 0
	for _, v := range g.Numbered {
		if v.DuplicateNumber {
			dup++
		}
	}
	if dup != 2 {
		t.Fatalf("%d duplicate flags", dup)
	}
	// Rereads count a book once; current status and history stay separate.
	if g.Books != 8 || g.Read != 1 || g.EverFinished != 2 || g.Queued != 5 || g.SetAside != 1 || g.ToReview != 1 {
		t.Fatalf("%+v", g)
	}
	// #1 is finished and #1.5 is fractional, so it never blocks. #2 has two
	// local books; the queued one is chosen over the one in To review, since
	// queueing is the reader's own plan.
	if g.Next == nil || g.Next.Volume.Title != "Two" || g.Next.Action != "start" {
		t.Fatalf("next %+v", g.Next)
	}
	if !strings.Contains(strings.Join(g.Gaps, "|"), "No local entry between #4 and #10") || !strings.Contains(strings.Join(g.Gaps, "|"), "between #2 and #4") || len(g.Gaps) != 2 {
		t.Fatalf("gaps %v", g.Gaps)
	}
	// The page shows the finished-before evidence and duplicates.
	b := &browser{t: t, handler: a.Handler()}
	page := b.request("GET", "/series", nil).Body.String()
	for _, want := range []string{"Finished before (2 readings logged)", "Same number as another book", "In between", "No local entry between #4 and #10", "Mystery"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "is missing") || strings.Contains(page, "incomplete") {
		t.Fatal("the page claims a series is incomplete")
	}
}

func TestSeriesNextVolume(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		books   []seriesBook
		want    string
		action  string
		message string
	}{
		{"set aside blocks", []seriesBook{{"A1", "S", fp(1), "read", 0}, {"A2", "S", fp(2), "abandoned", 0}, {"A3", "S", fp(3), "to-read", 0}}, "A2", "requeue", "set aside"},
		{"archived blocks", []seriesBook{{"A1", "S", fp(1), "archived", 0}, {"A2", "S", fp(2), "to-read", 0}}, "A1", "requeue", "set aside"},
		{"unreviewed blocks", []seriesBook{{"A1", "S", fp(1), "read", 0}, {"A2", "S", fp(2), "unreviewed", 0}, {"A3", "S", fp(3), "to-read", 0}}, "A2", "", "To review"},
		{"fraction does not block", []seriesBook{{"A1", "S", fp(1), "read", 0}, {"A15", "S", fp(1.5), "abandoned", 0}, {"A2", "S", fp(2), "to-read", 0}}, "A2", "start", "in your queue"},
		{"gap is not a prerequisite", []seriesBook{{"A1", "S", fp(1), "read", 0}, {"A3", "S", fp(3), "to-read", 0}}, "A3", "start", "in your queue"},
		{"reading now", []seriesBook{{"A1", "S", fp(1), "read", 0}, {"A2", "S", fp(2), "reading", 0}, {"A3", "S", fp(3), "to-read", 0}}, "A2", "", "reading #2 now"},
		{"finished before counts as done", []seriesBook{{"A1", "S", fp(1), "to-read", 1}, {"A2", "S", fp(2), "to-read", 0}}, "A2", "start", "in your queue"},
		{"unnumbered only", []seriesBook{{"A1", "S", nil, "to-read", 0}}, "", "", ""},
		{"all finished", []seriesBook{{"A1", "S", fp(1), "read", 0}, {"A2", "S", fp(2), "read", 0}}, "", "", ""},
		{"fraction only", []seriesBook{{"A1", "S", fp(0.5), "to-read", 0}}, "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := groupNamed(t, seriesLibrary(t, c.books), "S")
			if c.want == "" {
				if g.Next != nil {
					t.Fatalf("next %+v", g.Next)
				}
				return
			}
			if g.Next == nil || g.Next.Volume.Title != c.want || g.Next.Action != c.action || !strings.Contains(g.Next.Message, c.message) {
				t.Fatalf("next %+v", g.Next)
			}
		})
	}
	// Gap wording never promises a volume exists.
	for numbers, want := range map[string]string{"1,2,3": "", "1,3": "No local entry between #1 and #3", "1,2.5,4": "No local entry between #1 and #4", "2,5": "No local entry between #2 and #5", "1,1,2": ""} {
		vs := []seriesVolume{}
		for _, n := range strings.Split(numbers, ",") {
			var f float64
			fmt.Sscan(n, &f)
			vs = append(vs, seriesVolume{Number: &f})
		}
		got := strings.Join(seriesGaps(vs), "|")
		if got != want {
			t.Errorf("%s: %q, want %q", numbers, got, want)
		}
	}
}

func TestSeriesPageSearchPagingAndAuth(t *testing.T) {
	t.Parallel()
	books := []seriesBook{}
	for i := range 7 {
		books = append(books, seriesBook{fmt.Sprintf("Book %d", i), fmt.Sprintf("Saga %c", 'A'+i), fp(1), "to-read", 0})
	}
	books = append(books, seriesBook{"Solo", "", nil, "to-read", 0})
	a := seriesLibrary(t, books)
	b := &browser{t: t, handler: a.Handler()}
	before, rev := queueSnapshot(t, a), revisionOf(t, a)
	page := b.request("GET", "/series?per_page=3", nil).Body.String()
	if strings.Count(page, "<h2 id=\"series-") != 3 || !strings.Contains(page, `rel="next"`) {
		t.Fatal("paging")
	}
	if page = b.request("GET", "/series?q=saga%20c", nil).Body.String(); strings.Count(page, "<h2 id=\"series-") != 1 || !strings.Contains(page, "Saga C") {
		t.Fatal("search")
	}
	if page = b.request("GET", "/series?q=zzz", nil).Body.String(); !strings.Contains(page, "No series name contains") {
		t.Fatal("empty state")
	}
	empty := testApp(t, true)
	eb := &browser{t: t, handler: empty.Handler()}
	if page = eb.request("GET", "/series", nil).Body.String(); !strings.Contains(page, "No series yet") {
		t.Fatal("no-series state")
	}
	// Viewing the page changes nothing.
	if queueSnapshot(t, a) != before || revisionOf(t, a) != rev {
		t.Fatal("the page changed the library")
	}
	// The route needs a login.
	locked := testApp(t, false)
	lb := &browser{t: t, handler: locked.Handler()}
	if rr := lb.request("GET", "/series", nil); rr.Code != 303 || !strings.HasPrefix(rr.Header().Get("Location"), "/login") {
		t.Fatalf("%d %s", rr.Code, rr.Header().Get("Location"))
	}
	// The offered action is the ordinary book action and works.
	g := groupNamed(t, seriesLibrary(t, []seriesBook{{"V1", "S", fp(1), "abandoned", 0}}), "S")
	if g.Next == nil || g.Next.Action != "requeue" {
		t.Fatal("no action offered")
	}
	sb := seriesLibrary(t, []seriesBook{{"V1", "S", fp(1), "abandoned", 0}, {"V2", "S", fp(2), "to-read", 0}})
	bb := &browser{t: t, handler: sb.Handler()}
	sp := bb.request("GET", "/series", nil).Body.String()
	if !strings.Contains(sp, "Move back to my queue") {
		t.Fatal("no requeue button")
	}
	id := bookIDs(t, sb)["V1"]
	expect(t, bb.request("POST", fmt.Sprintf("/books/%d/status", id), url.Values{"action": {"requeue"}}), 303)
	if bookNamed(t, sb, "V1").Status != "to-read" {
		t.Fatal("the action did not apply")
	}
}

func queueSnapshot(t *testing.T, a *App) string {
	t.Helper()
	rows, err := a.store.DB.Query("SELECT id,reading_status,position,pinned,needs_placement FROM books ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, pos, pin, needs int
		var status string
		rows.Scan(&id, &status, &pos, &pin, &needs)
		out = append(out, fmt.Sprint(id, status, pos, pin, needs))
	}
	return strings.Join(out, ";")
}
