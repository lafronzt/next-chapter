package app

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestQueueReasonPrecedence(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	day := func(n int) string { return today.AddDate(0, 0, n).Format(time.DateOnly) }
	one, two := 1.0, 2.0
	pages := func(n int) *int { return &n }
	series := map[string]seriesInfo{"Foo": {First: 1, Queued: 2}, "Solo": {First: 1, Queued: 1}}
	loan := func(status, due string) []Loan { return []Loan{{Status: status, DueDate: due}} }
	prefs := Preferences{LibraryWeight: 1, Length: "short", Mood: "hopeful"}
	cases := []struct {
		name string
		book Book
		p    Preferences
		want string
	}{
		{"pinned beats everything", Book{Pinned: true, MovedByHand: true, NeedsPlacement: true, Loans: loan("borrowed", day(1))}, prefs, "Pinned: keeps its place"},
		{"hand placed beats loan", Book{MovedByHand: true, Loans: loan("borrowed", day(1))}, prefs, "You placed this"},
		{"needs a spot beats loan", Book{NeedsPlacement: true, Loans: loan("borrowed", day(1))}, prefs, "Needs a spot"},
		{"loan due in 5 days", Book{Loans: loan("borrowed", day(5))}, prefs, "Library loan due in 5 days"},
		{"loan due tomorrow", Book{Loans: loan("borrowed", day(1))}, prefs, "Library loan due in 1 day"},
		{"loan due today", Book{Loans: loan("borrowed", day(0))}, prefs, "Library loan due today"},
		{"loan overdue", Book{Loans: loan("borrowed", day(-2))}, prefs, "Library loan is overdue"},
		{"ready for pickup", Book{Loans: loan("ready", "")}, prefs, "Library copy ready for pickup"},
		{"loan beats series", Book{Series: "Foo", SeriesNumber: &one, Loans: loan("borrowed", day(3))}, prefs, "Library loan due in 3 days"},
		{"loan far off is not a reason", Book{Loans: loan("borrowed", day(30))}, Preferences{Length: "any"}, ""},
		{"next in series", Book{Series: "Foo", SeriesNumber: &one}, prefs, "Next in the Foo series"},
		{"not the first in series", Book{Series: "Foo", SeriesNumber: &two}, Preferences{Length: "any"}, ""},
		{"lone series book says nothing", Book{Series: "Solo", SeriesNumber: &one}, Preferences{Length: "any"}, ""},
		{"series beats mood", Book{Series: "Foo", SeriesNumber: &one, Tags: "hopeful"}, prefs, "Next in the Foo series"},
		{"mood", Book{Tags: "hopeful, quiet", Pages: pages(500)}, prefs, "Matches your hopeful mood"},
		{"short", Book{Pages: pages(200)}, prefs, "Short, fits your length preference"},
		{"long", Book{Pages: pages(450)}, Preferences{Length: "long"}, "Long, fits your length preference"},
		{"nothing", Book{Pages: pages(300)}, prefs, ""},
	}
	for _, c := range cases {
		if got := queueReason(c.book, c.p, today, series); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// countingDB counts the queries the reasons code issues.
type countingDB struct {
	queryer
	n int
}

func (c *countingDB) Query(q string, a ...any) (*sql.Rows, error) {
	c.n++
	return c.queryer.Query(q, a...)
}
func (c *countingDB) QueryRow(q string, a ...any) *sql.Row { c.n++; return c.queryer.QueryRow(q, a...) }

func TestReasonsAddNoPerRowQueries(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	names := []string{}
	for i := 0; i < 200; i++ {
		names = append(names, fmt.Sprintf("Book %03d", i))
	}
	addRecords(t, a, records(names...))
	if _, err := a.store.DB.Exec("UPDATE books SET series='S', series_number=id WHERE id%3=0"); err != nil {
		t.Fatal(err)
	}
	items, err := listBooks(a.store.DB, bookFilter{Status: "to-read"}, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 200 {
		t.Fatalf("%d items", len(items))
	}
	db := &countingDB{queryer: a.store.DB}
	if err = attachReasons(db, items, time.Now()); err != nil {
		t.Fatal(err)
	}
	if db.n > 2 {
		t.Fatalf("%d queries for 200 rows", db.n)
	}
	withReason := 0
	for _, it := range items {
		if it.Reason != "" {
			withReason++
		}
	}
	if withReason != 200 { // every new book needs a spot
		t.Fatalf("%d rows have a reason", withReason)
	}
}

func TestRenderingReasonsChangesNothing(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("A", "B", "C"))
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1 WHERE title='B'; UPDATE books SET moved_by_hand=1 WHERE title='C'"); err != nil {
		t.Fatal(err)
	}
	before := positionsOf(t, a) + waitingFlags(t, a)
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/", nil).Body.String()
	for _, want := range []string{"Pinned: keeps its place", "You placed this", "Needs a spot", `class="book-row hand-placed"`, "stay where you put them"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if after := positionsOf(t, a) + waitingFlags(t, a); after != before {
		t.Fatalf("%s -> %s", before, after)
	}
}

func waitingFlags(t *testing.T, a *App) string {
	t.Helper()
	var s string
	if err := a.store.DB.QueryRow("SELECT group_concat(needs_placement) FROM (SELECT needs_placement FROM books ORDER BY id)").Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
