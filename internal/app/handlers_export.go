package app

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

func (a *App) history(w http.ResponseWriter, r *http.Request) error {
	events, err := loadEvents(a.store.DB, 0, 500)
	if err != nil {
		return err
	}
	return a.render(w, r, "history.html", page{"Events": events}, 200)
}
func (a *App) export(w http.ResponseWriter, r *http.Request) error {
	var data []byte
	err := a.store.Write(func(tx *sql.Tx) error {
		books, err := loadBooks(tx)
		if err != nil {
			return err
		}
		events, err := loadEvents(tx, 0, 0)
		if err != nil {
			return err
		}
		prefs, err := readMeta(tx, "preferences", defaultPreferences)
		if err != nil {
			return err
		}
		version, _, err := schemaVersion(tx)
		if err != nil {
			return err
		}
		aliases, err := loadAliases(tx)
		if err != nil {
			return err
		}
		data, err = json.MarshalIndent(map[string]any{"schema_version": version, "aliases": aliases, "exported_at": canonicalTime(time.Now()), "books": books, "events": events, "preferences": prefs}, "", "  ")
		return err
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="next-chapter.json"`)
	_, err = w.Write(data)
	return err
}
func (a *App) backup(w http.ResponseWriter, r *http.Request) error {
	data, err := a.store.Backup()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="next-chapter.sqlite3"`)
	_, err = w.Write(data)
	return err
}
