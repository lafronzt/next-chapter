package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Sending to Hardcover is the outbound half of the sync. It is always reader
// initiated and previewed. Only a book's Hardcover ids, its shelf status, its
// rating, and (for a new row) a privacy setting are ever sent. Reviews, notes,
// reasons, queue position, pins, interest, estimates, loans and the reading
// log stay here. The Hardcover server starts and finishes reads and writes the
// journal itself when a status changes, so this code never does.
//
// The shapes below come from Hardcover's own SDK (@hardcover/sdk 0.1.1):
// insert_user_book(object: UserBookCreateInput!) and
// update_user_book(id:, object: UserBookUpdateInput!), each returning
// { id, error, user_book }. See testdata/hardcover/.

// hcStatusNames are Hardcover's shelf names, for the preview.
var hcStatusNames = map[int]string{1: "Want to Read", 2: "Currently Reading", 3: "Read", 4: "Paused", 5: "Did Not Finish", 6: "Ignored"}

// pushStatuses is the local status each Hardcover status id stands for when
// sending; archived and unreviewed books have no shelf on Hardcover.
var pushStatuses = map[string]int{"to-read": 1, "reading": 2, "read": 3, "abandoned": 5}

// hardcoverPushBatch is how many books one request carries. Each mutation is
// one top-level field, and the preview already spent one request of the burst
// of 10, so a batch of 9 can follow it at once.
const hardcoverPushBatch = 9

// hardcoverPushLimit is the most books one preview covers.
const hardcoverPushLimit = 50

// hardcoverSyncKeep is how many history rows are kept for one book.
const hardcoverSyncKeep = 20

// halfStars rounds a quarter-star rating to the nearest half star, the finest
// step Hardcover takes. Halves round up (4.25 is sent as 4.5). The rounded
// value is only ever sent, never written back locally.
func halfStars(r float64) float64 {
	if r <= 0 || math.IsNaN(r) {
		return 0
	}
	return math.Min(5, math.Max(0.5, math.Floor(r*2+0.5)/2))
}

// hcPrivacy is the privacy setting for new rows: private unless the reader
// chose otherwise.
func hcPrivacy(p Preferences) int {
	if p.HardcoverPrivacy >= 1 && p.HardcoverPrivacy <= 3 {
		return p.HardcoverPrivacy
	}
	return 3
}

var hcPrivacyNames = map[int]string{1: "public", 2: "followers only", 3: "private"}

// hcPushItem is one book in a send preview. Action is add, update, none,
// conflict, or skip. State is blank until a send, then sent, failed (a
// definite refusal; can be retried), or unknown (a timeout; checked before any
// retry).
type hcPushItem struct {
	BookID          int64   `json:"book_id"`
	Title           string  `json:"title"`
	Author          string  `json:"author"`
	HardcoverBookID int64   `json:"hardcover_book_id"`
	EditionID       int64   `json:"edition_id,omitempty"`
	UserBookID      int64   `json:"user_book_id,omitempty"`
	LocalStatus     string  `json:"local_status"`
	Action          string  `json:"action"`
	Target          int     `json:"target,omitempty"`
	LocalRating     float64 `json:"local_rating,omitempty"`
	SendRating      float64 `json:"send_rating,omitempty"`
	Privacy         int     `json:"privacy,omitempty"`
	// Remote is what Hardcover had when the preview was made.
	RemoteStatus int      `json:"remote_status,omitempty"`
	RemoteRating float64  `json:"remote_rating,omitempty"`
	ReadIDs      []int64  `json:"read_ids,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	Conflict     string   `json:"conflict,omitempty"`
	State        string   `json:"state,omitempty"`
	Result       string   `json:"result,omitempty"`
	// newReads are the reads Hardcover created for this send; used within one
	// request to record them, never stored in the plan.
	newReads []int64
}

// hcPushPlan is a previewed send. Stopped says why a run ended early.
type hcPushPlan struct {
	UserID  int64        `json:"user_id"`
	Items   []hcPushItem `json:"items"`
	Stopped string       `json:"stopped,omitempty"`
}

// Counts for the preview and the result.
func (p hcPushPlan) count(f func(hcPushItem) bool) (n int) {
	for _, it := range p.Items {
		if f(it) {
			n++
		}
	}
	return n
}
func (it hcPushItem) sendable() bool { return it.Action == "add" || it.Action == "update" }

// Pending is a book still to send. Unknown ones are checked first.
func (it hcPushItem) pending() bool {
	return it.sendable() && (it.State == "" || it.State == "failed")
}
func (p hcPushPlan) Pending() int { return p.count(hcPushItem.pending) }
func (p hcPushPlan) Sent() int    { return p.count(func(i hcPushItem) bool { return i.State == "sent" }) }
func (p hcPushPlan) Unknown() int {
	return p.count(func(i hcPushItem) bool { return i.State == "unknown" })
}
func (p hcPushPlan) Failed() int {
	return p.count(func(i hcPushItem) bool { return i.State == "failed" })
}
func (p hcPushPlan) Conflicts() int {
	return p.count(func(i hcPushItem) bool { return i.Action == "conflict" })
}
func (p hcPushPlan) Unchanged() int {
	return p.count(func(i hcPushItem) bool { return i.Action == "none" || i.Action == "skip" })
}
func (p hcPushPlan) Sendable() int { return p.count(hcPushItem.sendable) }

// Labels for the template.
func (it hcPushItem) LocalLabel() string  { return statusLabel(it.LocalStatus) }
func (it hcPushItem) RemoteLabel() string { return hcStatusNames[it.RemoteStatus] }
func (it hcPushItem) TargetLabel() string { return hcStatusNames[it.Target] }
func (it hcPushItem) PrivacyLabel() string {
	return hcPrivacyNames[it.Privacy]
}
func (it hcPushItem) Rounded() bool { return it.SendRating > 0 && it.SendRating != it.LocalRating }

// hcRemoteEntry is the user's row for one book on Hardcover.
type hcRemoteEntry struct {
	UserBookID int64
	StatusID   int
	Rating     float64
	ReadIDs    []int64
}

func remoteEntry(u hcUserBookJSON) hcRemoteEntry {
	e := hcRemoteEntry{UserBookID: int64(u.ID), StatusID: int(u.StatusID)}
	if u.Rating != nil {
		e.Rating = *u.Rating
	}
	for _, r := range u.Reads {
		if r.ID > 0 {
			e.ReadIDs = append(e.ReadIDs, int64(r.ID))
		}
	}
	return e
}

// hcLocalBook is what the preview reads locally for one linked book.
type hcLocalBook struct {
	ID              int64
	Title, Author   string
	Status          string
	HardcoverBookID int64
	EditionID       int64
	Rating          float64 // latest finished reading's rating; 0 when none
	// SyncedRating is the rating last sent for this book, 0 when never.
	SyncedRating float64
}

// hcRank orders the shelves a book moves along. A status with no rank (Paused,
// Ignored) cannot be compared, so it is never overwritten.
func hcRank(status int) int {
	switch status {
	case 1:
		return 1
	case 2:
		return 2
	case 3, 5:
		return 3
	}
	return 0
}

// classifyPush decides what sending would do for one book. It is pure: the
// remote entry was fetched just before, and nothing here writes. Hardcover
// being ahead of, or different from, this library is a conflict: nothing is
// sent for the book unless the reader resolves it.
func classifyPush(l hcLocalBook, remote *hcRemoteEntry, privacy int) hcPushItem {
	it := hcPushItem{BookID: l.ID, Title: l.Title, Author: l.Author, HardcoverBookID: l.HardcoverBookID, EditionID: l.EditionID, LocalStatus: l.Status, LocalRating: l.Rating, Action: "none"}
	target, ok := pushStatuses[l.Status]
	if !ok {
		it.Action = "skip"
		it.Notes = append(it.Notes, "“"+statusLabel(l.Status)+"” has no shelf on Hardcover, so nothing is sent.")
		return it
	}
	if remote != nil {
		it.UserBookID, it.RemoteStatus, it.RemoteRating, it.ReadIDs = remote.UserBookID, remote.StatusID, remote.Rating, remote.ReadIDs
	}
	// A rating goes with a Read book only, and a rating is never cleared.
	send := 0.0
	if target == 3 && l.Rating > 0 {
		send = halfStars(l.Rating)
	}
	if remote == nil {
		it.Action, it.Target, it.Privacy, it.SendRating = "add", target, privacy, send
		it.Notes = append(it.Notes, fmt.Sprintf("Not on your Hardcover shelves yet. It will be added as %s, %s.", hcStatusNames[target], hcPrivacyNames[privacy]))
		if privacy == 1 {
			it.Notes = append(it.Notes, "Public rows show on your Hardcover profile.")
		}
		it.noteRounding(l.Rating)
		return it
	}
	if remote.StatusID != target && (hcRank(remote.StatusID) == 0 || hcRank(remote.StatusID) >= hcRank(target)) {
		it.Action = "conflict"
		it.Conflict = fmt.Sprintf("Hardcover has this as %s, which is ahead of or different from %s here.", hcStatusNames[remote.StatusID], statusLabel(l.Status))
		return it
	}
	if send > 0 && remote.Rating > 0 && remote.Rating != send && remote.Rating != l.SyncedRating {
		it.Action = "conflict"
		it.Conflict = fmt.Sprintf("Hardcover already has a rating of %s that this app did not send.", trimStars(remote.Rating))
		return it
	}
	if remote.StatusID != target {
		it.Target = target
		it.Notes = append(it.Notes, fmt.Sprintf("Moves from %s to %s on Hardcover. Hardcover records the start or finish itself.", hcStatusNames[remote.StatusID], hcStatusNames[target]))
	}
	if send > 0 && remote.Rating != send {
		it.SendRating = send
		it.noteRounding(l.Rating)
	}
	if it.Target != 0 || it.SendRating != 0 {
		it.Action = "update"
	}
	return it
}

func (it *hcPushItem) noteRounding(local float64) {
	if it.SendRating > 0 {
		if it.SendRating != local {
			it.Notes = append(it.Notes, fmt.Sprintf("Rating %s will be sent as %s. Your local rating is not changed.", trimStars(local), trimStars(it.SendRating)))
		} else {
			it.Notes = append(it.Notes, "Rating "+trimStars(it.SendRating)+" will be sent.")
		}
	}
}

func trimStars(r float64) string { return strconv.FormatFloat(r, 'f', -1, 64) }

// ---- reading remote and local state ----

const hcShelfFields = `id book_id status_id rating privacy_setting_id updated_at
  book { id title slug pages contributions { author { name } } }
  edition { id isbn_13 isbn_10 pages }
  user_book_reads(order_by: {id: asc}) { id finished_at finished_at_precision }`

const hcShelfQuery = `query NextChapterShelf($uid: Int!, $ids: [Int!]!) {
 user_books(where: {user_id: {_eq: $uid}, book_id: {_in: $ids}}, limit: 50) {
  ` + hcShelfFields + `
 }
}`

// shelf reads the reader's own rows for the given Hardcover books, one
// request however many books.
func (c *hardcoverClient) shelf(ctx context.Context, userID int64, bookIDs []int64) (map[int64]hcUserBookJSON, error) {
	var out struct {
		UserBooks []hcUserBookJSON `json:"user_books"`
	}
	if err := c.do(ctx, hcShelfQuery, map[string]any{"uid": userID, "ids": bookIDs}, &out); err != nil {
		return nil, err
	}
	rows := map[int64]hcUserBookJSON{}
	for _, u := range out.UserBooks {
		if u.BookID > 0 {
			rows[int64(u.BookID)] = u
		}
	}
	return rows, nil
}

// loadPushBooks reads the linked books in ids, in the order given. A book that
// is not linked is reported by id so the caller can say so.
func loadPushBooks(q queryer, ids []int64) (books []hcLocalBook, unlinked []int64, err error) {
	for _, id := range ids {
		var l hcLocalBook
		var edition sql.NullInt64
		err = q.QueryRow(`SELECT b.id,b.title,b.author,b.reading_status,h.hardcover_book_id,h.hardcover_edition_id FROM books b JOIN book_hardcover h ON h.book_id=b.id WHERE b.id=?`, id).Scan(&l.ID, &l.Title, &l.Author, &l.Status, &l.HardcoverBookID, &edition)
		if errors.Is(err, sql.ErrNoRows) {
			unlinked = append(unlinked, id)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		l.EditionID = edition.Int64
		var rating sql.NullFloat64
		err = q.QueryRow("SELECT rating FROM reading_log WHERE book_id=? AND rating IS NOT NULL ORDER BY finished_at IS NULL, substr(finished_at,1,4) DESC, finished_precision=3, finished_at DESC, id DESC LIMIT 1", id).Scan(&rating)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		l.Rating = rating.Float64
		var synced string
		err = q.QueryRow("SELECT remote_value FROM hardcover_sync WHERE book_id=? AND field='rating' AND direction='push' ORDER BY id DESC LIMIT 1", id).Scan(&synced)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		l.SyncedRating, _ = strconv.ParseFloat(synced, 64)
		books = append(books, l)
	}
	return books, unlinked, nil
}

// buildPushPlan makes the preview from fresh remote state.
func buildPushPlan(books []hcLocalBook, remote map[int64]hcUserBookJSON, userID int64, privacy int) hcPushPlan {
	plan := hcPushPlan{UserID: userID}
	for _, l := range books {
		var re *hcRemoteEntry
		if u, ok := remote[l.HardcoverBookID]; ok {
			e := remoteEntry(u)
			re = &e
		}
		plan.Items = append(plan.Items, classifyPush(l, re, privacy))
	}
	return plan
}

// ---- sending ----

const hcMutationReturn = ` { id error user_book { id status_id rating privacy_setting_id user_book_reads(order_by: {id: asc}) { id } } }`

// pushRequest builds one request of at most hardcoverPushBatch mutations. Every
// value travels as a variable, and only ids, status, rating, and privacy are
// ever put in one.
func pushRequest(items []*hcPushItem) (string, map[string]any) {
	var decls, body []string
	vars := map[string]any{}
	for i, it := range items {
		obj := map[string]any{}
		if it.Target != 0 {
			obj["status_id"] = it.Target
		}
		if it.SendRating > 0 {
			obj["rating"] = it.SendRating
		}
		o := fmt.Sprintf("o%d", i)
		if it.Action == "add" {
			obj["book_id"] = it.HardcoverBookID
			obj["privacy_setting_id"] = it.Privacy
			if it.EditionID > 0 {
				obj["edition_id"] = it.EditionID
			}
			decls = append(decls, "$"+o+": UserBookCreateInput!")
			body = append(body, fmt.Sprintf("m%d: insert_user_book(object: $%s)%s", i, o, hcMutationReturn))
		} else {
			vars[fmt.Sprintf("i%d", i)] = it.UserBookID
			decls = append(decls, fmt.Sprintf("$i%d: Int!", i), "$"+o+": UserBookUpdateInput!")
			body = append(body, fmt.Sprintf("m%d: update_user_book(id: $i%d, object: $%s)%s", i, i, o, hcMutationReturn))
		}
		vars[o] = obj
	}
	return "mutation NextChapterPush(" + strings.Join(decls, ", ") + ") {\n " + strings.Join(body, "\n ") + "\n}", vars
}

type hcMutationResult struct {
	Error    json.RawMessage `json:"error"`
	UserBook *struct {
		ID     flexInt `json:"id"`
		Status flexInt `json:"status_id"`
		Reads  []struct {
			ID flexInt `json:"id"`
		} `json:"user_book_reads"`
	} `json:"user_book"`
}

func (r hcMutationResult) failed() bool {
	e := strings.TrimSpace(string(r.Error))
	return r.UserBook == nil || (e != "" && e != "null" && e != `""`)
}

// definiteRefusal reports errors after which the mutation certainly did not
// happen. Anything else (a timeout, a dropped connection, an unreadable
// answer) may have been applied, so it is checked before any retry.
func definiteRefusal(err error) bool {
	var rate hardcoverRateError
	var scope hardcoverScopeError
	return errors.As(err, &rate) || errors.As(err, &scope) || errors.Is(err, errHardcoverAuth) || errors.Is(err, errHardcoverCapacity)
}

// pushBatch sends one request. It updates the items' states and returns the
// request-level error, if any. It never retries.
func (c *hardcoverClient) pushBatch(ctx context.Context, items []*hcPushItem) error {
	query, vars := pushRequest(items)
	var out map[string]hcMutationResult
	if err := c.do(ctx, query, vars, &out); err != nil {
		if !definiteRefusal(err) {
			for _, it := range items {
				it.State, it.Result = "unknown", "Hardcover did not answer. It will be checked before this book is sent again."
			}
		}
		return err
	}
	for i, it := range items {
		res, ok := out[fmt.Sprintf("m%d", i)]
		if !ok || res.failed() {
			it.State, it.Result = "failed", "Hardcover did not accept this change. Nothing else was affected."
			continue
		}
		it.State, it.Result = "sent", "Sent."
		for _, r := range res.UserBook.Reads {
			if r.ID > 0 && !slices.Contains(it.ReadIDs, int64(r.ID)) {
				it.ReadIDs = append(it.ReadIDs, int64(r.ID))
				it.newReads = append(it.newReads, int64(r.ID))
			}
		}
		if res.UserBook.ID > 0 {
			it.UserBookID = int64(res.UserBook.ID)
		}
	}
	return nil
}

// verifyUnknown re-reads Hardcover for books whose send timed out. A book that
// now looks as intended counts as sent; any other goes back to pending.
func (c *hardcoverClient) verifyUnknown(ctx context.Context, userID int64, items []*hcPushItem) error {
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.HardcoverBookID)
	}
	remote, err := c.shelf(ctx, userID, ids)
	if err != nil {
		return err
	}
	for _, it := range items {
		u, ok := remote[it.HardcoverBookID]
		e := remoteEntry(u)
		landed := ok && (it.Target == 0 || e.StatusID == it.Target) && (it.SendRating == 0 || e.Rating == it.SendRating)
		if !landed {
			it.State, it.Result = "", "Hardcover shows this was not applied. It can be sent again."
			continue
		}
		it.State, it.Result = "sent", "Sent (confirmed on Hardcover after a timeout)."
		for _, id := range e.ReadIDs {
			if !slices.Contains(it.ReadIDs, id) {
				it.ReadIDs = append(it.ReadIDs, id)
				it.newReads = append(it.newReads, id)
			}
		}
		it.UserBookID = e.UserBookID
	}
	return nil
}

// ---- local record ----

// hcSyncRow is one remembered send (or, later, pull).
type hcSyncRow struct {
	BookID    int64  `json:"book_id"`
	Field     string `json:"field"`
	Local     string `json:"local_value"`
	Remote    string `json:"remote_value"`
	Direction string `json:"direction"`
	At        string `json:"at"`
}

func loadHardcoverSync(q queryer) ([]hcSyncRow, error) {
	rows, err := q.Query("SELECT book_id,field,local_value,remote_value,direction,at FROM hardcover_sync ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hcSyncRow
	for rows.Next() {
		var r hcSyncRow
		if err = rows.Scan(&r.BookID, &r.Field, &r.Local, &r.Remote, &r.Direction, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resetSync forgets what was sent for a book. It is called when the link
// changes, because history about one Hardcover book says nothing of another.
func resetSync(q queryer, bookID int64) error {
	_, err := q.Exec("DELETE FROM hardcover_sync WHERE book_id=?", bookID)
	return err
}

// recordPush remembers what a successful send changed, so a later pull can
// tell that Hardcover did not change it, and ignore the read Hardcover
// created for it.
func recordPush(q queryer, it hcPushItem, at string) error {
	add := func(field, local, remote string) error {
		_, err := q.Exec("INSERT INTO hardcover_sync(book_id,field,local_value,remote_value,direction,at) VALUES(?,?,?,?,'push',?)", it.BookID, field, local, remote, at)
		return err
	}
	if it.Target != 0 {
		if err := add("status", it.LocalStatus, strconv.Itoa(it.Target)); err != nil {
			return err
		}
	}
	if it.SendRating > 0 {
		if err := add("rating", trimStars(it.LocalRating), trimStars(it.SendRating)); err != nil {
			return err
		}
	}
	for _, id := range it.newReads {
		if err := add("read", "", strconv.FormatInt(id, 10)); err != nil {
			return err
		}
	}
	_, err := q.Exec("DELETE FROM hardcover_sync WHERE book_id=? AND id NOT IN (SELECT id FROM hardcover_sync WHERE book_id=? ORDER BY id DESC LIMIT ?)", it.BookID, it.BookID, hardcoverSyncKeep)
	return err
}

// pushedRead reports whether a Hardcover read was created by a send from here.
// A pull skips it: the finished reading is already in the reading log, with
// its real date, and Hardcover's copy is dated the day of the send.
func pushedRead(q queryer, bookID, readID int64) (bool, error) {
	var n int
	err := q.QueryRow("SELECT count(*) FROM hardcover_sync WHERE book_id=? AND field='read' AND direction='push' AND remote_value=?", bookID, strconv.FormatInt(readID, 10)).Scan(&n)
	return n > 0, err
}
