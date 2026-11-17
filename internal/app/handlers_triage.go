package app

import (
	"database/sql"
	"net/http"
	"net/url"
)

// Quick triage moves an unreviewed book to the status the reader chose.
var triageStatuses = map[string]string{"want": "to-read", "read": "read", "skip": "archived"}
var triageLabels = map[string]string{"to-read": "Want to read", "read": "Already read", "archived": "Not interested"}

func unreviewedURL(query, notice string) string {
	v := url.Values{"view": {"unreviewed"}, "notice": {notice}}
	if query != "" {
		v.Set("q", query)
	}
	return "/?" + v.Encode()
}
func (a *App) triageBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	status, ok := triageStatuses[r.FormValue("action")]
	if !ok {
		return bad("Choose Want to read, Already read, or Not interested.")
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		b, err := getBook(tx, id)
		if err != nil {
			return err
		}
		if b.Status != "unreviewed" {
			return bad("Only books awaiting a reading decision can be sorted this way. Use placement actions for queued books.")
		}
		if err = setStatus(tx, b, status); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, unreviewedURL(searchQuery(r.FormValue("q")), "triaged"))
}

// triageAll previews marking every visible unreviewed book; nothing changes
// until the reader confirms the plan.
func (a *App) triageAll(w http.ResponseWriter, r *http.Request) error {
	status, ok := triageStatuses[r.FormValue("action")]
	if !ok {
		return bad("Choose Want to read, Already read, or Not interested.")
	}
	query := searchQuery(r.FormValue("q"))
	var id string
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		ids, err := filteredIDs(tx, bookFilter{Status: "unreviewed", Query: query})
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return bad("No books are awaiting a reading decision here.")
		}
		id, err = savePlan(tx, "triage", Plan{BookIDs: ids, Status: status})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+id)
}
