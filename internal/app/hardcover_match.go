package app

import (
	"cmp"
	"slices"
	"strings"
)

// Matching a local book to Hardcover is deterministic and pure: this file
// reads no database and makes no request. An ISBN lookup that names exactly
// one Hardcover book is "confident"; every other result, including all text
// search results, needs the reader to pick. Nothing here ever links a book.

// hcCandidate is one Hardcover book offered for a local book. It carries
// metadata only, never a cover.
type hcCandidate struct {
	BookID    int64  `json:"book_id"`
	EditionID int64  `json:"edition_id,omitempty"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Slug      string `json:"slug,omitempty"`
	Year      int    `json:"year,omitempty"`
	Pages     int    `json:"pages,omitempty"`
	// LinkedTo names the local book that already holds this Hardcover book,
	// so the preview can show the conflict instead of failing on apply.
	LinkedTo      string `json:"linked_to,omitempty"`
	LinkedToID    int64  `json:"linked_to_id,omitempty"`
	Score         int    `json:"score"`
	IsCurrentLink bool   `json:"current,omitempty"`
}

const (
	matchConfident = "confident"
	matchAmbiguous = "ambiguous"
	matchNone      = "none"
)

// hcMatch is the result for one local book. Key says what matched: "ISBN" or
// "title and author".
type hcMatch struct {
	Class      string        `json:"class"`
	Key        string        `json:"key"`
	Candidates []hcCandidate `json:"candidates"`
}

// scoreCandidate ranks a candidate for a local book from what both sides
// state. Only the order and the displayed reasons use it, never a decision.
func scoreCandidate(local Book, c hcCandidate) int {
	score := 0
	if identity(c.Title, c.Author) == identity(local.Title, local.Author) {
		score += 100
	} else {
		if normalized(c.Title) == normalized(local.Title) {
			score += 60
		}
		if a, b := normalized(c.Author), normalized(local.Author); a != "" && b != "" && (strings.Contains(a, b) || strings.Contains(b, a)) {
			score += 30
		}
	}
	if local.PublishedYear != nil && c.Year != 0 && *local.PublishedYear == c.Year {
		score += 10
	}
	if local.Pages != nil && c.Pages != 0 {
		diff := *local.Pages - c.Pages
		if diff < 0 {
			diff = -diff
		}
		if diff*10 <= *local.Pages {
			score += 5
		}
	}
	return score
}

// uniqueByBook keeps the first candidate for each Hardcover book, so several
// editions of one book are one candidate.
func uniqueByBook(cands []hcCandidate) []hcCandidate {
	seen := map[int64]bool{}
	out := []hcCandidate{}
	for _, c := range cands {
		if c.BookID < 1 || seen[c.BookID] {
			continue
		}
		seen[c.BookID] = true
		out = append(out, c)
	}
	return out
}

// classifyMatch turns candidates found under one key into a result. Only an
// ISBN that names a single Hardcover book is confident; several books for one
// ISBN, and anything found by text search, are ambiguous.
func classifyMatch(local Book, key string, cands []hcCandidate) hcMatch {
	cands = uniqueByBook(cands)
	for i := range cands {
		cands[i].Score = scoreCandidate(local, cands[i])
	}
	slices.SortStableFunc(cands, func(a, b hcCandidate) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.BookID, b.BookID))
	})
	switch {
	case len(cands) == 0:
		return hcMatch{Class: matchNone, Key: key}
	case key == "ISBN" && len(cands) == 1:
		return hcMatch{Class: matchConfident, Key: key, Candidates: cands}
	}
	return hcMatch{Class: matchAmbiguous, Key: key, Candidates: cands}
}

// hardcoverSearchText is the only text sent to Hardcover's search: title and author.
func hardcoverSearchText(b Book) string {
	return strings.TrimSpace(b.Title + " " + b.Author)
}
