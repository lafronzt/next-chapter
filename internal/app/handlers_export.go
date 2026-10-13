package app

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
)

// history pages through every library event, optionally for one source
// or status. Unknown filter values are ignored.
func (a *App) history(w http.ResponseWriter, r *http.Request) error {
	sources, err := eventSources(a.store.DB)
	if err != nil {
		return err
	}
	f := eventFilter{Source: r.URL.Query().Get("source"), Status: r.URL.Query().Get("status")}
	if !in(f.Source, sources) {
		f.Source = ""
	}
	if !in(f.Status, libraryStates) {
		f.Status = ""
	}
	total, err := countEvents(a.store.DB, f)
	if err != nil {
		return err
	}
	pages := newPager(r, total, "/history", url.Values{"source": {f.Source}, "status": {f.Status}})
	events, err := pageEvents(a.store.DB, f, pages.PerPage, pages.offset())
	if err != nil {
		return err
	}
	return a.render(w, r, "history.html", page{"Title": pageTitle("Library history", pages), "Events": events, "Pager": pages, "Filter": f, "Sources": sources}, 200)
}
func (a *App) export(w http.ResponseWriter, r *http.Request) error {
	var data []byte
	err := a.store.Write(func(tx *sql.Tx) error {
		f, err := exportLibrary(tx)
		if err != nil {
			return err
		}
		data, err = json.MarshalIndent(f, "", "  ")
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
