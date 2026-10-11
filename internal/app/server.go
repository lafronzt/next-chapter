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
	"log"
	"net/http"
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
}
type App struct {
	store       *Store
	config      Config
	templates   *template.Template
	passwordKey []byte
	syncMu      sync.Mutex
	fetch       func(context.Context, Connection) ([]Record, error)
	now         func() time.Time
}
type sessionData struct {
	Authenticated bool   `json:"auth"`
	CSRF          string `json:"csrf"`
	Expires       int64  `json:"expires"`
}
type requestState struct {
	Session  sessionData
	Revision int64
}
type contextKey struct{}
type page map[string]any
type httpError struct {
	Status  int
	Message string
}

func (e httpError) Error() string   { return e.Message }
func bad(message string) error      { return httpError{400, message} }
func conflict(message string) error { return httpError{409, message} }
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
	return &App{store: store, config: c, templates: templates, passwordKey: key, fetch: FetchGoodreads, now: time.Now}, nil
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
func state(r *http.Request) requestState { return r.Context().Value(contextKey{}).(requestState) }
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
		"GET /settings": a.settings, "POST /settings": a.settings, "POST /sync": a.syncNow,
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/static/") {
			mux.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		s, ok := a.readSession(r)
		if !ok {
			s = sessionData{CSRF: randomToken(), Expires: time.Now().Add(time.Hour).Unix()}
			a.setSession(w, s)
		}
		if !a.config.Demo && !s.Authenticated && r.URL.Path != "/login" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		rev, err := revision(a.store.DB)
		if err != nil {
			log.Print(err)
			http.Error(w, "Database unavailable", 503)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), contextKey{}, requestState{s, rev}))
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
				a.fail(w, r, httpError{413, "The form or file is too large or malformed. Upload at most 2 MB."})
				return
			}
			if !hmac.Equal([]byte(s.CSRF), []byte(r.FormValue("csrf"))) {
				a.fail(w, r, bad("Your form expired. Reload the page and try again."))
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
	notices := map[string]string{"saved": "Saved to your library.", "ordered": "Your new order is saved. You can undo it until your next library change.", "undone": "Previous order restored.", "imported": "Import complete. Existing reading status and queue order were preserved.", "loan": "Library event recorded. Reading status stays the same.", "synced": "Goodreads checked. New books are marked Needs placement.", "deleted": "Book deleted. Future imports will skip it.", "merged": "Books merged. Future imports of the merged book will update this one."}
	data["Notice"] = notices[r.URL.Query().Get("notice")]
	if skipped, _ := strconv.Atoi(r.URL.Query().Get("skipped")); skipped > 0 && data["Notice"] != "" {
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
func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message := 500, "Something went wrong. Please try again."
	var h httpError
	if errors.As(err, &h) {
		status, message = h.Status, h.Message
	} else if errors.Is(err, sql.ErrNoRows) {
		status, message = 404, "That record or preview could not be found."
	} else {
		log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	}
	if err := a.render(w, r, "error.html", page{"Message": message}, status); err != nil {
		log.Print(err)
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
	if r.Method == http.MethodPost {
		limited := false
		err := a.store.Write(func(tx *sql.Tx) error {
			if _, err := tx.Exec("DELETE FROM login_attempts WHERE at<?", time.Now().Unix()-60); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow("SELECT count(*) FROM login_attempts").Scan(&count); err != nil {
				return err
			}
			if count >= 10 {
				limited = true
				return nil
			}
			_, err := tx.Exec("INSERT INTO login_attempts VALUES(?)", time.Now().Unix())
			return err
		})
		if err != nil {
			return err
		}
		if limited {
			return httpError{429, "Too many login attempts. Wait a minute and try again."}
		}
		key, err := pbkdf2.Key(sha256.New, r.FormValue("password"), []byte("next-chapter:"+a.config.Secret), 600000, 32)
		if err != nil {
			return err
		}
		if hmac.Equal(key, a.passwordKey) {
			a.setSession(w, sessionData{true, randomToken(), time.Now().Add(7 * 24 * time.Hour).Unix()})
			return redirect(w, r, "/")
		}
		return a.render(w, r, "login.html", page{"LoginError": "That password was not recognized."}, 401)
	}
	return a.render(w, r, "login.html", nil, 200)
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) error {
	a.setSession(w, sessionData{CSRF: randomToken(), Expires: time.Now().Add(time.Hour).Unix()})
	return redirect(w, r, "/login")
}

// matchesQuery is the list search: a lowercase substring of title, author, or tags.
func matchesQuery(b Book, query string) bool {
	return query == "" || strings.Contains(strings.ToLower(b.Title+" "+b.Author+" "+b.Tags), query)
}
func (a *App) index(w http.ResponseWriter, r *http.Request) error {
	all, err := loadBooks(a.store.DB)
	if err != nil {
		return err
	}
	queue := queueOnly(all)
	view := r.URL.Query().Get("view")
	if !in(view, []string{"queue", "all", "reading", "read"}) {
		view = "queue"
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
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
		target := view
		if target == "queue" {
			target = "to-read"
		}
		if (view == "all" || b.Status == target) && matchesQuery(b, query) {
			selected = append(selected, b)
		}
	}
	var undoRev int64
	err = a.store.DB.QueryRow("SELECT revision FROM undo_queue WHERE id=1").Scan(&undoRev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	canUndo := err == nil && undoRev == state(r).Revision
	return a.render(w, r, "index.html", page{"Items": selected, "AllBooks": all, "Queue": queue, "View": view, "Query": query, "Positions": positions, "Pending": pending, "ActiveLoans": active, "CanUndo": canUndo}, 200)
}
