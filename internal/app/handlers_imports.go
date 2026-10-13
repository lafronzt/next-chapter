package app

import (
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

func (a *App) importPage(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodGet {
		return a.render(w, r, "imports.html", page{"Title": "Import books"}, 200)
	}
	source := r.FormValue("source")
	if !in(source, []string{"goodreads", "libby", "nypl"}) {
		return bad("Choose an import source.")
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		return bad("Choose a CSV file.")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil {
		return err
	}
	records, problems, parseErr := ParseCSV(data, source)
	if parseErr != nil {
		return bad(parseErr.Error())
	}
	if len(problems) > 0 {
		count := len(problems)
		if count > 30 {
			problems = problems[:30]
		}
		return a.render(w, r, "imports.html", page{"Title": "Import books", "Errors": problems, "ErrorCount": count}, 400)
	}
	var id string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		preview, err := previewImport(tx, records)
		if err != nil {
			return err
		}
		prefs, err := readMeta(tx, "preferences", defaultPreferences)
		if err != nil {
			return err
		}
		ignored := 0
		for i := range records {
			if records[i].RatingIgnored {
				ignored++
			}
			// Review text is personal; without the opt-in it is not even
			// kept in the stored preview. Its length always is.
			if !prefs.KeepReviews {
				records[i].Review = ""
			}
		}
		id, err = savePlan(tx, "import", Plan{Records: records, Source: source, Upgrades: preview.Upgraded, Rated: preview.Rated, RatingsKept: preview.RatingsKept, RatingsIgnored: ignored, Logged: preview.Logged})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+id)
}

// restoreUpload validates a Next Chapter JSON export and previews restoring it
// into this empty library.
func (a *App) restoreUpload(w http.ResponseWriter, r *http.Request) error {
	file, _, err := r.FormFile("file")
	if err != nil {
		return bad("Choose a Next Chapter JSON export.")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRestoreUpload+1))
	if err != nil {
		return err
	}
	f, problems, err := decodeExport(data)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		count := len(problems)
		problems = problems[:min(count, maxProblemsShown)]
		return a.render(w, r, "imports.html", page{"Title": "Import books", "RestoreErrors": problems, "RestoreErrorCount": count}, 400)
	}
	var id string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		preview, err := previewRestore(tx, f)
		if err != nil {
			return err
		}
		id, err = savePlan(tx, "restore", Plan{Restore: &preview})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+id)
}

func (a *App) example(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimSuffix(r.PathValue("name"), ".csv")
	if !in(name, []string{"goodreads", "nypl", "libby"}) {
		return sql.ErrNoRows
	}
	data, err := assets.ReadFile("web/examples/" + name + ".csv")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	_, err = w.Write(data)
	return err
}

func (a *App) syncNow(w http.ResponseWriter, r *http.Request) error {
	// A panic here is recovered by Handler, which logs it with the request ID.
	if _, err := a.syncOnce(r.Context(), a.now()); err != nil {
		var s *syncError
		if !errors.As(err, &s) {
			return err
		}
		status := 400
		if errors.Is(err, errSyncBusy) {
			status = 409
		} else if errors.Is(err, errFeedUnavailable) {
			log.Printf("Goodreads check failed (class=%s)", s.class)
		}
		return httpError{Status: status, Message: syncMessage(err, true)}
	}
	return redirect(w, r, "/settings?notice=synced")
}
