package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func floatp(f float64) *float64 { return &f }

func TestBaselineEstimate(t *testing.T) {
	rated := func(id int64, rating int, author, tags, series string) ratedBook {
		return ratedBook{ID: id, Title: fmt.Sprint("Book ", id), Author: author, Tags: tags, Series: series, Rating: rating}
	}
	many := func(n, rating int, author, tags string, from int64) []ratedBook {
		out := []ratedBook{}
		for i := range n {
			out = append(out, rated(from+int64(i), rating, author, tags, ""))
		}
		return out
	}
	cases := []struct {
		name       string
		candidate  Book
		history    []ratedBook
		estimate   float64
		rated      int
		confidence string
		factor     string
	}{
		{"no ratings", Book{ID: 1, Author: "A"}, nil, 0, 0, "low", "no rated books yet"},
		{"average only, few ratings", Book{ID: 1, Author: "Nobody"}, []ratedBook{rated(2, 5, "X", "", ""), rated(3, 4, "Y", "", ""), rated(4, 3, "Z", "", "")}, 4.0, 3, "low", "None of your rated books share"},
		{"same author", Book{ID: 1, Author: "ursula k. le guin"}, append(many(5, 5, "Ursula K. Le Guin", "", 10), many(5, 3, "Other", "", 20)...), 4.6, 10, "high", "By ursula k. le guin: 5.0★ over 5 books"},
		{"shared tags", Book{ID: 1, Author: "New", Tags: "Fantasy, cozy"}, []ratedBook{rated(2, 2, "X", "fantasy", ""), rated(3, 2, "X", "FANTASY, dark", ""), rated(4, 5, "Y", "cozy, mystery", ""), rated(5, 5, "Z", "", "")}, 3.3, 4, "low", "Sharing tags cozy, Fantasy: 3.0★ over 3 books"},
		{"same series", Book{ID: 1, Author: "New", Series: "Earthsea"}, []ratedBook{rated(2, 5, "X", "", "earthsea"), rated(3, 5, "Y", "", "Earthsea "), rated(4, 1, "Z", "", "")}, 4.2, 3, "low", "Same series (Earthsea): 5.0★ over 2 books"},
		{"Goodreads is a weak prior", Book{ID: 1, Author: "New", AvgRating: floatp(4.5)}, []ratedBook{rated(2, 1, "X", "", "")}, 1.9, 1, "low", "Goodreads average 4.50★"},
		{"own rating excluded", Book{ID: 1, Author: "New"}, []ratedBook{rated(1, 5, "New", "", ""), rated(2, 2, "X", "", "")}, 2.0, 1, "low", "over 1 rated book"},
		{"enough ratings, little overlap", Book{ID: 1, Author: "New", Tags: "poetry"}, append(many(10, 4, "X", "", 10), rated(30, 2, "Y", "poetry", "")), 3.4, 11, "medium", "Sharing tags poetry: 2.0★ over 1 book"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := baselineEstimate(c.candidate, c.history)
			if got.Estimate != c.estimate || got.Rated != c.rated || got.Confidence != c.confidence {
				t.Fatalf("got %.2f from %d (%s), want %.2f from %d (%s); factors %q", got.Estimate, got.Rated, got.Confidence, c.estimate, c.rated, c.confidence, got.Factors)
			}
			if !slices.ContainsFunc(got.Factors, func(f string) bool { return strings.Contains(f, c.factor) }) {
				t.Fatalf("factors %q lack %q", got.Factors, c.factor)
			}
		})
	}
}

func TestSampleHistory(t *testing.T) {
	history := []ratedBook{}
	for i := range 100 {
		h := ratedBook{ID: int64(i + 1), Author: "Someone", Rating: 1 + i%5}
		if i%17 == 0 {
			h.Tags = "space"
		}
		history = append(history, h)
	}
	c := Book{ID: 500, Author: "New", Tags: "Space"}
	sample := sampleHistory(c, history)
	if len(sample) != estimateSampleSize {
		t.Fatalf("sampled %d books", len(sample))
	}
	counts := map[int]int{}
	ids := map[int64]bool{}
	for _, h := range sample {
		counts[h.Rating]++
		ids[h.ID] = true
	}
	if counts[5] < estimateEnds || counts[1] < estimateEnds {
		t.Fatalf("highest and lowest ratings missing: %v", counts)
	}
	for _, h := range history {
		if h.Tags != "" && !ids[h.ID] {
			t.Fatalf("book %d shares a tag but was not sampled", h.ID)
		}
	}
	if !reflect.DeepEqual(sample, sampleHistory(c, history)) {
		t.Fatal("sampling is not deterministic")
	}
	for i := 1; i < len(sample); i++ {
		if sample[i-1].Rating < sample[i].Rating {
			t.Fatal("sample is not sorted by rating")
		}
	}
}

// libraryFixture adds a queued candidate with an ISBN, Goodreads ID, and a
// library loan, plus finished books with ratings and reviews.
func libraryFixture(t *testing.T, a *App, finished int) int64 {
	t.Helper()
	candidate := Record{Book: Book{Title: "The Candidate", Author: "Le Guin", Status: "to-read", Source: "goodreads", Interest: 3, ISBN: "9780000000001", GoodreadsID: "gr-candidate-77", Tags: "fantasy", AvgRating: floatp(4.1)}}
	addRecords(t, a, []Record{candidate})
	addRecords(t, a, []Record{{Book: Book{Title: "The Candidate", Author: "Le Guin", Source: "libby", Status: "to-read"}, Event: &Event{Source: "libby", Format: "ebook", Status: "borrowed", Activity: "Borrowed", At: "2026-10-01T10:00:00Z", DueDate: "2026-10-20"}}})
	for i := range finished {
		r := Record{Book: Book{Title: fmt.Sprintf("Finished %02d", i), Author: []string{"Le Guin", "Other"}[i%2], Status: "read", Source: "goodreads", Interest: 3, ISBN: fmt.Sprintf("97800000001%02d", i), GoodreadsID: fmt.Sprintf("gr-finished-%d", i), Tags: []string{"fantasy", "history"}[i%2]}}
		addRecords(t, a, []Record{r})
	}
	ensurePreferences(t, a, Preferences{LibraryWeight: 1, Length: "any", KeepReviews: true})
	err := a.store.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO reading_log(book_id,finished_at,rating,source,review_chars,review,fingerprint)
SELECT id, '2025-0'||(1+id%9)||'-15', 1+id%5, 'manual', 19, 'PRIVATE REVIEW TEXT '||id, 'manual:'||id FROM books WHERE title LIKE 'Finished%'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := a.store.DB.QueryRow("SELECT id FROM books WHERE title='The Candidate'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func ensurePreferences(t *testing.T, a *App, p Preferences) {
	t.Helper()
	if err := a.store.Write(func(tx *sql.Tx) error { return setMeta(tx, "preferences", p) }); err != nil {
		t.Fatal(err)
	}
}

// sentData returns the JSON inside <library_data> of a captured request.
func sentData(t *testing.T, body []byte) (map[string]any, string) {
	t.Helper()
	var req struct {
		Messages []struct{ Content string } `json:"messages"`
		System   string                     `json:"system"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) != 1 {
		t.Fatalf("request %s: %v", body, err)
	}
	text := req.Messages[0].Content
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "<library_data>\n"), "\n</library_data>")
	var data map[string]any
	if err := json.Unmarshal([]byte(inner), &data); err != nil {
		t.Fatalf("library data %q: %v", inner, err)
	}
	return data, req.System
}

// keys collects every object key in v.
func keys(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			into[k] = true
			keys(child, into)
		}
	case []any:
		for _, child := range x {
			keys(child, into)
		}
	}
}

func TestEstimateRequestSendsOnlyWhitelistedFields(t *testing.T) {
	for _, reviews := range []bool{false, true} {
		t.Run(fmt.Sprint("reviews=", reviews), func(t *testing.T) {
			var captured []byte
			a, _ := aiApp(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
				captured, _ = io.ReadAll(r.Body)
				replyFor("anthropic", `{"estimate":4.26,"confidence":"medium","reasons":["You rate Le Guin highly."]}`)(w, r)
			})
			id := libraryFixture(t, a, 12)
			in, err := loadEstimateInputs(a.store.DB, id)
			if err != nil {
				t.Fatal(err)
			}
			if err = a.estimateAI(context.Background(), id, in.Fingerprint, reviews); err != nil {
				t.Fatal(err)
			}
			data, system := sentData(t, captured)
			if !strings.Contains(system, "untrusted data") {
				t.Fatal("system prompt lacks the untrusted-data notice")
			}
			found := map[string]bool{}
			keys(data, found)
			allowed := map[string]bool{"candidate": true, "stats": true, "rated_books": true, "title": true, "author": true, "tags": true, "pages": true, "series": true,
				"goodreads_average": true, "baseline_estimate": true, "average_rating": true, "rating_counts_1_to_5": true, "your_rating": true, "finished_year": true}
			if reviews {
				allowed["your_review"] = true
			}
			for k := range found {
				if !allowed[k] {
					t.Errorf("sent field %q", k)
				}
			}
			raw := string(captured)
			for _, forbidden := range []string{"9780000000001", "97800000001", "gr-candidate", "gr-finished", "libby", "borrowed", "2026-10-20", "ebook", testAIKey, "manual:"} {
				if strings.Contains(raw, forbidden) {
					t.Errorf("request contains %q", forbidden)
				}
			}
			if strings.Contains(raw, "PRIVATE REVIEW TEXT") != reviews {
				t.Errorf("review text sent = %v, want %v", !reviews, reviews)
			}
			saved, _ := readEstimate(a.store.DB, id)
			if saved == nil || saved.AI == nil || *saved.AI != 4.3 || saved.Confidence != "medium" || saved.Model != "test-model" || saved.Fingerprint != in.Fingerprint {
				t.Fatalf("saved %+v", saved)
			}
		})
	}
}

func TestEstimateRejectsBadAnswers(t *testing.T) {
	for _, answer := range []string{
		`{"estimate":5.5,"confidence":"high","reasons":["x"]}`,
		`{"estimate":0,"confidence":"high","reasons":["x"]}`,
		`{"estimate":4,"confidence":"certain","reasons":["x"]}`,
		`{"estimate":4,"confidence":"high","reasons":[]}`,
		`{"estimate":4,"confidence":"high","reasons":["a","b","c","d"]}`,
		`{"estimate":4,"confidence":"high","reasons":["` + strings.Repeat("x", 241) + `"]}`,
		`{"estimate":4,"confidence":"high","reasons":["x"],"apply":true}`,
		`{"estimate":"4","confidence":"high","reasons":["x"]}`,
		`Sure! {"estimate":4,"confidence":"high","reasons":["x"]}`,
	} {
		t.Run(answer[:min(40, len(answer))], func(t *testing.T) {
			a, _ := aiApp(t, "openai", replyFor("openai", answer))
			id := libraryFixture(t, a, 3)
			in, _ := loadEstimateInputs(a.store.DB, id)
			err := a.estimateAI(context.Background(), id, in.Fingerprint, false)
			var h httpError
			if !errors.As(err, &h) || h.Status != 502 || !strings.Contains(h.Message, "could not be used") {
				t.Fatalf("err = %v", err)
			}
			if saved, _ := readEstimate(a.store.DB, id); saved != nil {
				t.Fatalf("saved %+v from a bad answer", saved)
			}
		})
	}
}

func TestEstimateCacheAndStaleness(t *testing.T) {
	var calls atomic.Int32
	a, srv := aiApp(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		replyFor("anthropic", `{"estimate":3.8,"confidence":"low","reasons":["Few books like it."]}`)(w, r)
	})
	id := libraryFixture(t, a, 4)
	b := &browser{t: t, handler: a.Handler()}
	page := b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil).Body.String()
	if !strings.Contains(page, "What gets shared") || !strings.Contains(page, "Finished 01") || !strings.Contains(page, "Also send my review text") {
		t.Fatal("preview missing")
	}
	in, _ := loadEstimateInputs(a.store.DB, id)
	rev := b.rev
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate/ai", id), url.Values{"fingerprint": {in.Fingerprint}}), 303)
	if calls.Load() != 1 {
		t.Fatalf("%d calls", calls.Load())
	}
	// Viewing the cached estimate makes no request.
	a.ai = newAIClient(AIConfig{Provider: "anthropic", Key: testAIKey, Model: "m", BaseURL: srv.URL}, failTransport{t})
	page = b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil).Body.String()
	if !strings.Contains(page, "3.8★") || !strings.Contains(page, "Few books like it.") || strings.Contains(page, "may be out of date") {
		t.Fatal("cached AI estimate not shown")
	}
	if b.rev != rev {
		t.Fatal("estimating changed the library revision")
	}
	// A new rating elsewhere changes the inputs: the estimate is stale, and
	// the old preview can no longer be sent.
	if err := a.store.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE reading_log SET rating=CASE rating WHEN 5 THEN 1 ELSE rating+1 END WHERE id=(SELECT min(id) FROM reading_log WHERE rating IS NOT NULL)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	page = b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil).Body.String()
	if !strings.Contains(page, "may be out of date") || !strings.Contains(page, "Update the saved estimate") {
		t.Fatal("stale estimate not marked")
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate/ai", id), url.Values{"fingerprint": {in.Fingerprint}}), 409)
	// Refreshing the baseline under new inputs drops the old AI value.
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate", id), nil), 303)
	if saved, _ := readEstimate(a.store.DB, id); saved == nil || saved.AI != nil || saved.Model != "" {
		t.Fatalf("saved %+v", saved)
	}
}

func TestEstimateWithAIOff(t *testing.T) {
	a := testApp(t, true)
	cfg := AIConfig{Provider: "anthropic", Key: testAIKey, Model: "m", MaxMonthly: 100}
	a.config.AI, a.ai = cfg, newAIClient(cfg, failTransport{t})
	id := libraryFixture(t, a, 12)
	b := &browser{t: t, handler: a.Handler()}
	queue := b.request("GET", "/", nil).Body.String()
	if !strings.Contains(queue, "Predict my rating") {
		t.Fatal("queue lacks Predict my rating")
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate", id), nil), 303)
	page := b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil).Body.String()
	if strings.Contains(page, "What gets shared") || !strings.Contains(page, "Turn on AI assistance") || !strings.Contains(page, "Based on 12 rated books") {
		t.Fatal("AI-off estimate page is wrong")
	}
	in, _ := loadEstimateInputs(a.store.DB, id)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate/ai", id), url.Values{"fingerprint": {in.Fingerprint}}), 400)
	queue = b.request("GET", "/", nil).Body.String()
	if !strings.Contains(queue, fmt.Sprintf("estimated %.1f★", in.Baseline.Estimate)) {
		t.Fatal("queue does not show the saved estimate")
	}
	a.ai = nil
	page = b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil).Body.String()
	if !strings.Contains(page, "No AI provider is set up") {
		t.Fatal("no-provider estimate page is wrong")
	}
}

func TestEstimateAccuracyAndRatedBooks(t *testing.T) {
	a := testApp(t, true)
	id := libraryFixture(t, a, 12)
	b := &browser{t: t, handler: a.Handler()}
	b.request("GET", "/", nil)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate", id), nil), 303)
	saved, _ := readEstimate(a.store.DB, id)
	if saved == nil {
		t.Fatal("baseline not saved")
	}
	b.request("GET", fmt.Sprintf("/books/%d", id), nil)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/readings", id), url.Values{"finished_at": {"2026-10-01"}, "rating": {"2"}}), 303)
	acc, err := loadAccuracy(a.store.DB, 0)
	want := saved.Baseline - 2
	if err != nil || acc.Books != 1 || acc.AIBooks != 0 || acc.BaselineError < want-1e-9 || acc.BaselineError > want+1e-9 {
		t.Fatalf("accuracy %+v %v, want error %.2f", acc, err, want)
	}
	reading := b.request("GET", "/reading", nil).Body.String()
	if !strings.Contains(reading, "How accurate was I?") || !strings.Contains(reading, fmt.Sprintf("off by %.1f★", want)) {
		t.Fatal("reading history lacks the accuracy line")
	}
	book := b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String()
	if !strings.Contains(book, fmt.Sprintf("the estimate was %.1f★", saved.Baseline)) || !strings.Contains(book, "you gave it 2★") {
		t.Fatal("book page lacks estimate vs actual")
	}
	// A rated book gets no new estimate.
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate", id), nil), 400)
	// Deleting the book removes its estimate.
	b.request("GET", fmt.Sprintf("/books/%d/delete", id), nil)
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/delete", id), nil), 303)
	if saved, _ := readEstimate(a.store.DB, id); saved != nil {
		t.Fatal("estimate outlived its book")
	}
}

func TestEstimatesLeaveRankingUnchangedUntilUsed(t *testing.T) {
	today := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	queue := []Book{}
	for i := range 30 {
		b := Book{ID: int64(i + 1), Title: fmt.Sprint("Q", i), Interest: 1 + i%5, Pinned: i == 7, Tags: []string{"cozy", "dark", ""}[i%3]}
		if i%4 == 0 {
			b.Loans = []Loan{{Status: "borrowed", DueDate: "2026-10-16"}}
		}
		if i%6 == 1 {
			b.Series, b.SeriesNumber = "S", floatp(float64(30-i))
		}
		queue = append(queue, b)
	}
	prefs := Preferences{LibraryWeight: 1, Length: "any", Mood: "cozy"}
	encode := func(p Preferences, books []Book) string {
		rows, err := Propose(books, p, 0, today)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(rows)
		return string(raw)
	}
	before := encode(prefs, queue)
	on := prefs
	on.UseEstimates = true
	if got := encode(on, queue); got != before {
		t.Fatal("turning estimates on without any estimates changed the ranking")
	}
	with := slices.Clone(queue)
	for i := range with {
		if i%2 == 0 {
			with[i].Estimate = floatp(1 + float64(i%5))
		}
	}
	if got := encode(prefs, with); got != before {
		t.Fatal("estimates changed the ranking while the preference is off")
	}
	if got := encode(on, with); got == before || !strings.Contains(got, "Estimated 5.0★ for you") {
		t.Fatal("estimates had no effect when on")
	}
	s1, _ := score(Book{Interest: 3, Estimate: floatp(4.3)}, on, today)
	s2, _ := score(Book{Interest: 3}, on, today)
	if s1-s2 != 6.5 {
		t.Fatalf("bonus %v, want 6.5", s1-s2)
	}
	s3, _ := score(Book{Interest: 3, Estimate: floatp(1)}, on, today)
	if s2-s3 != 10 {
		t.Fatalf("penalty %v, want 10", s2-s3)
	}
}

func TestSuggestionsUseSavedEstimates(t *testing.T) {
	a := testApp(t, true)
	addRecords(t, a, records("First", "Second"))
	ensurePreferences(t, a, Preferences{LibraryWeight: 1, Length: "any", UseEstimates: true})
	err := a.store.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO rating_estimates(book_id,basis_fingerprint,baseline,ai,created_at) SELECT id,'x',2.0,4.8,'2026-01-01T00:00:00Z' FROM books WHERE title='Second'")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var plan string
	err = a.store.Write(func(tx *sql.Tx) error {
		var err error
		plan, err = makeSuggestion(tx, 0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, p, _, _ := readPlan(a.store.DB, plan)
	if p.Rows[0].Title != "Second" || !slices.Contains(p.Rows[0].Reasons, "Estimated 4.8★ for you") {
		t.Fatalf("rows %+v", p.Rows)
	}
}

// Migration 7 adds an empty estimates table to a version 6 library, keeping
// its books and readings, after a backup.
func TestMigrateRatingEstimatesFromVersionSix(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:6]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	err = s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO books(id,title,author,identity,reading_status,position,added_at,source) VALUES
  (1,'Kept','Writer','kept|writer','read',0,'2026-10-01T00:00:00.000000000+00:00','manual');
INSERT INTO reading_log(book_id,finished_at,rating,source,fingerprint) VALUES(1,'2026-09-01',4,'manual','manual:x');`)
		return err
	})
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateFile(path, migrations[:7]); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 7 {
		t.Fatal(v)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM books") + scalar[int](t, path, "SELECT count(*) FROM reading_log"); n != 2 {
		t.Fatal("lost data")
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM rating_estimates"); n != 0 {
		t.Fatal(n)
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v7.bak" {
		t.Fatalf("backups %v", b)
	}
}
