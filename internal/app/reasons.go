package app

import (
	"fmt"
	"strings"
	"time"
)

// seriesInfo is what the queue holds of one series: its lowest number and how
// many of its books are queued.
type seriesInfo struct {
	First  float64
	Queued int
}

// queueReason is the one short reason a queued book sits where it does, or "".
// The precedence is fixed, and no reason is better than a vague one:
//  1. pinned, then moved by hand (the reader's own placement)
//  2. waiting for a spot
//  3. a library deadline (a loan due within a week, or ready for pickup)
//  4. the next book in a series with more queued
//  5. a mood or length match from preferences
//
// It only describes; it never changes order. The mood and length rules are
// the ones score uses.
func queueReason(b Book, p Preferences, today time.Time, series map[string]seriesInfo) string {
	switch {
	case b.Pinned:
		return "Pinned: keeps its place"
	case b.MovedByHand:
		return "You placed this"
	case b.NeedsPlacement:
		return "Needs a spot"
	}
	if r := loanReason(b, today); r != "" {
		return r
	}
	if info, ok := series[b.Series]; ok && b.Series != "" && info.Queued > 1 && b.SeriesNumber != nil && *b.SeriesNumber == info.First {
		return fmt.Sprintf("Next in the %s series", b.Series)
	}
	switch {
	case moodMatch(b, p):
		return "Matches your " + strings.TrimSpace(p.Mood) + " mood"
	case lengthFit(b, p) && p.Length == "short":
		return "Short, fits your length preference"
	case lengthFit(b, p):
		return "Long, fits your length preference"
	}
	return ""
}

// loanReason names a library deadline worth showing: a ready pickup, or a
// borrowed book due within a week.
func loanReason(b Book, today time.Time) string {
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	best, bestDays := "", 1<<30
	for _, l := range b.Loans {
		switch l.Status {
		case "ready":
			if best == "" {
				best = "Library copy ready for pickup"
			}
		case "borrowed":
			due, err := time.Parse(time.DateOnly, l.DueDate)
			if err != nil {
				continue
			}
			days := int(due.Sub(today).Hours() / 24)
			if days > 7 || days >= bestDays {
				continue
			}
			bestDays = days
			switch {
			case days < 0:
				best = "Library loan is overdue"
			case days == 0:
				best = "Library loan due today"
			default:
				best = fmt.Sprintf("Library loan due in %d %s", days, plural(days, "day", "days"))
			}
		}
	}
	return best
}

// attachReasons fills in each queued row's reason from data already loaded
// for the page, plus at most two queries however many rows there are: the
// preferences, and the series summary (only if a row has a series).
func attachReasons(q queryer, items []listedBook, today time.Time) error {
	queued := false
	for _, it := range items {
		queued = queued || it.Status == "to-read"
	}
	if !queued {
		return nil
	}
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	var series map[string]seriesInfo
	for _, it := range items {
		if it.Status == "to-read" && it.Series != "" {
			if series, err = queuedSeries(q); err != nil {
				return err
			}
			break
		}
	}
	for i := range items {
		if items[i].Status == "to-read" {
			items[i].Reason = queueReason(items[i].Book, prefs, today, series)
			items[i].HandPlaced = items[i].Pinned || items[i].MovedByHand
		}
	}
	return nil
}

func queuedSeries(q queryer) (map[string]seriesInfo, error) {
	rows, err := q.Query("SELECT series,MIN(series_number),count(*) FROM books WHERE reading_status='to-read' AND series!='' AND series_number IS NOT NULL GROUP BY series")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]seriesInfo{}
	for rows.Next() {
		var name string
		var info seriesInfo
		if err = rows.Scan(&name, &info.First, &info.Queued); err != nil {
			return nil, err
		}
		out[name] = info
	}
	return out, rows.Err()
}
