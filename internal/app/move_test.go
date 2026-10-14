package app

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// moveTo posts a Move to # for the named book, reloading the queue first so
// the revision is current.
func moveTo(t *testing.T, b *browser, a *App, title, position string) string {
	t.Helper()
	b.request("GET", "/", nil)
	rr := b.request("POST", fmt.Sprintf("/books/%d/action", bookIDs(t, a)[title]), url.Values{"action": {"move"}, "position": {position}})
	if rr.Code == 303 {
		return ""
	}
	expect(t, rr, 400)
	return rr.Body.String()
}

func queueNames(t *testing.T, a *App) string {
	t.Helper()
	titles, _ := queueTitles(t, a)
	return strings.ReplaceAll(strings.Join(titles, ","), "Book ", "")
}

func TestMoveToQueuePosition(t *testing.T) {
	t.Parallel()
	a, b := queueApp(t, 6)
	steps := []struct{ title, to, want string }{
		{"Book 004", "2", "000,004,001,002,003,005"},   // up: books between shift down
		{"Book 004", "5", "000,001,002,003,004,005"},   // down: books between shift up
		{"Book 003", "1", "003,000,001,002,004,005"},   // to first
		{"Book 003", "6", "000,001,002,004,005,003"},   // to last
		{"Book 001", " 2 ", "000,001,002,004,005,003"}, // already there
	}
	for _, s := range steps {
		before := currentRevision(t, a)
		if msg := moveTo(t, b, a, s.title, s.to); msg != "" {
			t.Fatalf("%s to %s: %s", s.title, s.to, msg)
		}
		if got := queueNames(t, a); got != s.want {
			t.Fatalf("%s to %s: %s, want %s", s.title, s.to, got, s.want)
		}
		if moved := strings.TrimSpace(s.to) != "2"; moved && currentRevision(t, a) != before+1 {
			t.Fatalf("%s to %s did not bump the revision", s.title, s.to)
		}
	}
	// A stale page is refused like any other change.
	rr := b.request("POST", fmt.Sprintf("/books/%d/action", bookIDs(t, a)["Book 000"]), url.Values{"action": {"move"}, "position": {"3"}, "revision": {"0"}})
	expect(t, rr, 409)
}

func TestMoveToQueuePositionRejectsBadNumbers(t *testing.T) {
	t.Parallel()
	a, b := queueApp(t, 4)
	before, rev := queueNames(t, a), currentRevision(t, a)
	for _, to := range []string{"0", "-1", "5", "99", "", "two", "2.5"} {
		if msg := moveTo(t, b, a, "Book 001", to); !strings.Contains(msg, "Enter a queue position from 1 to 4.") {
			t.Fatalf("%q: %s", to, msg)
		}
	}
	if queueNames(t, a) != before || currentRevision(t, a) != rev {
		t.Fatal("a refused move changed the queue")
	}
	// Books outside the queue have no position to move.
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='finished' WHERE title='Book 003'"); err != nil {
		t.Fatal(err)
	}
	if msg := moveTo(t, b, a, "Book 003", "1"); !strings.Contains(msg, "Choose a book in your reading queue.") {
		t.Fatal(msg)
	}
}

func TestMoveToQueuePositionKeepsPins(t *testing.T) {
	t.Parallel()
	a, b := queueApp(t, 6)
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1 WHERE title='Book 002'"); err != nil {
		t.Fatal(err)
	}
	before, rev := queueNames(t, a), currentRevision(t, a)
	for _, c := range []struct{ title, to, msg string }{
		{"Book 000", "4", "cross the pinned book at #3"}, // across the pin
		{"Book 005", "1", "cross the pinned book at #3"},
		{"Book 004", "3", "cross the pinned book at #3"}, // onto the pin's slot
		{"Book 000", "3", "cross the pinned book at #3"},
		{"Book 002", "1", "Pinned books stay in place."}, // the pin itself
	} {
		if msg := moveTo(t, b, a, c.title, c.to); !strings.Contains(msg, c.msg) {
			t.Fatalf("%s to %s: %s", c.title, c.to, msg)
		}
	}
	if queueNames(t, a) != before || currentRevision(t, a) != rev {
		t.Fatal("a refused move changed the queue")
	}
	// Moves on either side of the pin still work, and it keeps its slot.
	for _, c := range []struct{ title, to string }{{"Book 001", "1"}, {"Book 003", "6"}} {
		if msg := moveTo(t, b, a, c.title, c.to); msg != "" {
			t.Fatal(msg)
		}
	}
	if got := queueNames(t, a); got != "001,000,002,004,005,003" {
		t.Fatal(got)
	}
}

// Queue numbers count from the top of the whole queue, so a move typed on a
// later page lands where its number says.
func TestMoveToQueuePositionAcrossPages(t *testing.T) {
	t.Parallel()
	a, b := queueApp(t, 60)
	body, _ := listed(t, b, "/?page=2")
	if !strings.Contains(body, `id="move-`+fmt.Sprint(bookIDs(t, a)["Book 055"])+`" name="position" type="number" inputmode="numeric" min="1" max="60" required placeholder="56"`) {
		t.Fatal("move input on page 2")
	}
	rr := b.request("POST", fmt.Sprintf("/books/%d/action", bookIDs(t, a)["Book 055"]), url.Values{"action": {"move"}, "position": {"51"}, "return": {"/?page=2"}})
	expect(t, rr, 303)
	if got := rr.Header().Get("Location"); got != "/?page=2" {
		t.Fatal(got)
	}
	titles, _ := queueTitles(t, a)
	if i := slices.Index(titles, "Book 055"); i != 50 || titles[51] != "Book 050" || titles[55] != "Book 054" || titles[56] != "Book 056" {
		t.Fatal(titles[48:58])
	}
	// The pinned book's form is disabled with the reason.
	if _, err := a.store.DB.Exec("UPDATE books SET pinned=1 WHERE title='Book 000'"); err != nil {
		t.Fatal(err)
	}
	body, _ = listed(t, b, "/")
	if !strings.Contains(body, `placeholder="1" disabled title="Pinned books stay in place. Unpin it to move it."`) {
		t.Fatal("pinned move input is not disabled")
	}
}

func movedByHand(t *testing.T, a *App) string {
	t.Helper()
	books := noErr(loadQueue(a.store.DB))
	out := []string{}
	for _, b := range books {
		if b.MovedByHand {
			out = append(out, strings.TrimPrefix(b.Title, "Book "))
		}
	}
	return strings.Join(out, ",")
}

// A manual move marks the book, the usual ranking says so without acting on
// it, and applying a suggestion replaces the mark until undo restores it.
func TestManualMovesAreMarked(t *testing.T) {
	t.Parallel()
	a, b := queueApp(t, 5)
	if msg := moveTo(t, b, a, "Book 004", "2"); msg != "" {
		t.Fatal(msg)
	}
	b.request("GET", "/", nil)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/action", bookIDs(t, a)["Book 000"]), url.Values{"action": {"down"}}), 303)
	if got := movedByHand(t, a); got != "004,000" {
		t.Fatal(got)
	}
	// Refused moves and moves to the same place mark nothing.
	if moveTo(t, b, a, "Book 002", "9") == "" || moveTo(t, b, a, "Book 002", "4") != "" || movedByHand(t, a) != "004,000" {
		t.Fatal("marked without a move")
	}
	body, _ := listed(t, b, "/")
	if strings.Count(body, `<span class="badge">Moved by hand</span>`) != 2 {
		t.Fatal("badge missing")
	}
	rows := noErr(Propose(noErr(loadQueue(a.store.DB)), defaultPreferences, 0, time.Now()))
	for _, row := range rows {
		marked := slices.Contains(row.Reasons, "You moved this by hand")
		if marked != (row.Title == "Book 004" || row.Title == "Book 000") {
			t.Fatalf("%s reasons %v", row.Title, row.Reasons)
		}
	}
	// Placing one book clears only that book's mark.
	b.request("GET", "/", nil)
	rr := b.request("POST", "/suggest", url.Values{"candidate_id": {fmt.Sprint(bookIDs(t, a)["Book 004"])}})
	expect(t, rr, 303)
	expect(t, b.request("POST", rr.Header().Get("Location"), nil), 303)
	if got := movedByHand(t, a); got != "000" {
		t.Fatal(got)
	}
	// Applying a full suggestion clears the rest; undo brings them back.
	b.request("GET", "/", nil)
	rr = b.request("POST", "/suggest", nil)
	expect(t, rr, 303)
	expect(t, b.request("POST", rr.Header().Get("Location"), nil), 303)
	if got := movedByHand(t, a); got != "" {
		t.Fatal(got)
	}
	b.request("GET", "/", nil)
	expect(t, b.request("POST", "/undo", nil), 303)
	if got := movedByHand(t, a); got != "000" {
		t.Fatal(got)
	}
}

// The AI sees which books the reader moved, even ones the usual ranking
// would not send, and the prompt says what the mark means.
func TestManualMovesReachTheAI(t *testing.T) {
	t.Parallel()
	a, b := queueApp(t, 70)
	if msg := moveTo(t, b, a, "Book 069", "65"); msg != "" {
		t.Fatal(msg)
	}
	ri := noErr(loadRerankInputs(a.store.DB, "reorder", Plan{}, time.Now()))
	sent := map[string]bool{}
	for _, book := range ri.Payload.Books {
		sent[book.Title] = book.MovedByHand
	}
	if marked, ok := sent["Book 069"]; !ok || !marked || len(ri.Payload.Books) != rerankTop+1 || sent["Book 000"] {
		t.Fatalf("payload: %d books, 069 %v", len(ri.Payload.Books), sent)
	}
	req := noErr(ri.request())
	if !strings.Contains(req.User, `"moved_by_hand":true`) || !strings.Contains(req.System+req.User, "moved_by_hand is where the reader put it themselves") {
		t.Fatal("prompt does not explain hand moves")
	}
	place := noErr(loadRerankInputs(a.store.DB, "insert", Plan{CandidateID: bookIDs(t, a)["Book 040"]}, time.Now()))
	if req := noErr(place.request()); !strings.Contains(req.System+req.User, "moved_by_hand is where the reader put it themselves") {
		t.Fatal("placement prompt does not explain hand moves")
	}
}
