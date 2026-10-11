package app

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func prerequisitesReference(b Book, books []Book) map[int64]bool {
	result := map[int64]bool{}
	if b.Series == "" || b.SeriesNumber == nil {
		return result
	}
	for _, other := range books {
		if strings.EqualFold(b.Series, other.Series) && other.SeriesNumber != nil && *other.SeriesNumber < *b.SeriesNumber {
			result[other.ID] = true
		}
	}
	return result
}

// proposeReference is a verbatim copy of Propose at 4bafb11, kept as a test oracle.
// Propose never mutates the input. Pins occupy absolute queue slots; insertion
// preserves every other relative position. Full ranking reserves prerequisite
// slots before pinned books rather than greedily creating avoidable conflicts.
func proposeReference(books []Book, p Preferences, candidateID int64, today time.Time) ([]Suggestion, error) {
	scores := map[int64]float64{}
	reasons := map[int64][]string{}
	original := map[int64]int{}
	pins := map[int]int64{}
	deps := map[int64]map[int64]bool{}
	for i, b := range books {
		scores[b.ID], reasons[b.ID] = score(b, p, today)
		original[b.ID] = i
		if b.Pinned {
			pins[i] = b.ID
		}
		deps[b.ID] = prerequisitesReference(b, books)
	}
	result := []Book{}
	if candidateID != 0 {
		var candidate *Book
		rest := []Book{}
		for _, b := range books {
			if b.ID == candidateID {
				c := b
				candidate = &c
			} else {
				rest = append(rest, b)
			}
		}
		if candidate == nil || candidate.Pinned {
			return nil, fmt.Errorf("choose an unpinned book in your queue")
		}
		ideal := len(rest)
		for i, b := range rest {
			if scores[candidateID] > scores[b.ID] {
				ideal = i
				break
			}
		}
		bestDistance := math.MaxInt
		for i := 0; i <= len(rest); i++ {
			trial := append([]Book{}, rest[:i]...)
			trial = append(trial, *candidate)
			trial = append(trial, rest[i:]...)
			pos := map[int64]int{}
			valid := true
			for j, b := range trial {
				pos[b.ID] = j
			}
			for j, bid := range pins {
				if trial[j].ID != bid {
					valid = false
				}
			}
			for bid := range deps[candidateID] {
				if pos[bid] > i {
					valid = false
				}
			}
			for _, b := range rest {
				if deps[b.ID][candidateID] && pos[b.ID] < i {
					valid = false
				}
			}
			distance := int(math.Abs(float64(i - ideal)))
			if valid && distance < bestDistance {
				result = trial
				bestDistance = distance
			}
		}
		if len(result) == 0 {
			return nil, fmt.Errorf("pins and series order leave no valid position; unpin a book or check series numbers")
		}
	} else {
		done := map[int64]bool{}
		byID := map[int64]Book{}
		for _, b := range books {
			byID[b.ID] = b
		}
		eligible := func(b Book) bool {
			if done[b.ID] || b.Pinned {
				return false
			}
			for id := range deps[b.ID] {
				if !done[id] {
					return false
				}
			}
			return true
		}
		for slot := range books {
			var chosen *Book
			if id, ok := pins[slot]; ok {
				c := byID[id]
				chosen = &c
				for dep := range deps[id] {
					if !done[dep] {
						return nil, fmt.Errorf("a pinned book precedes an earlier queued book in its series; unpin it first")
					}
				}
			} else {
				urgent := map[int64]bool{}
				for deadline := slot + 1; deadline < len(books); deadline++ {
					if _, ok := pins[deadline]; !ok {
						continue
					}
					required := map[int64]bool{}
					available := 0
					for j := slot; j < deadline; j++ {
						if _, ok := pins[j]; !ok {
							available++
						}
					}
					for d, id := range pins {
						if d > slot && d <= deadline {
							for dep := range deps[id] {
								if !done[dep] && !byID[dep].Pinned {
									required[dep] = true
								}
							}
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
				for _, b := range books {
					if eligible(b) && (len(urgent) == 0 || urgent[b.ID]) && (chosen == nil || scores[b.ID] > scores[chosen.ID]) {
						c := b
						chosen = &c
					}
				}
				if chosen == nil {
					return nil, fmt.Errorf("pins conflict with series order; unpin a book first")
				}
			}
			result = append(result, *chosen)
			done[chosen.ID] = true
		}
	}
	rows := make([]Suggestion, 0, len(result))
	for i, b := range result {
		why := append([]string{}, reasons[b.ID]...)
		if b.Pinned {
			why = append(why, "Pinned position")
		}
		rows = append(rows, Suggestion{b.ID, b.Title, b.Author, original[b.ID] + 1, i + 1, b.Pinned, scores[b.ID], why})
	}
	return rows, nil
}
