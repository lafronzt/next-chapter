package app

import (
	"database/sql"
	"net/http"
)

func (a *App) settings(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodPost {
		prefs, err := preferenceFields(r)
		if err != nil {
			return err
		}
		conn, err := connectionFields(r)
		if err != nil {
			return err
		}
		err = a.store.Write(func(tx *sql.Tx) error {
			if err := current(tx, r); err != nil {
				return err
			}
			if err := setMeta(tx, "preferences", prefs); err != nil {
				return err
			}
			if err := setMeta(tx, "sync", conn); err != nil {
				return err
			}
			return bump(tx)
		})
		if err != nil {
			return err
		}
		return redirect(w, r, "/settings?notice=saved")
	}
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	conn, err := readMeta(a.store.DB, "sync", Connection{Shelf: "to-read"})
	if err != nil {
		return err
	}
	last, err := readMeta(a.store.DB, "last_sync", SyncResult{})
	if err != nil {
		return err
	}
	poll, err := readMeta(a.store.DB, "poll_state", PollState{})
	if err != nil {
		return err
	}
	version, _, err := schemaVersion(a.store.DB)
	if err != nil {
		return err
	}
	return a.render(w, r, "settings.html", page{"Prefs": prefs, "Connection": conn, "LastSync": last, "Poll": poll, "SchemaVersion": version}, 200)
}
