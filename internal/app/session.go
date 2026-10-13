package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type sessionData struct {
	Authenticated bool   `json:"auth"`
	CSRF          string `json:"csrf"`
	Expires       int64  `json:"expires"`
	// Binding ties an authenticated session to the current session epoch and
	// password; see sessionBinding.
	Binding string `json:"binding,omitempty"`
}

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

// session reads the session cookie and the current revision. An invalid or
// missing cookie gets a fresh anonymous session, except on POSTs: a failing
// POST gets no new cookie; the next page load issues one.
func (a *App) session(w http.ResponseWriter, r *http.Request) (s sessionData, ok bool, rev int64, err error) {
	epoch, err := readMeta(a.store.DB, "session_epoch", "")
	if err == nil {
		rev, err = revision(a.store.DB)
	}
	if err != nil {
		return s, false, 0, err
	}
	s, ok = a.readSession(r)
	if ok && s.Authenticated && !hmac.Equal([]byte(s.Binding), []byte(a.sessionBinding(epoch))) {
		s.Authenticated, s.Binding = false, ""
	}
	if !ok && r.Method != http.MethodPost {
		s = sessionData{CSRF: randomToken(), Expires: time.Now().Add(time.Hour).Unix()}
		a.setSession(w, s)
	}
	return s, ok, rev, nil
}

// checkCSRF requires a valid session cookie and a matching form token.
func checkCSRF(r *http.Request, s sessionData, ok bool) error {
	if !ok {
		return bad(sessionMissing)
	}
	if !hmac.Equal([]byte(s.CSRF), []byte(r.FormValue("csrf"))) {
		return bad(sessionMismatch)
	}
	return nil
}
