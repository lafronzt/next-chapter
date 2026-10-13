package app

import (
	"database/sql"
	"net/http"
)

// aiSettings switches AI assistance on or off for this library. It changes no
// books, so the revision stays the same.
func (a *App) aiSettings(w http.ResponseWriter, r *http.Request) error {
	if a.ai == nil {
		return bad("No AI provider is configured for this app.")
	}
	var on bool
	switch r.FormValue("ai") {
	case "on":
		on = true
	case "off":
	default:
		return bad("Choose whether to turn AI assistance on or off.")
	}
	err := a.store.Write(func(tx *sql.Tx) error { return setMeta(tx, "ai", aiSettings{Enabled: on}) })
	if err != nil {
		return err
	}
	if on {
		return redirect(w, r, "/settings?notice=ai-on#ai")
	}
	return redirect(w, r, "/settings?notice=ai-off#ai")
}

// aiTest sends a fixed prompt with no library data.
func (a *App) aiTest(w http.ResponseWriter, r *http.Request) error {
	if err := a.testAI(r.Context()); err != nil {
		return err
	}
	return redirect(w, r, "/settings?notice=ai-tested#ai")
}
