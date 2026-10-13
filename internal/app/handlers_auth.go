package app

import (
	"crypto/hmac"
	"database/sql"
	"net"
	"net/http"
	"strings"
	"time"
)

func (a *App) login(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return a.render(w, r, "login.html", page{"Title": "Sign in"}, 200)
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
	key, err := deriveKey(r.FormValue("password"), a.config.Secret)
	if err != nil {
		return err
	}
	if !hmac.Equal(key, a.passwordKey) {
		return a.render(w, r, "login.html", page{"Title": "Sign in", "LoginError": "That password was not recognized."}, 401)
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
