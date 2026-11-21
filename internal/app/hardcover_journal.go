package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The journal check is the inbound half of note and quote sharing. It reads
// the connected reader's own entries for one linked book, only when asked,
// compares them with the local entries tied to them, and applies nothing until
// the reader picks an action per entry. Every action defaults to leaving the
// entry alone, and nothing here deletes anything on either side.
const (
	hcJournalPage    = 100 // Hardcover's row limit for reading_journals.
	hcJournalPages   = 5   // At most this many requests per check.
	hcJournalMaxKeep = 500 // Entries kept in a stored preview.
)

const hcJournalListQuery = `query NextChapterJournalList($uid: Int!, $book: Int!, $limit: Int!, $offset: Int!) {
 reading_journals(where: {user_id: {_eq: $uid}, book_id: {_eq: $book}}, order_by: [{action_at: desc}, {id: desc}], limit: $limit, offset: $offset) {
 id event entry privacy_setting_id metadata created_at
 }
}`

// Item states.
const (
	jsRemoteOnly    = "remote-only"
	jsRemoteChanged = "remote-changed"
	jsLocalChanged  = "local-changed"
	jsBothChanged   = "both-changed"
	jsMissing       = "missing-remote"
	jsUnchanged     = "unchanged"
	jsMismatch      = "kind-mismatch"
)

type hcJournalItem struct {
	Key   string
	State string
	// Local side, when the entry is tied to one.
	NoteID      int64
	NoteVersion int
	LocalText   string
	LocalPage   *int
	// Remote side, when it exists.
	RemoteID       int64
	Event          string
	RemoteEntry    string
	RemotePrivacy  int
	RemotePage     *int
	RemoteMetadata string
	RemoteCreated  string
	// NoBaseline is true when no earlier exchange was recorded, so a
	// difference cannot be attributed to one side.
	NoBaseline bool
	// Candidates are unlinked local entries of the same kind with exactly the
	// remote text. They are offered, never chosen for the reader.
	Candidates []hcJournalCandidate
}

type hcJournalCandidate struct {
	NoteID  int64
	Version int
	Where   string
}

type hcJournalSkipped struct {
	RemoteID int64
	Event    string
	Reason   string
}

type hcJournalPlan struct {
	BookID, UserID, HardcoverBookID int64
	Title                           string
	Items                           []hcJournalItem
	Unchanged                       int
	// Automatic counts entries Hardcover wrote itself (status, progress,
	// rating, review...). They are never imported as writing.
	Automatic   int
	Unsupported []hcJournalSkipped
	// Truncated means more entries exist than were read, so a local entry
	// missing from the page is not evidence that it is gone.
	Truncated bool
	Fetched   int
}

// entryUsable reports why a remote entry cannot be imported, or "".
func entryUsable(kind, text string) string {
	if strings.TrimSpace(text) == "" {
		return "It has no text."
	}
	if !utf8.ValidString(text) {
		return "Its text is not valid UTF-8."
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
			return "Its text has control characters."
		}
	}
	limit := maxNote
	if kind == "quote" {
		limit = maxQuote
	}
	if utf8.RuneCountInString(noteText(text)) > limit {
		return fmt.Sprintf("It is longer than the %d characters this app keeps, and is not shortened.", limit)
	}
	return ""
}

// fetchJournal reads every entry the reader has for a book, newest first, in
// pages within the request budget. A failed page fails the whole check, so an
// incomplete read is never mistaken for a complete one.
func (a *App) fetchJournal(ctx context.Context, userID, hcBook int64) (entries []hcJournal, truncated bool, err error) {
	for page := range hcJournalPages {
		var out struct {
			Entries []hcJournal `json:"reading_journals"`
		}
		vars := map[string]any{"uid": userID, "book": hcBook, "limit": hcJournalPage, "offset": page * hcJournalPage}
		if err = a.hardcover.do(ctx, hcJournalListQuery, vars, &out); err != nil {
			return nil, false, err
		}
		entries = append(entries, out.Entries...)
		if len(out.Entries) < hcJournalPage {
			return entries, false, nil
		}
	}
	return entries, true, nil
}

// classifyJournal compares the remote entries with the book's local ones.
func classifyJournal(p *hcJournalPlan, remote []hcJournal, local []bookNote) {
	byRemote := map[int64]bookNote{}
	for _, n := range local {
		if n.HardcoverJournalID > 0 && n.HardcoverUserID == p.UserID && n.HardcoverBookID == p.HardcoverBookID {
			byRemote[n.HardcoverJournalID] = n
		}
	}
	seen := map[int64]bool{}
	for _, j := range remote {
		id := int64(j.ID)
		if seen[id] {
			continue // a repeated row is the same entry
		}
		seen[id] = true
		if j.Event != "note" && j.Event != "quote" {
			p.Automatic++
			continue
		}
		skip := func(reason string) {
			p.Unsupported = append(p.Unsupported, hcJournalSkipped{id, j.Event, reason})
		}
		if j.Privacy < 1 || j.Privacy > 3 {
			skip("Its audience setting is not one this app knows.")
			continue
		}
		page, ok := j.page()
		if !ok {
			skip("Its page number is not a usable whole number.")
			continue
		}
		if reason := entryUsable(j.Event, j.Entry); reason != "" {
			skip(reason)
			continue
		}
		item := hcJournalItem{Key: "r" + strconv.FormatInt(id, 10), RemoteID: id, Event: j.Event, RemoteEntry: j.Entry, RemotePrivacy: j.Privacy,
			RemotePage: page, RemoteMetadata: string(j.Metadata), RemoteCreated: j.CreatedAt}
		n, linked := byRemote[id]
		if !linked {
			item.State = jsRemoteOnly
			for _, c := range local {
				if !c.Linked() && c.Event() == j.Event && c.PayloadEntry() == noteText(j.Entry) && samePage(c.PayloadPage(), page) {
					item.Candidates = append(item.Candidates, hcJournalCandidate{c.ID, c.Version, c.Where()})
				}
			}
			p.Items = append(p.Items, item)
			continue
		}
		item.NoteID, item.NoteVersion = n.ID, n.Version
		item.LocalText, item.LocalPage = n.PayloadEntry(), n.PayloadPage()
		if n.Event() != j.Event {
			item.State = jsMismatch
			p.Items = append(p.Items, item)
			continue
		}
		remoteEntry := noteText(j.Entry)
		var localChanged, remoteChanged bool
		if n.BasePrivacy != 0 {
			localChanged = n.ChangedHere()
			remoteChanged = remoteEntry != noteText(n.BaseEntry) || !samePage(page, n.BasePage) || j.Privacy != n.BasePrivacy
		} else {
			item.NoBaseline = true
			differs := item.LocalText != remoteEntry || !samePage(item.LocalPage, page)
			localChanged, remoteChanged = differs, differs
		}
		switch {
		case localChanged && remoteChanged:
			item.State = jsBothChanged
		case localChanged:
			item.State = jsLocalChanged
		case remoteChanged:
			item.State = jsRemoteChanged
		default:
			item.State = jsUnchanged
		}
		if item.State == jsUnchanged {
			p.Unchanged++
			if !item.NoBaseline {
				continue
			}
		}
		p.Items = append(p.Items, item)
	}
	if !p.Truncated {
		for _, n := range local {
			if n.HardcoverJournalID > 0 && n.HardcoverUserID == p.UserID && n.HardcoverBookID == p.HardcoverBookID && !seen[n.HardcoverJournalID] {
				p.Items = append(p.Items, hcJournalItem{Key: "l" + strconv.FormatInt(n.ID, 10), State: jsMissing, NoteID: n.ID, NoteVersion: n.Version, RemoteID: n.HardcoverJournalID, LocalText: n.PayloadEntry(), LocalPage: n.PayloadPage(), Event: n.Event()})
			}
		}
	}
}

// journalLink returns the connected account and the book's confirmed link.
func (a *App) journalLink(q queryer, bookID int64) (userID, hcBook int64, err error) {
	acct, err := loadHardcoverLink(q)
	if err != nil {
		return 0, 0, err
	}
	link, err := getBookLink(q, bookID)
	if err != nil {
		return 0, 0, err
	}
	if acct == nil || link == nil {
		return 0, 0, bad("Link this book to Hardcover and test the connection first.")
	}
	return acct.UserID, link.HardcoverBookID, nil
}

// hardcoverJournalCheck reads the reader's journal for one book and stores a
// preview. It is the only place the journal is read, and only on request.
func (a *App) hardcoverJournalCheck(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	id, err := requestID(r)
	if err != nil {
		return err
	}
	b, err := getBook(a.store.DB, id)
	if err != nil {
		return err
	}
	userID, hcBook, err := a.journalLink(a.store.DB, id)
	if err != nil {
		return err
	}
	a.hardcoverMu.Lock()
	remote, truncated, err := a.fetchJournal(r.Context(), userID, hcBook)
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	var planID string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		local, err := loadNotes(tx, id)
		if err != nil {
			return err
		}
		p := hcJournalPlan{BookID: id, UserID: userID, HardcoverBookID: hcBook, Title: b.Title, Truncated: truncated, Fetched: len(remote)}
		classifyJournal(&p, remote, local)
		if len(p.Items) > hcJournalMaxKeep {
			p.Items, p.Truncated = p.Items[:hcJournalMaxKeep], true
		}
		planID, err = savePlan(tx, "hc-journal", Plan{HardcoverJournal: &p})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/journal/"+planID)
}

type hcJournalResult struct {
	Title, Outcome string
	Applied        bool
}

// hardcoverJournalPlan shows the preview and applies the reader's choices.
func (a *App) hardcoverJournalPlan(w http.ResponseWriter, r *http.Request) error {
	planID := r.PathValue("id")
	kind, plan, _, err := readPlan(a.store.DB, planID)
	if err != nil {
		return err
	}
	if kind != "hc-journal" || plan.HardcoverJournal == nil {
		return sql.ErrNoRows
	}
	p := *plan.HardcoverJournal
	if r.Method == http.MethodGet {
		return a.render(w, r, "hardcover_journal.html", page{"Title": "Check Hardcover journal", "Nav": "hardcover", "PlanID": planID, "Plan": p, "Sections": journalSections(p)}, 200)
	}
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	a.hardcoverMu.Lock()
	defer a.hardcoverMu.Unlock()
	// Re-read inside the lock so a double submission applies once.
	kind, plan, _, err = readPlan(a.store.DB, planID)
	if err != nil {
		return err
	}
	if kind != "hc-journal" || plan.HardcoverJournal == nil {
		return sql.ErrNoRows
	}
	userID, hcBook, err := a.journalLink(a.store.DB, p.BookID)
	if err != nil || userID != p.UserID || hcBook != p.HardcoverBookID {
		return conflict("The Hardcover account or this book's link changed. Check the journal again.")
	}
	// Read Hardcover again: a choice applies only to what was previewed.
	fresh, _, err := a.fetchJournal(r.Context(), userID, hcBook)
	if err != nil {
		return hardcoverHTTPError(err)
	}
	remoteNow := map[int64]hcJournal{}
	for _, j := range fresh {
		remoteNow[int64(j.ID)] = j
	}
	var results []hcJournalResult
	err = a.store.Write(func(tx *sql.Tx) error {
		now := canonicalTime(a.now())
		count, err := countNotes(tx, p.BookID)
		if err != nil {
			return err
		}
		for _, it := range p.Items {
			choice := r.FormValue("act_" + it.Key)
			title := journalTitle(it)
			// Entries with no selection are left alone. An unchanged entry
			// with no recorded exchange only gets that record, which is
			// local metadata, so reading it never needs a choice.
			if it.State == jsUnchanged {
				if ok, err := a.recordBaseline(tx, it, remoteNow, p, now); err != nil {
					return err
				} else if ok {
					results = append(results, hcJournalResult{title, "Recorded as in sync.", true})
				}
				continue
			}
			if choice == "" || choice == "leave" {
				continue
			}
			if !a.unchangedRemote(it, remoteNow) {
				results = append(results, hcJournalResult{title, "Skipped: it changed on Hardcover after the preview. Check again.", false})
				continue
			}
			note, noteErr := getNote(tx, it.NoteID)
			if it.NoteID > 0 && errors.Is(noteErr, sql.ErrNoRows) {
				results = append(results, hcJournalResult{title, "Skipped: the local entry was deleted after the preview.", false})
				continue
			} else if it.NoteID > 0 && noteErr != nil {
				return noteErr
			}
			if it.NoteID > 0 && (note.Version != it.NoteVersion || note.BookID != p.BookID) {
				results = append(results, hcJournalResult{title, "Skipped: the local entry changed after the preview. Check again.", false})
				continue
			}
			switch {
			case it.State == jsRemoteOnly && choice == "import":
				if count >= maxNotesPerBook {
					results = append(results, hcJournalResult{title, fmt.Sprintf("Skipped: this book already has %d entries.", maxNotesPerBook), false})
					continue
				}
				var exists int
				if err := tx.QueryRow("SELECT count(*) FROM book_notes WHERE book_id=? AND hardcover_journal_id=? AND hardcover_user_id=? AND hardcover_book_id=?", p.BookID, it.RemoteID, p.UserID, p.HardcoverBookID).Scan(&exists); err != nil {
					return err
				}
				if exists > 0 {
					results = append(results, hcJournalResult{title, "Skipped: already imported.", false})
					continue
				}
				created := now
				if t, err := parseTime(it.RemoteCreated); err == nil && t != "" {
					created = t
				}
				text, body, quote := noteText(it.RemoteEntry), "", ""
				if it.Event == "quote" {
					quote = text
				} else {
					body = text
				}
				if _, err := tx.Exec(`INSERT INTO book_notes(book_id,kind,quote,body,page,created_at,updated_at,hardcover_journal_id,hardcover_user_id,hardcover_book_id,origin,hc_base_entry,hc_base_page,hc_base_privacy,hc_synced_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,'hardcover',?,?,?,?)`, p.BookID, it.Event, quote, body, importPage(it), created, created, it.RemoteID, p.UserID, p.HardcoverBookID, text, it.RemotePage, it.RemotePrivacy, now); err != nil {
					return err
				}
				count++
				results = append(results, hcJournalResult{title, "Imported as a new " + it.Event + ".", true})
			case it.State == jsRemoteOnly && strings.HasPrefix(choice, "link:"):
				nid, _ := strconv.ParseInt(strings.TrimPrefix(choice, "link:"), 10, 64)
				cand := candidate(it, nid)
				if cand == nil {
					results = append(results, hcJournalResult{title, "Skipped: that is not one of the entries offered.", false})
					continue
				}
				ln, err := getNote(tx, nid)
				if err != nil || ln.BookID != p.BookID || ln.Version != cand.Version || ln.Linked() || ln.Event() != it.Event || ln.PayloadEntry() != noteText(it.RemoteEntry) {
					results = append(results, hcJournalResult{title, "Skipped: that local entry changed or is already linked.", false})
					continue
				}
				if err := linkNote(tx, nid, it, p, now); err != nil {
					return err
				}
				results = append(results, hcJournalResult{title, "Linked to your existing local entry. Nothing was sent.", true})
			case (it.State == jsRemoteChanged || it.State == jsBothChanged) && choice == "remote":
				text := noteText(it.RemoteEntry)
				q, body := "", text
				if it.Event == "quote" {
					q, body = text, note.Body
				}
				pageVal := note.Page
				if it.Event == "quote" {
					pageVal = it.RemotePage
				}
				if _, err := tx.Exec("UPDATE book_notes SET quote=?,body=?,page=?,version=version+1,updated_at=? WHERE id=?", q, body, pageVal, now, it.NoteID); err != nil {
					return err
				}
				if err := linkNote(tx, it.NoteID, it, p, now); err != nil {
					return err
				}
				results = append(results, hcJournalResult{title, "Replaced the local text with Hardcover's.", true})
			case it.State == jsMissing && choice == "unlink":
				if _, err := tx.Exec("UPDATE book_notes SET hardcover_journal_id=0,hardcover_user_id=0,hardcover_book_id=0,hc_base_entry='',hc_base_page=NULL,hc_base_privacy=0,hc_synced_at='' WHERE id=?", it.NoteID); err != nil {
					return err
				}
				results = append(results, hcJournalResult{title, "Unlinked. Your local entry is kept.", true})
			default:
				results = append(results, hcJournalResult{title, "Skipped: that choice does not apply.", false})
			}
		}
		_, err = tx.Exec("DELETE FROM plans WHERE id=?", planID)
		return err
	})
	if err != nil {
		return err
	}
	return a.render(w, r, "hardcover_journal.html", page{"Title": "Check Hardcover journal", "Nav": "hardcover", "Plan": p, "Results": results, "Done": true, "BookID": p.BookID}, 200)
}

func journalTitle(it hcJournalItem) string {
	text := it.RemoteEntry
	if text == "" {
		text = it.LocalText
	}
	return truncate(strings.Join(strings.Fields(text), " "), 80)
}

// importPage is the page an imported entry gets: a quote's page, from the
// documented metadata key. A note keeps its location in its text.
func importPage(it hcJournalItem) *int {
	if it.Event == "quote" {
		return it.RemotePage
	}
	return nil
}

func candidate(it hcJournalItem, id int64) *hcJournalCandidate {
	for i := range it.Candidates {
		if it.Candidates[i].NoteID == id {
			return &it.Candidates[i]
		}
	}
	return nil
}

// unchangedRemote reports whether the entry is on Hardcover as previewed. A
// missing-entry item is unchanged while it is still missing.
func (a *App) unchangedRemote(it hcJournalItem, now map[int64]hcJournal) bool {
	j, ok := now[it.RemoteID]
	if it.State == jsMissing {
		return !ok
	}
	if !ok || j.Entry != it.RemoteEntry || j.Privacy != it.RemotePrivacy {
		return false
	}
	page, _ := j.page()
	return samePage(page, it.RemotePage)
}

// linkNote records what both sides now hold for an entry.
func linkNote(tx *sql.Tx, noteID int64, it hcJournalItem, p hcJournalPlan, now string) error {
	_, err := tx.Exec("UPDATE book_notes SET hardcover_journal_id=?,hardcover_user_id=?,hardcover_book_id=?,hc_base_entry=?,hc_base_page=?,hc_base_privacy=?,hc_synced_at=? WHERE id=?",
		it.RemoteID, p.UserID, p.HardcoverBookID, noteText(it.RemoteEntry), it.RemotePage, it.RemotePrivacy, now, noteID)
	return err
}

// recordBaseline stores the exchange record for an entry that is identical on
// both sides but has none. It reports whether it wrote.
func (a *App) recordBaseline(tx *sql.Tx, it hcJournalItem, remoteNow map[int64]hcJournal, p hcJournalPlan, now string) (bool, error) {
	if !it.NoBaseline || !a.unchangedRemote(it, remoteNow) {
		return false, nil
	}
	n, err := getNote(tx, it.NoteID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if n.Version != it.NoteVersion || n.BasePrivacy != 0 {
		return false, nil
	}
	return true, linkNote(tx, it.NoteID, it, p, now)
}

// Section groups items for the page.
type hcJournalSection struct {
	Title, Help string
	Items       []hcJournalItem
}

func journalSections(p hcJournalPlan) []hcJournalSection {
	order := []struct{ state, title, help string }{
		{jsBothChanged, "Changed in both places", "The two copies differ and the app cannot tell which is newer. Nothing is chosen for you."},
		{jsRemoteChanged, "Changed on Hardcover", "Hardcover's copy changed since you last exchanged this entry."},
		{jsLocalChanged, "Changed here", "Your copy changed since you last exchanged it. Sending is its own preview, where you choose the audience."},
		{jsRemoteOnly, "On Hardcover, not here", "Entries you wrote on Hardcover. Importing adds a local copy and never creates a duplicate on a later check."},
		{jsMissing, "Missing on Hardcover", "These were shared, but are not on Hardcover now. They are not deleted or recreated."},
		{jsMismatch, "Type changed", "The entry is a different type on Hardcover now (note or quote). Nothing is applied."},
		{jsUnchanged, "In sync, no exchange record", "Identical on both sides. Applying records that, so later changes can be told apart."},
	}
	var out []hcJournalSection
	for _, o := range order {
		s := hcJournalSection{Title: o.title, Help: o.help}
		for _, it := range p.Items {
			if it.State == o.state {
				s.Items = append(s.Items, it)
			}
		}
		if len(s.Items) > 0 {
			out = append(out, s)
		}
	}
	return out
}
