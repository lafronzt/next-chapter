package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var assets embed.FS

type Config struct {
	Database, Password, Secret string
	Demo, SecureCookie         bool
	// TrustedProxyHops is how many reverse proxies append to X-Forwarded-For
	// in front of the app. Zero ignores the header.
	TrustedProxyHops int
}
type App struct {
	store       *Store
	config      Config
	templates   *template.Template
	passwordKey []byte
	syncMu      sync.Mutex
	fetch       func(context.Context, Connection) ([]Record, error)
	logger      *slog.Logger
}
type sessionData struct {
	Authenticated bool   `json:"auth"`
	CSRF          string `json:"csrf"`
	Expires       int64  `json:"expires"`
	// Binding ties an authenticated session to the current session epoch and
	// password; see sessionBinding.
	Binding string `json:"binding,omitempty"`
}
type requestState struct {
	ID       string
	Session  sessionData
	Revision int64
}
type contextKey struct{}
type page map[string]any
type httpError struct {
	Status  int
	Message string
	Header  http.Header
}

func (e httpError) Error() string   { return e.Message }
func bad(message string) error      { return httpError{Status: 400, Message: message} }
func conflict(message string) error { return httpError{Status: 409, Message: message} }

const (
	loginClientLimit = 10  // failed logins per client per minute
	loginGlobalLimit = 100 // failed logins across all clients per minute
	genericFailure   = "Something went wrong. Please try again."
	sessionMissing   = "Your session expired or was reset, so this form could not be sent. Reload the page and try again."
	sessionMismatch  = "Your form expired. Reload the page and try again."
)

func newRequestID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func New(c Config) (*App, error) {
	if c.Demo && c.Secret == "" {
		c.Secret = randomToken()
	}
	if len(c.Secret) < 32 {
		return nil, fmt.Errorf("APP_SECRET_KEY must contain at least 32 characters")
	}
	if !c.Demo && len(c.Password) < 12 {
		return nil, fmt.Errorf("APP_PASSWORD must contain at least 12 characters")
	}
	store, err := OpenStore(c.Database)
	if err != nil {
		return nil, err
	}
	funcs := template.FuncMap{
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
	templates, err := template.New("").Funcs(funcs).ParseFS(assets, "web/templates/*.html")
	if err != nil {
		store.Close()
		return nil, err
	}
	key, err := pbkdf2.Key(sha256.New, c.Password, []byte("next-chapter:"+c.Secret), 600000, 32)
	if err != nil {
		store.Close()
		return nil, err
	}
	err = store.Write(func(tx *sql.Tx) error {
		epoch, err := readMeta(tx, "session_epoch", "")
		if err != nil || epoch != "" {
			return err
		}
		return setMeta(tx, "session_epoch", randomToken())
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	return &App{store: store, config: c, templates: templates, passwordKey: key, fetch: FetchGoodreads, logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}, nil
}
func (a *App) Close() error { return a.store.Close() }
func (a *App) signSession(s sessionData) string {
	data, _ := json.Marshal(s)
	body := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, []byte(a.config.Secret))
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (a *App) readSession(r *http.Request) (sessionData, bool) {
	cookie, err := r.Cookie("nc_session")
	if err != nil || len(cookie.Value) > 4096 {
		return sessionData{}, false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return sessionData{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return sessionData{}, false
	}
	mac := hmac.New(sha256.New, []byte(a.config.Secret))
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return sessionData{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return sessionData{}, false
	}
	var s sessionData
	if json.Unmarshal(data, &s) != nil || s.Expires <= time.Now().Unix() || len(s.CSRF) != 43 {
		return sessionData{}, false
	}
	return s, true
}
func (a *App) setSession(w http.ResponseWriter, s sessionData) {
	http.SetCookie(w, &http.Cookie{Name: "nc_session", Value: a.signSession(s), Path: "/", HttpOnly: true, Secure: a.config.SecureCookie, SameSite: http.SameSiteLaxMode, Expires: time.Unix(s.Expires, 0)})
}

// sessionBinding is HMAC(secret, epoch || SHA-256(passwordKey)). Rotating the
// epoch or changing APP_PASSWORD changes it, which signs out every session,
// without storing the password or its hash anywhere.
func (a *App) sessionBinding(epoch string) string {
	digest := sha256.Sum256(a.passwordKey)
	mac := hmac.New(sha256.New, []byte(a.config.Secret))
	mac.Write([]byte(epoch))
	mac.Write(digest[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// clientIP identifies a client for login throttling. X-Forwarded-For is used
// only when the operator says how many proxies append to it; the Nth entry
// from the right is the address the outermost trusted proxy saw.
func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if hops := a.config.TrustedProxyHops; hops > 0 {
		entries := []string{}
		for _, value := range r.Header.Values("X-Forwarded-For") {
			for _, entry := range strings.Split(value, ",") {
				entries = append(entries, strings.TrimSpace(entry))
			}
		}
		if len(entries) >= hops {
			if ip := net.ParseIP(entries[len(entries)-hops]); ip != nil {
				return ip.String()
			}
		}
	}
	return host
}
func state(r *http.Request) requestState {
	s, _ := r.Context().Value(contextKey{}).(requestState)
	return s
}
func withState(r *http.Request, s requestState) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), contextKey{}, s))
}
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	static, _ := fs.Sub(assets, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	endpoints := map[string]func(http.ResponseWriter, *http.Request) error{
		"GET /{$}": a.index, "GET /login": a.login, "POST /login": a.login, "POST /logout": a.logout,
		"GET /books/new": a.newBook, "POST /books/new": a.newBook, "GET /books/{id}": a.editBook, "POST /books/{id}": a.editBook,
		"POST /books/{id}/action": a.bookAction, "POST /books/{id}/loan": a.loan,
		"POST /suggest": a.suggest, "GET /plans/{id}": a.plan, "POST /plans/{id}": a.plan, "POST /undo": a.undo,
		"GET /imports": a.importPage, "POST /imports": a.importPage, "GET /examples/{name}": a.example,
		"GET /settings": a.settings, "POST /settings": a.settings, "POST /sync": a.syncNow, "POST /sessions/revoke": a.revokeSessions,
		"POST /books/{id}/triage": a.triageBook, "POST /triage": a.triageAll,
		"GET /history": a.history, "GET /export.json": a.export, "GET /backup.sqlite3": a.backup, "POST /demo": a.demo,
	}
	for pattern, handler := range endpoints {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := handler(w, r); err != nil {
				a.fail(w, r, err)
			}
		})
	}
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if a.config.SecureCookie {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/static/") {
			mux.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		epoch, err := readMeta(a.store.DB, "session_epoch", "")
		var rev int64
		if err == nil {
			rev, err = revision(a.store.DB)
		}
		if err != nil {
			a.logger.Error("database unavailable", "request_id", id, "method", r.Method, "path", r.URL.Path, "err", err)
			http.Error(w, "Database unavailable. Reference: "+id, 503)
			return
		}
		s, ok := a.readSession(r)
		if ok && s.Authenticated && !hmac.Equal([]byte(s.Binding), []byte(a.sessionBinding(epoch))) {
			s.Authenticated, s.Binding = false, ""
		}
		// A failing POST gets no new cookie; the next page load issues one.
		if !ok && r.Method != http.MethodPost {
			s = sessionData{CSRF: randomToken(), Expires: time.Now().Add(time.Hour).Unix()}
			a.setSession(w, s)
		}
		if !a.config.Demo && !s.Authenticated && r.URL.Path != "/login" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		r = withState(r, requestState{id, s, rev})
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				err = r.ParseMultipartForm(maxUpload)
				if r.MultipartForm != nil {
					defer r.MultipartForm.RemoveAll()
				}
			} else {
				err = r.ParseForm()
			}
			if err != nil {
				a.fail(w, r, httpError{Status: 413, Message: "The form or file is too large or malformed. Upload at most 2 MB."})
				return
			}
			if !ok {
				a.fail(w, r, bad(sessionMissing))
				return
			}
			if !hmac.Equal([]byte(s.CSRF), []byte(r.FormValue("csrf"))) {
				a.fail(w, r, bad(sessionMismatch))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
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
	notices := map[string]string{"triaged": "Sorted. Only reading status changed; books you want to read wait at the end of your queue for placement.", "saved": "Saved to your library.", "ordered": "Your new order is saved. You can undo it until your next library change.", "undone": "Previous order restored.", "imported": "Import complete. Existing reading status and queue order were preserved.", "loan": "Library event recorded. Reading status stays the same.", "synced": "Goodreads checked. New books are marked Needs placement.", "signedout": "Signed out on every device. Sign in again to continue."}
	notice := r.URL.Query().Get("notice")
	data["Notice"] = notices[notice]
	if moved, _ := strconv.Atoi(r.URL.Query().Get("moved")); notice == "imported" && moved > 0 {
		data["Notice"] = fmt.Sprintf("Import complete. %d library-only books moved out of To review to match Goodreads; other books kept their reading status and queue order.", moved)
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
func current(q queryer, r *http.Request) error {
	rev, err := revision(q)
	if err != nil {
		return err
	}
	if r.FormValue("revision") != strconv.FormatInt(rev, 10) {
		return conflict("Your library changed since this page loaded. Reload before making another change.")
	}
	return nil
}
func requestID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, bad("Choose a valid book.")
	}
	return id, nil
}
func bookByID(q queryer, id int64) (Book, error) {
	all, err := loadBooks(q)
	if err != nil {
		return Book{}, err
	}
	for _, b := range all {
		if b.ID == id {
			return b, nil
		}
	}
	return Book{}, sql.ErrNoRows
}

func (a *App) login(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return a.render(w, r, "login.html", nil, 200)
	}
	// Each attempt is recorded as a failure before the expensive key
	// derivation, so concurrent guesses cannot exceed the limit; a successful
	// login removes its own row again.
	client := a.clientIP(r)
	var attempt int64
	limited := false
	err := a.store.Write(func(tx *sql.Tx) error {
		now := float64(time.Now().UnixNano()) / 1e9
		if _, err := tx.Exec("DELETE FROM login_failures WHERE at<?", now-60); err != nil {
			return err
		}
		var mine, all int
		if err := tx.QueryRow("SELECT COALESCE(SUM(client=?),0), count(*) FROM login_failures", client).Scan(&mine, &all); err != nil {
			return err
		}
		if mine >= loginClientLimit || all >= loginGlobalLimit {
			limited = true
			return nil
		}
		result, err := tx.Exec("INSERT INTO login_failures(client,at) VALUES(?,?)", client, now)
		if err != nil {
			return err
		}
		attempt, err = result.LastInsertId()
		return err
	})
	if err != nil {
		return err
	}
	if limited {
		return httpError{Status: 429, Message: "Too many login attempts. Wait a minute and try again.", Header: http.Header{"Retry-After": {"60"}}}
	}
	key, err := pbkdf2.Key(sha256.New, r.FormValue("password"), []byte("next-chapter:"+a.config.Secret), 600000, 32)
	if err != nil {
		return err
	}
	if !hmac.Equal(key, a.passwordKey) {
		return a.render(w, r, "login.html", page{"LoginError": "That password was not recognized."}, 401)
	}
	var epoch string
	err = a.store.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM login_failures WHERE rowid=?", attempt); err != nil {
			return err
		}
		var err error
		epoch, err = readMeta(tx, "session_epoch", "")
		return err
	})
	if err != nil {
		return err
	}
	a.setSession(w, sessionData{Authenticated: true, CSRF: randomToken(), Expires: time.Now().Add(7 * 24 * time.Hour).Unix(), Binding: a.sessionBinding(epoch)})
	return redirect(w, r, "/")
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) error {
	a.setSession(w, sessionData{CSRF: randomToken(), Expires: time.Now().Add(time.Hour).Unix()})
	return redirect(w, r, "/login")
}

// revokeSessions rotates the session epoch, signing out every browser. It is
// not a library change, so the revision stays the same.
func (a *App) revokeSessions(w http.ResponseWriter, r *http.Request) error {
	if a.config.Demo {
		return sql.ErrNoRows
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		return setMeta(tx, "session_epoch", randomToken())
	})
	if err != nil {
		return err
	}
	a.setSession(w, sessionData{CSRF: randomToken(), Expires: time.Now().Add(time.Hour).Unix()})
	return redirect(w, r, "/login?notice=signedout")
}

// listView is one tab of the book list. views is the only list of tabs; the
// index handler and templates both use it.
type listView struct {
	Key, Label, Status, Heading, Intro string
}

const defaultIntro = "Your list should follow your curiosity, not the date you added a book."

var views = []listView{
	{"queue", "Up next", "to-read", "A good book. Then another.", defaultIntro},
	{"reading", "Reading", "reading", "In good company.", defaultIntro},
	{"read", "Finished", "read", "Chapters completed.", defaultIntro},
	{"unreviewed", "To review", "unreviewed", "Borrowed, but not yet decided.", "Library loans arrive here so borrowing never counts as wanting to read. Tell us what each one means to you."},
	{"abandoned", "Set aside", "abandoned", "Not every book is for now.", defaultIntro},
	{"archived", "Archived", "archived", "Kept for the record.", defaultIntro},
	{"all", "All books", "", "Your books, together.", defaultIntro},
}

func findView(key string) listView {
	for _, v := range views {
		if v.Key == key {
			return v
		}
	}
	return views[0]
}
func searchQuery(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// shows reports whether a book belongs in this view for a normalized query.
func (v listView) shows(b Book, query string) bool {
	return (v.Status == "" || b.Status == v.Status) && (query == "" || strings.Contains(strings.ToLower(b.Title+" "+b.Author+" "+b.Tags), query))
}
func (a *App) index(w http.ResponseWriter, r *http.Request) error {
	all, err := loadBooks(a.store.DB)
	if err != nil {
		return err
	}
	queue := queueOnly(all)
	view := findView(r.URL.Query().Get("view"))
	query := searchQuery(r.URL.Query().Get("q"))
	selected := []Book{}
	positions := map[int64]int{}
	pending, active := 0, 0
	for i, b := range queue {
		positions[b.ID] = i + 1
		if b.NeedsPlacement {
			pending++
		}
	}
	for _, b := range all {
		for _, l := range b.Loans {
			if l.Status == "borrowed" || l.Status == "ready" {
				active++
				break
			}
		}
		if view.shows(b, query) {
			selected = append(selected, b)
		}
	}
	var undoRev int64
	err = a.store.DB.QueryRow("SELECT revision FROM undo_queue WHERE id=1").Scan(&undoRev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	canUndo := err == nil && undoRev == state(r).Revision
	return a.render(w, r, "index.html", page{"Items": selected, "AllBooks": all, "Queue": queue, "View": view.Key, "ViewInfo": view, "Views": views, "Query": query, "Positions": positions, "Pending": pending, "ActiveLoans": active, "CanUndo": canUndo}, 200)
}
