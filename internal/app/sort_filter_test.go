package app

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var rowIDPattern = regexp.MustCompile(`<article class="book-row" id="book-(\d+)">`)
var rowTitlePattern = regexp.MustCompile(`<h2><a href="/books/\d+">([^<]*)</a></h2>`)

func listedTitles(t *testing.T, b *browser, path string) []string {
	t.Helper()
	body := b.request("GET", path, nil).Body.String()
	out := []string{}
	for _, m := range rowTitlePattern.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func positionsOf(t *testing.T, a *App) string {
	t.Helper()
	rows, err := a.store.DB.Query("SELECT id,position FROM books ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var id, pos int
		rows.Scan(&id, &pos)
		fmt.Fprintf(&sb, "%d:%d ", id, pos)
	}
	return sb.String()
}

func TestAllBooksSortAndFilterLeavePositionsAlone(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{
		{Book: Book{Title: "Zebra", Author: "Adams", Status: "to-read", Source: "manual", Interest: 5}},
		{Book: Book{Title: "apple", Author: "Zed", Status: "to-read", Source: "manual", Interest: 1}},
		{Book: Book{Title: "Mango", Author: "Moss", Status: "read", Source: "manual", Interest: 3}},
		{Book: Book{Title: "Kiwi", Author: "Baker", Status: "archived", Source: "manual", Interest: 2}},
	})
	before := positionsOf(t, a)
	b := &browser{t: t, handler: a.Handler()}
	cases := map[string]string{
		"/?view=all":                           "Zebra,apple,Mango,Kiwi",
		"/?view=all&sort=title":                "apple,Kiwi,Mango,Zebra",
		"/?view=all&sort=author":               "Zebra,Kiwi,Mango,apple",
		"/?view=all&sort=interest":             "Zebra,Mango,Kiwi,apple",
		"/?view=all&status=to-read":            "Zebra,apple",
		"/?view=all&status=to-read&sort=title": "apple,Zebra",
		"/?view=all&status=nonsense&sort=bad":  "Zebra,apple,Mango,Kiwi",
		// Sort and status mean nothing outside All books.
		"/?view=queue&sort=title": "Zebra,apple",
	}
	for path, want := range cases {
		if got := strings.Join(listedTitles(t, b, path), ","); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
	// A sorted page turns off moves rather than moving by the sorted order.
	if body := b.request("GET", "/?view=all&sort=title", nil).Body.String(); !strings.Contains(body, "This list is sorted. Show queue order to move books.") {
		t.Fatal("sorted list still offers moves")
	}
	if after := positionsOf(t, a); after != before {
		t.Fatalf("positions changed: %s -> %s", before, after)
	}
}

func TestSortAndFilterSurvivePagination(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	names := []string{}
	for i := 1; i <= 12; i++ {
		names = append(names, fmt.Sprintf("Book %02d", 13-i))
	}
	addRecords(t, a, records(names...))
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/?view=all&status=to-read&sort=title&per_page=5", nil).Body.String()
	if !strings.Contains(body, `href="/?per_page=5&amp;sort=title&amp;status=to-read&amp;view=all&amp;page=2"`) && !strings.Contains(body, "sort=title") {
		t.Fatal("next link drops the filters")
	}
	next := regexp.MustCompile(`href="([^"]*)" rel="next"`).FindStringSubmatch(body)
	if next == nil || !strings.Contains(next[1], "sort=title") || !strings.Contains(next[1], "status=to-read") {
		t.Fatalf("%v", next)
	}
	got := listedTitles(t, b, strings.ReplaceAll(next[1], "&amp;", "&"))
	if strings.Join(got, ",") != "Book 06,Book 07,Book 08,Book 09,Book 10" {
		t.Fatal(got)
	}
}

func TestActionRedirectsPointAtAnExistingUniqueRow(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("One", "Two", "Three"))
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/", nil).Body.String()
	ids := map[string]bool{}
	for _, m := range rowIDPattern.FindAllStringSubmatch(body, -1) {
		if ids[m[1]] {
			t.Fatalf("duplicate row id %s", m[1])
		}
		ids[m[1]] = true
	}
	if len(ids) != 3 {
		t.Fatalf("%d row ids", len(ids))
	}
	id := bookNamed(t, a, "Two").ID
	rr := b.form(fmt.Sprintf("/books/%d/action", id), map[string][]string{"action": {"pin"}})
	expect(t, rr, 303)
	loc := rr.Header().Get("Location")
	frag := loc[strings.Index(loc, "#")+1:]
	if frag != fmt.Sprintf("book-%d", id) || !strings.Contains(b.request("GET", "/", nil).Body.String(), `id="`+frag+`"`) {
		t.Fatalf("%s", loc)
	}
}

func TestBookFormCollapsesDetailsUnlessFilled(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	if body := b.request("GET", "/books/new", nil).Body.String(); !strings.Contains(body, `<details class="span-2 more-details" >`) {
		t.Fatal("details should start closed on a new book")
	}
	addRecords(t, a, []Record{{Book: Book{Title: "Tagged", Author: "A", Status: "to-read", Source: "manual", Interest: 3, Tags: "x"}}})
	body := b.request("GET", fmt.Sprintf("/books/%d/edit", bookNamed(t, a, "Tagged").ID), nil).Body.String()
	if !strings.Contains(body, `<details class="span-2 more-details" open>`) {
		t.Fatal("details should be open when a value exists")
	}
}
