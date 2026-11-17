package app

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestReviewIncludesAllBooksAwaitingPlacement(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, append(records("Placed first", "Manual waiting", "Placed middle", "Imported waiting", "Reading", "Archived"), libbyLoan("Undecided")))
	if _, err := a.store.DB.Exec(`UPDATE books SET needs_placement=0 WHERE title LIKE 'Placed%';
UPDATE books SET source='goodreads' WHERE title='Imported waiting';
UPDATE books SET reading_status='reading' WHERE title='Reading';
UPDATE books SET reading_status='archived' WHERE title='Archived'`); err != nil {
		t.Fatal(err)
	}
	before, rev := queueOrderOf(t, a), revisionOf(t, a)
	items, err := listBooks(a.store.DB, findView("unreviewed").filter(""), 50, 0)
	if err != nil || len(items) != 3 {
		t.Fatalf("%+v %v", items, err)
	}
	positions := map[string]int{}
	for _, item := range items {
		positions[item.Title] = item.QueuePos
	}
	if positions["Manual waiting"] != 2 || positions["Imported waiting"] != 4 {
		t.Fatal(positions)
	}
	filtered, err := countBooks(a.store.DB, findView("unreviewed").filter("manual"))
	if err != nil || filtered != 1 {
		t.Fatal(filtered, err)
	}
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/?view=unreviewed&per_page=1&page=2", nil)
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "page 2 of 3") {
		t.Fatal("review pagination did not count placement items")
	}
	body := b.request("GET", "/?view=unreviewed", nil).Body.String()
	for _, want := range []string{"Manual waiting", "Imported waiting", "Undecided", "Find its place", "Keep here", "Mark 1 undecided book as", `<strong class="nav-count">3<span class="sr-only"> waiting</span></strong>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if queueOrderOf(t, a) != before || revisionOf(t, a) != rev {
		t.Fatal("opening review changed the queue")
	}
	// A reading decision moves a book into the placement stage, not out of review.
	undecided := bookNamed(t, a, "Undecided")
	expect(t, b.form(fmt.Sprintf("/books/%d/triage", undecided.ID), url.Values{"action": {"want"}}), 303)
	if count, err := countBooks(a.store.DB, bookFilter{Review: true}); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	if book := bookNamed(t, a, "Undecided"); book.Status != "to-read" || !book.NeedsPlacement {
		t.Fatalf("%+v", book)
	}
	// Keeping the saved spot completes review without moving any queue entries.
	pending := bookNamed(t, a, "Manual waiting")
	order := queueOrderOf(t, a)
	expect(t, b.form(fmt.Sprintf("/books/%d/action", pending.ID), url.Values{"action": {"placed"}, "return": {"/?view=unreviewed"}}), 303)
	if count, err := countBooks(a.store.DB, bookFilter{Review: true}); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if queueOrderOf(t, a) != order {
		t.Fatal("Keep here reordered the queue")
	}
	body = b.request("GET", "/?view=unreviewed", nil).Body.String()
	if strings.Contains(body, `href="/books/`+fmt.Sprint(pending.ID)+`"`) || strings.Contains(body, `action="/triage"`) {
		t.Fatal("placed book or empty triage action remained in review")
	}
}

func TestReviewBulkDecisionNeverIncludesPlacementBooks(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, append(records("Waiting place"), libbyLoan("Waiting decision")))
	queued := bookNamed(t, a, "Waiting place")
	before, rev := queueOrderOf(t, a), revisionOf(t, a)
	b := &browser{t: t, handler: a.Handler()}
	b.request("GET", "/?view=unreviewed", nil)
	path := locationOf(t, b.form("/triage", url.Values{"action": {"skip"}, "q": {"waiting"}}))
	kind, p, _, err := readPlan(a.store.DB, strings.TrimPrefix(path, "/plans/"))
	if err != nil || kind != "triage" || len(p.BookIDs) != 1 || p.BookIDs[0] == queued.ID {
		t.Fatalf("%+v %v", p.BookIDs, err)
	}
	if revisionOf(t, a) != rev {
		t.Fatal("preview changed library")
	}
	b.request("GET", path, nil)
	expect(t, b.form(path, nil), 303)
	after, _ := getBook(a.store.DB, queued.ID)
	if after.Status != queued.Status || after.Position != queued.Position || !after.NeedsPlacement || queueOrderOf(t, a) != before {
		t.Fatal("bulk decision touched a queued placement book")
	}
	if count, err := countBooks(a.store.DB, bookFilter{Review: true}); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
