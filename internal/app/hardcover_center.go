package app

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The sync center ties the pieces together on /hardcover. "Check for changes"
// is one button and never a timer: it runs an incremental pull fetch, compares
// each changed book with this library and with the sync history, and sorts the
// result into three groups. Acting on a group opens the existing previewed
// flow (import, send, take one book), each with its own revision check, so
// there is one way to change anything, and nothing is applied from this page.

const (
	groupHardcover = "hardcover" // Hardcover is ahead
	groupLocal     = "local"     // this library is ahead
	groupBoth      = "both"      // both changed
)

// hcChangeRow is one book that differs between the two sides.
type hcChangeRow struct {
	BookID          int64  `json:"book_id"`
	Title           string `json:"title"`
	Author          string `json:"author,omitempty"`
	HardcoverBookID int64  `json:"hardcover_book_id"`
	Group           string `json:"group"`
	LocalText       string `json:"local_text"`
	RemoteText      string `json:"remote_text"`
	Why             string `json:"why"`
	LocalStatus     string `json:"-"`
}

// hcCheck is the result of one "Check for changes", kept as a plan so every
// action taken from it checks the library revision.
type hcCheck struct {
	At       string        `json:"at"`
	Since    string        `json:"since,omitempty"`
	Fetched  int           `json:"fetched"`
	Complete bool          `json:"complete"`
	Stopped  string        `json:"stopped,omitempty"`
	Rows     []hcChangeRow `json:"rows"`
	// Items are the Hardcover rows fetched, so a pull plan made from this check
	// needs no further request. New are those with no local match.
	Items  []hcPullItem `json:"items"`
	New    []hcPullItem `json:"new"`
	InSync int          `json:"in_sync"`
	// Plan fields carried so an import made from the check keeps the high-water mark.
	HighWater  string `json:"high_water,omitempty"`
	NextOffset int    `json:"next_offset,omitempty"`
}

func (c hcCheck) in(group string) []hcChangeRow {
	var out []hcChangeRow
	for _, r := range c.Rows {
		if r.Group == group {
			out = append(out, r)
		}
	}
	return out
}
func (c hcCheck) Ahead() []hcChangeRow { return c.in(groupHardcover) }
func (c hcCheck) Local() []hcChangeRow { return c.in(groupLocal) }
func (c hcCheck) Both() []hcChangeRow  { return c.in(groupBoth) }
func (c hcCheck) Total() int           { return len(c.Rows) + len(c.New) }

// sideChange compares one field against the last sync. localChanged and
// remoteChanged say who moved since; without history, rank decides.
func fieldGroup(last *hcSyncRow, localNow, remoteNow string, localRank, remoteRank int) string {
	if last != nil {
		lc, rc := localNow != last.Local, remoteNow != last.Remote
		switch {
		case lc && rc:
			return groupBoth
		case rc:
			return groupHardcover
		case lc:
			return groupLocal
		}
	}
	switch {
	case remoteRank > localRank:
		return groupHardcover
	case localRank > remoteRank:
		return groupLocal
	}
	return groupBoth
}

// classifyChange decides which side is ahead for one matched book, or "" when
// they agree. Status and rating are the only fields compared, the same two
// that sending and importing move.
func classifyChange(q queryer, localID int64, it hcPullItem) (hcChangeRow, bool, error) {
	var title, author, status string
	if err := q.QueryRow("SELECT title,author,reading_status FROM books WHERE id=?", localID).Scan(&title, &author, &status); err != nil {
		return hcChangeRow{}, false, err
	}
	row := hcChangeRow{BookID: localID, Title: title, Author: author, HardcoverBookID: it.HardcoverBookID, LocalStatus: status,
		LocalText: statusLabel(status), RemoteText: hcStatusNames[it.StatusID]}
	target, shelved := pushStatuses[status]
	if !shelved || hardcoverStatuses[it.StatusID].Local == "" {
		return row, false, nil // no shelf here, or none there (Paused, Ignored)
	}
	var rating sql.NullFloat64
	if err := q.QueryRow("SELECT rating FROM reading_log WHERE book_id=? AND rating IS NOT NULL ORDER BY finished_at IS NULL, substr(finished_at,1,4) DESC, finished_precision=3, finished_at DESC, id DESC LIMIT 1", localID).Scan(&rating); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return row, false, err
	}
	if rating.Valid {
		row.LocalText += ", " + trimStars(rating.Float64) + " stars"
	}
	if it.Rating > 0 {
		row.RemoteText += ", " + trimStars(it.Rating) + " stars"
	}
	groups := []string{}
	if target != it.StatusID {
		last, err := lastSync(q, localID, "status")
		if err != nil {
			return row, false, err
		}
		groups = append(groups, fieldGroup(last, status, strconv.Itoa(it.StatusID), hcRank(target), hcRank(it.StatusID)))
		row.Why = "status differs"
	}
	// A rating differs when both have one and they do not round to the same
	// half star, or only one side has one.
	localHalf := halfStars(rating.Float64)
	if rating.Valid && it.Rating > 0 && localHalf != halfStars(it.Rating) || rating.Valid != (it.Rating > 0) {
		last, err := lastSync(q, localID, "rating")
		if err != nil {
			return row, false, err
		}
		g := fieldGroup(last, trimStars(rating.Float64), trimStars(it.Rating), boolRank(rating.Valid), boolRank(it.Rating > 0))
		groups = append(groups, g)
		row.Why = strings.TrimPrefix(row.Why+", rating differs", ", ")
	}
	if len(groups) == 0 {
		return row, false, nil
	}
	row.Group = groups[0]
	for _, g := range groups[1:] {
		if g != row.Group {
			row.Group = groupBoth
		}
	}
	return row, true, nil
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// localAhead lists linked books that changed here since they were last synced
// and that Hardcover did not change since the last pull (so they are not among
// the fetched items). It reads the library only.
func localAhead(q queryer, fetched map[int64]bool) ([]hcChangeRow, error) {
	rows, err := q.Query(`SELECT b.id,b.title,b.author,b.reading_status,h.hardcover_book_id,
 (SELECT local_value FROM hardcover_sync s WHERE s.book_id=b.id AND s.field='status' ORDER BY s.id DESC LIMIT 1)
 FROM books b JOIN book_hardcover h ON h.book_id=b.id ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hcChangeRow
	for rows.Next() {
		var r hcChangeRow
		var last sql.NullString
		if err = rows.Scan(&r.BookID, &r.Title, &r.Author, &r.LocalStatus, &r.HardcoverBookID, &last); err != nil {
			return nil, err
		}
		if !last.Valid || last.String == r.LocalStatus || fetched[r.HardcoverBookID] {
			continue
		}
		if _, ok := pushStatuses[r.LocalStatus]; !ok {
			continue
		}
		r.Group, r.Why = groupLocal, "changed here since the last sync"
		r.LocalText, r.RemoteText = statusLabel(r.LocalStatus), "unchanged since the last sync ("+statusLabel(last.String)+")"
		out = append(out, r)
	}
	return out, rows.Err()
}

// buildCheck sorts fetched Hardcover rows against this library. It reads the
// database and never writes it.
func buildCheck(q queryer, run hcPullPlan, at string) (hcCheck, error) {
	c := hcCheck{At: at, Since: run.Since, Fetched: len(run.Items), Complete: run.Complete, Stopped: run.Stopped, HighWater: run.HighWater, NextOffset: run.NextOffset}
	fetched := map[int64]bool{}
	for _, it := range run.Items {
		fetched[it.HardcoverBookID] = true
		id, _, skip, err := pullMatch(q, it)
		if err != nil {
			return c, err
		}
		switch {
		case skip != "":
			continue
		case id == 0:
			if hardcoverStatuses[it.StatusID].Local != "" {
				c.New = append(c.New, it)
			}
			continue
		}
		row, differs, err := classifyChange(q, id, it)
		if err != nil {
			return c, err
		}
		if !differs {
			c.InSync++
			continue
		}
		c.Rows = append(c.Rows, row)
		c.Items = append(c.Items, it)
	}
	local, err := localAhead(q, fetched)
	if err != nil {
		return c, err
	}
	c.Rows = append(c.Rows, local...)
	slices.SortStableFunc(c.Rows, func(a, b hcChangeRow) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Title, b.Title))
	})
	return c, nil
}

// ---- history ----

const (
	hardcoverHistoryKey  = "hardcover_history"
	hardcoverHistoryKeep = 30
	hardcoverDetailKeep  = 25
)

// hcHistoryDetail is one book in a history entry: its title and what moved.
// Never a review or a note.
type hcHistoryDetail struct {
	BookID int64  `json:"book_id,omitempty"`
	Title  string `json:"title"`
	What   string `json:"what"`
}

// hcHistoryEntry is one sync that changed something, in either direction.
type hcHistoryEntry struct {
	At        string            `json:"at"`
	Direction string            `json:"direction"` // pull or push
	Summary   string            `json:"summary"`
	Details   []hcHistoryDetail `json:"details,omitempty"`
	More      int               `json:"more,omitempty"`
}

// addHistory records a sync, newest first, capped. It is part of the same
// transaction as the change it describes.
func addHistory(q queryer, e hcHistoryEntry) error {
	if len(e.Details) > hardcoverDetailKeep {
		e.More = len(e.Details) - hardcoverDetailKeep
		e.Details = e.Details[:hardcoverDetailKeep]
	}
	for i := range e.Details {
		e.Details[i].Title, e.Details[i].What = truncate(e.Details[i].Title, 200), truncate(e.Details[i].What, 300)
	}
	list, err := readMeta(q, hardcoverHistoryKey, []hcHistoryEntry{})
	if err != nil {
		return err
	}
	list = append([]hcHistoryEntry{e}, list...)
	if len(list) > hardcoverHistoryKeep {
		list = list[:hardcoverHistoryKeep]
	}
	return setMeta(q, hardcoverHistoryKey, list)
}

// ---- forgetting ids ----

// hcForgetPlan counts what "Forget Hardcover ids" would clear.
type hcForgetPlan struct {
	Links, Aliases, History, Cached, ReadIDs, NoteIDs int
}

func countForget(q queryer) (hcForgetPlan, error) {
	var p hcForgetPlan
	err := q.QueryRow(`SELECT (SELECT count(*) FROM book_hardcover),(SELECT count(*) FROM hardcover_alias),(SELECT count(*) FROM hardcover_sync),
 (SELECT count(*) FROM hardcover_catalog)+(SELECT count(*) FROM series_cache),(SELECT count(*) FROM reading_log WHERE hardcover_read_id IS NOT NULL),(SELECT count(*) FROM book_notes WHERE hardcover_journal_id>0)`).Scan(&p.Links, &p.Aliases, &p.History, &p.Cached, &p.ReadIDs, &p.NoteIDs)
	return p, err
}

// forgetIDs clears every Hardcover id kept here. Books, readings, ratings,
// and everything else stay; a reading that came from Hardcover keeps its
// source, only its tie to the Hardcover read is removed.
func forgetIDs(q queryer) error {
	for _, stmt := range []string{"DELETE FROM book_hardcover", "DELETE FROM hardcover_alias", "DELETE FROM hardcover_sync", "DELETE FROM hardcover_catalog", "DELETE FROM series_cache", "UPDATE reading_log SET hardcover_read_id=NULL", "UPDATE book_notes SET hardcover_journal_id=0,hardcover_user_id=0,hardcover_book_id=0", "DELETE FROM meta WHERE key IN ('hardcover_pull_since','hardcover_history')"} {
		if _, err := q.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// ---- banners and estimates ----

// hcBanner is a plain-language explanation of the last failed request.
type hcBanner struct {
	Kind, Message string
	Link          bool
}

func bannerFor(f *hcFailure, blocked, now time.Time) *hcBanner {
	if f == nil {
		return nil
	}
	var rate hardcoverRateError
	var scope hardcoverScopeError
	switch {
	case errors.Is(f.Err, errHardcoverAuth):
		return &hcBanner{"auth", "Hardcover rejected the token. It may have expired or been reset. Create a new one and update HARDCOVER_API_TOKEN on the server.", true}
	case errors.As(f.Err, &scope):
		return &hcBanner{"scope", "The token lacks the scope " + scope.Need + ". Add it to the token and update HARDCOVER_API_TOKEN.", true}
	case errors.As(f.Err, &rate):
		wait := max(time.Second, blocked.Sub(now))
		return &hcBanner{"rate", fmt.Sprintf("Hardcover's rate limit was reached. Try again in about %d seconds. Nothing is retried for you.", int(wait.Seconds())+1), false}
	case errors.Is(f.Err, errHardcoverUnavailable), errors.Is(f.Err, errHardcoverTimeout):
		return &hcBanner{"down", "Hardcover is unavailable or too slow right now. Nothing changed. Try again later.", false}
	}
	return &hcBanner{"other", "The last request to Hardcover failed. Nothing changed.", false}
}
