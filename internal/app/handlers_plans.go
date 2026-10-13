package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func makeSuggestion(q queryer, candidate int64) (string, error) {
	queue, err := loadQueue(q)
	if err != nil {
		return "", err
	}
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return "", err
	}
	rows, err := Propose(queue, prefs, candidate, time.Now().UTC())
	if err != nil {
		return "", bad(err.Error())
	}
	kind := "reorder"
	if candidate != 0 {
		kind = "insert"
	}
	return savePlan(q, kind, Plan{Rows: rows, CandidateID: candidate})
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
	var id string
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = makeSuggestion(tx, candidate)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+id)
}

type undoBook struct {
	ID             int64 `json:"id"`
	NeedsPlacement bool  `json:"needs_placement"`
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
		data := page{"Kind": kind, "Plan": p, "PreviewRecords": preview, "Stale": rev != state(r).Revision, "UpgradeSummary": upgradeSummary(p.Upgrades), "Skipped": p.Skipped}
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
		default:
			queue, err := queueOrder(tx)
			if err != nil {
				return err
			}
			previous := []undoBook{}
			for _, b := range queue {
				previous = append(previous, undoBook{b.ID, b.NeedsPlacement})
			}
			raw, err := json.Marshal(previous)
			if err != nil {
				return err
			}
			ids := []int64{}
			for _, row := range p.Rows {
				ids = append(ids, row.ID)
			}
			if err = reorder(tx, ids); err != nil {
				return err
			}
			if p.CandidateID != 0 {
				_, err = tx.Exec("UPDATE books SET needs_placement=0 WHERE id=?", p.CandidateID)
			} else {
				_, err = tx.Exec("UPDATE books SET needs_placement=0 WHERE reading_status='to-read'")
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
			if _, err = tx.Exec("UPDATE books SET position=?,needs_placement=? WHERE id=?", i, b.NeedsPlacement, b.ID); err != nil {
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
