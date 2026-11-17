package app

import (
	"database/sql"
	"net/http"
	"strings"
)

type hardcoverTestResult struct {
	At        string `json:"at"`
	Message   string `json:"message"`
	OK        bool   `json:"ok"`
	Remaining int    `json:"remaining"`
}

func (a *App) hardcoverPage(w http.ResponseWriter, r *http.Request) error {
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	link, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	last, err := readMeta(a.store.DB, "hardcover_test", hardcoverTestResult{Remaining: -1})
	if err != nil {
		return err
	}
	remaining := last.Remaining
	if a.hardcover != nil && a.hardcover.knownRemaining() >= 0 {
		remaining = a.hardcover.knownRemaining()
	}
	var linked, total int
	if err = a.store.DB.QueryRow("SELECT (SELECT count(*) FROM book_hardcover),(SELECT count(*) FROM books)").Scan(&linked, &total); err != nil {
		return err
	}
	data := page{"PrivacyName": hcPrivacyNames[hcPrivacy(prefs)], "Linked": linked, "Total": total, "Title": "Hardcover", "Nav": "hardcover", "Enabled": prefs.Hardcover, "Configured": a.hardcover != nil, "Link": link, "Last": last, "Remaining": remaining}
	if err = a.centerData(data, prefs); err != nil {
		return err
	}
	return a.render(w, r, "hardcover.html", data, 200)
}

const hardcoverConnectQuery = `query NextChapterConnection {
 me { id username }
 library: __type(name: "user_books") { fields { name } }
 reads: __type(name: "user_book_reads") { fields { name } }
}`

type hardcoverConnectionResponse struct {
	Me []struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"me"`
	Library *hardcoverType `json:"library"`
	Reads   *hardcoverType `json:"reads"`
}
type hardcoverType struct {
	Fields []struct {
		Name string `json:"name"`
	} `json:"fields"`
}

func hardcoverMissing(t *hardcoverType, required []string) []string {
	found := map[string]bool{}
	if t != nil {
		for _, f := range t.Fields {
			found[f.Name] = true
		}
	}
	var missing []string
	for _, name := range required {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	return missing
}
func (a *App) hardcoverTest(w http.ResponseWriter, r *http.Request) error {
	a.hardcoverMu.Lock()
	defer a.hardcoverMu.Unlock()
	if err := current(a.store.DB, r); err != nil {
		return err
	}
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	if !prefs.Hardcover {
		return bad("Enable Hardcover in Preferences → Privacy before testing the connection.")
	}
	if a.hardcover == nil {
		return bad("Set HARDCOVER_API_TOKEN in the server environment first.")
	}
	var out hardcoverConnectionResponse
	remoteErr := a.hardcover.do(r.Context(), hardcoverConnectQuery, nil, &out)
	result := hardcoverTestResult{At: canonicalTime(a.now()), Remaining: a.hardcover.knownRemaining()}
	if remoteErr != nil {
		result.Message = remoteErr.Error()
	} else {
		missing := hardcoverMissing(out.Library, []string{"id", "user_id", "book_id", "edition_id", "status_id", "rating", "updated_at"})
		missingReads := hardcoverMissing(out.Reads, []string{"id", "user_book_id", "started_at", "finished_at", "finished_at_precision", "progress_pages", "progress_seconds"})
		if len(missing)+len(missingReads) > 0 {
			result.Message = "Hardcover schema changed. Missing user_books fields: " + strings.Join(missing, ", ") + "; user_book_reads fields: " + strings.Join(missingReads, ", ") + ". No account was linked."
		} else if len(out.Me) != 1 || out.Me[0].ID < 1 || out.Me[0].Username == "" || len(out.Me[0].Username) > 300 {
			result.Message = "Hardcover did not identify one account. No account was linked."
		} else {
			result.OK = true
			result.Message = "Connection verified. No book data was sent or changed."
		}
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if result.OK {
			user := out.Me[0]
			if old, err := loadHardcoverLink(tx); err != nil {
				return err
			} else if old != nil && old.UserID != user.ID {
				result.Message += " The linked Hardcover account changed from " + old.Username + "; review any later Hardcover links."
			}
			username := strings.ReplaceAll(user.Username, a.hardcover.token, "[redacted]")
			if _, err := tx.Exec("INSERT INTO hardcover_link(id,hardcover_user_id,username,linked_at) VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET hardcover_user_id=excluded.hardcover_user_id,username=excluded.username,linked_at=excluded.linked_at", user.ID, username, result.At); err != nil {
				return err
			}
		}
		if err := setMeta(tx, "hardcover_test", result); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover")
}
func (a *App) hardcoverDisconnect(w http.ResponseWriter, r *http.Request) error {
	a.hardcoverMu.Lock()
	defer a.hardcoverMu.Unlock()
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM hardcover_link; DELETE FROM meta WHERE key IN ('hardcover_test','hardcover_history','hardcover_pull_since')"); err != nil {
			return err
		}
		prefs, err := readMeta(tx, "preferences", defaultPreferences)
		if err != nil {
			return err
		}
		// The account is gone, so what was sent to it is history of nothing.
		if _, err = tx.Exec("DELETE FROM hardcover_sync"); err != nil {
			return err
		}
		prefs.Hardcover, prefs.HardcoverSyncPull, prefs.HardcoverSyncPush = false, false, false
		if err = setMeta(tx, "preferences", prefs); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover")
}
