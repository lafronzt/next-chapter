package app

import (
	"net/url"
	"strings"
	"testing"
)

func TestStartReadingWithHardcoverPreview(t *testing.T) {
	t.Parallel()
	for _, remoteStatus := range []int{0, 1, 2, 3} {
		t.Run(hcStatusNames[remoteStatus], func(t *testing.T) {
			remote := &pushRemote{rows: map[int]*pushRow{}}
			if remoteStatus != 0 {
				remote.rows[500] = &pushRow{ID: 61, Status: remoteStatus}
			}
			a, b, ids := pushApp(t, remote, "to-read:500")
			id := ids[0]
			b.request("GET", bookPath(id), nil)
			loc := locationOf(t, b.request("POST", bookPath(id)+"/status", url.Values{"action": {"start-hardcover"}}))
			if !strings.HasPrefix(loc, "/hardcover/pushes/") || remote.mutations() != 0 {
				t.Fatalf("expected preview without remote mutation: %s", loc)
			}
			if dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "reading" {
				t.Fatal("local start was not saved")
			}
			page := b.request("GET", loc, nil)
			if strings.Contains(page.Body.String(), "Your library has changed since this preview") {
				t.Fatal("the local start made its own preview stale")
			}
			if remoteStatus == 2 || remoteStatus == 3 {
				// Already reading creates no duplicate read; Read remains a conflict.
				want := "Already matches"
				if remoteStatus == 3 {
					want = "Conflict, not sent"
				}
				if !strings.Contains(page.Body.String(), want) {
					t.Fatalf("missing %q", want)
				}
				return
			}
			expect(t, b.request("POST", loc, nil), 303)
			if remote.rowAt(500).Status != 2 {
				t.Fatalf("Hardcover did not receive Currently Reading: %+v", remote.rowAt(500))
			}
			b.request("GET", loc, nil)
			expect(t, b.request("POST", loc, nil), 400)
			if remote.mutations() != 1 {
				t.Fatal("confirmation was sent twice")
			}
		})
	}
}

func TestStartReadingKeepsLocalSaveWhenHardcoverFails(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}, hook: func(n int, query string) (int, map[string]string, string, bool) {
		return 503, nil, `{}`, true
	}}
	a, b, ids := pushApp(t, remote, "to-read:500")
	id := ids[0]
	b.request("GET", bookPath(id), nil)
	loc := locationOf(t, b.request("POST", bookPath(id)+"/status", url.Values{"action": {"start-hardcover"}}))
	if !strings.HasSuffix(loc, "notice=started-hc-failed") || dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "reading" {
		t.Fatalf("failed preview lost the local start: %s", loc)
	}
}

func TestOrdinaryStartDoesNotContactHardcover(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, ids := pushApp(t, remote, "to-read:500", "to-read:501")
	remote.hook = func(n int, query string) (int, map[string]string, string, bool) {
		t.Error("ordinary start contacted Hardcover")
		return 503, nil, `{}`, true
	}
	b.request("GET", bookPath(ids[0]), nil)
	expect(t, b.request("POST", bookPath(ids[0])+"/status", url.Values{"action": {"start"}}), 303)
	if dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", ids[1]) != "to-read" {
		t.Fatal("starting a book changed another book")
	}
}

func TestStartHardcoverPreviewRejectsConcurrentChange(t *testing.T) {
	t.Parallel()
	remote := &pushRemote{rows: map[int]*pushRow{}}
	a, b, ids := pushApp(t, remote, "to-read:500")
	remote.hook = func(n int, query string) (int, map[string]string, string, bool) {
		if strings.Contains(query, "NextChapterShelf") {
			if err := bump(a.store.DB); err != nil {
				t.Error(err)
			}
		}
		return 0, nil, "", false
	}
	b.request("GET", bookPath(ids[0]), nil)
	loc := locationOf(t, b.request("POST", bookPath(ids[0])+"/status", url.Values{"action": {"start-hardcover"}}))
	if !strings.HasSuffix(loc, "notice=started-hc-failed") || dbValue[int](t, a, "SELECT count(*) FROM plans WHERE kind='hc-push'") != 0 || remote.mutations() != 0 {
		t.Fatalf("concurrent change produced a sendable preview: %s", loc)
	}
}
