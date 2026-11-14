package app

import (
	"database/sql"
	"net/http"
	"strings"
)

// settings shows the preferences page and saves one section of it at a time.
// A form carries a hidden "section" (ranking or privacy) and changes only
// that section's keys, so saving one cannot undo the other. A post with no
// section saves every preference at once, as before.
func (a *App) settings(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodPost {
		return a.saveSettings(w, r)
	}
	return a.renderSettings(w, r, "", 0)
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) error {
	section := r.FormValue("section")
	if section != "" && section != "ranking" && section != "privacy" {
		return bad("Choose a section to save.")
	}
	// The privacy form has no ranking fields, so only the others validate them.
	form := Preferences{KeepReviews: r.FormValue("keep_reviews") != "", BookLookup: r.FormValue("book_lookup") != ""}
	if section != "privacy" {
		var err error
		if form, err = preferenceFields(r); err != nil {
			return err
		}
	}
	stored, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	prefs := form
	switch section {
	case "ranking":
		prefs = stored
		prefs.LibraryWeight, prefs.Length, prefs.Mood, prefs.UseEstimates = form.LibraryWeight, form.Length, form.Mood, form.UseEstimates
	case "privacy":
		prefs = stored
		prefs.KeepReviews, prefs.BookLookup = form.KeepReviews, form.BookLookup
		// Turning review text off deletes it, so ask first.
		if stored.KeepReviews && !prefs.KeepReviews && r.FormValue("confirm") != "yes" {
			var kept int
			if err = a.store.DB.QueryRow("SELECT count(*) FROM reading_log WHERE review IS NOT NULL").Scan(&kept); err != nil {
				return err
			}
			if kept > 0 {
				return a.renderSettings(w, r, "privacy", kept)
			}
		}
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if err := setMeta(tx, "preferences", prefs); err != nil {
			return err
		}
		// Opting out of keeping reviews deletes the text already kept.
		if !prefs.KeepReviews && section != "ranking" {
			if _, err := tx.Exec("UPDATE reading_log SET review=NULL WHERE review IS NOT NULL"); err != nil {
				return err
			}
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	dest := "/settings?notice=saved"
	if section == "privacy" {
		dest += "&open=privacy#privacy"
	}
	return redirect(w, r, dest)
}

// renderSettings shows the page. open names a section to expand (Ranking is
// always open); confirmDelete, when non-zero, asks the reader to confirm
// deleting that many stored reviews.
func (a *App) renderSettings(w http.ResponseWriter, r *http.Request, open string, confirmDelete int) error {
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	version, _, err := schemaVersion(a.store.DB)
	if err != nil {
		return err
	}
	ai, err := a.aiStatus(a.store.DB)
	if err != nil {
		return err
	}
	if open == "" {
		open = r.URL.Query().Get("open")
	}
	if strings.HasPrefix(r.URL.Query().Get("notice"), "ai-") {
		open = "ai"
	}
	return a.render(w, r, "settings.html", page{"Title": "Preferences", "Prefs": prefs, "SchemaVersion": version, "AI": ai, "Open": open, "ConfirmDelete": confirmDelete}, 200)
}
