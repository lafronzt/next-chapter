package app

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestBookViewIsReadOnlyAndEditHasTheForm(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("Viewed"))
	id := bookNamed(t, a, "Viewed").ID
	b := &browser{t: t, handler: a.Handler()}
	view := b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String()
	if strings.Contains(view, `name="title"`) || strings.Contains(view, `name="interest"`) || !strings.Contains(view, fmt.Sprintf(`href="/books/%d/edit"`, id)) {
		t.Fatal("view page has form fields or lacks the Edit details link")
	}
	edit := b.request("GET", fmt.Sprintf("/books/%d/edit", id), nil).Body.String()
	if !strings.Contains(edit, `name="title"`) || !strings.Contains(edit, `name="interest"`) {
		t.Fatal("edit page lacks the form")
	}
	for _, path := range []string{"/books/999", "/books/999/edit"} {
		expect(t, b.request("GET", path, nil), 404)
	}
	for _, path := range []string{"/books/0", "/books/0/edit"} {
		expect(t, b.request("GET", path, nil), 400)
	}
	// Saving redirects to the view page.
	rr := b.form(fmt.Sprintf("/books/%d/edit", id), bookForm("Viewed", "to-read"))
	expect(t, rr, 303)
	if got := rr.Header().Get("Location"); got != fmt.Sprintf("/books/%d?notice=saved", id) {
		t.Fatal(got)
	}
}

func TestEachStatusShowsItsOwnPrimaryAction(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{shelf("Q", "to-read"), shelf("R", "reading"), shelf("D", "read"), shelf("S", "archived"), libbyLoan("U")})
	b := &browser{t: t, handler: a.Handler()}
	want := map[string][]string{
		"Q": {"Start reading Q"},
		"R": {"Finished R"},
		"D": {"Add another reading"},
		"U": {"Want to read U", "Already read U", "Not interested in U"},
		"S": {"Move S back to my queue"},
	}
	all := []string{"Start reading", "Finished ", "Add another reading", "Want to read ", "Already read ", "Not interested in ", "back to my queue"}
	for title, labels := range want {
		body := b.request("GET", fmt.Sprintf("/books/%d", bookNamed(t, a, title).ID), nil).Body.String()
		for _, l := range labels {
			if !strings.Contains(body, l) {
				t.Errorf("%s: missing %q", title, l)
			}
		}
		for _, other := range all {
			listed := false
			for _, l := range labels {
				listed = listed || strings.Contains(l, strings.TrimSpace(other))
			}
			if !listed && strings.Contains(body, other) {
				t.Errorf("%s: shows another status's action %q", title, other)
			}
		}
	}
}

func TestViewPageActionsMatchTheRowButtons(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, shelfBooks("A", "B", "C"))
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	_, before := queueTitles(t, a)
	idA := bookNamed(t, a, "A").ID
	rr := b.form(fmt.Sprintf("/books/%d/status", idA), url.Values{"action": {"start"}, "back": {"book"}})
	expect(t, rr, 303)
	if got := rr.Header().Get("Location"); !strings.HasPrefix(got, fmt.Sprintf("/books/%d?n=start.%d.0.", idA, idA)) {
		t.Fatal(got)
	}
	_, after := queueTitles(t, a)
	if len(after) != 2 || after[0] != before[1] || after[1] != before[2] || bookNamed(t, a, "A").Status != "reading" {
		t.Fatalf("%v -> %v", before, after)
	}
	// Setting aside, then moving back, appends it marked Needs placement.
	expect(t, b.form(fmt.Sprintf("/books/%d/status", idA), url.Values{"action": {"stop"}, "back": {"book"}}), 303)
	expect(t, b.form(fmt.Sprintf("/books/%d/status", idA), url.Values{"action": {"requeue"}, "back": {"book"}}), 303)
	if got := bookNamed(t, a, "A"); got.Status != "to-read" || !got.NeedsPlacement || got.Pinned {
		t.Fatalf("%+v", got)
	}
	titles, _ := queueTitles(t, a)
	if strings.Join(titles, ",") != "B,C,A" {
		t.Fatal(titles)
	}
	expect(t, b.form(fmt.Sprintf("/books/%d/status", idA), url.Values{"action": {"requeue"}}), 400)
}

func shelfBooks(names ...string) []Record {
	out := []Record{}
	for _, n := range names {
		out = append(out, shelf(n, "to-read"))
	}
	return out
}
