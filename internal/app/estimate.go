package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// An estimated rating guesses what the reader might give a book they have not
// rated yet, from their own rated history. The baseline is deterministic and
// needs no AI; an AI estimate is a second opinion the reader asks for, shown
// beside it and never replacing it.

// ratedBook is one rated book in the reader's history: its latest rated
// reading. Review is set only when review text is kept.
type ratedBook struct {
	ID                          int64
	Title, Author, Tags, Series string
	Pages                       *int
	Rating                      int
	Year                        string // finish year, or ""
	Review                      string
}

// estimateLowConfidence is how many rated books an estimate needs before it is
// more than a rough guess.
const estimateLowConfidence = 10

// ratingBaseline is the deterministic estimate and how it was reached.
type ratingBaseline struct {
	// Estimate is 1-5 with one decimal, or 0 with no rated books.
	Estimate   float64
	Rated      int
	Confidence string // low, medium, or high
	Factors    []string
}

// tagSet returns the folded, trimmed tags of a comma-separated list.
func tagSet(tags string) map[string]string {
	set := map[string]string{}
	for _, tag := range strings.Split(tags, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			if _, ok := set[foldKey(tag)]; !ok {
				set[foldKey(tag)] = tag
			}
		}
	}
	return set
}

func sameName(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && foldKey(a) == foldKey(b)
}

// baselineEstimate blends the reader's average rating with their averages for
// books sharing the candidate's author, series, or tags, each weighted by how
// many books it covers, and the Goodreads average as a weak prior:
//
//	(3×overall + Σ nᵢ×avgᵢ + 1×goodreads) / (3 + Σ nᵢ + 1)
//
// The candidate's own readings are never part of history.
func baselineEstimate(c Book, history []ratedBook) ratingBaseline {
	var b ratingBaseline
	sum := 0
	for _, h := range history {
		if h.ID != c.ID {
			sum += h.Rating
			b.Rated++
		}
	}
	if b.Rated == 0 {
		b.Confidence = "low"
		b.Factors = []string{"You have no rated books yet. Rate a few finished books in Reading history first."}
		return b
	}
	overall := float64(sum) / float64(b.Rated)
	b.Factors = append(b.Factors, fmt.Sprintf("Your average: %.1f★ over %d rated %s", overall, b.Rated, plural(b.Rated, "book", "books")))
	weight, total := 3.0, 3*overall
	candidateTags := tagSet(c.Tags)
	type group struct {
		label  string
		match  func(ratedBook) bool
		n, sum int
		shared map[string]string
	}
	groups := []*group{
		{label: "By " + strings.TrimSpace(c.Author), match: func(h ratedBook) bool { return sameName(h.Author, c.Author) }},
		{label: "Same series (" + strings.TrimSpace(c.Series) + ")", match: func(h ratedBook) bool { return sameName(h.Series, c.Series) }},
		{label: "Sharing tags", shared: map[string]string{}},
	}
	groups[2].match = func(h ratedBook) bool {
		found := false
		for key := range tagSet(h.Tags) {
			if tag, ok := candidateTags[key]; ok {
				groups[2].shared[key], found = tag, true
			}
		}
		return found
	}
	matched := 0
	for _, g := range groups {
		for _, h := range history {
			if h.ID != c.ID && g.match(h) {
				g.n++
				g.sum += h.Rating
			}
		}
		if g.n == 0 {
			continue
		}
		matched += g.n
		avg := float64(g.sum) / float64(g.n)
		weight += float64(g.n)
		total += float64(g.sum)
		label := g.label
		if g.shared != nil {
			keys := slices.Sorted(maps.Keys(g.shared))
			names := []string{}
			for _, k := range keys[:min(3, len(keys))] {
				names = append(names, g.shared[k])
			}
			label += " " + strings.Join(names, ", ")
		}
		b.Factors = append(b.Factors, fmt.Sprintf("%s: %.1f★ over %d %s", label, avg, g.n, plural(g.n, "book", "books")))
	}
	if matched == 0 {
		b.Factors = append(b.Factors, "None of your rated books share this author, series, or tags")
	}
	if c.AvgRating != nil {
		weight++
		total += *c.AvgRating
		b.Factors = append(b.Factors, fmt.Sprintf("Goodreads average %.2f★, counted lightly: it is other readers’ opinion", *c.AvgRating))
	}
	b.Estimate = math.Round(min(5, max(1, total/weight))*10) / 10
	switch {
	case b.Rated < estimateLowConfidence:
		b.Confidence = "low"
	case matched >= 5:
		b.Confidence = "high"
	default:
		b.Confidence = "medium"
	}
	return b
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// loadRatedBooks returns each rated book once, with its latest rated reading.
func loadRatedBooks(q queryer) ([]ratedBook, error) {
	rows, err := q.Query(`SELECT b.id,b.title,b.author,b.tags,b.series,b.pages,l.rating,COALESCE(substr(l.finished_at,1,4),''),COALESCE(l.review,'')
FROM reading_log l JOIN books b ON b.id=l.book_id
WHERE l.id=(SELECT x.id FROM reading_log x WHERE x.book_id=l.book_id AND x.rating IS NOT NULL ORDER BY x.finished_at IS NULL, x.finished_at DESC, x.id DESC LIMIT 1)
ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ratedBook{}
	for rows.Next() {
		var h ratedBook
		if err = rows.Scan(&h.ID, &h.Title, &h.Author, &h.Tags, &h.Series, &h.Pages, &h.Rating, &h.Year, &h.Review); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// estimateFingerprint identifies the inputs of an estimate: the candidate's
// fields and every rated book. A saved estimate with another fingerprint is
// stale. Review text is left out, so keeping reviews does not invalidate it.
func estimateFingerprint(c Book, history []ratedBook) string {
	type row struct {
		ID                          int64
		Title, Author, Tags, Series string
		Pages                       *int
		Rating                      int
		Year                        string
	}
	rows := []row{}
	for _, h := range history {
		if h.ID != c.ID {
			rows = append(rows, row{h.ID, h.Title, h.Author, h.Tags, h.Series, h.Pages, h.Rating, h.Year})
		}
	}
	raw, _ := json.Marshal(map[string]any{"title": c.Title, "author": c.Author, "tags": c.Tags, "series": c.Series, "pages": c.Pages, "avg": c.AvgRating, "history": rows})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ratingEstimate is a saved estimate.
type ratingEstimate struct {
	BookID      int64
	Fingerprint string
	Baseline    float64
	AI          *float64
	Confidence  string
	Reasons     []string
	Model       string
	CreatedAt   string
}

// Value is the estimate ranking uses: the AI's when there is one.
func (e ratingEstimate) Value() float64 {
	if e.AI != nil {
		return *e.AI
	}
	return e.Baseline
}

func readEstimate(q queryer, bookID int64) (*ratingEstimate, error) {
	var e ratingEstimate
	var reasons string
	err := q.QueryRow("SELECT book_id,basis_fingerprint,baseline,ai,COALESCE(confidence,''),reasons,model,created_at FROM rating_estimates WHERE book_id=?", bookID).
		Scan(&e.BookID, &e.Fingerprint, &e.Baseline, &e.AI, &e.Confidence, &reasons, &e.Model, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, json.Unmarshal([]byte(reasons), &e.Reasons)
}

// saveBaseline stores a baseline. A saved AI estimate is kept only while the
// inputs are the same; otherwise it described other history and is cleared.
func saveBaseline(q queryer, bookID int64, fingerprint string, baseline float64, now time.Time) error {
	_, err := q.Exec(`INSERT INTO rating_estimates(book_id,basis_fingerprint,baseline,created_at) VALUES(?,?,?,?)
ON CONFLICT(book_id) DO UPDATE SET baseline=excluded.baseline, created_at=CASE WHEN basis_fingerprint=excluded.basis_fingerprint THEN created_at ELSE excluded.created_at END,
 ai=CASE WHEN basis_fingerprint=excluded.basis_fingerprint THEN ai END, confidence=CASE WHEN basis_fingerprint=excluded.basis_fingerprint THEN confidence END,
 reasons=CASE WHEN basis_fingerprint=excluded.basis_fingerprint THEN reasons ELSE '[]' END, model=CASE WHEN basis_fingerprint=excluded.basis_fingerprint THEN model ELSE '' END,
 basis_fingerprint=excluded.basis_fingerprint`, bookID, fingerprint, baseline, canonicalTime(now))
	return err
}

func saveAIEstimate(q queryer, e ratingEstimate) error {
	reasons, err := json.Marshal(e.Reasons)
	if err != nil {
		return err
	}
	_, err = q.Exec(`INSERT INTO rating_estimates(book_id,basis_fingerprint,baseline,ai,confidence,reasons,model,created_at) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(book_id) DO UPDATE SET basis_fingerprint=excluded.basis_fingerprint, baseline=excluded.baseline, ai=excluded.ai, confidence=excluded.confidence,
 reasons=excluded.reasons, model=excluded.model, created_at=excluded.created_at`, e.BookID, e.Fingerprint, e.Baseline, e.AI, e.Confidence, string(reasons), e.Model, e.CreatedAt)
	return err
}

// attachEstimates fills Estimate on books with a saved estimate.
func attachEstimates(q queryer, books []Book) error {
	if len(books) == 0 {
		return nil
	}
	index := make(map[int64]int, len(books))
	ids := make([]int64, len(books))
	for i, b := range books {
		index[b.ID], ids[i] = i, b.ID
	}
	rows, err := q.Query("SELECT book_id,COALESCE(ai,baseline) FROM rating_estimates WHERE baseline IS NOT NULL AND book_id IN (SELECT value FROM json_each(?))", jsonIDs(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var v float64
		if err = rows.Scan(&id, &v); err != nil {
			return err
		}
		books[index[id]].Estimate = &v
	}
	return rows.Err()
}

// hasRating reports whether the book has a rated reading. Estimates are only
// for books the reader has not rated, so accuracy compares a guess made
// before the reader's own rating.
func hasRating(q queryer, bookID int64) (bool, error) {
	var n int
	err := q.QueryRow("SELECT count(*) FROM reading_log WHERE book_id=? AND rating IS NOT NULL", bookID).Scan(&n)
	return n > 0, err
}

// estimateAccuracy compares saved estimates with the ratings the reader gave
// afterwards, as mean absolute error in stars.
type estimateAccuracy struct {
	Books, AIBooks         int
	BaselineError, AIError float64
	BaselineHits, AIHits   int // within half a star
	BaselineValue, AIValue *float64
	Actual                 int // for one book: its rating
}

func loadAccuracy(q queryer, bookID int64) (estimateAccuracy, error) {
	var acc estimateAccuracy
	where, args := "", []any{}
	if bookID != 0 {
		where, args = " AND e.book_id=?", []any{bookID}
	}
	rows, err := q.Query(`SELECT e.baseline,e.ai,(SELECT x.rating FROM reading_log x WHERE x.book_id=e.book_id AND x.rating IS NOT NULL ORDER BY x.finished_at IS NULL, x.finished_at DESC, x.id DESC LIMIT 1) AS actual
FROM rating_estimates e WHERE e.baseline IS NOT NULL AND actual IS NOT NULL`+where+" ORDER BY e.book_id", args...)
	if err != nil {
		return acc, err
	}
	defer rows.Close()
	for rows.Next() {
		var baseline float64
		var ai *float64
		var actual int
		if err = rows.Scan(&baseline, &ai, &actual); err != nil {
			return acc, err
		}
		d := math.Abs(baseline - float64(actual))
		acc.Books++
		acc.BaselineError += d
		if d <= 0.5 {
			acc.BaselineHits++
		}
		if ai != nil {
			d = math.Abs(*ai - float64(actual))
			acc.AIBooks++
			acc.AIError += d
			if d <= 0.5 {
				acc.AIHits++
			}
		}
		acc.BaselineValue, acc.AIValue, acc.Actual = &baseline, ai, actual
	}
	if acc.Books > 0 {
		acc.BaselineError /= float64(acc.Books)
	}
	if acc.AIBooks > 0 {
		acc.AIError /= float64(acc.AIBooks)
	}
	return acc, rows.Err()
}

// The AI request sends a compact taste profile, never the whole library.
const (
	estimateSampleSize = 40
	estimateEnds       = 10 // highest- and lowest-rated books sent
	maxReviewSent      = 1000
	maxEstimateReason  = 240
)

type estimateRow struct {
	Title    string `json:"title"`
	Author   string `json:"author"`
	Tags     string `json:"tags,omitempty"`
	Pages    *int   `json:"pages,omitempty"`
	Series   string `json:"series,omitempty"`
	Rating   int    `json:"your_rating"`
	Finished string `json:"finished_year,omitempty"`
	Review   string `json:"your_review,omitempty"`
}
type estimateCandidate struct {
	Title     string   `json:"title"`
	Author    string   `json:"author"`
	Tags      string   `json:"tags,omitempty"`
	Pages     *int     `json:"pages,omitempty"`
	Series    string   `json:"series,omitempty"`
	Goodreads *float64 `json:"goodreads_average,omitempty"`
	Baseline  float64  `json:"baseline_estimate"`
}
type estimateStats struct {
	RatedBooks    int     `json:"rated_books"`
	AverageRating float64 `json:"average_rating"`
	// RatingCounts[i] is how many books were rated i+1 stars.
	RatingCounts [5]int `json:"rating_counts_1_to_5"`
}

// estimatePayload is exactly what the AI request sends; the preview shows it.
type estimatePayload struct {
	Candidate  estimateCandidate `json:"candidate"`
	Stats      estimateStats     `json:"stats"`
	RatedBooks []estimateRow     `json:"rated_books"`
}

// sampleHistory picks at most 40 rated books: the highest- and lowest-rated,
// then those sharing most tags with the candidate (then its author or series),
// in a deterministic order.
func sampleHistory(c Book, history []ratedBook) []ratedBook {
	pool := []ratedBook{}
	for _, h := range history {
		if h.ID != c.ID {
			pool = append(pool, h)
		}
	}
	if len(pool) <= estimateSampleSize {
		return sortSample(pool)
	}
	chosen := map[int64]bool{}
	pick := func(list []ratedBook, n int) {
		for _, h := range list {
			if n == 0 || len(chosen) >= estimateSampleSize {
				return
			}
			if !chosen[h.ID] {
				chosen[h.ID] = true
				n--
			}
		}
	}
	byRating := slices.Clone(pool)
	slices.SortStableFunc(byRating, func(a, b ratedBook) int { return cmp.Or(cmp.Compare(b.Rating, a.Rating), cmp.Compare(a.ID, b.ID)) })
	pick(byRating, estimateEnds)
	slices.SortStableFunc(byRating, func(a, b ratedBook) int { return cmp.Or(cmp.Compare(a.Rating, b.Rating), cmp.Compare(a.ID, b.ID)) })
	pick(byRating, estimateEnds)
	tags := tagSet(c.Tags)
	closeness := func(h ratedBook) int {
		n := 0
		for key := range tagSet(h.Tags) {
			if _, ok := tags[key]; ok {
				n += 2
			}
		}
		if sameName(h.Author, c.Author) {
			n += 3
		}
		if sameName(h.Series, c.Series) {
			n += 3
		}
		return n
	}
	near := slices.Clone(pool)
	slices.SortStableFunc(near, func(a, b ratedBook) int {
		return cmp.Or(cmp.Compare(closeness(b), closeness(a)), cmp.Compare(a.ID, b.ID))
	})
	pick(near, estimateSampleSize)
	sample := []ratedBook{}
	for _, h := range pool {
		if chosen[h.ID] {
			sample = append(sample, h)
		}
	}
	return sortSample(sample)
}
func sortSample(s []ratedBook) []ratedBook {
	slices.SortStableFunc(s, func(a, b ratedBook) int { return cmp.Or(cmp.Compare(b.Rating, a.Rating), cmp.Compare(a.ID, b.ID)) })
	return s
}

// buildEstimatePayload assembles the request. Reviews are included only when
// asked for, and only those already kept, cut to 1,000 characters.
func buildEstimatePayload(c Book, history []ratedBook, base ratingBaseline, reviews bool) estimatePayload {
	p := estimatePayload{Candidate: estimateCandidate{c.Title, c.Author, c.Tags, c.Pages, c.Series, c.AvgRating, base.Estimate}, RatedBooks: []estimateRow{}}
	sum := 0
	for _, h := range history {
		if h.ID != c.ID {
			p.Stats.RatedBooks++
			p.Stats.RatingCounts[h.Rating-1]++
			sum += h.Rating
		}
	}
	if p.Stats.RatedBooks > 0 {
		p.Stats.AverageRating = math.Round(float64(sum)/float64(p.Stats.RatedBooks)*100) / 100
	}
	for _, h := range sampleHistory(c, history) {
		row := estimateRow{Title: h.Title, Author: h.Author, Tags: h.Tags, Pages: h.Pages, Series: h.Series, Rating: h.Rating, Finished: h.Year}
		if reviews {
			row.Review = truncate(h.Review, maxReviewSent)
		}
		p.RatedBooks = append(p.RatedBooks, row)
	}
	return p
}

const estimateTask = `You estimate the star rating (1 to 5) a reader would probably give a book they have not read yet, from their own ratings of books they finished. The data has the candidate book, the reader's rating statistics, a sample of their rated books, and a baseline estimate computed from their averages for the same author, series, and tags. Adjust the baseline only when the sample gives a reason. The Goodreads average is other readers' opinion and only a weak signal. Answer with "estimate" (a number from 1 to 5, one decimal), "confidence" ("low", "medium", or "high"; low when the history says little about this kind of book), and "reasons": one to three short plain-language sentences addressed to the reader as "you", each under 200 characters.`

const estimateSchema = `{"type":"object","properties":{"estimate":{"type":"number"},"confidence":{"type":"string","enum":["low","medium","high"]},"reasons":{"type":"array","items":{"type":"string"}}},"required":["estimate","confidence","reasons"],"additionalProperties":false}`

// estimateLocalSchema adds length limits for self-hosted servers, which
// enforce them while generating (see AIRequest.LocalSchema).
const estimateLocalSchema = `{"type":"object","properties":{"estimate":{"type":"number"},"confidence":{"type":"string","enum":["low","medium","high"]},"reasons":{"type":"array","items":{"type":"string","maxLength":240},"minItems":1,"maxItems":3}},"required":["estimate","confidence","reasons"],"additionalProperties":false}`

type estimateAnswer struct {
	Estimate   float64  `json:"estimate"`
	Confidence string   `json:"confidence"`
	Reasons    []string `json:"reasons"`
}

// checkEstimateAnswer validates a decoded answer and rounds the estimate.
func checkEstimateAnswer(a *estimateAnswer) error {
	if math.IsNaN(a.Estimate) || a.Estimate < 1 || a.Estimate > 5 {
		return fmt.Errorf("the estimate %v is not between 1 and 5", a.Estimate)
	}
	if !in(a.Confidence, []string{"low", "medium", "high"}) {
		return fmt.Errorf("the confidence %q is not low, medium, or high", a.Confidence)
	}
	if len(a.Reasons) < 1 || len(a.Reasons) > 3 {
		return fmt.Errorf("it gave %d reasons instead of one to three", len(a.Reasons))
	}
	for i, r := range a.Reasons {
		r = strings.TrimSpace(r)
		if r == "" || len([]rune(r)) > maxEstimateReason {
			return fmt.Errorf("a reason was empty or longer than %d characters", maxEstimateReason)
		}
		a.Reasons[i] = r
	}
	a.Estimate = math.Round(a.Estimate*10) / 10
	return nil
}

// estimateInputs reads everything an estimate needs in one short read.
type estimateInputs struct {
	Book        Book
	History     []ratedBook
	Baseline    ratingBaseline
	Fingerprint string
	Rated       bool // the book itself has a rating already
	KeepReviews bool
}

func loadEstimateInputs(q queryer, bookID int64) (estimateInputs, error) {
	var ei estimateInputs
	var err error
	if ei.Book, err = getBook(q, bookID); err != nil {
		return ei, err
	}
	if ei.Rated, err = hasRating(q, bookID); err != nil {
		return ei, err
	}
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return ei, err
	}
	ei.KeepReviews = prefs.KeepReviews
	if ei.History, err = loadRatedBooks(q); err != nil {
		return ei, err
	}
	if !ei.KeepReviews {
		for i := range ei.History {
			ei.History[i].Review = ""
		}
	}
	ei.Baseline = baselineEstimate(ei.Book, ei.History)
	ei.Fingerprint = estimateFingerprint(ei.Book, ei.History)
	return ei, nil
}

// estimateRequest is the prompt for one estimate.
func estimateRequest(payload estimatePayload) (AIRequest, error) {
	req, err := libraryPrompt(estimateTask, payload, 4000, estimateSchema)
	req.LocalSchema = json.RawMessage(estimateLocalSchema)
	return req, err
}

// estimateAI asks the provider for its estimate of a book, given the
// fingerprint of the inputs the reader saw in the preview.
func (a *App) estimateAI(ctx context.Context, bookID int64, fingerprint string, reviews bool) error {
	_, send, err := a.prepareEstimateAI(bookID, fingerprint, reviews)
	if err != nil {
		return err
	}
	return send(ctx)
}

// prepareEstimateAI checks an estimate request against the preview and
// returns the book's title and a function that sends it, which may run in the
// background. The network call happens outside any transaction; the result is
// saved in a short write that first checks the inputs did not change
// meanwhile.
func (a *App) prepareEstimateAI(bookID int64, fingerprint string, reviews bool) (string, func(context.Context) error, error) {
	ei, err := loadEstimateInputs(a.store.DB, bookID)
	if err != nil {
		return "", nil, err
	}
	if ei.Rated {
		return "", nil, bad("You have already rated this book.")
	}
	if ei.Baseline.Rated == 0 {
		return "", nil, bad("Rate a few finished books first; an estimate needs your own ratings.")
	}
	if ei.Fingerprint != fingerprint {
		return "", nil, conflict("Your reading history or this book changed since the preview. Check what gets shared again.")
	}
	req, err := estimateRequest(buildEstimatePayload(ei.Book, ei.History, ei.Baseline, reviews && ei.KeepReviews))
	if err != nil {
		return "", nil, err
	}
	send := func(ctx context.Context) error {
		resp, err := a.askAI(ctx, req)
		if err != nil {
			return err
		}
		var answer estimateAnswer
		if err = decodeAnswer(resp.Text, &answer); err == nil {
			err = checkEstimateAnswer(&answer)
		}
		if err != nil {
			return aiRejected(err)
		}
		return a.store.Write(func(tx *sql.Tx) error {
			now, err := loadEstimateInputs(tx, bookID)
			if err != nil {
				return err
			}
			if now.Fingerprint != fingerprint {
				return conflict("Your reading history changed while the AI was answering, so its estimate was not saved. Try again.")
			}
			return saveAIEstimate(tx, ratingEstimate{BookID: bookID, Fingerprint: fingerprint, Baseline: ei.Baseline.Estimate, AI: &answer.Estimate,
				Confidence: answer.Confidence, Reasons: answer.Reasons, Model: a.config.AI.Model, CreatedAt: canonicalTime(a.now())})
		})
	}
	return ei.Book.Title, send, nil
}
