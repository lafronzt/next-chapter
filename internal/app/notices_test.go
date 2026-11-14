package app

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func noticeLibrary(t *testing.T) (*App, *browser) {
	t.Helper()
	a := testApp(t, true)
	addRecords(t, a, records("Alpha", "Bravo", "Charlie", "Delta"))
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	return a, &browser{t: t, handler: a.Handler()}
}

func TestEachActionNamesItsBookAndUndoRestoresIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, title, path string
		values            url.Values
		prep, want        string
	}{
		{"down", "Alpha", "action", url.Values{"action": {"down"}}, "", "Moved “Alpha” down one place."},
		{"up", "Charlie", "action", url.Values{"action": {"up"}}, "", "Moved “Charlie” up one place."},
		{"move", "Delta", "action", url.Values{"action": {"move"}, "position": {"2"}}, "", "Moved “Delta” to #2."},
		{"pin", "Bravo", "action", url.Values{"action": {"pin"}}, "", "Pinned “Bravo”."},
		{"unpin", "Bravo", "action", url.Values{"action": {"pin"}}, "UPDATE books SET pinned=1 WHERE title='Bravo'", "Unpinned “Bravo”."},
		{"placed", "Charlie", "action", url.Values{"action": {"placed"}}, "UPDATE books SET needs_placement=1 WHERE title='Charlie'", "“Charlie” keeps its place and no longer needs placement."},
		{"start", "Alpha", "status", url.Values{"action": {"start"}}, "UPDATE books SET pinned=1 WHERE title='Alpha'", "“Alpha” is now Reading."},
		{"stop", "Bravo", "status", url.Values{"action": {"stop"}}, "UPDATE books SET reading_status='reading' WHERE title='Bravo'", "“Bravo” is now Set aside."},
		{"requeue", "Delta", "status", url.Values{"action": {"requeue"}}, "UPDATE books SET reading_status='abandoned' WHERE title='Delta'", "“Delta” is back in your queue and needs a spot."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := noticeLibrary(t)
			if c.prep != "" {
				if _, err := a.store.DB.Exec(c.prep); err != nil {
					t.Fatal(err)
				}
			}
			before, err := loadBooks(a.store.DB)
			if err != nil {
				t.Fatal(err)
			}
			id := bookNamed(t, a, c.title).ID
			rr := b.form(fmt.Sprintf("/books/%d/%s", id, c.path), c.values)
			expect(t, rr, 303)
			loc, _, _ := strings.Cut(rr.Header().Get("Location"), "#") // browsers never send the fragment
			page := b.request("GET", loc, nil).Body.String()
			if got := regexpNotice(page); got != c.want {
				t.Fatalf("notice %q, want %q (%s)", got, c.want, loc)
			}
			if !strings.Contains(page, `aria-label="Undo: `+c.want+`"`) {
				t.Fatal("the notice has no named Undo button")
			}
			changed, _ := loadBooks(a.store.DB)
			if reflect.DeepEqual(before, changed) {
				t.Fatal("the action changed nothing")
			}
			expect(t, b.form("/undo", nil), 303)
			after, err := loadBooks(a.store.DB)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("undo did not restore the books:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

func TestUndoIsRefusedAfterAnotherChange(t *testing.T) {
	t.Parallel()
	a, b := noticeLibrary(t)
	id := bookNamed(t, a, "Alpha").ID
	expect(t, b.form(fmt.Sprintf("/books/%d/action", id), url.Values{"action": {"down"}}), 303)
	// An unrelated change comes first (a loan is recorded).
	expect(t, b.form(fmt.Sprintf("/books/%d/edit", bookNamed(t, a, "Bravo").ID), bookForm("Bravo", "to-read", "interest", "5")), 303)
	after, _ := loadBooks(a.store.DB)
	rr := b.form("/undo", nil)
	expect(t, rr, 409)
	if !strings.Contains(rr.Body.String(), "Undo expired") {
		t.Fatal(rr.Body.String())
	}
	if again, _ := loadBooks(a.store.DB); !reflect.DeepEqual(after, again) {
		t.Fatal("a refused undo changed the library")
	}
}

func TestNoticesAreEscapedAndTruncated(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	long := strings.Repeat("Very Long Title ", 40)
	addRecords(t, a, records("<script>alert(1)</script>", long))
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	for _, title := range []string{"<script>alert(1)</script>", long} {
		id := bookNamed(t, a, title).ID
		rr := b.form(fmt.Sprintf("/books/%d/action", id), url.Values{"action": {"pin"}})
		expect(t, rr, 303)
		loc, _, _ := strings.Cut(rr.Header().Get("Location"), "#")
		body := b.request("GET", loc, nil).Body.String()
		if strings.Contains(body, "<script>alert(1)") {
			t.Fatal("title not escaped in notice")
		}
		notice := regexpNotice(body)
		if title == long && (len([]rune(notice)) > 140 || !strings.Contains(notice, "…")) {
			t.Fatalf("long title not truncated: %d runes", len([]rune(notice)))
		}
		if title != long && !strings.Contains(notice, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Fatalf("escaped title missing: %s", notice)
		}
	}
}

func regexpNotice(body string) string {
	i := strings.Index(body, `<div class="notice" role="status">`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(`<div class="notice" role="status">`):]
	end := strings.Index(rest, "</div>")
	if f := strings.Index(rest, "<form"); f >= 0 && f < end {
		end = f
	}
	return strings.TrimSpace(rest[:end])
}

func TestForgedNoticesNeverRenderAttackerText(t *testing.T) {
	t.Parallel()
	a, b := noticeLibrary(t)
	id := bookNamed(t, a, "Alpha").ID
	for _, forged := range []string{
		"pin.1.0.deadbeefdeadbeef",                  // bad signature
		"pin.1.0",                                   // unsigned
		"<b>hacked</b>",                             // free text
		"x.1.0." + strings.Repeat("a", 16),          // unknown action
		fmt.Sprintf("pin.%d.0.", id),                // empty signature
		"move.1.999999999999999999999.0123456789ab", // junk number
	} {
		body := b.request("GET", "/?n="+url.QueryEscape(forged), nil).Body.String()
		if n := regexpNotice(body); strings.Contains(n, "hacked") || strings.Contains(n, "Pinned") {
			t.Errorf("%q rendered attacker text", forged)
		}
		if !strings.Contains(body, `role="status">`+genericNotice) {
			t.Errorf("%q: forged key did not show the generic notice", forged)
		}
	}
	// A genuine key for a book that was since deleted is stale: generic too.
	good := a.noticeValue("pin", 9999, 0)
	if body := b.request("GET", "/?n="+url.QueryEscape(good), nil).Body.String(); !strings.Contains(body, `role="status">`+genericNotice) {
		t.Error("stale key did not show the generic notice")
	}
}

func TestImportNoticeSaysHowManyNeedASpot(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	b.request("GET", "/imports", nil)
	rr := uploadCSV(b, "goodreads", "Title,Author\nOne,A\nTwo,B\nThree,C\n")
	expect(t, rr, 303)
	b.request("GET", rr.Header().Get("Location"), nil)
	rr = b.request("POST", rr.Header().Get("Location"), nil)
	expect(t, rr, 303)
	if body := b.request("GET", rr.Header().Get("Location"), nil).Body.String(); !strings.Contains(body, "Import complete: 3 books added, 3 need a spot.") {
		t.Fatal(regexpNotice(body))
	}
}

func TestEmptyStatesSayTheNextStep(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	cases := map[string][]string{
		"/":                 {"Import your Goodreads CSV", "Add a book"},
		"/?q=nothing":       {"Clear the search"},
		"/?view=unreviewed": {"Import library activity"},
		"/reading":          {"Mark a book as finished"},
	}
	for path, wants := range cases {
		body := b.request("GET", path, nil).Body.String()
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", path, w)
			}
		}
	}
}
