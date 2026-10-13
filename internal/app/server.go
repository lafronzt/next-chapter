package app

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
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
	now         func() time.Time
	pollTick    time.Duration // how often Poll checks whether a sync is due
	logger      *slog.Logger
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
	templates, err := template.New("").Funcs(templateFuncs()).ParseFS(assets, "web/templates/*.html")
	if err != nil {
		store.Close()
		return nil, err
	}
	key, err := deriveKey(c.Password, c.Secret)
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
	return &App{store: store, config: c, templates: templates, passwordKey: key, fetch: FetchGoodreads, now: time.Now, pollTick: time.Minute, logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}, nil
}

// passwordIterations is the PBKDF2-SHA256 cost (OWASP's 2023 figure). Tests
// lower it, because the race detector makes each derivation take about a second.
var passwordIterations = 600_000

func deriveKey(password, secret string) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, []byte("next-chapter:"+secret), passwordIterations, 32)
}
func (a *App) Close() error { return a.store.Close() }
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
		"GET /books/{id}/delete": a.deleteBook, "POST /books/{id}/delete": a.deleteBook, "GET /books/{id}/merge": a.mergeBook, "POST /books/{id}/merge": a.mergeBook,
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
	return a.middleware(mux)
}
