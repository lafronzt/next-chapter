package app

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestImportPageLeadsWithGoodreads(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/imports", nil).Body.String()
	g, l := strings.Index(body, "My Books → Import and export → Export Library"), strings.Index(body, "Libby or NYPL library activity")
	if g < 0 || l < 0 || g > l {
		t.Fatalf("Goodreads guide %d, Libby %d", g, l)
	}
	for _, want := range []string{`<ol class="steps">`, `accept=".csv,text/csv"`, "No Goodreads import yet.", `<details class="panel library-sources">`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, old := range []string{"check Goodreads", "poll", "feed"} {
		if strings.Contains(strings.ToLower(body), old) {
			t.Errorf("stale wording %q", old)
		}
	}
}

func TestExampleReimportShowsNothingToChange(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	data, err := assets.ReadFile("web/examples/goodreads.csv")
	if err != nil {
		t.Fatal(err)
	}
	b.request("GET", "/imports", nil)
	rr := uploadCSV(b, "goodreads", string(data))
	expect(t, rr, 303)
	first := b.request("GET", rr.Header().Get("Location"), nil).Body.String()
	if !regexp.MustCompile(`stat-number">[1-9]\d*</span><span>new books`).MatchString(first) {
		t.Fatal("first import shows no new books")
	}
	// The summary leads, before the record list.
	if s, r := strings.Index(first, "new books"), strings.Index(first, `class="panel preview-list"`); s < 0 || r < 0 || s > r {
		t.Fatalf("summary at %d, records at %d", s, r)
	}
	expect(t, b.request("POST", rr.Header().Get("Location"), url.Values{}), 303)
	// Last imported now says what happened.
	if body := b.request("GET", "/imports", nil).Body.String(); !strings.Contains(body, "Last imported") || strings.Contains(body, "No Goodreads import yet.") {
		t.Fatal("last import not shown")
	}
	b.request("GET", "/imports", nil)
	rr = uploadCSV(b, "goodreads", string(data))
	expect(t, rr, 303)
	again := b.request("GET", rr.Header().Get("Location"), nil).Body.String()
	for _, want := range []string{`stat-number">0</span><span>new books`, `stat-number">0</span><span>finished`, `stat-number">0</span><span>started`} {
		if !strings.Contains(again, want) {
			t.Errorf("second import missing %s", want)
		}
	}
}

func TestImportPreviewNamesBooksLeavingTheQueue(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{shelf("Was Queued", "to-read")})
	b := &browser{t: t, handler: a.Handler()}
	b.request("GET", "/imports", nil)
	rr := uploadCSV(b, "goodreads", "Title,Author,Exclusive Shelf,Date Read\nWas Queued,Author,read,2026/10/01\n")
	expect(t, rr, 303)
	body := b.request("GET", rr.Header().Get("Location"), nil).Body.String()
	if !strings.Contains(body, "1 book leaves your queue") || !strings.Contains(body, "<strong>Was Queued</strong> · Author → Read") {
		t.Fatal(body)
	}
	if strings.Index(body, "leaves your queue") > strings.Index(body, `class="panel preview-list"`) {
		t.Fatal("moved books are not listed before the records")
	}
}

func TestOldStoredImportPlansStillDecode(t *testing.T) {
	t.Parallel()
	var p Plan
	if err := json.Unmarshal([]byte(`{"records":[{"title":"A"}],"source":"goodreads","advances":{"read":1}}`), &p); err != nil {
		t.Fatal(err)
	}
	if s := summarizeImport(p); s.Finished != 1 || s.New != 0 {
		t.Fatalf("%+v", s)
	}
}
