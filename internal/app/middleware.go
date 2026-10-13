package app

import (
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
)

// middleware wraps every request: a request ID and panic recovery, security
// headers, the session cookie, the login redirect, and form parsing with the
// CSRF check for POSTs.
func (a *App) middleware(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		r = withState(r, requestState{ID: id})
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			// r is reassigned below, so this sees the fullest state available.
			a.logger.Error("panic", "request_id", id, "method", r.Method, "path", r.URL.Path, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			a.respond(w, r, 500, genericFailure)
		}()
		a.securityHeaders(w)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			mux.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		s, ok, rev, err := a.session(w, r)
		if err != nil {
			a.logger.Error("database unavailable", "request_id", id, "method", r.Method, "path", r.URL.Path, "err", err)
			http.Error(w, "Database unavailable. Reference: "+id, 503)
			return
		}
		if !a.config.Demo && !s.Authenticated && r.URL.Path != "/login" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		r = withState(r, requestState{id, s, rev})
		if r.Method == http.MethodPost {
			err := parseForm(w, r, uploadLimit(r.URL.Path))
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
			if err == nil {
				err = checkCSRF(r, s, ok)
			}
			if err != nil {
				a.fail(w, r, err)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *App) securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if a.config.SecureCookie {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

// uploadLimit is the largest POST body a route accepts: 2 MB, except for a
// JSON restore.
func uploadLimit(path string) int64 {
	if path == "/imports/restore" {
		return maxRestoreUpload
	}
	return maxUpload
}

// parseForm reads a POST body of at most limit bytes. The caller removes any
// multipart temporary files.
func parseForm(w http.ResponseWriter, r *http.Request, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		err = r.ParseMultipartForm(limit)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		return httpError{Status: 413, Message: fmt.Sprintf("The form or file is too large or malformed. Upload at most %d MB.", limit/1_000_000)}
	}
	return nil
}
