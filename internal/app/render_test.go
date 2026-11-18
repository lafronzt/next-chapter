package app

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

type disconnectedResponse struct {
	header   http.Header
	statuses []int
	writes   int
	partial  bool
}

func (w *disconnectedResponse) Header() http.Header    { return w.header }
func (w *disconnectedResponse) WriteHeader(status int) { w.statuses = append(w.statuses, status) }
func (w *disconnectedResponse) Write(p []byte) (int, error) {
	w.writes++
	if w.partial {
		return len(p) / 2, syscall.EPIPE
	}
	return 0, syscall.EPIPE
}

func TestRenderDisconnectDoesNotRetryResponse(t *testing.T) {
	for _, partial := range []bool{false, true} {
		for _, errorPage := range []bool{false, true} {
			t.Run(fmt.Sprintf("partial=%v/errorPage=%v", partial, errorPage), func(t *testing.T) {
				a := testApp(t, true)
				var logs bytes.Buffer
				a.logger = slog.New(slog.NewTextHandler(&logs, nil))
				w := &disconnectedResponse{header: make(http.Header), partial: partial}
				r := withState(httptest.NewRequest("GET", "/books/1?token=secret", nil), requestState{ID: "test-request"})
				status := 200
				if errorPage {
					status = 500
					a.respond(w, r, status, genericFailure)
				} else {
					err := a.render(w, r, "error.html", page{"Title": "Test", "Message": "Test"}, status)
					if !errors.Is(err, syscall.EPIPE) {
						t.Fatalf("lost write error: %v", err)
					}
					a.fail(w, r, err)
				}
				if len(w.statuses) != 1 || w.statuses[0] != status || w.writes != 1 {
					t.Fatalf("retried response: statuses=%v writes=%d", w.statuses, w.writes)
				}
				if !strings.Contains(logs.String(), "response write failed") || !strings.Contains(logs.String(), "request_id=test-request") || strings.Contains(logs.String(), "secret") {
					t.Fatalf("unexpected log: %s", logs.String())
				}
			})
		}
	}
}

func TestRespondFallsBackBeforeResponseCommitted(t *testing.T) {
	a := testApp(t, true)
	a.templates = template.Must(template.New("error.html").Parse(`partial output{{template "missing" .}}`))
	w := httptest.NewRecorder()
	r := withState(httptest.NewRequest("GET", "/", nil), requestState{ID: "test-request"})
	a.respond(w, r, 500, genericFailure)
	if w.Code != 500 || w.Body.String() != genericFailure+"\n" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("fallback failed: %d %s %s", w.Code, w.Header(), w.Body)
	}
}
