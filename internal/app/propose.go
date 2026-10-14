package app

import (
	"container/heap"
	"fmt"
	"strings"
	"time"
	"unicode"
)

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
	var order []int
	if candidateID != 0 {
		c, err := candidateIndex(books, candidateID)
		if err != nil {
			return nil, err
		}
		lo, hi, ideal, err := insertBounds(books, scores, c)
		if err != nil {
			return nil, err
		}
		order = insertAt(books, c, min(max(ideal, lo), hi))
	} else {
		var err error
		if order, err = rankOrder(books, scores); err != nil {
			return nil, err
		}
	}
	return suggestionRows(books, order, scores, reasons), nil
}

// candidateIndex finds the unpinned book to place.
func candidateIndex(books []Book, candidateID int64) (int, error) {
	for i, b := range books {
		if b.ID == candidateID && !b.Pinned {
			return i, nil
		}
	}
	return -1, fmt.Errorf("choose an unpinned book in your queue")
}

// insertBounds returns the valid slots [lo, hi] for book c and the slot its
// score suggests. Slot i inserts the candidate before rest[i], where rest is
// the queue without the candidate; each constraint bounds i from one side.
func insertBounds(books []Book, scores []float64, c int) (lo, hi, ideal int, err error) {
	restIndex := func(j int) int {
		if j > c {
			return j - 1
		}
		return j
	}
	lo, hi = 0, len(books)-1
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
		return 0, 0, 0, fmt.Errorf("pins and series order leave no valid position; unpin a book or check series numbers")
	}
	return lo, hi, ideal, nil
}

// insertAt returns the queue order with book c moved to slot.
func insertAt(books []Book, c, slot int) []int {
	order := make([]int, 0, len(books))
	r := 0
	for j := range books {
		if j == c {
			continue
		}
		if r == slot {
			order = append(order, c)
		}
		order = append(order, j)
		r++
	}
	if slot == len(books)-1 {
		order = append(order, c)
	}
	return order
}

// rankOrder orders the whole queue by keys, highest first and ties in queue
// order, while keeping pins in their slots and series prerequisites first.
func rankOrder(books []Book, keys []float64) ([]int, error) {
	order := make([]int, 0, len(books))
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
	ready := &readyBooks{scores: keys}
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
	return order, nil
}

// suggestionRows describes an order as preview rows.
func suggestionRows(books []Book, order []int, scores []float64, reasons [][]string) []Suggestion {
	rows := make([]Suggestion, 0, len(order))
	for i, j := range order {
		b := books[j]
		why := append([]string{}, reasons[j]...)
		if b.Pinned {
			why = append(why, "Pinned position")
		}
		if b.MovedByHand {
			why = append(why, "You moved this by hand")
		}
		rows = append(rows, Suggestion{ID: b.ID, Title: b.Title, Author: b.Author, Old: j + 1, New: i + 1, Pinned: b.Pinned, Score: scores[j], Reasons: why})
	}
	return rows
}
