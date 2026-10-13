package app

import (
	"fmt"
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
	// Rating is the reader's own Goodreads rating, 1-5, or 0 when unrated.
	// Parsers also copy it into Interest, which only a new book takes.
	Rating int `json:"rating,omitempty"`
	// RatingIgnored marks a rating that was present but not 0-5.
	RatingIgnored bool `json:"-"`
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
	// Upgrades previews how many unreviewed books an import moves, by status.
	Upgrades map[string]int `json:"upgrades,omitempty"`
	// Skipped previews how many import rows match deleted books.
	Skipped int `json:"skipped,omitempty"`
	// Rated counts new books that will start with their Goodreads rating as
	// interest, RatingsKept matched books whose local interest wins, and
	// RatingsIgnored ratings that were not a whole number from 0 to 5.
	Rated          int `json:"rated,omitempty"`
	RatingsKept    int `json:"ratings_kept,omitempty"`
	RatingsIgnored int `json:"ratings_ignored,omitempty"`
	// BookIDs and Status describe a bulk triage of unreviewed books.
	BookIDs []int64 `json:"book_ids,omitempty"`
	Status  string  `json:"status,omitempty"`
	// Restore is a validated JSON export and what restoring it would do.
	Restore *restorePlan `json:"restore,omitempty"`
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
