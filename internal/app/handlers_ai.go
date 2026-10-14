package app

import (
	"cmp"
	"context"
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

// aiTest sends a fixed prompt with no library data, in the background.
func (a *App) aiTest(w http.ResponseWriter, r *http.Request) error {
	if ai, err := a.aiStatus(a.store.DB); err != nil || !ai.Available() {
		return cmp.Or(err, error(errAIOff))
	}
	id, err := a.startAIJob("the connection test", "/settings#ai", "", func(ctx context.Context) (string, error) {
		return "/settings?notice=ai-tested#ai", a.testAI(ctx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/ai/jobs/"+id)
}
