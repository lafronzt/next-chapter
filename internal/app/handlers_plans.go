package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func makeSuggestion(q queryer, candidate int64) (string, error) {
	return suggestWithIntent(q, candidate, "")
}

// suggestWithIntent saves a rules plan with the reader's optional note for an
// AI suggestion, kept only as long as the plan.
func suggestWithIntent(q queryer, candidate int64, intent string) (string, error) {
	queue, err := loadQueue(q)
	if err != nil {
		return "", err
	}
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return "", err
	}
	if prefs.UseEstimates {
		if err = attachEstimates(q, queue); err != nil {
			return "", err
		}
	}
	rows, err := Propose(queue, prefs, candidate, time.Now().UTC())
	if err != nil {
		return "", bad(err.Error())
	}
	kind := "reorder"
	if candidate != 0 {
		kind = "insert"
	}
	return savePlan(q, kind, Plan{Rows: rows, CandidateID: candidate, Intent: intent})
}
func (a *App) suggest(w http.ResponseWriter, r *http.Request) error {
	candidate := int64(0)
	if raw := r.FormValue("candidate_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			return bad("Choose a valid book.")
		}
		candidate = id
	}
	intent := strings.TrimSpace(r.FormValue("intent"))
	if len([]rune(intent)) > maxIntent {
		return bad(fmt.Sprintf("Keep the note for the AI under %d characters.", maxIntent))
	}
	var id string
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = suggestWithIntent(tx, candidate, intent)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+id)
}

// planTitles name the preview page for each kind of plan.
var planTitles = map[string]string{"import": "Import preview", "restore": "Restore preview", "triage": "Confirm sorting", "insert": "Suggested place", "reorder": "Suggested order"}

type undoBook struct {
	ID             int64 `json:"id"`
	NeedsPlacement bool  `json:"needs_placement"`
	MovedByHand    bool  `json:"moved_by_hand,omitempty"`
}

func (a *App) plan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		kind, p, rev, err := readPlan(a.store.DB, id)
		if err != nil {
			return err
		}
		preview := p.Records
		if len(preview) > 100 {
			preview = preview[:100]
		}
		if p.Restore != nil {
			for _, b := range p.Restore.File.Books[:min(100, len(p.Restore.File.Books))] {
				preview = append(preview, Record{Book: b})
			}
		}
		data := page{"Title": planTitles[kind], "Kind": kind, "PlanID": id, "Plan": p, "PreviewRecords": preview, "Stale": rev != state(r).Revision, "UpgradeSummary": upgradeSummary(p.Upgrades), "Skipped": p.Skipped}
		if kind == "triage" {
			byID, err := getBooks(a.store.DB, p.BookIDs)
			if err != nil {
				return err
			}
			books := []Book{}
			for _, id := range p.BookIDs {
				if b, ok := byID[id]; ok {
					books = append(books, b)
				}
			}
			data["TriageBooks"], data["TriageLabel"] = books, triageLabels[p.Status]
		}
		if kind == "reorder" || kind == "insert" {
			if err = a.planAIData(data, kind, p, r); err != nil {
				return err
			}
		}
		return a.render(w, r, "plan.html", data, 200)
	}
	location := "/?notice=ordered"
	err := a.store.Write(func(tx *sql.Tx) error {
		kind, p, rev, err := readPlan(tx, id)
		if err != nil {
			return err
		}
		now, err := revision(tx)
		if err != nil {
			return err
		}
		if now != rev {
			return conflict("Your library changed. Generate a fresh preview before applying it.")
		}
		switch kind {
		case "import":
			result, err := importRecords(tx, p.Records)
			if err != nil {
				return err
			}
			if result.Changed {
				if err = bump(tx); err != nil {
					return err
				}
			}
			location = fmt.Sprintf("/?notice=imported&moved=%d", result.upgradedTotal())
			if result.Skipped > 0 {
				location += fmt.Sprintf("&skipped=%d", result.Skipped)
			}
		case "triage":
			byID, err := getBooks(tx, p.BookIDs)
			if err != nil {
				return err
			}
			for _, id := range p.BookIDs {
				b, ok := byID[id]
				if !ok || b.Status != "unreviewed" {
					return conflict("Some of these books were already sorted. Generate a fresh preview.")
				}
				if err = setStatus(tx, b, p.Status); err != nil {
					return err
				}
			}
			if err = bump(tx); err != nil {
				return err
			}
			location = unreviewedURL("", "triaged")
		case "restore":
			if p.Restore == nil {
				return fmt.Errorf("restore plan %s has no data", id)
			}
			result, err := restoreExport(tx, p.Restore.File)
			if err != nil {
				return err
			}
			if err = bump(tx); err != nil {
				return err
			}
			location = "/?notice=restored"
			if result.SyncWasOn {
				location = "/?notice=restored-sync"
			}
		case "reorder", "insert":
			queue, err := queueOrder(tx)
			if err != nil {
				return err
			}
			previous := []undoBook{}
			for _, b := range queue {
				previous = append(previous, undoBook{b.ID, b.NeedsPlacement, b.MovedByHand})
			}
			raw, err := json.Marshal(previous)
			if err != nil {
				return err
			}
			rows := p.Rows
			if r.FormValue("use") == "ai" {
				if len(p.AIRows) == 0 {
					return bad("This preview has no AI suggestion to apply.")
				}
				rows = p.AIRows
			}
			ids := []int64{}
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			if err = reorder(tx, ids); err != nil {
				return err
			}
			// The reader chose the suggested place, which replaces any place
			// they had given the book by hand.
			if p.CandidateID != 0 {
				_, err = tx.Exec("UPDATE books SET needs_placement=0,moved_by_hand=0 WHERE id=?", p.CandidateID)
			} else {
				_, err = tx.Exec("UPDATE books SET needs_placement=0,moved_by_hand=0 WHERE reading_status='to-read'")
			}
			if err != nil {
				return err
			}
			if err = bump(tx); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT OR REPLACE INTO undo_queue VALUES(1,?,?)", string(raw), now+1); err != nil {
				return err
			}
		default:
			// Never fall through to a reorder for a kind this code does not know.
			return fmt.Errorf("unknown plan kind %q", kind)
		}
		_, err = tx.Exec("DELETE FROM plans WHERE id=?", id)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, location)
}
func (a *App) undo(w http.ResponseWriter, r *http.Request) error {
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var raw string
		var rev int64
		if err := tx.QueryRow("SELECT payload,revision FROM undo_queue WHERE id=1").Scan(&raw, &rev); err != nil {
			return err
		}
		now, err := revision(tx)
		if err != nil {
			return err
		}
		if rev != now {
			return conflict("Undo expired after another library change.")
		}
		var previous []undoBook
		if err = json.Unmarshal([]byte(raw), &previous); err != nil {
			return err
		}
		for i, b := range previous {
			if _, err = tx.Exec("UPDATE books SET position=?,needs_placement=?,moved_by_hand=? WHERE id=?", i, b.NeedsPlacement, b.MovedByHand, b.ID); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("DELETE FROM undo_queue"); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/?notice=undone")
}
func upgradeSummary(upgrades map[string]int) string {
	parts, total := []string{}, 0
	for _, s := range [][2]string{{"to-read", "your queue"}, {"read", "your finished list"}, {"reading", "Reading"}, {"archived", "Archived"}} {
		if n := upgrades[s[0]]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d to %s", n, s[1]))
			total += n
		}
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%d library-only books will move out of To review to match Goodreads: %s.", total, strings.Join(parts, ", "))
}

// planAIData adds the AI parts of a queue suggestion's preview: which order
// is shown, and before the AI is asked, exactly what would be shared.
func (a *App) planAIData(data page, kind string, p Plan, r *http.Request) error {
	ai, err := a.aiStatus(a.store.DB)
	if err != nil {
		return err
	}
	data["AI"] = ai
	showAI := len(p.AIRows) > 0 && r.URL.Query().Get("show") != "rules"
	data["ShowAI"], data["Shown"] = showAI, p.Rows
	if showAI {
		data["Shown"] = p.AIRows
	}
	if !ai.Available() || len(p.AIRows) > 0 || p.AINotice != "" || data["Stale"] == true {
		return nil
	}
	ri, err := loadRerankInputs(a.store.DB, kind, p, a.now().UTC())
	var h httpError
	if errors.As(err, &h) {
		return nil // the usual preview already explains constraint problems
	}
	if err != nil {
		return err
	}
	req, err := ri.request()
	if err != nil {
		return err
	}
	data["Shared"], data["SharedFingerprint"], data["Prompt"] = ri.Payload, ri.Fingerprint, a.previewPrompt(req)
	return nil
}

// planAI checks a queue suggestion's previewed request and sends it to the AI
// provider in the background; the result is shown beside the usual ranking.
func (a *App) planAI(w http.ResponseWriter, r *http.Request) error {
	if ai, err := a.aiStatus(a.store.DB); err != nil || !ai.Available() {
		return cmp.Or(err, error(errAIOff))
	}
	plan := r.PathValue("id")
	send, err := a.prepareRerankAI(plan, r.FormValue("fingerprint"))
	if err != nil {
		return err
	}
	job, err := a.startAIJob("a queue suggestion", "/plans/"+plan, "If your library changes before it answers, the suggestion cannot be used and you will need a fresh one.", func(ctx context.Context) (string, error) {
		id, err := send(ctx)
		return "/plans/" + id, err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/ai/jobs/"+job)
}
