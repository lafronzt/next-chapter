package app

import (
	"fmt"
	"math"
	"strings"
	"time"
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

func prerequisites(b Book, books []Book) map[int64]bool {
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

// Propose never mutates the input. Pins occupy absolute queue slots; insertion
// preserves every other relative position. Full ranking reserves prerequisite
// slots before pinned books rather than greedily creating avoidable conflicts.
func Propose(books []Book, p Preferences, candidateID int64, today time.Time) ([]Suggestion, error) {
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
		deps[b.ID] = prerequisites(b, books)
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
