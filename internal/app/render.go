package app

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, data page, status int) error {
	if data == nil {
		data = page{}
	}
	s := state(r)
	data["CSRF"] = s.Session.CSRF
	data["Revision"] = s.Revision
	data["Demo"] = a.config.Demo
	data["Authenticated"] = s.Session.Authenticated
	data["ReadingStates"] = readingStates
	data["LibraryStates"] = libraryStates
	data["Path"] = r.URL.Path
	if a.config.Demo || s.Session.Authenticated {
		var waiting int
		if err := a.store.DB.QueryRow("SELECT count(*) FROM books WHERE reading_status='unreviewed'").Scan(&waiting); err != nil {
			return err
		}
		data["UnreviewedCount"] = waiting
	}
	notice := r.URL.Query().Get("notice")
	data["Notice"] = notices[notice]
	if moved, _ := strconv.Atoi(r.URL.Query().Get("moved")); notice == "imported" && moved > 0 {
		data["Notice"] = fmt.Sprintf("Import complete. %d library-only books moved out of To review to match Goodreads; other books kept their reading status and queue order.", moved)
	}
	if skipped, _ := strconv.Atoi(r.URL.Query().Get("skipped")); notice == "imported" && skipped > 0 {
		data["Notice"] = fmt.Sprintf("%s Skipped %d rows for books you deleted.", data["Notice"], skipped)
	}
	var buf bytes.Buffer
	if err := a.templates.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

// notices are the confirmations a redirect can request with ?notice=.
var notices = map[string]string{"triaged": "Sorted. Only reading status changed; books you want to read wait at the end of your queue for placement.", "saved": "Saved to your library.", "ordered": "Your new order is saved. You can undo it until your next library change.", "undone": "Previous order restored.", "imported": "Import complete. Existing reading status and queue order were preserved.", "loan": "Library event recorded. Reading status stays the same.", "synced": "Goodreads checked. New books are marked Needs placement.", "signedout": "Signed out on every device. Sign in again to continue.", "deleted": "Book deleted. Future imports will skip it.", "merged": "Books merged. Future imports of the merged book will update this one."}

func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message := 500, genericFailure
	var h httpError
	if errors.As(err, &h) {
		status, message = h.Status, h.Message
		for key, values := range h.Header {
			w.Header()[key] = values
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		status, message = 404, "That record or preview could not be found."
	}
	if status >= 500 {
		// Never log query strings, form values, or cookies.
		a.logger.Error("request failed", "request_id", state(r).ID, "method", r.Method, "path", r.URL.Path, "err", err)
	}
	a.respond(w, r, status, message)
}

// respond renders the error page. Server errors show the request ID so a
// report can be matched to the log line.
func (a *App) respond(w http.ResponseWriter, r *http.Request, status int, message string) {
	data := page{"Message": message}
	if status >= 500 {
		data["RequestID"] = state(r).ID
	}
	if err := a.render(w, r, "error.html", data, status); err != nil {
		a.logger.Error("render error page", "request_id", state(r).ID, "err", err)
		http.Error(w, message, status)
	}
}
func redirect(w http.ResponseWriter, r *http.Request, path string) error {
	http.Redirect(w, r, path, http.StatusSeeOther)
	return nil
}

// templateFuncs are the helpers available to every template.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"label": func(s string) string { return strings.ReplaceAll(s, "-", " ") },
		"day": func(s string) string {
			if len(s) >= 10 {
				return s[:10]
			}
			return s
		},
		"pages": func(n *int) string {
			if n == nil {
				return ""
			}
			return strconv.Itoa(*n)
		},
		"seriesNumber": func(n *float64) string {
			if n == nil {
				return ""
			}
			return strconv.FormatFloat(*n, 'f', -1, 64)
		},
		"cover": func(id int64) int64 { return id % 5 },
		"pos": func(n int) string {
			if n == 0 {
				return "—"
			}
			return fmt.Sprintf("%02d", n)
		},
		"activeLoan": func(s string) bool { return in(s, []string{"borrowed", "ready", "hold"}) },
		"upper":      strings.ToUpper,
		"version":    func() string { return Version },
		"join":       strings.Join,
		"minute": func(s string) string {
			if len(s) >= 16 {
				return strings.Replace(s[:16], "T", " ", 1)
			}
			return s
		},
		"rankLabel": func(s Suggestion) string {
			if s.Pinned {
				return "Pinned"
			}
			if s.New < s.Old {
				return fmt.Sprintf("Moves up %d", s.Old-s.New)
			}
			if s.New > s.Old {
				return fmt.Sprintf("Moves down %d", s.New-s.Old)
			}
			return "Stays here"
		},
	}
}
