package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Progress and read dates are the one place both sides change often, so each
// direction is explicit. Progress goes to Hardcover only for a book the reader
// opted in, when they save it, and comes back only through "Pull progress".
// Read dates go only through "Send this reading". Neither changes status,
// pins, queue order, loans, or ranking, and neither writes a journal entry:
// the Hardcover server does that itself when a read starts, advances, or ends.
//
// Mutation shapes come from the official SDK (see testdata/hardcover/):
// update_user_book_read(id:, object: DatesReadInput) for progress and dates,
// insert_user_book_read(user_book_id:, user_book_read: DatesReadInput).

const hcOpenReadQuery = `query NextChapterOpenRead($uid: Int!, $book: Int!) {
 user_books(where: {user_id: {_eq: $uid}, book_id: {_eq: $book}}, limit: 1) {
  id status_id
  book { pages }
  edition { pages audio_seconds }
  user_book_reads(where: {finished_at: {_is_null: true}}, order_by: {id: desc}, limit: 1) {
   id progress_pages progress_seconds
   edition { pages audio_seconds }
  }
 }
}`

// hcOpenRead is the read in progress on Hardcover, with the length of its
// edition. ReadID is 0 when there is none.
type hcOpenRead struct {
	UserBookID, ReadID int64
	StatusID           int
	Pages, Seconds     int // progress
	EdPages, EdAudio   int // length of the edition being read
}

type hcLength struct {
	Pages flexInt `json:"pages"`
	Audio flexInt `json:"audio_seconds"`
}

// openRead is one request. It returns nil when the book is not on the
// reader's shelves.
func (c *hardcoverClient) openRead(ctx context.Context, userID, hcBook int64) (*hcOpenRead, error) {
	var out struct {
		UserBooks []struct {
			ID     flexInt   `json:"id"`
			Status flexInt   `json:"status_id"`
			Book   *hcLength `json:"book"`
			Ed     *hcLength `json:"edition"`
			Reads  []struct {
				ID      flexInt   `json:"id"`
				Pages   flexInt   `json:"progress_pages"`
				Seconds flexInt   `json:"progress_seconds"`
				Ed      *hcLength `json:"edition"`
			} `json:"user_book_reads"`
		} `json:"user_books"`
	}
	if err := c.do(ctx, hcOpenReadQuery, map[string]any{"uid": userID, "book": hcBook}, &out); err != nil {
		return nil, err
	}
	if len(out.UserBooks) == 0 {
		return nil, nil
	}
	u := out.UserBooks[0]
	r := &hcOpenRead{UserBookID: int64(u.ID), StatusID: int(u.Status)}
	// Lengths belong to one edition. In particular, an audiobook must not
	// inherit the book's print page count, or progress would be sent as pages.
	useLength := func(ed *hcLength) {
		if ed != nil {
			r.EdPages, r.EdAudio = int(ed.Pages), int(ed.Audio)
		}
	}
	useLength(u.Book)
	useLength(u.Ed)
	if len(u.Reads) > 0 {
		rd := u.Reads[0]
		r.ReadID, r.Pages, r.Seconds = int64(rd.ID), int(rd.Pages), int(rd.Seconds)
		useLength(rd.Ed)
	}
	return r, nil
}

// ---- conversion: pure ----

// localProgressKey and remoteProgressKey name a progress value for the sync
// history, so "who changed since the last sync" is a string comparison.
func localProgressKey(b Book) string {
	switch {
	case b.ProgressSeconds != nil:
		return "s:" + strconv.Itoa(*b.ProgressSeconds)
	case b.ProgressPercent != nil:
		return "%:" + strconv.Itoa(*b.ProgressPercent)
	case b.ProgressPage != nil:
		return "p:" + strconv.Itoa(*b.ProgressPage)
	}
	return ""
}

func remoteProgressKey(r hcOpenRead) string {
	if r.EdAudio > 0 || r.Seconds > 0 && r.Pages == 0 {
		return "s:" + strconv.Itoa(r.Seconds)
	}
	if r.Pages > 0 {
		return "p:" + strconv.Itoa(r.Pages)
	}
	return ""
}

// remoteProgress is what would be sent: pages or, for an audiobook, seconds.
type remoteProgress struct {
	Pages, Seconds int
	Audio          bool
	Note, Skip     string
}

func ceilDiv(pct float64, whole int) int { return int(math.Ceil(pct * float64(whole) / 100)) }

// progressToRemote converts the reader's progress to what Hardcover stores.
// Hardcover has no percent field, so a percentage, or pages in an edition of a
// different length, is converted through the percentage and the preview says so.
func progressToRemote(b Book, r hcOpenRead) remoteProgress {
	if r.EdAudio > 0 {
		// Audio length identifies an audiobook even if catalog data also has pages.
		r.EdPages = 0
	}
	out := remoteProgress{Audio: r.EdAudio > 0}
	local := 0
	if b.Pages != nil {
		local = *b.Pages
	}
	pct := -1.0
	switch {
	case b.ProgressSeconds != nil:
		if r.EdAudio == 0 && r.EdPages > 0 {
			out.Skip = "Hardcover's selected edition is print. Choose an audiobook edition there before sending elapsed time."
			break
		}
		out.Audio, out.Seconds = true, *b.ProgressSeconds
		if r.EdAudio > 0 && out.Seconds > r.EdAudio {
			out.Seconds = r.EdAudio
			out.Note = "The position is past the end of Hardcover's audiobook, so its full duration is sent."
		}
	case b.ProgressPercent != nil:
		pct = float64(*b.ProgressPercent)
	case b.ProgressPage != nil:
		page := *b.ProgressPage
		switch {
		case r.EdPages > 0 && local == r.EdPages, r.EdPages > 0 && local == 0, r.EdPages == 0 && r.EdAudio == 0:
			out.Pages = page
			if local == 0 {
				out.Note = fmt.Sprintf("Your book has no page count, so page %d is sent as it is.", page)
			}
		case local > 0:
			pct = math.Min(100, float64(page)*100/float64(local))
		default:
			out.Skip = "Hardcover's edition is an audiobook and your book has no page count to convert from."
		}
	default:
		out.Skip = "There is no progress to send."
	}
	if pct >= 0 {
		switch {
		case r.EdPages > 0:
			out.Pages = ceilDiv(pct, r.EdPages)
			out.Note = fmt.Sprintf("Converted through %.0f%%: Hardcover's edition has %d pages, so page %d is sent.", pct, r.EdPages, out.Pages)
		case r.EdAudio > 0:
			out.Seconds = ceilDiv(pct, r.EdAudio)
			out.Note = fmt.Sprintf("Converted through %.0f%% of Hardcover's audiobook length: %d seconds is sent.", pct, out.Seconds)
		default:
			out.Skip = "Hardcover's edition has no page count or length to convert a percentage with."
		}
	}
	if out.Skip == "" && r.EdPages > 0 && out.Pages > r.EdPages {
		out.Note = fmt.Sprintf("Page %d is past the end of Hardcover's edition (%d pages), so %d is sent.", out.Pages, r.EdPages, r.EdPages)
		out.Pages = r.EdPages
	}
	if out.Skip == "" && r.EdPages > 0 && out.Pages == r.EdPages {
		out.Note = strings.TrimSpace(out.Note + " Reaching the end finishes the read on Hardcover, which then marks the book Read there.")
	}
	if out.Skip == "" && out.Audio && r.EdAudio > 0 && out.Seconds == r.EdAudio {
		out.Note = strings.TrimSpace(out.Note + " Reaching the end finishes the read on Hardcover, which then marks the book Read there.")
	}
	return out
}

// localProgress is what a pull would store: a page or a percentage, in the
// unit the reader already uses for the book.
type localProgress struct {
	Page, Percent, Seconds *int
	Note, Skip             string
}

func progressFromRemote(b Book, r hcOpenRead) localProgress {
	var out localProgress
	if b.ProgressSeconds != nil {
		if r.EdAudio > 0 || r.Seconds > 0 && r.Pages == 0 {
			seconds := r.Seconds
			if seconds < 0 || seconds > maxProgressSeconds {
				out.Skip = "Hardcover’s audiobook position is out of range."
				return out
			}
			out.Seconds = &seconds
		} else {
			out.Skip = "Hardcover's selected edition has no audiobook progress to pull as elapsed time."
		}
		return out
	}
	local := 0
	if b.Pages != nil {
		local = *b.Pages
	}
	pct := -1.0
	switch {
	case r.EdAudio > 0 && (r.Seconds > 0 || r.Pages > 0):
		pct = math.Min(100, float64(r.Seconds)*100/float64(r.EdAudio))
	case r.Pages > 0 && r.EdPages > 0:
		pct = math.Min(100, float64(r.Pages)*100/float64(r.EdPages))
	case r.Pages == 0 && r.Seconds == 0:
		out.Skip = "Hardcover has no progress for this read."
		return out
	}
	toPercent := func() {
		p := int(math.Round(pct))
		out.Percent = &p
	}
	switch {
	case b.ProgressUnit() == "percent" && pct >= 0:
		toPercent()
	case b.ProgressUnit() == "percent":
		out.Skip = "Hardcover's edition has no length to turn its progress into a percentage."
	case local > 0 && r.Pages > 0 && r.EdPages == local:
		p := min(r.Pages, local)
		out.Page = &p
	case local > 0 && pct >= 0:
		p := int(math.Round(pct * float64(local) / 100))
		out.Page = &p
		out.Note = fmt.Sprintf("Converted through %.0f%%: your edition has %d pages, so page %d.", pct, local, p)
	case local > 0:
		p := min(r.Pages, local)
		out.Page = &p
		out.Note = "Hardcover's edition has no page count, so its page is used as it is."
	case r.Pages > 0:
		p := r.Pages
		out.Page = &p
		out.Note = "Your book has no page count, so Hardcover's page is used as it is."
	case pct >= 0:
		toPercent()
	default:
		out.Skip = "This progress cannot be converted."
	}
	return out
}

// progressState compares the two sides against the last sync.
//
//	same: nothing differs; remote: only Hardcover changed (or this side has
//	nothing); local: only this library changed; both: both changed, or there
//	is no history to tell. Only "remote" pulls by default.
func progressState(last *hcSyncRow, localKey, remoteKey string, equal bool) string {
	switch {
	case equal:
		return "same"
	case localKey == "":
		return "remote"
	case last == nil:
		return "both"
	}
	localChanged, remoteChanged := localKey != last.Local, remoteKey != last.Remote
	switch {
	case localChanged && remoteChanged:
		return "both"
	case localChanged:
		return "local"
	}
	return "remote"
}

// lastSync is the newest history row for a book and field, or nil.
func lastSync(q queryer, bookID int64, field string) (*hcSyncRow, error) {
	var r hcSyncRow
	err := q.QueryRow("SELECT book_id,field,local_value,remote_value,direction,at FROM hardcover_sync WHERE book_id=? AND field=? ORDER BY id DESC LIMIT 1", bookID, field).Scan(&r.BookID, &r.Field, &r.Local, &r.Remote, &r.Direction, &r.At)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

func addSync(q queryer, bookID int64, field, local, remote, direction, at string) error {
	if _, err := q.Exec("INSERT INTO hardcover_sync(book_id,field,local_value,remote_value,direction,at) VALUES(?,?,?,?,?,?)", bookID, field, local, remote, direction, at); err != nil {
		return err
	}
	_, err := q.Exec("DELETE FROM hardcover_sync WHERE book_id=? AND id NOT IN (SELECT id FROM hardcover_sync WHERE book_id=? ORDER BY id DESC LIMIT ?)", bookID, bookID, hardcoverSyncKeep)
	return err
}

// ---- mutations ----

const hcMutationBody = ` { id error user_book_read { id finished_at finished_at_precision progress_pages progress_seconds } }`

const (
	hcUpdateReadMutation = `mutation NextChapterUpdateRead($id: Int!, $read: DatesReadInput!) {
 update: update_user_book_read(id: $id, object: $read)` + hcMutationBody + `
}`
	hcInsertReadMutation = `mutation NextChapterInsertRead($ub: Int!, $read: DatesReadInput!) {
 insert: insert_user_book_read(user_book_id: $ub, user_book_read: $read)` + hcMutationBody + `
}`
)

type hcReadResult struct {
	Error json.RawMessage `json:"error"`
	Read  *struct {
		ID flexInt `json:"id"`
	} `json:"user_book_read"`
}

func (r hcReadResult) failed() bool {
	e := strings.TrimSpace(string(r.Error))
	return r.Read == nil || (e != "" && e != "null" && e != `""`)
}

// readMutation sends one mutation and returns the id of the read it touched.
// ambiguous is true when the write may have landed (a timeout, an unreadable
// answer), so the caller must say to look before sending again.
func (c *hardcoverClient) readMutation(ctx context.Context, query string, vars map[string]any) (id int64, ambiguous bool, err error) {
	var out map[string]hcReadResult
	if err = c.do(ctx, query, vars, &out); err != nil {
		return 0, !definiteRefusal(err), err
	}
	res := out["update"]
	if r, ok := out["insert"]; ok {
		res = r
	}
	if res.failed() {
		return 0, false, errHardcoverRefused
	}
	return int64(res.Read.ID), false, nil
}

const errHardcoverRefused = hardcoverError("Hardcover did not accept this change. Nothing else was affected.")

// progressVars is the whole body of a progress mutation: the read id, the
// progress, and, only when the update is dated earlier than now, action_at.
func progressVars(readID int64, p remoteProgress, actionAt string) map[string]any {
	read := map[string]any{}
	if p.Audio || p.Seconds > 0 {
		read["progress_seconds"] = p.Seconds
	} else {
		read["progress_pages"] = p.Pages
	}
	if actionAt != "" {
		read["action_at"] = actionAt
	}
	return map[string]any{"id": readID, "read": read}
}
