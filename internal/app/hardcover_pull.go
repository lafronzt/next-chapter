package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Pulling a Hardcover library is an import: previewed, transactional,
// idempotent, and it never reorders the queue. Hardcover becomes a third
// source beside the CSV files. Nothing here reads reviews, notes, journal
// entries, lists, or any other reader's data.

// hardcoverStatuses is the one status table: Hardcover's status id, what it
// becomes in Next Chapter, and the reason when it becomes nothing. The README
// documents the same table.
var hardcoverStatuses = map[int]struct{ Local, Why string }{
	1: {"to-read", ""},
	2: {"reading", ""},
	3: {"read", ""},
	4: {"", "Paused has no equivalent here, so the book is left as it is."},
	5: {"abandoned", ""},
	6: {"", "Ignored on Hardcover, so it is not imported."},
}

const hardcoverPageSize = 100

// hardcoverPagesPerRun keeps one run under the burst of 10; a bigger library
// continues in the next run.
const hardcoverPagesPerRun = 8

// hcPullRead is one read of a book on Hardcover.
type hcPullRead struct {
	ID         int64  `json:"id"`
	FinishedAt string `json:"finished_at,omitempty"`
	Precision  int    `json:"precision,omitempty"`
}

// hcPullItem is one Hardcover library entry, reduced to what an import needs.
type hcPullItem struct {
	HardcoverBookID int64        `json:"book_id"`
	EditionID       int64        `json:"edition_id,omitempty"`
	Slug            string       `json:"slug,omitempty"`
	Title           string       `json:"title"`
	Author          string       `json:"author"`
	ISBN            string       `json:"isbn,omitempty"`
	Pages           int          `json:"pages,omitempty"`
	StatusID        int          `json:"status_id"`
	Rating          float64      `json:"rating,omitempty"`
	UpdatedAt       string       `json:"updated_at,omitempty"`
	Reads           []hcPullRead `json:"reads,omitempty"`
}

// hcPullPlan is one run of a pull. Offset and NextOffset let a library larger
// than one run continue; HighWater becomes the next incremental pull's start
// only when the pull completed.
type hcPullPlan struct {
	Items      []hcPullItem `json:"items"`
	Since      string       `json:"since,omitempty"`
	HighWater  string       `json:"high_water,omitempty"`
	NextOffset int          `json:"next_offset,omitempty"`
	Complete   bool         `json:"complete"`
	Stopped    string       `json:"stopped,omitempty"`
}

const hcLibraryFields = `id status_id rating updated_at
  book { id title slug pages contributions { author { name } } }
  edition { id isbn_13 isbn_10 pages }
  user_book_reads(order_by: {id: asc}) { id finished_at finished_at_precision }`

// hardcoverLibraryQuery reads one page of the reader's own library. The
// where clause names the reader's user id and nothing else.
func hardcoverLibraryQuery(incremental bool) string {
	if incremental {
		return `query NextChapterLibrary($uid: Int!, $since: timestamptz!, $limit: Int!, $offset: Int!) {
 user_books(where: {user_id: {_eq: $uid}, updated_at: {_gt: $since}}, order_by: {id: asc}, limit: $limit, offset: $offset) {
  ` + hcLibraryFields + `
 }
}`
	}
	return `query NextChapterLibrary($uid: Int!, $limit: Int!, $offset: Int!) {
 user_books(where: {user_id: {_eq: $uid}}, order_by: {id: asc}, limit: $limit, offset: $offset) {
  ` + hcLibraryFields + `
 }
}`
}

type hcUserBookJSON struct {
	ID        flexInt     `json:"id"`
	BookID    flexInt     `json:"book_id"`
	StatusID  flexInt     `json:"status_id"`
	Rating    *float64    `json:"rating"`
	UpdatedAt string      `json:"updated_at"`
	Book      *hcBookJSON `json:"book"`
	Edition   *struct {
		ID     flexInt `json:"id"`
		ISBN13 string  `json:"isbn_13"`
		ISBN10 string  `json:"isbn_10"`
		Pages  flexInt `json:"pages"`
	} `json:"edition"`
	Reads []struct {
		ID         flexInt `json:"id"`
		FinishedAt string  `json:"finished_at"`
		Precision  flexInt `json:"finished_at_precision"`
	} `json:"user_book_reads"`
}

// quarterStars rounds a Hardcover rating (0 to 5, halves) to the quarter-star
// scale; zero means unrated.
func quarterStars(r float64) float64 {
	if r <= 0 || math.IsNaN(r) {
		return 0
	}
	return math.Min(5, math.Max(0.25, math.Round(r*4)/4))
}

var hcDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// periodStart is the stored date for a finish date and its precision: the
// day, or the first day of the month or year. The precision, kept beside it,
// says the rest is unknown.
func periodStart(date string, precision int) (string, int, bool) {
	if !hcDatePattern.MatchString(date) {
		return "", 0, false
	}
	day := date[:10]
	if !validFinishDate(day) {
		return "", 0, false
	}
	switch precision {
	case 2:
		return day[:7] + "-01", 2, true
	case 3:
		return day[:4] + "-01-01", 3, true
	}
	return day, 1, true
}

func (u hcUserBookJSON) item() (hcPullItem, bool) {
	if u.Book == nil || u.Book.ID < 1 || strings.TrimSpace(u.Book.Title) == "" {
		return hcPullItem{}, false
	}
	c := u.Book.candidate(0)
	it := hcPullItem{HardcoverBookID: c.BookID, Slug: c.Slug, Title: c.Title, Author: c.Author, Pages: c.Pages, StatusID: int(u.StatusID), UpdatedAt: truncate(u.UpdatedAt, 64)}
	if u.Rating != nil {
		it.Rating = quarterStars(*u.Rating)
	}
	if e := u.Edition; e != nil {
		it.EditionID = int64(e.ID)
		it.ISBN = cmp.Or(cleanISBN(e.ISBN13), cleanISBN(e.ISBN10))
		if it.Pages == 0 {
			it.Pages = int(e.Pages)
		}
	}
	for _, r := range u.Reads {
		if r.ID < 1 {
			continue
		}
		read := hcPullRead{ID: int64(r.ID)}
		if _, p, ok := periodStart(r.FinishedAt, int(r.Precision)); ok {
			read.FinishedAt, read.Precision = r.FinishedAt[:10], p
		}
		it.Reads = append(it.Reads, read)
	}
	return it, true
}

// pullRun fetches pages of the reader's library until it is complete, the run
// limit is reached, or Hardcover stops it. It runs no database transaction.
// What it returns can be previewed and applied even when it stopped early:
// applying is idempotent, so the next run simply continues.
func (c *hardcoverClient) pullRun(ctx context.Context, userID int64, since string, offset int) (hcPullPlan, error) {
	plan := hcPullPlan{Since: since, NextOffset: offset}
	for range hardcoverPagesPerRun {
		vars := map[string]any{"uid": userID, "limit": hardcoverPageSize, "offset": plan.NextOffset}
		if since != "" {
			vars["since"] = since
		}
		var out struct {
			UserBooks []hcUserBookJSON `json:"user_books"`
		}
		if err := c.do(ctx, hardcoverLibraryQuery(since != ""), vars, &out); err != nil {
			if recoverableHardcover(err) && (len(plan.Items) > 0 || plan.NextOffset > offset) {
				plan.Stopped = err.Error() + " Apply what was fetched, then continue the pull."
				return plan, nil
			}
			return plan, err
		}
		for _, u := range out.UserBooks {
			if it, ok := u.item(); ok {
				plan.Items = append(plan.Items, it)
				if it.UpdatedAt > plan.HighWater {
					plan.HighWater = it.UpdatedAt
				}
			}
		}
		plan.NextOffset += len(out.UserBooks)
		if len(out.UserBooks) < hardcoverPageSize {
			plan.Complete = true
			return plan, nil
		}
	}
	return plan, nil
}

// ---- applying ----

// pullOptions are the reader's per-row choices.
type pullOptions struct {
	// UseRating lists Hardcover book ids whose rating replaces a local one.
	UseRating map[int64]bool
}

// pullDecision is what a pull did, or would do, for one library entry.
type pullDecision struct {
	Title, Author string
	HardcoverID   int64
	// Outcome is "new", "matched", or "skipped".
	Outcome string
	Key     string
	Changes []string
	Notes   []string
}

func (d *pullDecision) change(s string) { d.Changes = append(d.Changes, s) }
func (d *pullDecision) note(s string)   { d.Notes = append(d.Notes, s) }

// ratingChoice is a local rating that differs from Hardcover's, offered to
// the reader per row. The default keeps the local one.
type ratingChoice struct {
	HardcoverID int64   `json:"hardcover_id"`
	BookID      int64   `json:"book_id"`
	Title       string  `json:"title"`
	Local       float64 `json:"local"`
	Remote      float64 `json:"remote"`
}

type pullResult struct {
	New, Matched, Linked, Advanced, Readings int
	RatingsFilled, RatingsUsed               int
	Disagree, Ignored, Paused, Deleted       int
	Conflicts                                int
	RatingChoices                            []ratingChoice
	Changed                                  bool
}

func (r pullResult) Skipped() int { return r.Ignored + r.Paused + r.Deleted + r.Conflicts }

// hcAdvances is advances() plus Did Not Finish: a queued or started book may
// be set aside, and nothing ever moves backward.
func hcAdvances(from, to string) bool {
	if to == "abandoned" {
		return from == "to-read" || from == "reading"
	}
	return advances(from, to)
}

// applyPull applies a run to the library inside q. Preview and apply both run
// this function, so what the preview says is what applying does. It never
// changes positions or pins of books that are not moving, and every new book
// is appended as Needs placement.
func applyPull(q queryer, items []hcPullItem, opt pullOptions, onDecision func(pullDecision)) (out pullResult, err error) {
	now := canonicalTime(time.Now())
	for _, it := range items {
		d := pullDecision{Title: it.Title, Author: it.Author, HardcoverID: it.HardcoverBookID}
		done := func() {
			if onDecision != nil {
				onDecision(d)
			}
		}
		status, known := hardcoverStatuses[it.StatusID]
		if !known {
			status.Why = "Hardcover sent a status this version does not know."
		}
		if it.StatusID == 6 {
			out.Ignored++
			d.Outcome = "skipped"
			d.note(status.Why)
			done()
			continue
		}
		id, key, skip, err := pullMatch(q, it)
		if err != nil {
			return out, err
		}
		d.Key = key
		if skip != "" {
			d.Outcome = "skipped"
			d.note(skip)
			if strings.Contains(skip, "deleted") {
				out.Deleted++
			} else {
				out.Conflicts++
			}
			done()
			continue
		}
		if id == 0 {
			if status.Local == "" {
				out.Paused++
				d.Outcome = "skipped"
				d.note(cmp.Or(status.Why, "Not imported."))
				done()
				continue
			}
			rec := Record{Book: Book{Title: it.Title, Author: it.Author, ISBN: it.ISBN, Status: status.Local, Source: "hardcover", Interest: 3}}
			if it.Pages > 0 {
				p := it.Pages
				rec.Pages = &p
			}
			if it.Rating > 0 {
				rec.Interest = interestFor(it.Rating)
			}
			if _, err = importRecords(q, []Record{rec}); err != nil {
				return out, err
			}
			if id, _, err = lookupBook(q, rec, true); err != nil || id == 0 {
				return out, errors.Join(err, errors.New("a new Hardcover book was not found after adding it"))
			}
			out.New++
			out.Changed = true
			d.Outcome = "new"
			d.change("added as a new book (" + statusLabel(status.Local) + ", Needs placement if queued)")
		} else {
			out.Matched++
			d.Outcome = "matched"
		}
		linked, err := pullLink(q, id, it, now)
		if err != nil {
			return out, err
		}
		if linked == "linked" {
			out.Linked++
			out.Changed = true
			d.change("linked to Hardcover")
		} else if linked != "" {
			d.Outcome = "skipped"
			d.note(linked)
			out.Conflicts++
			done()
			continue
		}
		if d.Outcome == "matched" {
			if err = pullFill(q, id, it, &d, &out); err != nil {
				return out, err
			}
			if err = pullStatus(q, id, status.Local, status.Why, &d, &out); err != nil {
				return out, err
			}
		}
		if err = pullReadings(q, id, it, opt, &d, &out); err != nil {
			return out, err
		}
		done()
	}
	return out, nil
}

// pullMatch finds the local book for an entry: its Hardcover link, then the
// Hardcover id remembered for a deleted or merged book, then ISBN, then
// title and author, then the remembered identities. skip says why the entry
// must not be imported (a book the reader deleted).
func pullMatch(q queryer, it hcPullItem) (id int64, key, skip string, err error) {
	err = q.QueryRow("SELECT book_id FROM book_hardcover WHERE hardcover_book_id=?", it.HardcoverBookID).Scan(&id)
	if err == nil {
		return id, "Hardcover link", "", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", err
	}
	var target sql.NullInt64
	var live int
	err = q.QueryRow(`SELECT a.target_book_id,(SELECT count(*) FROM books b WHERE b.id=a.target_book_id) FROM hardcover_alias h JOIN book_aliases a ON a.identity=h.identity WHERE h.hardcover_book_id=? LIMIT 1`, it.HardcoverBookID).Scan(&target, &live)
	if err == nil {
		if target.Valid && live == 1 {
			return target.Int64, "merged-book record", "", nil
		}
		return 0, "merged-book record", "You deleted this book, so it is not brought back. Add it by hand to restore it.", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", err
	}
	m, err := matchBook(q, Record{Book: Book{Title: it.Title, Author: it.Author, ISBN: it.ISBN}}, true, false)
	if err != nil {
		return 0, "", "", err
	}
	if m.Tombstoned {
		return 0, m.Key, "You deleted this book, so it is not brought back. Add it by hand to restore it.", nil
	}
	return m.ID, m.Key, "", nil
}

// pullLink links a book to its Hardcover book. It returns "linked" when it
// did, "" when the link already existed, or a reason it cannot be linked.
func pullLink(q queryer, id int64, it hcPullItem, at string) (string, error) {
	l, err := getBookLink(q, id)
	if err != nil {
		return "", err
	}
	if l != nil {
		if l.HardcoverBookID != it.HardcoverBookID {
			return "This book is linked to a different Hardcover book than the one Hardcover returned. Your link was kept; relink it from the book page if Hardcover merged the book.", nil
		}
		return "", nil
	}
	owner, title, err := linkOwner(q, it.HardcoverBookID, id)
	if err != nil {
		return "", err
	}
	if owner != 0 {
		return fmt.Sprintf("This Hardcover book is already linked to “%s”.", title), nil
	}
	return "linked", saveBookLink(q, id, hcCandidate{BookID: it.HardcoverBookID, EditionID: it.EditionID, Slug: it.Slug}, at)
}

// pullFill fills only empty local fields.
func pullFill(q queryer, id int64, it hcPullItem, d *pullDecision, out *pullResult) error {
	if it.ISBN != "" {
		changed, err := execChanged(q, "UPDATE books SET isbn=? WHERE id=? AND isbn=''", it.ISBN, id)
		if err != nil {
			return err
		}
		if changed {
			out.Changed = true
			d.change("ISBN filled in")
		}
	}
	if it.Pages > 0 {
		changed, err := execChanged(q, "UPDATE books SET pages=? WHERE id=? AND pages IS NULL", it.Pages, id)
		if err != nil {
			return err
		}
		if changed {
			out.Changed = true
			d.change("page count filled in")
		}
	}
	return nil
}

// pullStatus moves a book forward when Hardcover is ahead, and otherwise says
// where Hardcover disagrees. It never moves a book backward.
func pullStatus(q queryer, id int64, want, why string, d *pullDecision, out *pullResult) error {
	var local Book
	if err := q.QueryRow("SELECT id,reading_status,pinned,needs_placement,position FROM books WHERE id=?", id).Scan(&local.ID, &local.Status, &local.Pinned, &local.NeedsPlacement, &local.Position); err != nil {
		return err
	}
	switch {
	case want == "":
		out.Paused++
		d.note(cmp.Or(why, "Status not imported.") + " Local status kept.")
	case local.Status == want:
	case local.Status == "unreviewed" || hcAdvances(local.Status, want):
		if err := setStatus(q, local, want); err != nil {
			return err
		}
		out.Advanced++
		out.Changed = true
		d.change("reading status " + statusLabel(local.Status) + " → " + statusLabel(want) + " (Hardcover is ahead)")
	default:
		out.Disagree++
		d.note("Hardcover disagrees: it says " + statusLabel(want) + ", you have " + statusLabel(local.Status) + ". Your status was kept.")
	}
	return nil
}

// pullReadings adds Hardcover's finished reads to reading history and applies
// its rating. A manual row is never overwritten, a local rating is replaced
// only when the reader ticked "use Hardcover's rating" for that row, and a
// month-only or year-only date keeps its precision.
func pullReadings(q queryer, bookID int64, it hcPullItem, opt pullOptions, d *pullDecision, out *pullResult) error {
	type finished struct {
		read        hcPullRead
		date        string
		precision   int
		fingerprint string
	}
	var reads []finished
	for _, r := range it.Reads {
		if date, p, ok := periodStart(r.FinishedAt, r.Precision); ok {
			reads = append(reads, finished{r, date, p, fmt.Sprintf("hardcover:read:%d", r.ID)})
		}
	}
	slices.SortStableFunc(reads, func(a, b finished) int { return cmp.Or(cmp.Compare(a.date, b.date), cmp.Compare(a.read.ID, b.read.ID)) })
	var lastRow int64
	for _, f := range reads {
		var rowID int64
		var source, finishedAt string
		var precision int
		err := q.QueryRow("SELECT id,source,COALESCE(finished_at,''),finished_precision FROM reading_log WHERE hardcover_read_id=?", f.read.ID).Scan(&rowID, &source, &finishedAt, &precision)
		switch {
		case err == nil:
			if source == "hardcover" && (finishedAt != f.date || precision != f.precision) {
				if _, err = q.Exec("UPDATE reading_log SET finished_at=?,finished_precision=? WHERE id=?", f.date, f.precision, rowID); err != nil {
					return err
				}
				out.Changed = true
				d.change("a finished reading's date updated from Hardcover")
			}
		case errors.Is(err, sql.ErrNoRows):
			if pushed, e := pushedRead(q, bookID, f.read.ID); e != nil {
				return e
			} else if pushed {
				// Hardcover made this read when this app sent the status; the
				// finished reading is already here with its own date.
				continue
			}
			// An existing reading of the same day is the same reading: tie it to
			// Hardcover without touching its date, rating, or source.
			err = q.QueryRow("SELECT id FROM reading_log WHERE book_id=? AND finished_at=? AND hardcover_read_id IS NULL ORDER BY id LIMIT 1", bookID, f.date).Scan(&rowID)
			if err == nil && f.precision == 1 {
				if _, err = q.Exec("UPDATE reading_log SET hardcover_read_id=? WHERE id=?", f.read.ID, rowID); err != nil {
					return err
				}
				out.Changed = true
			} else if err == nil || errors.Is(err, sql.ErrNoRows) {
				res, e := q.Exec("INSERT INTO reading_log(book_id,finished_at,source,fingerprint,finished_precision,hardcover_read_id) VALUES(?,?,'hardcover',?,?,?)", bookID, f.date, f.fingerprint, f.precision, f.read.ID)
				if e != nil {
					return e
				}
				if rowID, e = res.LastInsertId(); e != nil {
					return e
				}
				out.Readings++
				out.Changed = true
				d.change("finished reading added to your reading history")
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		lastRow = rowID
	}
	if len(reads) == 0 && it.StatusID == 3 {
		// Marked Read with no dated read: one undated reading, once.
		fp := fmt.Sprintf("hardcover:book:%d", it.HardcoverBookID)
		var n int
		if err := q.QueryRow("SELECT count(*) FROM reading_log WHERE book_id=?", bookID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			res, err := q.Exec("INSERT INTO reading_log(book_id,source,fingerprint) VALUES(?,'hardcover',?)", bookID, fp)
			if err != nil {
				return err
			}
			if lastRow, err = res.LastInsertId(); err != nil {
				return err
			}
			out.Readings++
			out.Changed = true
			d.change("finished reading (date unknown) added to your reading history")
		} else if err := q.QueryRow("SELECT id FROM reading_log WHERE book_id=? ORDER BY finished_at IS NULL, finished_at DESC, id DESC LIMIT 1", bookID).Scan(&lastRow); err != nil {
			return err
		}
	}
	if lastRow == 0 || it.Rating <= 0 {
		return nil
	}
	var source string
	var rating sql.NullFloat64
	var bookTitle string
	if err := q.QueryRow("SELECT l.source,l.rating,b.title FROM reading_log l JOIN books b ON b.id=l.book_id WHERE l.id=?", lastRow).Scan(&source, &rating, &bookTitle); err != nil {
		return err
	}
	switch {
	case rating.Valid && (rating.Float64 == it.Rating || quarterStars(halfStars(rating.Float64)) == it.Rating):
		// Same rating, or the one this app would send for it: not a disagreement.
	case !rating.Valid || source == "hardcover":
		if _, err := q.Exec("UPDATE reading_log SET rating=? WHERE id=?", it.Rating, lastRow); err != nil {
			return err
		}
		out.RatingsFilled++
		out.Changed = true
		d.change(fmt.Sprintf("rating %.2g from Hardcover saved", it.Rating))
	case opt.UseRating[it.HardcoverBookID]:
		if _, err := q.Exec("UPDATE reading_log SET rating=? WHERE id=?", it.Rating, lastRow); err != nil {
			return err
		}
		out.RatingsUsed++
		out.Changed = true
		d.change(fmt.Sprintf("rating changed from %.2g to %.2g (you chose Hardcover's)", rating.Float64, it.Rating))
	default:
		out.RatingChoices = append(out.RatingChoices, ratingChoice{it.HardcoverBookID, bookID, bookTitle, rating.Float64, it.Rating})
		d.note(fmt.Sprintf("Your rating (%.2g) differs from Hardcover's (%.2g); yours is kept unless you choose Hardcover's.", rating.Float64, it.Rating))
	}
	return nil
}

// previewPull runs applyPull in a savepoint and rolls it back.
func previewPull(q queryer, items []hcPullItem, opt pullOptions) (pullResult, []pullDecision, error) {
	if _, err := q.Exec("SAVEPOINT pull_preview"); err != nil {
		return pullResult{}, nil, err
	}
	var decisions []pullDecision
	res, err := applyPull(q, items, opt, func(d pullDecision) { decisions = append(decisions, d) })
	if _, e := q.Exec("ROLLBACK TO pull_preview; RELEASE pull_preview"); err == nil {
		err = e
	}
	return res, decisions, err
}
