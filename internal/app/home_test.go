package app

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func dueLoan(title, due string) Record {
	r := libbyLoan(title)
	r.Event.DueDate = due
	return r
}

func TestStartReadingMovesOnlyThatBook(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("A", "B", "C", "D"))
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1 WHERE title='A'"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/", nil).Body.String()
	if !strings.Contains(body, `aria-label="Start reading A"`) {
		t.Fatal("A is not Next up")
	}
	_, before := queueTitles(t, a)
	id := bookNamed(t, a, "A").ID
	expect(t, b.form(fmt.Sprintf("/books/%d/status", id), url.Values{"action": {"start"}}), 303)
	if got := bookNamed(t, a, "A"); got.Status != "reading" || got.Pinned {
		t.Fatalf("%+v", got)
	}
	titles, after := queueTitles(t, a)
	if strings.Join(titles, ",") != "B,C,D" || len(after) != 3 || after[0] != before[1] || after[1] != before[2] || after[2] != before[3] {
		t.Fatalf("%v %v (was %v)", titles, after, before)
	}
	body = b.request("GET", "/", nil).Body.String()
	if !strings.Contains(body, `aria-label="Finished A"`) || !strings.Contains(body, `aria-label="Start reading B"`) {
		t.Fatal("Reading now or the new Next up is missing")
	}
	// A stale page cannot start it twice or start a non-queued book.
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/status", id), url.Values{"action": {"start"}, "revision": {"0"}}), 409)
	expect(t, b.form(fmt.Sprintf("/books/%d/status", id), url.Values{"action": {"start"}}), 400)
	expect(t, b.form(fmt.Sprintf("/books/%d/status", id), url.Values{"action": {"stop"}}), 303)
	if bookNamed(t, a, "A").Status != "abandoned" {
		t.Fatal("stop reading")
	}
}

func TestFinishedLogsExactlyOneReading(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, records("Solo", "Other"))
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='reading' WHERE title='Solo'"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	id := bookNamed(t, a, "Solo").ID
	page := b.request("GET", fmt.Sprintf("/books/%d/finish", id), nil).Body.String()
	if !strings.Contains(page, `value="`+time.Now().Format(time.DateOnly)+`"`) || !strings.Contains(page, `<fieldset class="rating-input">`) {
		t.Fatal("finish form lacks today's date or rating control")
	}
	interest := bookNamed(t, a, "Solo").Interest
	values := url.Values{"finished_at": {"2026-11-01"}, "rating": {"4.25"}}
	rr := b.form(fmt.Sprintf("/books/%d/finish", id), values)
	expect(t, rr, 303)
	// Resubmitting the same form (a double submit) is refused by the revision check.
	values.Set("csrf", b.csrf)
	values.Set("revision", "0")
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/finish", id), values), 409)
	if got := bookNamed(t, a, "Solo"); got.Status != "read" || got.Interest != interest {
		t.Fatalf("%+v", got)
	}
	rs := readingsOf(t, a, "Solo")
	if len(rs) != 1 || rs[0].Rating == nil || *rs[0].Rating != 4.25 {
		t.Fatalf("%+v", rs)
	}
	expect(t, b.form(fmt.Sprintf("/books/%d/finish", id), url.Values{"finished_at": {"2026-11-02"}}), 400)
	expect(t, b.form(fmt.Sprintf("/books/%d/finish", bookNamed(t, a, "Other").ID), url.Values{"finished_at": {""}}), 400)
}

func TestDueSoonListsActiveLoansInOrderWithoutChangingStatus(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	day := func(n int) string { return time.Now().AddDate(0, 0, n).Format(time.DateOnly) }
	addRecords(t, a, []Record{dueLoan("Later", day(5)), dueLoan("Sooner", day(2)), dueLoan("Overdue", day(-3)), dueLoan("Far", day(20))})
	addRecords(t, a, []Record{{Book: Book{Title: "Returned", Author: "Author", Status: "unreviewed", Source: "libby"}, Event: &Event{Format: "ebook", Status: "returned", Activity: "Returned", At: "2026-10-02T00:00:00.000000000+00:00", DueDate: day(1)}}})
	h, err := loadHome(a.store.DB, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range h.Due {
		got = append(got, d.Book.Title)
		if d.Book.Status != "unreviewed" {
			t.Fatalf("%s status %s", d.Book.Title, d.Book.Status)
		}
	}
	if strings.Join(got, ",") != "Overdue,Sooner,Later" {
		t.Fatal(got)
	}
	if h.Due[0].DaysText() != "3 days past the due date" || h.Due[1].DaysText() != "2 days left" {
		t.Fatalf("%q %q", h.Due[0].DaysText(), h.Due[1].DaysText())
	}
	b := &browser{t: t, handler: a.Handler()}
	if body := b.request("GET", "/", nil).Body.String(); !strings.Contains(body, `id="due-soon-title"`) {
		t.Fatal("no Due soon strip")
	}
}

func TestNextUpSkipsUnreviewedArchivedAndUnplaced(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{shelf("Archived One", "archived"), libbyLoan("Review Me")})
	addRecords(t, a, records("Fresh"))
	h, err := loadHome(a.store.DB, time.Now())
	if err != nil || h.Next != nil {
		t.Fatalf("%v %+v", err, h.Next)
	}
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0 WHERE title='Fresh'"); err != nil {
		t.Fatal(err)
	}
	if h, _ = loadHome(a.store.DB, time.Now()); h.Next == nil || h.Next.Title != "Fresh" {
		t.Fatalf("%+v", h.Next)
	}
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/?q=fresh", nil).Body.String()
	if strings.Contains(body, `id="next-up-title"`) {
		t.Fatal("Next up shown during a search")
	}
}
