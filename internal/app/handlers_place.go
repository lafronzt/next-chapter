package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// placement is the progress of one sorting session, carried in the URL so no
// server state is needed. Nothing here is trusted beyond display: what moves
// is always decided by the stored library and the same plan engine /suggest uses.
type placement struct {
	Total, Placed, Kept int
	Skipped             []int64
}

func parsePlacement(v url.Values) placement {
	p := placement{}
	p.Total, _ = strconv.Atoi(v.Get("total"))
	p.Placed, _ = strconv.Atoi(v.Get("placed"))
	p.Kept, _ = strconv.Atoi(v.Get("kept"))
	for _, raw := range strings.Split(v.Get("skip"), ",") {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			p.Skipped = append(p.Skipped, id)
		}
	}
	return p
}

func (p placement) query() url.Values {
	v := url.Values{"total": {strconv.Itoa(p.Total)}, "placed": {strconv.Itoa(p.Placed)}, "kept": {strconv.Itoa(p.Kept)}}
	if len(p.Skipped) > 0 {
		ids := make([]string, len(p.Skipped))
		for i, id := range p.Skipped {
			ids[i] = strconv.FormatInt(id, 10)
		}
		v.Set("skip", strings.Join(ids, ","))
	}
	return v
}

func (p placement) skipped(id int64) bool {
	for _, s := range p.Skipped {
		if s == id {
			return true
		}
	}
	return false
}

// Done is how many books this session has already decided.
func (p placement) Done() int { return p.Placed + p.Kept + len(p.Skipped) }

// placeSlot is the proposed spot for a book: the books around it in the
// suggested order, and why it fits there.
type placeSlot struct {
	Position            int
	Above, Below        []Suggestion
	Reasons             []string
	Unplaceable, Placed bool
}

// nextUnplaced returns the first queued book waiting for placement, in queue
// order, that this session has not skipped, and how many are waiting in all.
func nextUnplaced(q queryer, p placement) (*Book, int, error) {
	waiting, err := queryBooks(q, "WHERE b.reading_status='to-read' AND b.needs_placement=1 ORDER BY b.position,b.id")
	if err != nil {
		return nil, 0, err
	}
	for i := range waiting {
		if !p.skipped(waiting[i].ID) {
			return &waiting[i], len(waiting), nil
		}
	}
	return nil, len(waiting), nil
}

// place shows one book waiting for placement, with the spot the usual ranking
// proposes. Nothing moves until the reader chooses.
func (a *App) place(w http.ResponseWriter, r *http.Request) error {
	p := parsePlacement(r.URL.Query())
	book, waiting, err := nextUnplaced(a.store.DB, p)
	if err != nil {
		return err
	}
	if book == nil {
		return redirect(w, r, placementDone(p))
	}
	if p.Total < p.Done()+waiting-len(p.Skipped) {
		p.Total = p.Done() + waiting - len(p.Skipped)
	}
	queue, err := loadQueue(a.store.DB)
	if err != nil {
		return err
	}
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	if prefs.UseEstimates {
		if err = attachEstimates(a.store.DB, queue); err != nil {
			return err
		}
	}
	slot := placeSlot{}
	rows, err := Propose(queue, prefs, book.ID, time.Now().UTC())
	if err != nil {
		slot.Unplaceable = true
	} else {
		for i, row := range rows {
			if row.ID == book.ID {
				slot.Position, slot.Reasons = i+1, row.Reasons
				slot.Above = rows[max(0, i-2):i]
				slot.Below = rows[i+1 : min(len(rows), i+3)]
			}
		}
	}
	single := []Book{*book}
	if err = attachEstimates(a.store.DB, single); err != nil {
		return err
	}
	*book = single[0]
	return a.render(w, r, "place.html", page{"Title": "Place a book", "Nav": "queue", "Book": *book, "Slot": slot, "Progress": p, "Number": p.Done() + 1, "Carry": p.query()}, 200)
}

// placementDone is where a finished session ends: the queue, with a notice.
func placementDone(p placement) string {
	return fmt.Sprintf("/?notice=placement&placed=%d&kept=%d&skipped=%d", p.Placed, p.Kept, len(p.Skipped))
}

// placeDecision records one decision. "here" applies the same plan /suggest
// would preview, through the same applier, revision check, and Undo; "keep"
// clears the flag where the book already is; "later" changes nothing.
func (a *App) placeDecision(w http.ResponseWriter, r *http.Request) error {
	id, err := strconv.ParseInt(r.FormValue("candidate_id"), 10, 64)
	if err != nil || id < 1 {
		return bad("Choose a valid book.")
	}
	p := parsePlacement(r.Form)
	action := r.FormValue("action")
	switch action {
	case "here", "keep":
		err = a.store.Write(func(tx *sql.Tx) error {
			if err := current(tx, r); err != nil {
				return err
			}
			b, err := getBook(tx, id)
			if err != nil {
				return err
			}
			if b.Status != "to-read" || !b.NeedsPlacement {
				return conflict("That book is no longer waiting for a spot. Reload to see what is next.")
			}
			if action == "keep" {
				if _, err = tx.Exec("UPDATE books SET needs_placement=0 WHERE id=?", id); err != nil {
					return err
				}
				return bump(tx)
			}
			planID, err := suggestWithIntent(tx, id, "")
			if err != nil {
				return err
			}
			_, err = applyPlan(tx, planID, false)
			return err
		})
		if err != nil {
			return err
		}
		if action == "here" {
			p.Placed++
		} else {
			p.Kept++
		}
	case "later":
		if !p.skipped(id) {
			p.Skipped = append(p.Skipped, id)
		}
	default:
		return bad("Choose a valid action.")
	}
	return redirect(w, r, "/place?"+p.query().Encode())
}
