package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func queueIDs(books []Book) []int64 {
	out := []int64{}
	for _, b := range books {
		out = append(out, b.ID)
	}
	return out
}

// fakeDatabases stands in for Open Library, Google Books, and ISBNdb on one
// server, under /ol, /gb, and /idb. It records every request.
type fakeDatabases struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	handlers map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fakeDatabases {
	t.Helper()
	f := &fakeDatabases{handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		f.mu.Unlock()
		for prefix, h := range f.handlers {
			if strings.HasPrefix(r.URL.Path, prefix) {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
				h(w, r)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDatabases) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeDatabases) finder(cfg LookupConfig) *Finder {
	return newFinder(cfg, lookupHosts{OpenLibrary: f.srv.URL + "/ol", GoogleBooks: f.srv.URL + "/gb", ISBNdb: f.srv.URL + "/idb"}, f.srv.Client())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// standardFake knows one book, "Wind Song" by Ada Lee, in both databases.
func standardFake(t *testing.T) *fakeDatabases {
	f := newFake(t)
	f.handlers["/ol/api/books"] = func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("bibkeys")
		if key != "ISBN:9780000000002" && key != "ISBN:9780000000019" && key != "ISBN:9780000000026" {
			writeJSON(w, map[string]any{})
			return
		}
		writeJSON(w, map[string]any{key: map[string]any{"details": map[string]any{"works": []map[string]string{{"key": "/works/OL1W"}}, "number_of_pages": 320, "publish_date": "March 3, 2011"}}})
	}
	f.handlers["/ol/works/OL1W.json"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"description":        map[string]string{"type": "/type/text", "value": "A <b>storm</b> &amp; a song.<script>alert(1)</script>\r\n\r\n\r\n\r\nSecond paragraph."},
			"subjects":           []string{"Sailing", "series:wind_cycle", "Open Library Staff Picks", "place:Maine", "Fiction", "Sea stories"},
			"first_publish_date": "2011",
		})
	}
	f.handlers["/ol/search.json"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"docs": []map[string]any{{"key": "/works/OL1W", "title": "Wind Song", "author_name": []string{"Ada Lee"}, "first_publish_year": 2011, "number_of_pages_median": 320, "subject": []string{"Sailing"}}}})
	}
	f.handlers["/gb/books/v1/volumes"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"items": []map[string]any{{"volumeInfo": map[string]any{"title": "Wind Song", "authors": []string{"Ada Lee"}, "description": "Google blurb", "pageCount": 320, "publishedDate": "2012-05-01", "categories": []string{"Fiction / Sea Stories"}}}}})
	}
	return f
}

func TestLookupMergesSources(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	d, err := f.finder(LookupConfig{}).Find(context.Background(), LookupQuery{Title: "Wind Song", Author: "Ada Lee", ISBN: "978-0-00-000000-2"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Description != "A storm & a song.\n\nSecond paragraph." || d.DescriptionSource != "Open Library" {
		t.Fatalf("description %q from %q", d.Description, d.DescriptionSource)
	}
	if d.Pages == nil || *d.Pages != 320 || d.PublishedYear == nil || *d.PublishedYear != 2011 {
		t.Fatalf("pages %v year %v", d.Pages, d.PublishedYear)
	}
	if d.Series != "Wind Cycle" {
		t.Fatalf("series %q", d.Series)
	}
	if want := []string{"sailing", "sea stories"}; !slices.Equal(d.Tags, want) {
		t.Fatalf("tags %v, want %v", d.Tags, want)
	}
	if !slices.Equal(d.Sources, []string{"Open Library", "Google Books"}) {
		t.Fatalf("sources %v", d.Sources)
	}
}

func TestLookupSearchByTitleAndAuthor(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	d, err := f.finder(LookupConfig{}).Find(context.Background(), LookupQuery{Title: "wind  SONG", Author: "Ada Lee"})

	if err != nil || d.Description == "" || d.Pages == nil || *d.Pages != 320 {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err = f.finder(LookupConfig{}).Find(context.Background(), LookupQuery{Title: "Wind Song", Author: "Someone Else"}); !errors.Is(err, errLookupNotFound) {
		t.Fatalf("a different author matched: %v", err)
	}
}

func TestLookupAgreementPicksMostCommonPagesAndYear(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	f.handlers["/ol/api/books"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ISBN:9780000000002": map[string]any{"details": map[string]any{"number_of_pages": 300, "publish_date": "2001"}}})
	}
	f.handlers["/ol/works/"] = func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	f.handlers["/gb/books/v1/volumes"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"items": []map[string]any{{"volumeInfo": map[string]any{"title": "Wind Song", "authors": []string{"Ada Lee"}, "pageCount": 320, "publishedDate": "2005"}}}})
	}
	f.handlers["/idb/book/"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "secret-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"book": map[string]any{"synopsis": "ISBNdb blurb", "pages": 320, "date_published": "2005-01-01"}})
	}
	d, err := f.finder(LookupConfig{ISBNdbKey: "secret-key"}).Find(context.Background(), LookupQuery{Title: "Wind Song", Author: "Ada Lee", ISBN: "9780000000002"})
	if err != nil {
		t.Fatal(err)
	}
	if *d.Pages != 320 || *d.PublishedYear != 2005 || d.Description != "ISBNdb blurb" || d.DescriptionSource != "ISBNdb" {
		t.Fatalf("%+v", d)
	}
	// Without a key ISBNdb is never contacted.
	before := f.count()
	if _, err = f.finder(LookupConfig{}).Find(context.Background(), LookupQuery{Title: "Wind Song", Author: "Ada Lee", ISBN: "9780000000002"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.requests[before:] {
		if strings.HasPrefix(r.URL.Path, "/idb") {
			t.Fatalf("contacted ISBNdb without a key: %s", r.URL)
		}
	}
}

func TestLookupFailures(t *testing.T) {
	t.Parallel()
	q := LookupQuery{Title: "Wind Song", Author: "Ada Lee", ISBN: "9780000000002"}
	run := func(name string, setup func(f *fakeDatabases), check func(t *testing.T, f *fakeDatabases, err error)) {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			setup(f)
			_, err := f.finder(LookupConfig{}).Find(context.Background(), q)
			check(t, f, err)
		})
	}
	run("not found", func(f *fakeDatabases) {}, func(t *testing.T, f *fakeDatabases, err error) {
		if !errors.Is(err, errLookupNotFound) {
			t.Fatal(err)
		}
	})
	run("ambiguous", func(f *fakeDatabases) {
		f.handlers["/ol/search.json"] = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"docs": []map[string]any{{"key": "/works/OL1W", "title": "Wind Song", "author_name": []string{"Ada Lee"}}, {"key": "/works/OL2W", "title": "Wind Song", "author_name": []string{"Ada Lee"}}}})
		}
	}, func(t *testing.T, f *fakeDatabases, err error) {
		if !errors.Is(err, errLookupAmbiguous) {
			t.Fatal(err)
		}
	})
	run("oversized body", func(f *fakeDatabases) {
		f.handlers["/ol/"] = func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"x":"` + strings.Repeat("a", lookupBodyLimit+10) + `"}`))
		}
	}, func(t *testing.T, f *fakeDatabases, err error) {
		if err == nil || errors.Is(err, errLookupNotFound) || !strings.Contains(err.Error(), "too large") {
			t.Fatal(err)
		}
	})
	run("malformed JSON", func(f *fakeDatabases) {
		f.handlers["/ol/"] = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"docs": [`)) }
	}, func(t *testing.T, f *fakeDatabases, err error) {
		if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
			t.Fatal(err)
		}
	})
	run("server error", func(f *fakeDatabases) {
		f.handlers["/ol/"] = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }
	}, func(t *testing.T, f *fakeDatabases, err error) {
		if err == nil || !strings.Contains(err.Error(), "status 500") {
			t.Fatal(err)
		}
	})
	run("redirect refused", func(f *fakeDatabases) {
		f.handlers["/ol/"] = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere/leak", http.StatusFound)
		}
	}, func(t *testing.T, f *fakeDatabases, err error) {
		if err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Fatal(err)
		}
		for _, r := range f.requests {
			if strings.HasPrefix(r.URL.Path, "/elsewhere") {
				t.Fatalf("followed the redirect to %s", r.URL)
			}
		}
	})
	run("failure does not hide a result", func(f *fakeDatabases) {
		f.handlers["/ol/"] = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }
		f.handlers["/gb/books/v1/volumes"] = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"items": []map[string]any{{"volumeInfo": map[string]any{"title": "x", "pageCount": 12}}}})
		}
	}, func(t *testing.T, f *fakeDatabases, err error) {
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestLookupPicksTheDominantWorkAndSkipsAdaptations(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	docs := func(editions ...int) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			list := []map[string]any{{"key": "/works/OL9W", "title": "Wind Song", "author_name": []string{"Pat Writer", "Ada Lee"}, "edition_count": 900}}
			for i, n := range editions {
				list = append(list, map[string]any{"key": fmt.Sprintf("/works/OL%dW", i+1), "title": "Wind Song", "author_name": []string{"Ada Lee"}, "edition_count": n, "number_of_pages_median": 320 + i})
			}
			writeJSON(w, map[string]any{"docs": list})
		}
	}
	q := LookupQuery{Title: "Wind Song", Author: "Ada Lee"}
	f.handlers["/ol/search.json"] = docs(482, 2)
	if d, err := f.finder(LookupConfig{}).Find(context.Background(), q); err != nil || *d.Pages != 320 {
		t.Fatalf("%+v %v", d, err)
	}
	// Without a clear favorite the lookup refuses to guess.
	f.handlers["/ol/search.json"] = docs(40, 30)
	f.handlers["/gb/books/v1/volumes"] = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) }
	if _, err := f.finder(LookupConfig{}).Find(context.Background(), q); !errors.Is(err, errLookupAmbiguous) {
		t.Fatal(err)
	}
}

func TestLookupRequestsCarryOnlyTitleAuthorAndISBN(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	f.handlers["/idb/book/"] = func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	for _, q := range []LookupQuery{{Title: "Wind Song", Author: "Ada Lee", ISBN: "9780000000002"}, {Title: "Wind Song", Author: "Ada Lee"}} {
		if _, err := f.finder(LookupConfig{GoogleBooksKey: "gkey", ISBNdbKey: "ikey"}).Find(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	allowed := map[string]bool{"bibkeys": true, "format": true, "jscmd": true, "title": true, "author": true, "limit": true, "fields": true, "q": true, "maxResults": true, "printType": true, "key": true}
	for _, r := range f.requests {
		if !strings.HasPrefix(r.UserAgent(), "NextChapter/") {
			t.Errorf("User-Agent %q", r.UserAgent())
		}
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		for key := range r.URL.Query() {
			if !allowed[key] {
				t.Errorf("unexpected parameter %q in %s", key, r.URL)
			}
		}
		for name := range r.Header {
			switch http.CanonicalHeaderKey(name) {
			case "User-Agent", "Accept", "Accept-Encoding", "Authorization":
			default:
				t.Errorf("unexpected header %s", name)
			}
		}
		if len(r.Cookies()) > 0 {
			t.Error("cookies sent")
		}
	}
}

func TestCleanTextIsPlainAndCapped(t *testing.T) {
	t.Parallel()
	got := cleanText("<p>Hello <a href=\"x\">there</a></p><img src=x onerror=alert(1)>&lt;script&gt;bad&lt;/script&gt; \x00ok ", 2000)
	if strings.ContainsAny(got, "<>") && !strings.Contains(got, "bad") {
		t.Fatalf("markup kept: %q", got)
	}
	if strings.Contains(got, "<") || strings.Contains(got, "\x00") || strings.Contains(got, "onerror") {
		t.Fatalf("unsafe text kept: %q", got)
	}
	if long := cleanText(strings.Repeat("é", 5000), maxDescription); len([]rune(long)) != maxDescription {
		t.Fatalf("%d characters", len([]rune(long)))
	}
}

// --- through the app ---

func lookupApp(t *testing.T, f *fakeDatabases, on bool) (*App, *browser) {
	t.Helper()
	a := testApp(t, false)
	a.finder = f.finder(LookupConfig{})
	ensurePreferences(t, a, Preferences{LibraryWeight: 1, Length: "any", BookLookup: on})
	return a, signIn(t, a, "198.51.100.9:5000")
}

func windSong() Record {
	return Record{Book: Book{Title: "Wind Song", Author: "Ada Lee", ISBN: "9780000000002", Status: "to-read", Source: "manual", Interest: 3}}
}

func TestLookupOffMeansNoRequests(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	a, b := lookupApp(t, f, false)
	addRecords(t, a, []Record{windSong()})
	id := bookIDs(t, a)["Wind Song"]
	rr := b.request("POST", fmt.Sprintf("/books/%d/lookup", id), nil)
	expect(t, rr, 400)
	if !strings.Contains(rr.Body.String(), "Book lookup is off") {
		t.Fatal(rr.Body)
	}
	view := b.request("GET", fmt.Sprintf("/books/%d", id), nil)
	if strings.Contains(view.Body.String(), "Look up details") || !strings.Contains(view.Body.String(), "Book lookup is off") {
		t.Fatal("the lookup button shows while lookup is off")
	}
	var plan string
	if err := a.store.Write(func(tx *sql.Tx) error {
		var err error
		plan, err = savePlan(tx, "import", Plan{Records: []Record{windSong()}, Source: "goodreads"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", "/plans/"+plan+"/lookup", nil), 400)
	if n := f.count(); n != 0 {
		t.Fatalf("%d outbound requests with lookup off", n)
	}
	// Nothing happens on a page load or an import preview either.
	b.request("GET", "/plans/"+plan, nil)
	b.request("GET", "/", nil)
	if n := f.count(); n != 0 {
		t.Fatalf("%d outbound requests from page loads", n)
	}
}

func TestPreferencesBookLookupSwitch(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	b := signIn(t, a, "198.51.100.9:5000")
	if p, _ := readMeta(a.store.DB, "preferences", defaultPreferences); p.BookLookup {
		t.Fatal("book lookup is on by default")
	}
	expect(t, b.request("POST", "/settings", url.Values{"section": {"privacy"}, "book_lookup": {"on"}}), 303)
	if p, _ := readMeta(a.store.DB, "preferences", defaultPreferences); !p.BookLookup {
		t.Fatal("switch not saved")
	}
	b.request("GET", "/settings", nil)
	expect(t, b.request("POST", "/settings", url.Values{"section": {"ranking"}, "library_weight": {"1"}, "length": {"any"}}), 303)
	if p, _ := readMeta(a.store.DB, "preferences", defaultPreferences); !p.BookLookup {
		t.Fatal("saving ranking turned lookup off")
	}
	b.request("GET", "/settings", nil)
	expect(t, b.request("POST", "/settings", url.Values{"section": {"privacy"}}), 303)
	if p, _ := readMeta(a.store.DB, "preferences", defaultPreferences); p.BookLookup {
		t.Fatal("switch not cleared")
	}
}

func bookByTitle(t *testing.T, a *App, title string) Book {
	t.Helper()
	all, err := loadBooks(a.store.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.Title == title {
			return b
		}
	}
	t.Fatalf("no book %q", title)
	return Book{}
}

func applyLookup(t *testing.T, b *browser, id int64, take ...string) {
	t.Helper()
	b.request("GET", "/", nil) // pick up the current revision
	rr := b.request("POST", fmt.Sprintf("/books/%d/lookup", id), nil)
	expect(t, rr, 303)
	loc := rr.Header().Get("Location")
	page := b.request("GET", loc, nil)
	expect(t, page, 200)
	if !strings.Contains(page.Body.String(), "Sent: this book’s title, author, and ISBN") {
		t.Fatalf("the page does not say what was sent: %s", page.Body)
	}
	expect(t, b.request("POST", loc, url.Values{"take": take}), 303)
}

func TestLookupFillsOnlyEmptyFields(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	a, b := lookupApp(t, f, true)
	typedPages := 111
	typed := windSong()
	typed.Description, typed.Pages, typed.Tags = "My own words.", &typedPages, "keeper"
	addRecords(t, a, []Record{typed})
	id := bookByTitle(t, a, "Wind Song").ID
	applyLookup(t, b, id, "description", "pages", "published_year", "series", "tags")
	got := bookByTitle(t, a, "Wind Song")
	if got.Description != "My own words." || got.DescriptionSource != "" {
		t.Fatalf("typed description replaced: %q from %q", got.Description, got.DescriptionSource)
	}
	if got.Pages == nil || *got.Pages != typedPages || got.Tags != "keeper" {
		t.Fatalf("typed pages or tags replaced: %v %q", got.Pages, got.Tags)
	}
	if got.PublishedYear == nil || *got.PublishedYear != 2011 || got.Series != "Wind Cycle" {
		t.Fatalf("empty fields not filled: %v %q", got.PublishedYear, got.Series)
	}
	// A second book with nothing gets everything, with its source.
	second := windSong()
	second.Title, second.ISBN = "Wind Song Again", "9780000000019"
	addRecords(t, a, []Record{second})
	id2 := bookByTitle(t, a, "Wind Song Again").ID
	applyLookup(t, b, id2, "description", "pages", "published_year", "series", "tags")
	got = bookByTitle(t, a, "Wind Song Again")
	if got.Description != "A storm & a song.\n\nSecond paragraph." || got.DescriptionSource != "Open Library" || got.Pages == nil || *got.Pages != 320 || got.Tags != "sailing, sea stories" {
		t.Fatalf("%+v", got)
	}
	// Unchecked fields stay empty, and the book page escapes the text.
	third := windSong()
	third.Title, third.ISBN = "Third", "9780000000026"
	addRecords(t, a, []Record{third})
	applyLookup(t, b, bookByTitle(t, a, "Third").ID, "pages")
	if got = bookByTitle(t, a, "Third"); got.Description != "" || got.Pages == nil {
		t.Fatalf("%+v", got)
	}
}

func TestLookupPageOffersOnlyEmptyFields(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	a, b := lookupApp(t, f, true)
	r := windSong()
	r.Description = "Mine."
	addRecords(t, a, []Record{r})
	id := bookByTitle(t, a, "Wind Song").ID
	rr := b.request("POST", fmt.Sprintf("/books/%d/lookup", id), nil)
	body := b.request("GET", rr.Header().Get("Location"), nil).Body.String()
	if strings.Contains(body, `value="description"`) || !strings.Contains(body, "Keeps your value") {
		t.Fatalf("a filled description is offered for replacement: %s", body)
	}
	if !strings.Contains(body, `value="pages" checked`) {
		t.Fatalf("an empty field is not pre-selected: %s", body)
	}
	// Posting the description anyway still cannot replace it.
	expect(t, b.request("POST", rr.Header().Get("Location"), url.Values{"take": {"description"}}), 303)
	if got := bookByTitle(t, a, "Wind Song"); got.Description != "Mine." {
		t.Fatalf("%q", got.Description)
	}
}

func TestLookupFailureShowsMessageAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	a, b := lookupApp(t, f, true)
	addRecords(t, a, []Record{windSong()})
	id := bookByTitle(t, a, "Wind Song").ID
	rev, _ := revision(a.store.DB)
	rr := b.request("POST", fmt.Sprintf("/books/%d/lookup", id), nil)
	// Not finding a book is an answer, not a server error: it is not logged as one.
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "mention reference") {
		t.Fatal("not found is shown as a server error")
	}
	if !strings.Contains(rr.Body.String(), "No book database had this book") {
		t.Fatal(rr.Body)
	}
	if now, _ := revision(a.store.DB); now != rev {
		t.Fatal("a failed lookup changed the library")
	}
}

func TestLookupNeverReordersOrChangesRanking(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	a, b := lookupApp(t, f, true)
	addRecords(t, a, append(records("First", "Second"), windSong(), records("Fourth")[0]))
	queue := func() []Book {
		q, err := loadQueue(a.store.DB)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	ranking := func() string {
		rows, err := Propose(queue(), defaultPreferences, 0, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(rows)
		return string(raw)
	}
	beforeRank, beforeIDs := ranking(), queueIDs(queue())
	standaloneStrip := func() { // the fixture's series would be a ranking input
		f.handlers["/ol/works/OL1W.json"] = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"description": "A song.", "subjects": []string{"Sailing"}})
		}
	}
	standaloneStrip()
	applyLookup(t, b, bookByTitle(t, a, "Wind Song").ID, "description", "published_year", "tags")
	if got := ranking(); got != beforeRank {
		t.Fatalf("ranking changed after a lookup:\n%s\n%s", beforeRank, got)
	}
	if got := queueIDs(queue()); !slices.Equal(got, beforeIDs) {
		t.Fatalf("order changed: %v then %v", beforeIDs, got)
	}
}

func TestImportLookupIsOptInIdempotentAndTransactional(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	a, b := lookupApp(t, f, true)
	// One book already here with a description it must keep.
	kept := Record{Book: Book{Title: "Kept", Author: "Someone", Status: "to-read", Source: "manual", Interest: 3, Description: "Reader's own."}}
	addRecords(t, a, []Record{kept})
	f.handlers["/ol/search.json"] = func(w http.ResponseWriter, r *http.Request) {
		title := r.URL.Query().Get("title")
		if title != "Wind Song" && title != "Kept" {
			writeJSON(w, map[string]any{"docs": []any{}})
			return
		}
		writeJSON(w, map[string]any{"docs": []map[string]any{{"key": "/works/OL1W", "title": title, "author_name": []string{r.URL.Query().Get("author")}, "first_publish_year": 2011, "number_of_pages_median": 320}}})
	}
	f.handlers["/gb/books/v1/volumes"] = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) }
	rows := []Record{
		{Book: Book{Title: "Wind Song", Author: "Ada Lee", Status: "to-read", Source: "goodreads", Interest: 3, NeedsPlacement: true}},
		{Book: Book{Title: "Unknown Book", Author: "Nobody", Status: "to-read", Source: "goodreads", Interest: 3, NeedsPlacement: true}},
		{Book: Book{Title: "Kept", Author: "Someone", Status: "to-read", Source: "goodreads", Interest: 3, NeedsPlacement: true}},
	}
	var plan string
	if err := a.store.Write(func(tx *sql.Tx) error {
		var err error
		plan, err = savePlan(tx, "import", Plan{Records: rows, Source: "goodreads"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	preview := b.request("GET", "/plans/"+plan, nil).Body.String()
	if !strings.Contains(preview, "Look up details for these 3 books") || !strings.Contains(preview, "title, author, and ISBN") {
		t.Fatalf("no opt-in step: %s", preview)
	}
	if n := f.count(); n != 0 {
		t.Fatalf("%d requests before the reader asked", n)
	}
	expect(t, b.request("POST", "/plans/"+plan+"/lookup", nil), 303)
	after := f.count()
	if after == 0 {
		t.Fatal("no lookup happened")
	}
	if p := b.request("GET", "/plans/"+plan, nil).Body.String(); !strings.Contains(p, "Looked up 3 books; 2 had details") {
		t.Fatalf("summary missing: %s", p)
	}
	// A second click has nothing left to look up.
	expect(t, b.request("POST", "/plans/"+plan+"/lookup", nil), 303)
	if f.count() != after {
		t.Fatalf("a second lookup sent %d more requests", f.count()-after)
	}
	_, stored, _, err := readPlan(a.store.DB, plan)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Records[0].Pages == nil || *stored.Records[0].Pages != 320 || stored.Records[0].DescriptionSource == "" || !stored.Records[0].LookedUp {
		t.Fatalf("%+v", stored.Records[0])
	}
	// Nothing reaches the library until the reader imports.
	if all, _ := loadBooks(a.store.DB); len(all) != 1 {
		t.Fatalf("%d books before import", len(all))
	}
	expect(t, b.request("POST", "/plans/"+plan, nil), 303)
	song := bookByTitle(t, a, "Wind Song")
	if song.Description == "" || song.Pages == nil || *song.Pages != 320 {
		t.Fatalf("%+v", song)
	}
	if got := bookByTitle(t, a, "Kept"); got.Description != "Reader's own." || got.Pages == nil {
		t.Fatalf("existing book: %+v", got)
	}
	// Importing the same looked-up records again changes nothing.
	for i := range stored.Records {
		stored.Records[i].LookedUp = true
	}
	res := importOnce(t, a, stored.Records)
	if res.Changed || res.Added != 0 {
		t.Fatalf("a second import changed the library: %+v", res)
	}
}

func TestLookupBatchIsLimited(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.handlers["/ol/search.json"] = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"docs": []any{}}) }
	a, b := lookupApp(t, f, true)
	rows := []Record{}
	for i := range lookupBatchLimit + 5 {
		rows = append(rows, Record{Book: Book{Title: fmt.Sprint("Title ", i), Author: "Author", Status: "to-read", Source: "goodreads", Interest: 3}})
	}
	var plan string
	if err := a.store.Write(func(tx *sql.Tx) error {
		var err error
		plan, err = savePlan(tx, "import", Plan{Records: rows, Source: "goodreads"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, b.request("POST", "/plans/"+plan+"/lookup", nil), 303)
	_, p, _, _ := readPlan(a.store.DB, plan)
	if p.LookupAttempted != lookupBatchLimit {
		t.Fatalf("looked up %d books", p.LookupAttempted)
	}
	if !strings.Contains(b.request("GET", "/plans/"+plan, nil).Body.String(), "Look up details for these 5 books") {
		t.Fatal("the remaining books are not offered")
	}
}

func TestDescriptionsRoundTripAndOldExportsRestore(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	r := windSong()
	r.Description, r.DescriptionSource = "A song.", "Open Library"
	addRecords(t, a, []Record{r, records("Plain")[0]})
	raw := exportOf(t, a)
	f, problems, err := decodeExport(raw)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	target := testApp(t, false)
	if err = target.store.Write(func(tx *sql.Tx) error { _, err := restoreExport(tx, f); return err }); err != nil {
		t.Fatal(err)
	}
	if got := bookByTitle(t, target, "Wind Song"); got.Description != "A song." || got.DescriptionSource != "Open Library" {
		t.Fatalf("%+v", got)
	}
	// An export from before descriptions has neither key.
	old := editExport(t, raw, func(m map[string]any) {
		for _, b := range m["books"].([]any) {
			delete(b.(map[string]any), "description")
			delete(b.(map[string]any), "description_source")
		}
	})
	f, problems, err = decodeExport(old)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	older := testApp(t, false)
	if err = older.store.Write(func(tx *sql.Tx) error { _, err := restoreExport(tx, f); return err }); err != nil {
		t.Fatal(err)
	}
	if got := bookByTitle(t, older, "Wind Song"); got.Description != "" {
		t.Fatalf("%+v", got)
	}
	// Too long is refused.
	tooLong := editExport(t, raw, func(m map[string]any) {
		m["books"].([]any)[0].(map[string]any)["description"] = strings.Repeat("a", maxDescription+1)
	})
	if _, problems, _ = decodeExport(tooLong); len(problems) == 0 {
		t.Fatal("an oversized description was accepted")
	}
}

func TestMergeKeepsTargetDescriptionElseSources(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	withDesc := func(title, desc string) Record {
		return Record{Book: Book{Title: title, Author: "Author", Status: "to-read", Source: "manual", Interest: 3, Description: desc, DescriptionSource: "Open Library"}}
	}
	addRecords(t, a, []Record{withDesc("Target A", "target text"), withDesc("Source A", "source text"), withDesc("Target B", ""), withDesc("Source B", "source text")})
	ids := bookIDs(t, a)
	for _, pair := range [][2]string{{"Source A", "Target A"}, {"Source B", "Target B"}} {
		if err := a.store.Write(func(tx *sql.Tx) error { _, err := mergeBooks(tx, ids[pair[0]], ids[pair[1]], true); return err }); err != nil {
			t.Fatal(err)
		}
	}
	if got := bookByTitle(t, a, "Target A"); got.Description != "target text" {
		t.Fatalf("%q", got.Description)
	}
	if got := bookByTitle(t, a, "Target B"); got.Description != "source text" || got.DescriptionSource != "Open Library" {
		t.Fatalf("%+v", got)
	}
}

func TestMigrateDescriptionsFromVersionNine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:9]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	err = s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO books(id,title,author,identity,reading_status,position,added_at,source) VALUES (1,'Old','Writer','old|writer','to-read',0,'2026-10-01T00:00:00.000000000+00:00','manual')`)
		return err
	})
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 10 {
		t.Fatal(v)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM books WHERE id=1 AND title='Old' AND description='' AND description_source=''"); n != 1 {
		t.Fatal("the existing book changed")
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v10.bak" {
		t.Fatalf("backups %v", b)
	}
	// The backup is the version-9 database: it has no description column.
	if scalar[int](t, path+".pre-v10.bak", "SELECT count(*) FROM pragma_table_info('books') WHERE name='description'") != 0 {
		t.Fatal("the backup is not the pre-migration database")
	}
}

func TestEstimateUsesDescriptionAndItInvalidatesSavedAIEstimate(t *testing.T) {
	t.Parallel()
	c := Book{ID: 1, Title: "Candidate", Author: "Writer"}
	history := []ratedBook{{ID: 2, Title: "Rated", Author: "Other", Rating: 4}}
	base := baselineEstimate(c, history)
	plain, _ := json.Marshal(buildEstimatePayload(c, history, base, false))
	if strings.Contains(string(plain), "description") {
		t.Fatalf("an empty description is sent: %s", plain)
	}
	fpPlain := estimateFingerprint(c, history)
	c.Description = strings.Repeat("d", 1500)
	payload := buildEstimatePayload(c, history, base, false)
	if n := len([]rune(payload.Candidate.Description)); n != 1000 {
		t.Fatalf("description cut to %d characters", n)
	}
	req, err := estimateRequest(payload)
	if err != nil || !strings.Contains(req.User, `"description"`) || !strings.HasPrefix(req.User, "<library_data>") {
		t.Fatalf("%v %s", err, req.User)
	}
	if !strings.Contains(req.System, "descriptions") {
		t.Fatal("the untrusted-data notice does not name descriptions")
	}
	fpWith := estimateFingerprint(c, history)
	if fpWith == fpPlain {
		t.Fatal("a new description did not change the fingerprint")
	}
	c.Description = "Another."
	if estimateFingerprint(c, history) == fpWith {
		t.Fatal("a changed description did not change the fingerprint")
	}
	// Only the candidate's description counts: a rated book's does not.
	rated := slices.Clone(history)
	if estimateFingerprint(c, rated) != estimateFingerprint(c, history) {
		t.Fatal("fingerprint is not stable")
	}

	// A saved AI estimate goes stale when the description is filled in.
	a := testApp(t, false)
	addRecords(t, a, []Record{{Book: Book{Title: "Wind Song", Author: "Ada Lee", Status: "to-read", Source: "manual", Interest: 3}}})
	id := bookByTitle(t, a, "Wind Song").ID
	before, err := loadEstimateInputs(a.store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	ai := 4.0
	if err = saveAIEstimate(a.store.DB, ratingEstimate{BookID: id, Fingerprint: before.Fingerprint, Baseline: 3, AI: &ai, Confidence: "low", Reasons: []string{"x"}, Model: "m", CreatedAt: canonicalTime(a.now())}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.DB.Exec("UPDATE books SET description='New blurb' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	after, err := loadEstimateInputs(a.store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := readEstimate(a.store.DB, id)
	if saved.Fingerprint == after.Fingerprint {
		t.Fatal("the saved estimate is still current after the description changed")
	}
}

func TestEditFormKeepsAndClearsDescriptionSource(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	b := signIn(t, a, "198.51.100.9:5000")
	r := windSong()
	r.Description, r.DescriptionSource = "From a database.", "Open Library"
	addRecords(t, a, []Record{r})
	id := bookByTitle(t, a, "Wind Song").ID
	form := func(desc string) url.Values {
		return url.Values{"title": {"Wind Song"}, "author": {"Ada Lee"}, "interest": {"3"}, "reading_status": {"to-read"}, "description": {desc}}
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/edit", id), form("From a database.")), 303)
	if got := bookByTitle(t, a, "Wind Song"); got.DescriptionSource != "Open Library" {
		t.Fatalf("an unchanged description lost its source: %+v", got)
	}
	b.request("GET", fmt.Sprintf("/books/%d/edit", id), nil)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/edit", id), form("Rewritten by me.")), 303)
	if got := bookByTitle(t, a, "Wind Song"); got.Description != "Rewritten by me." || got.DescriptionSource != "" {
		t.Fatalf("%+v", got)
	}
	page := b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String()
	if !strings.Contains(page, "Rewritten by me.") || !strings.Contains(page, "Written or imported by you") {
		t.Fatal("the Details block does not show the description and its source")
	}
	b.request("GET", fmt.Sprintf("/books/%d/edit", id), nil)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/edit", id), form("<script>x</script>")), 303)
	if page = b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String(); strings.Contains(page, "<script>x") {
		t.Fatal("description rendered unescaped")
	}
}

func TestLookupLogsOutcomeWithoutLibraryData(t *testing.T) {
	t.Parallel()
	f := standardFake(t)
	a, b := lookupApp(t, f, true)
	var logs bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&logs, nil))
	b.gateway = true
	addRecords(t, a, []Record{windSong(), {Book: Book{Title: "Secret Title", Author: "Private Author", Status: "to-read", Source: "manual", Interest: 3}}})
	ok, missing := bookByTitle(t, a, "Wind Song").ID, bookByTitle(t, a, "Secret Title").ID
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/lookup", ok), nil), 303)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/lookup", missing), nil), 404)
	limited := func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", http.StatusTooManyRequests) }
	clear(f.handlers)
	f.handlers["/ol/"], f.handlers["/gb/"] = limited, limited
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/lookup", missing), nil), 502)
	out := logs.String()
	for _, want := range []string{"outcome=found", "sources=\"Open Library,Google Books\"", "outcome=not_found", "level=WARN", "outcome=error", "status 429", fmt.Sprintf("book_id=%d", ok)} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"Secret Title", "Private Author", "Wind Song", "9780000000002"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains library data %q", secret)
		}
	}
	// Only the genuine upstream failure (the 502) is a server error.
	if n := strings.Count(out, "level=ERROR"); n != 1 {
		t.Errorf("%d error lines, want 1 (the failed request):\n%s", n, out)
	}
}
