package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

type reviewFake struct {
	remote                     hcReview
	writes, calls              int
	ambiguous, refuse, missing bool
	variables                  map[string]any
}

func (f *reviewFake) handle(query string, vars map[string]any, _ int) (int, map[string]string, string) {
	f.calls++
	if strings.Contains(query, "NextChapterReviewUpdate") {
		f.writes++
		f.variables = vars
		if f.refuse {
			return 200, nil, `{"data":{"update_user_book":{"error":"refused","user_book":null}}}`
		}
		f.remote.Markdown = vars["review"].(string)
		f.remote.Updated = "later"
		if f.ambiguous {
			f.ambiguous = false
			return 200, nil, `invalid json`
		}
		data, _ := json.Marshal(map[string]any{"data": map[string]any{"update_user_book": map[string]any{"id": f.remote.ID, "error": nil, "user_book": f.remote}}})
		return 200, nil, string(data)
	}
	if strings.Contains(query, "NextChapterReview(") {
		books := []hcReview{f.remote}
		if f.missing {
			books = nil
		}
		data, _ := json.Marshal(map[string]any{"data": map[string]any{"user_books": books}})
		return 200, nil, string(data)
	}
	return 500, nil, `{}`
}

func reviewApp(t *testing.T) (*App, *browser, int64, *reviewFake) {
	t.Helper()
	f := &reviewFake{remote: hcReview{ID: 50, Markdown: "Earlier remote review", Raw: "Earlier remote review", Updated: "before", Privacy: 1}}
	a, b, _ := newHCApp(t, f.handle)
	b.gateway = true
	linkAccount(t, a)
	id := addBooks(t, a, Book{Title: "Review Book", Author: "Writer", Reason: "PRIVATE REASON"})[0]
	if _, err := a.store.DB.Exec("INSERT INTO book_hardcover(book_id,hardcover_book_id,linked_at) VALUES(?,500,'now')", id); err != nil {
		t.Fatal(err)
	}
	expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), url.Values{"rating": {"4.25"}, "review": {"**A review**\n\n- café"}}), 303)
	return a, b, readingsOf(t, a, "Review Book")[0].ID, f
}

func previewReview(t *testing.T, a *App, b *browser, rid int64) string {
	t.Helper()
	refill(a)
	return locationOf(t, b.form(fmt.Sprintf("/readings/%d/hardcover/review", rid), nil))
}

func TestHardcoverReviewExplicitSendAndRetry(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			a, b, rid, f := reviewApp(t)
			if f.calls != 0 {
				t.Fatal("local save contacted Hardcover")
			}
			path := previewReview(t, a, b, rid)
			body := b.request("GET", path, nil).Body.String()
			for _, want := range []string{"<strong>A review</strong>", "public", "Earlier remote review", "one review per book"} {
				if !strings.Contains(body, want) {
					t.Fatalf("preview missing %q", want)
				}
			}
			if f.writes != 0 {
				t.Fatal("preview sent review")
			}
			f.ambiguous = ambiguous
			refill(a)
			rr := b.form(path, url.Values{"confirm": {"share"}})
			if ambiguous {
				expect(t, rr, 502)
				refill(a)
				rr = b.form(path, url.Values{"confirm": {"share"}})
			}
			expect(t, rr, 303)
			if f.writes != 1 || len(f.variables) != 2 || f.variables["review"] != "**A review**\n\n- café" || f.remote.Privacy != 1 {
				t.Fatal("review send changed unrelated data", f)
			}
			refill(a)
			expect(t, b.form(path, url.Values{"confirm": {"share"}}), 404)
			path = previewReview(t, a, b, rid)
			refill(a)
			expect(t, b.form(path, url.Values{"confirm": {"share"}}), 303)
			if f.writes != 1 {
				t.Fatal("unchanged review resent")
			}
		})
	}
}

func TestHardcoverReviewConflictsAndRefusal(t *testing.T) {
	for _, mode := range []string{"local", "remote", "privacy", "account", "book", "missing", "refusal", "unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			a, b, rid, f := reviewApp(t)
			path := previewReview(t, a, b, rid)
			status := 409
			switch mode {
			case "local":
				expect(t, b.form(fmt.Sprintf("/readings/%d", rid), url.Values{"action": {"save"}, "review": {"new"}}), 303)
			case "remote":
				f.remote.Markdown = "Remote edit"
			case "privacy":
				f.remote.Privacy = 3
			case "account":
				_, _ = a.store.DB.Exec("UPDATE hardcover_link SET hardcover_user_id=999")
			case "book":
				_, _ = a.store.DB.Exec("UPDATE book_hardcover SET hardcover_book_id=999")
			case "missing":
				f.missing = true
				status = 400
			case "refusal":
				f.refuse = true
				status = 502
			case "unconfirmed":
				status = 400
			}
			refill(a)
			confirm := "share"
			if mode == "unconfirmed" {
				confirm = ""
			}
			expect(t, b.form(path, url.Values{"confirm": {confirm}}), status)
			if mode != "refusal" && f.writes != 0 {
				t.Fatal("conflict sent review")
			}
			if e := readingsOf(t, a, "Review Book")[0]; e.Review == nil {
				t.Fatal("lost local review")
			}
		})
	}
}
