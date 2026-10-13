package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// An AI suggestion is a second plan beside the usual one. The model sees a
// bounded part of the queue under opaque ids and proposes an order (or, to
// place one book, a position); the app validates it, then the same
// constraint solver as the usual ranking enforces pins and series order. The
// reader previews both and applies one with the usual revision check and
// undo. Nothing is reordered automatically.

const (
	rerankTop       = 60 // highest-scoring books sent for a full reorder
	rerankPins      = 40 // pinned books sent as fixed points
	rerankRecent    = 5  // recently finished books sent for variety
	insertWindow    = 30 // books sent on each side of the usual slot
	maxRerankReason = 200
	maxIntent       = 300
)

type rerankBook struct {
	ID           int      `json:"id"`
	Title        string   `json:"title"`
	Author       string   `json:"author"`
	Tags         string   `json:"tags,omitempty"`
	Pages        *int     `json:"pages,omitempty"`
	Series       string   `json:"series,omitempty"`
	SeriesNumber *float64 `json:"series_number,omitempty"`
	Interest     int      `json:"interest"`
	Estimate     *float64 `json:"estimated_rating,omitempty"`
	Library      string   `json:"library,omitempty"`
	Pinned       bool     `json:"pinned,omitempty"`
	Position     int      `json:"current_position"`
}
type recentBook struct {
	Title    string `json:"title"`
	Author   string `json:"author"`
	Tags     string `json:"tags,omitempty"`
	Rating   *int   `json:"your_rating,omitempty"`
	Finished string `json:"finished,omitempty"` // YYYY-MM
}

// rerankPayload is exactly what the request sends; the preview shows it.
type rerankPayload struct {
	Task      string       `json:"task"` // "reorder" or "place"
	Note      string       `json:"reader_note,omitempty"`
	QueueSize int          `json:"queue_size"`
	Books     []rerankBook `json:"books"`
	Candidate *rerankBook  `json:"book_to_place,omitempty"`
	// Positions is the number of valid positions when placing one book.
	Positions int          `json:"positions,omitempty"`
	Recent    []recentBook `json:"recently_finished"`
}

// rerankInputs is everything one AI suggestion needs, read in one short
// transaction.
type rerankInputs struct {
	Kind    string
	Queue   []Book
	Scores  []float64
	Reasons [][]string
	// Sent are the queue indices of Books, by opaque id - 1.
	Sent []int
	// Candidate is the queue index of the book to place, or -1. Its valid
	// slots are [Lo, Hi]; the payload offers slots [WindowLo, WindowHi].
	Candidate                  int
	Lo, Hi, WindowLo, WindowHi int
	Payload                    rerankPayload
	Fingerprint                string
}

// libraryStatus describes a current loan without its source or account.
func libraryStatus(b Book, today time.Time) string {
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	best := ""
	for _, l := range b.Loans {
		var s string
		switch l.Status {
		case "ready":
			s = "ready for pickup"
		case "borrowed":
			s = "borrowed"
		default:
			continue
		}
		if due, err := time.Parse(time.DateOnly, l.DueDate); err == nil {
			switch days := int(due.Sub(today).Hours() / 24); {
			case days < 0:
				s += fmt.Sprintf(", overdue by %d %s", -days, plural(-days, "day", "days"))
			default:
				s += fmt.Sprintf(", due in %d %s", days, plural(days, "day", "days"))
			}
		}
		if best == "" || strings.HasPrefix(s, "ready") {
			best = s
		}
	}
	return best
}

func recentlyFinished(q queryer) ([]recentBook, error) {
	rows, err := q.Query("SELECT b.title,b.author,b.tags,l.rating,COALESCE(substr(l.finished_at,1,7),'') FROM reading_log l JOIN books b ON b.id=l.book_id"+readingOrder+" LIMIT ?", rerankRecent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []recentBook{}
	for rows.Next() {
		var r recentBook
		if err = rows.Scan(&r.Title, &r.Author, &r.Tags, &r.Rating, &r.Finished); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// loadRerankInputs builds the request for a reorder or insert plan.
func loadRerankInputs(q queryer, kind string, p Plan, today time.Time) (rerankInputs, error) {
	ri := rerankInputs{Kind: kind, Candidate: -1}
	var err error
	if ri.Queue, err = loadQueue(q); err != nil {
		return ri, err
	}
	if err = attachEstimates(q, ri.Queue); err != nil {
		return ri, err
	}
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return ri, err
	}
	ri.Scores, ri.Reasons = make([]float64, len(ri.Queue)), make([][]string, len(ri.Queue))
	for i, b := range ri.Queue {
		ri.Scores[i], ri.Reasons[i] = score(b, prefs, today)
	}
	recent, err := recentlyFinished(q)
	if err != nil {
		return ri, err
	}
	describe := func(id, i int) rerankBook {
		b := ri.Queue[i]
		return rerankBook{ID: id, Title: b.Title, Author: b.Author, Tags: b.Tags, Pages: b.Pages, Series: b.Series, SeriesNumber: b.SeriesNumber,
			Interest: b.Interest, Estimate: b.Estimate, Library: libraryStatus(b, today), Pinned: b.Pinned, Position: i + 1}
	}
	ri.Payload = rerankPayload{Task: "reorder", Note: p.Intent, QueueSize: len(ri.Queue), Books: []rerankBook{}, Recent: recent}
	if kind == "insert" {
		ri.Payload.Task = "place"
		if ri.Candidate, err = candidateIndex(ri.Queue, p.CandidateID); err != nil {
			return ri, bad(err.Error())
		}
		var ideal int
		if ri.Lo, ri.Hi, ideal, err = insertBounds(ri.Queue, ri.Scores, ri.Candidate); err != nil {
			return ri, bad(err.Error())
		}
		slot := min(max(ideal, ri.Lo), ri.Hi)
		ri.WindowLo = max(ri.Lo, slot-insertWindow)
		ri.WindowHi = min(ri.Hi, ri.WindowLo+2*insertWindow)
		ri.WindowLo = max(ri.Lo, ri.WindowHi-2*insertWindow)
		// Slot s inserts before rest[s]; rest skips the candidate.
		for s := ri.WindowLo; s <= ri.WindowHi && s < len(ri.Queue)-1; s++ {
			i := s
			if s >= ri.Candidate {
				i++
			}
			ri.Sent = append(ri.Sent, i)
			ri.Payload.Books = append(ri.Payload.Books, describe(len(ri.Sent), i))
		}
		c := describe(0, ri.Candidate)
		ri.Payload.Candidate, ri.Payload.Positions = &c, ri.WindowHi-ri.WindowLo+1
	} else {
		byScore := make([]int, len(ri.Queue))
		for i := range byScore {
			byScore[i] = i
		}
		slices.SortStableFunc(byScore, func(a, b int) int { return cmp.Compare(ri.Scores[b], ri.Scores[a]) })
		chosen := map[int]bool{}
		for _, i := range byScore[:min(rerankTop, len(byScore))] {
			chosen[i] = true
		}
		pins := 0
		for i, b := range ri.Queue {
			if b.Pinned && pins < rerankPins {
				chosen[i] = true
				pins++
			}
		}
		for i := range ri.Queue {
			if chosen[i] {
				ri.Sent = append(ri.Sent, i)
				ri.Payload.Books = append(ri.Payload.Books, describe(len(ri.Sent), i))
			}
		}
	}
	raw, err := json.Marshal(ri.Payload)
	if err != nil {
		return ri, err
	}
	sum := sha256.Sum256(raw)
	ri.Fingerprint = hex.EncodeToString(sum[:])
	return ri, nil
}

const reorderTask = `You suggest a reading order for one reader's queue. The data lists part of the queue (each book has an id, its current position, interest from 1 to 5, and sometimes an estimated rating and a library deadline), the books the reader finished most recently, and an optional note from the reader about what they want now. Order every listed book from what to read first to last, weighing the reader's note, variety after what they just finished, pacing between long and short or heavy and light books, interest, estimated rating, and library deadlines. The app keeps pinned books in place and series in order afterwards, so you need not. Answer with "order", every listed id exactly once, and "reasons", an array of {"id", "reason"} objects giving one short plain-language reason (under 160 characters, addressed to the reader as "you") for each book you moved noticeably; other books may have no reason.`

const reorderSchema = `{"type":"object","properties":{"order":{"type":"array","items":{"type":"integer"}},"reasons":{"type":"array","items":{"type":"object","properties":{"id":{"type":"integer"},"reason":{"type":"string"}},"required":["id","reason"],"additionalProperties":false}}},"required":["order","reasons"],"additionalProperties":false}`

const placeTask = `You suggest where one new book belongs in a reader's reading queue. The data has the book to place, a stretch of the queue in order (each book with an id, its current position, interest from 1 to 5, and sometimes an estimated rating and a library deadline), the books the reader finished most recently, and an optional note from the reader. Positions are numbered from 1 to "positions": position 1 puts the book before the first listed book, position n puts it before the n-th listed book, and position positions, when it is one more than the number of listed books, puts it after the last. Weigh the reader's note, variety, pacing, interest, estimated rating, and library deadlines. Answer with "position" and "reason", one short plain-language sentence under 160 characters addressed to the reader as "you".`

const placeSchema = `{"type":"object","properties":{"position":{"type":"integer"},"reason":{"type":"string"}},"required":["position","reason"],"additionalProperties":false}`

// request builds the prompt. Self-hosted servers get the schema with length
// limits that their decoding enforces (see AIRequest.LocalSchema): exactly
// one id per book sent, and short reasons.
func (ri rerankInputs) request() (AIRequest, error) {
	if ri.Kind == "insert" {
		req, err := libraryPrompt(placeTask, ri.Payload, 4000, placeSchema)
		req.LocalSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"position":{"type":"integer"},"reason":{"type":"string","maxLength":%d}},"required":["position","reason"],"additionalProperties":false}`, maxRerankReason))
		return req, err
	}
	req, err := libraryPrompt(reorderTask, ri.Payload, 16000, reorderSchema)
	n := len(ri.Sent)
	req.LocalSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"order":{"type":"array","items":{"type":"integer"},"minItems":%d,"maxItems":%d},"reasons":{"type":"array","items":{"type":"object","properties":{"id":{"type":"integer"},"reason":{"type":"string","maxLength":%d}},"required":["id","reason"],"additionalProperties":false},"maxItems":%d}},"required":["order","reasons"],"additionalProperties":false}`, n, n, maxRerankReason, n))
	return req, err
}

// checkReason trims a reason and caps its length. Reasons are optional, and
// small local models sometimes send empty ones, so a blank reason just means
// none.
func checkReason(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > maxRerankReason {
		return "", fmt.Errorf("a reason was longer than %d characters", maxRerankReason)
	}
	return s, nil
}

// aiRows validates the model's answer and turns it into plan rows. The order
// is only a preference: rankOrder enforces pins and series, and books moved
// from the model's order to satisfy them are marked Repaired.
func (ri rerankInputs) aiRows(text string) ([]Suggestion, error) {
	sent := make(map[int]bool, len(ri.Sent))
	for id := range ri.Sent {
		sent[id+1] = true
	}
	why := map[int]string{} // by queue index
	var order []int
	if ri.Kind == "insert" {
		var answer struct {
			Position int    `json:"position"`
			Reason   string `json:"reason"`
		}
		if err := decodeAnswer(text, &answer); err != nil {
			return nil, err
		}
		if answer.Position < 1 || answer.Position > ri.Payload.Positions {
			return nil, fmt.Errorf("position %d is not between 1 and %d", answer.Position, ri.Payload.Positions)
		}
		slot := ri.WindowLo + answer.Position - 1
		if slot < ri.Lo || slot > ri.Hi {
			return nil, fmt.Errorf("position %d would break a pin or series order", answer.Position)
		}
		reason, err := checkReason(answer.Reason)
		if err != nil {
			return nil, err
		}
		why[ri.Candidate] = reason
		order = insertAt(ri.Queue, ri.Candidate, slot)
	} else {
		var answer struct {
			Order   []int `json:"order"`
			Reasons []struct {
				ID     int    `json:"id"`
				Reason string `json:"reason"`
			} `json:"reasons"`
		}
		if err := decodeAnswer(text, &answer); err != nil {
			return nil, err
		}
		if err := checkIDs(answer.Order, sent, true); err != nil {
			return nil, err
		}
		ids := make([]int, len(answer.Reasons))
		for i, r := range answer.Reasons {
			ids[i] = r.ID
		}
		if err := checkIDs(ids, sent, false); err != nil {
			return nil, fmt.Errorf("in the reasons, %w", err)
		}
		for _, r := range answer.Reasons {
			reason, err := checkReason(r.Reason)
			if err != nil {
				return nil, err
			}
			why[ri.Sent[r.ID-1]] = reason
		}
		keys := make([]float64, len(ri.Queue))
		desired := []int{}
		for rank, id := range answer.Order {
			i := ri.Sent[id-1]
			keys[i] = float64(len(answer.Order) - rank)
			if !ri.Queue[i].Pinned {
				desired = append(desired, i)
			}
		}
		var err error
		if order, err = rankOrder(ri.Queue, keys); err != nil {
			return nil, err
		}
		repaired := movedFrom(desired, order)
		rows := suggestionRows(ri.Queue, order, ri.Scores, ri.Reasons)
		for k, i := range order {
			rows[k].Source, rows[k].AIReason, rows[k].Repaired = "ai", why[i], repaired[i]
		}
		return rows, nil
	}
	rows := suggestionRows(ri.Queue, order, ri.Scores, ri.Reasons)
	for k, i := range order {
		rows[k].Source, rows[k].AIReason = "ai", why[i]
	}
	return rows, nil
}

// movedFrom reports which books of desired are out of its order in final:
// those outside a longest common subsequence, the fewest books that had to
// move to keep pins and series order.
func movedFrom(desired, final []int) map[int]bool {
	wanted := make(map[int]bool, len(desired))
	for _, i := range desired {
		wanted[i] = true
	}
	got := []int{}
	for _, i := range final {
		if wanted[i] {
			got = append(got, i)
		}
	}
	n := len(desired)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if desired[i] == got[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	moved := make(map[int]bool, len(desired))
	for _, i := range desired {
		moved[i] = true
	}
	for i, j := 0, 0; i < n && j < n; {
		switch {
		case desired[i] == got[j]:
			delete(moved, desired[i])
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	return moved
}

// rerankAI asks the provider about plan id, given the fingerprint of the
// request the reader saw. Inputs are read in one short transaction, the
// provider is called outside it, and the result is saved as a new plan in
// another short one that first checks the library did not change meanwhile.
// An answer that fails validation falls back to the usual ranking with a
// notice.
func (a *App) rerankAI(ctx context.Context, id, fingerprint string) (string, error) {
	var kind string
	var p Plan
	var rev int64
	var ri rerankInputs
	err := a.store.Write(func(tx *sql.Tx) error {
		var err error
		if kind, p, rev, err = readPlan(tx, id); err != nil {
			return err
		}
		if kind != "reorder" && kind != "insert" {
			return bad("Only queue suggestions can ask the AI.")
		}
		if len(p.AIRows) > 0 || p.AINotice != "" {
			return bad("This suggestion already asked the AI. Start a new suggestion to ask again.")
		}
		now, err := revision(tx)
		if err != nil {
			return err
		}
		if now != rev {
			return conflict("Your library changed. Generate a fresh preview before asking the AI.")
		}
		ri, err = loadRerankInputs(tx, kind, p, a.now().UTC())
		return err
	})
	if err != nil {
		return "", err
	}
	if ri.Fingerprint != fingerprint {
		return "", conflict("What would be shared changed since the preview. Check it again before sending.")
	}
	req, err := ri.request()
	if err != nil {
		return "", err
	}
	resp, err := a.askAI(ctx, req)
	if err != nil {
		return "", err
	}
	next := p
	next.Rows = slices.Clone(p.Rows)
	for i := range next.Rows {
		next.Rows[i].Source = "rules"
	}
	next.AIModel, next.AIInputTokens, next.AIOutputTokens = a.config.AI.Model, resp.InputTokens, resp.OutputTokens
	if next.AIRows, err = ri.aiRows(resp.Text); err != nil {
		next.AIRows = nil
		next.AINotice = "The AI’s answer could not be used (" + err.Error() + "), so this is your usual ranking. Nothing in your library changed."
	}
	var newID string
	err = a.store.Write(func(tx *sql.Tx) error {
		now, err := revision(tx)
		if err != nil {
			return err
		}
		if now != rev {
			return conflict("Your library changed while the AI was answering, so its suggestion was not saved. Generate a fresh suggestion.")
		}
		if newID, err = savePlan(tx, kind, next); err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM plans WHERE id=?", id)
		return err
	})
	return newID, err
}
