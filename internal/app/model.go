package app

import (
	"container/heap"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type Book struct {
	ID             int64    `json:"id"`
	Title          string   `json:"title"`
	Author         string   `json:"author"`
	Identity       string   `json:"identity"`
	ISBN           string   `json:"isbn"`
	GoodreadsID    string   `json:"goodreads_id"`
	Pages          *int     `json:"pages"`
	Tags           string   `json:"tags"`
	Interest       int      `json:"interest"`
	Status         string   `json:"reading_status"`
	Position       int      `json:"position"`
	Pinned         bool     `json:"pinned"`
	NeedsPlacement bool     `json:"needs_placement"`
	Series         string   `json:"series"`
	SeriesNumber   *float64 `json:"series_number"`
	AddedAt        string   `json:"added_at"`
	Source         string   `json:"source"`
	Loans          []Loan   `json:"loans"`
}

type Loan struct {
	BookID     int64  `json:"book_id"`
	Source     string `json:"source"`
	Format     string `json:"format"`
	Status     string `json:"status"`
	DueDate    string `json:"due_date"`
	ObservedAt string `json:"observed_at"`
}

type Event struct {
	ID          int64  `json:"id"`
	BookID      int64  `json:"book_id"`
	Source      string `json:"source"`
	Format      string `json:"format"`
	Status      string `json:"status"`
	Activity    string `json:"activity"`
	At          string `json:"at"`
	DueDate     string `json:"due_date"`
	Fingerprint string `json:"fingerprint"`
	Title       string `json:"title,omitempty"`
	Author      string `json:"author,omitempty"`
}

type Record struct {
	Book
	Event *Event `json:"event"`
}

type Preferences struct {
	LibraryWeight float64 `json:"library_weight"`
	Length        string  `json:"length"`
	Mood          string  `json:"mood"`
}

var defaultPreferences = Preferences{LibraryWeight: 1, Length: "any"}
var readingStates = []string{"to-read", "reading", "read", "abandoned", "archived", "unreviewed"}
var libraryStates = []string{"hold", "ready", "borrowed", "returned", "cancelled", "available"}

type Suggestion struct {
	ID      int64    `json:"id"`
	Title   string   `json:"title"`
	Author  string   `json:"author"`
	Old     int      `json:"old"`
	New     int      `json:"new"`
	Pinned  bool     `json:"pinned"`
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

type Plan struct {
	Rows        []Suggestion `json:"rows,omitempty"`
	Records     []Record     `json:"records,omitempty"`
	CandidateID int64        `json:"candidate_id,omitempty"`
	Source      string       `json:"source,omitempty"`
}

func score(b Book, p Preferences, today time.Time) (float64, []string) {
	total := float64(b.Interest * 10)
	reasons := []string{fmt.Sprintf("Interest %d/5", b.Interest)}
	best := 0
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	for _, l := range b.Loans {
		bonus := map[string]int{"borrowed": 15, "ready": 25, "available": 5}[l.Status]
		if l.Status == "borrowed" || l.Status == "ready" {
			if l.Status == "borrowed" {
				reasons = append(reasons, "Already borrowed")
			} else {
				reasons = append(reasons, "Ready for pickup")
			}
			if due, err := time.Parse(time.DateOnly, l.DueDate); err == nil {
				days := int(due.Sub(today).Hours() / 24)
				if days <= 7 {
					bonus += 20
					if days < 0 {
						reasons = append(reasons, "Deadline has passed")
					} else {
						reasons = append(reasons, "Deadline within 7 days")
					}
				}
			}
		}
		if bonus > best {
			best = bonus
		}
	}
	total += float64(best) * p.LibraryWeight
	if mood := strings.TrimSpace(p.Mood); mood != "" {
		for _, tag := range strings.Split(b.Tags, ",") {
			if strings.EqualFold(mood, strings.TrimSpace(tag)) {
				total += 15
				reasons = append(reasons, "Matches your "+mood+" mood")
				break
			}
		}
	}
	if b.Pages != nil && ((p.Length == "short" && *b.Pages <= 250) || (p.Length == "long" && *b.Pages >= 400)) {
		total += 10
		reasons = append(reasons, "Matches your preferred length")
	}
	return total, reasons
}

// foldKey maps a string to a canonical form such that foldKey(a) == foldKey(b)
// exactly when strings.EqualFold(a, b): every rune becomes the smallest member
// of its simple case-folding orbit.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}

// prerequisite reports whether other is an earlier book in b's series.
func prerequisite(other, b Book) bool {
	return b.Series != "" && b.SeriesNumber != nil && other.SeriesNumber != nil && *other.SeriesNumber < *b.SeriesNumber && strings.EqualFold(b.Series, other.Series)
}

// seriesPrerequisites returns, for every book, the IDs of earlier books in its
// series. Books are grouped by folded series name first, so the cost follows
// series sizes rather than the square of the queue length.
func seriesPrerequisites(books []Book) map[int64]map[int64]bool {
	groups := map[string][]int{}
	for i, b := range books {
		if b.Series != "" && b.SeriesNumber != nil {
			key := foldKey(b.Series)
			groups[key] = append(groups[key], i)
		}
	}
	deps := make(map[int64]map[int64]bool, len(books))
	for _, b := range books {
		deps[b.ID] = map[int64]bool{}
	}
	for _, group := range groups {
		for _, i := range group {
			for _, j := range group {
				if *books[j].SeriesNumber < *books[i].SeriesNumber {
					deps[books[i].ID][books[j].ID] = true
				}
			}
		}
	}
	return deps
}

// readyBooks orders books whose prerequisites are all placed: highest score
// first, then earliest in the current queue.
type readyBooks struct {
	items  []int
	scores []float64
}

func (h readyBooks) Len() int           { return len(h.items) }
func (h readyBooks) Less(i, j int) bool { return h.before(h.items[i], h.items[j]) }
func (h readyBooks) before(a, b int) bool {
	if h.scores[a] != h.scores[b] {
		return h.scores[a] > h.scores[b]
	}
	return a < b
}
func (h readyBooks) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *readyBooks) Push(x any)   { h.items = append(h.items, x.(int)) }
func (h *readyBooks) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return last
}

// Propose never mutates the input. Pins occupy absolute queue slots; insertion
// preserves every other relative position. Full ranking reserves prerequisite
// slots before pinned books rather than greedily creating avoidable conflicts.
func Propose(books []Book, p Preferences, candidateID int64, today time.Time) ([]Suggestion, error) {
	scores := make([]float64, len(books))
	reasons := make([][]string, len(books))
	for i, b := range books {
		scores[i], reasons[i] = score(b, p, today)
	}
	order := make([]int, 0, len(books))
	if candidateID != 0 {
		c := -1
		for i, b := range books {
			if b.ID == candidateID {
				c = i
				break
			}
		}
		if c < 0 || books[c].Pinned {
			return nil, fmt.Errorf("choose an unpinned book in your queue")
		}
		// Slot i inserts the candidate before rest[i], where rest is the queue
		// without the candidate. Each constraint bounds i from one side, so the
		// valid slots form the interval [lo, hi].
		restIndex := func(j int) int {
			if j > c {
				return j - 1
			}
			return j
		}
		lo, hi := 0, len(books)-1
		ideal, found := len(books)-1, false
		for j, b := range books {
			if j == c {
				continue
			}
			r := restIndex(j)
			if !found && scores[c] > scores[j] {
				ideal, found = r, true
			}
			switch {
			case b.Pinned && j < c:
				lo = max(lo, j+1) // a pin before the candidate must not shift
			case b.Pinned:
				hi = min(hi, j-1) // a pin after the candidate must shift into place
			}
			if prerequisite(b, books[c]) {
				lo = max(lo, r+1)
			}
			if prerequisite(books[c], b) {
				hi = min(hi, r)
			}
		}
		if lo > hi {
			return nil, fmt.Errorf("pins and series order leave no valid position; unpin a book or check series numbers")
		}
		slot := min(max(ideal, lo), hi)
		for j := range books {
			if j == c {
				continue
			}
			if restIndex(j) == slot {
				order = append(order, c)
			}
			order = append(order, j)
		}
		if slot == len(books)-1 {
			order = append(order, c)
		}
	} else {
		deps := seriesPrerequisites(books)
		index := make(map[int64]int, len(books))
		pins := []int{}
		for i, b := range books {
			index[b.ID] = i
			if b.Pinned {
				pins = append(pins, i)
			}
		}
		done := make([]bool, len(books))
		waiting := make([]int, len(books))
		dependents := make([][]int, len(books))
		ready := &readyBooks{scores: scores}
		for i, b := range books {
			waiting[i] = len(deps[b.ID])
			for id := range deps[b.ID] {
				dependents[index[id]] = append(dependents[index[id]], i)
			}
			if waiting[i] == 0 && !b.Pinned {
				ready.items = append(ready.items, i)
			}
		}
		heap.Init(ready)
		nextPin := 0
		for slot := range books {
			chosen := -1
			for nextPin < len(pins) && pins[nextPin] < slot {
				nextPin++
			}
			if nextPin < len(pins) && pins[nextPin] == slot {
				chosen = slot
				if waiting[chosen] > 0 {
					return nil, fmt.Errorf("a pinned book precedes an earlier queued book in its series; unpin it first")
				}
			} else {
				// Find the first later pin whose unplaced, unpinned prerequisites
				// exactly fill the free slots before it; only those may go here.
				var urgent map[int64]bool
				required := map[int64]bool{}
				for k, deadline := range pins[nextPin:] {
					available := deadline - slot - k
					for dep := range deps[books[deadline].ID] {
						if j := index[dep]; !done[j] && !books[j].Pinned {
							required[dep] = true
						}
					}
					if len(required) > available {
						return nil, fmt.Errorf("pinned positions conflict with series order; unpin a book first")
					}
					if len(required) > 0 && len(required) == available {
						urgent = required
						break
					}
				}
				if urgent == nil {
					for ready.Len() > 0 && done[ready.items[0]] {
						heap.Pop(ready)
					}
					if ready.Len() > 0 {
						chosen = heap.Pop(ready).(int)
					}
				} else {
					for id := range urgent {
						j := index[id]
						if waiting[j] == 0 && (chosen < 0 || ready.before(j, chosen)) {
							chosen = j
						}
					}
				}
				if chosen < 0 {
					return nil, fmt.Errorf("pins conflict with series order; unpin a book first")
				}
			}
			order = append(order, chosen)
			done[chosen] = true
			for _, j := range dependents[chosen] {
				if waiting[j]--; waiting[j] == 0 && !books[j].Pinned {
					heap.Push(ready, j)
				}
			}
		}
	}
	rows := make([]Suggestion, 0, len(order))
	for i, j := range order {
		b := books[j]
		why := append([]string{}, reasons[j]...)
		if b.Pinned {
			why = append(why, "Pinned position")
		}
		rows = append(rows, Suggestion{b.ID, b.Title, b.Author, j + 1, i + 1, b.Pinned, scores[j], why})
	}
	return rows, nil
}

func in(value string, values []string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func queueOnly(all []Book) []Book {
	result := []Book{}
	for _, b := range all {
		if b.Status == "to-read" {
			result = append(result, b)
		}
	}
	return result
}
