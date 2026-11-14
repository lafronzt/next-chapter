package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Specific notices say what just happened to which book. The redirect carries
// only a short signed key (action, book ID, number), never text, so a link
// cannot be forged into arbitrary words on the page. The title is read from
// the library when the page renders and HTML-escaped by the template.

const genericNotice = "Done."

// noticeTitleMax caps a title in a notice so a huge one cannot swamp the page.
const noticeTitleMax = 60

func capTitle(title string) string {
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) <= noticeTitleMax {
		return title
	}
	return string([]rune(title)[:noticeTitleMax]) + "…"
}

// noticeText is the sentence for an action key and book title.
func noticeText(action, title string, arg int) string {
	t := "“" + capTitle(title) + "”"
	switch action {
	case "up":
		return "Moved " + t + " up one place."
	case "down":
		return "Moved " + t + " down one place."
	case "move":
		return fmt.Sprintf("Moved %s to #%d.", t, arg)
	case "pin":
		return "Pinned " + t + "."
	case "unpin":
		return "Unpinned " + t + "."
	case "placed":
		return t + " keeps its place and no longer needs placement."
	case "start":
		return t + " is now Reading."
	case "stop":
		return t + " is now Set aside."
	case "requeue":
		return t + " is back in your queue and needs a spot."
	}
	return ""
}

func (a *App) noticeSig(body string) string {
	mac := hmac.New(sha256.New, []byte(a.config.Secret))
	mac.Write([]byte("notice:" + body))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// noticeValue is the signed value for the "n" query parameter.
func (a *App) noticeValue(action string, id int64, arg int) string {
	body := fmt.Sprintf("%s.%d.%d", action, id, arg)
	return body + "." + a.noticeSig(body)
}

// parseNotice verifies a value from noticeValue.
func (a *App) parseNotice(v string) (action string, id int64, arg int, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 4 {
		return "", 0, 0, false
	}
	body := strings.Join(parts[:3], ".")
	if !hmac.Equal([]byte(parts[3]), []byte(a.noticeSig(body))) {
		return "", 0, 0, false
	}
	id, err1 := strconv.ParseInt(parts[1], 10, 64)
	arg, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || noticeText(parts[0], "x", arg) == "" {
		return "", 0, 0, false
	}
	return parts[0], id, arg, true
}

// withNotice adds the signed notice to a redirect target, before any fragment.
func (a *App) withNotice(dest, action string, id int64, arg int) string {
	frag := ""
	if i := strings.Index(dest, "#"); i >= 0 {
		dest, frag = dest[:i], dest[i:]
	}
	u, err := url.Parse(dest)
	if err != nil {
		return dest + frag
	}
	q := u.Query()
	q.Set("n", a.noticeValue(action, id, arg))
	u.RawQuery = q.Encode()
	return u.String() + frag
}

// resolveNotice turns the "n" parameter into a sentence and the label of an
// Undo that would reverse it. A forged, stale, or unknown key gives the
// generic notice, never text chosen by the sender.
func (a *App) resolveNotice(q queryer, v string, revision int64) (text, undoLabel string) {
	action, id, arg, ok := a.parseNotice(v)
	if !ok {
		return genericNotice, ""
	}
	var title string
	if err := q.QueryRow("SELECT title FROM books WHERE id=?", id).Scan(&title); err != nil {
		return genericNotice, ""
	}
	text = noticeText(action, title, arg)
	if rev, label, err := readUndo(q); err == nil && rev == revision && label != "" {
		undoLabel = label
	}
	return text, undoLabel
}

// bookState is the part of a book a covered action can change.
type bookState struct {
	ID             int64  `json:"id"`
	Status         string `json:"status"`
	Position       int    `json:"position"`
	Pinned         bool   `json:"pinned"`
	NeedsPlacement bool   `json:"needs_placement"`
	MovedByHand    bool   `json:"moved_by_hand"`
}

// undoSnapshot is the single-slot Undo for moves, pins, and status changes.
// Older snapshots (a JSON array) belong to suggested orders; see undo.
type undoSnapshot struct {
	Label string      `json:"label"`
	Books []bookState `json:"books"`
}

// snapshotBooks captures bookStates for the books a WHERE clause selects.
func snapshotBooks(q queryer, where string, args ...any) ([]bookState, error) {
	rows, err := q.Query("SELECT id,reading_status,position,pinned,needs_placement,moved_by_hand FROM books WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bookState
	for rows.Next() {
		var s bookState
		if err = rows.Scan(&s.ID, &s.Status, &s.Position, &s.Pinned, &s.NeedsPlacement, &s.MovedByHand); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// recordUndo stores the snapshot taken before an action. Call it after the
// action's bump, so the guard is the revision the action produced: any later
// change makes the Undo refuse.
func recordUndo(q queryer, label string, before []bookState) error {
	raw, err := json.Marshal(undoSnapshot{Label: label, Books: before})
	if err != nil {
		return err
	}
	now, err := revision(q)
	if err != nil {
		return err
	}
	_, err = q.Exec("INSERT OR REPLACE INTO undo_queue VALUES(1,?,?)", string(raw), now)
	return err
}

// readUndo returns the stored Undo's revision and a label for its button, or
// sql.ErrNoRows when there is none.
func readUndo(q queryer) (rev int64, label string, err error) {
	var raw string
	if err = q.QueryRow("SELECT payload,revision FROM undo_queue WHERE id=1").Scan(&raw, &rev); err != nil {
		return 0, "", err
	}
	if strings.HasPrefix(raw, "[") {
		return rev, "the last suggested order", nil
	}
	var snap undoSnapshot
	if err = json.Unmarshal([]byte(raw), &snap); err != nil {
		return rev, "", nil
	}
	return rev, snap.Label, nil
}

// restoreStates puts books back exactly as a snapshot recorded them.
func restoreStates(q queryer, books []bookState) error {
	for _, s := range books {
		if _, err := q.Exec("UPDATE books SET reading_status=?,position=?,pinned=?,needs_placement=?,moved_by_hand=? WHERE id=?", s.Status, s.Position, s.Pinned, s.NeedsPlacement, s.MovedByHand, s.ID); err != nil {
			return err
		}
	}
	return nil
}
