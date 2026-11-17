package app

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestHealthcheckNetworkDiagnostics(t *testing.T) {
	t.Parallel()
	for _, status := range []int{200, 503} {
		var logs bytes.Buffer
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		_, port, err := net.SplitHostPort(s.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		err = CheckHealth(diagnosticContext(&logs), port)
		s.Close()
		if (err == nil) != (status == 200) {
			t.Fatal("unexpected healthcheck result", err)
		}
		if !strings.Contains(logs.String(), "service=healthcheck") || !strings.Contains(logs.String(), "http_status="+strconv.Itoa(status)) {
			t.Fatal(logs.String())
		}
	}
}

func diagnosticContext(buf *bytes.Buffer) context.Context {
	ctx := context.WithValue(context.Background(), networkLoggerKey{}, slog.New(slog.NewTextHandler(buf, nil)))
	return context.WithValue(ctx, contextKey{}, requestState{ID: "test-request"})
}

func TestNetworkTransportCauses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&net.DNSError{Err: "private", Name: "private"}, "dns"},
		{x509.UnknownAuthorityError{}, "tls_certificate"},
		{x509.HostnameError{}, "tls_certificate"},
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "canceled"},
		{syscall.ECONNREFUSED, "connection_refused"},
		{syscall.ECONNRESET, "connection_reset"},
		{errors.New("private"), "connection"},
	} {
		err := &url.Error{Op: "Post", URL: "https://private/?key=private", Err: tc.err}
		if got := networkTransportCause(err); got != tc.want {
			t.Fatalf("got %s, want %s", got, tc.want)
		}
	}
}

func TestAINetworkDiagnostics(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"anthropic", "openai", "ollama", "custom"} {
		for _, outcome := range []string{"dns", "status", "malformed", "success"} {
			t.Run(provider+"/"+outcome, func(t *testing.T) {
				var logs bytes.Buffer
				transport := hardcoverTransport(func(r *http.Request) (*http.Response, error) {
					if outcome == "dns" {
						return nil, &net.DNSError{Err: "private-key", Name: "private-title"}
					}
					status, body := 200, "private-title private-key"
					if outcome == "status" {
						status = 503
					}
					if outcome == "success" {
						body = map[string]func(string) string{"anthropic": anthropicReply, "openai": openAIReply, "ollama": ollamaReply, "custom": chatReply}[provider]("private-title")
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				c := newAIClient(AIConfig{Provider: provider, Key: "private-key", Model: "private-model", BaseURL: "https://example.invalid/private-path?key=private-key"}, transport)
				_, err := c.Complete(diagnosticContext(&logs), AIRequest{User: "private-title"})
				if (err == nil) != (outcome == "success") {
					t.Fatal("unexpected request result", err)
				}
				want := "outcome=failed"
				if outcome == "success" {
					want = "outcome=success"
				}
				for _, field := range []string{"outbound request started", "outbound request finished", "service=ai", "operation=" + provider, "request_id=test-request", "duration=", want} {
					if !strings.Contains(logs.String(), field) {
						t.Fatalf("missing %s in %s", field, logs.String())
					}
				}
				if outcome == "dns" && !strings.Contains(logs.String(), "cause=dns") {
					t.Fatal(logs.String())
				}
				if strings.Contains(logs.String(), "private-") {
					t.Fatal("private data in logs")
				}
			})
		}
	}
}

func TestLookupNetworkDiagnosticsIncludeFailedFallbacks(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	f := newFinder(LookupConfig{GoogleBooksKey: "private-key", ISBNdbKey: "private-key"}, lookupHosts{}, &http.Client{Transport: hardcoverTransport(func(r *http.Request) (*http.Response, error) {
		return nil, &net.DNSError{Err: "private-key", Name: "private-title"}
	})})
	_, err := f.Find(diagnosticContext(&logs), LookupQuery{Title: "private-title", Author: "private-author", ISBN: "9780306406157"})
	if err == nil {
		t.Fatal("expected lookup failure")
	}
	for _, service := range []string{"open_library", "google_books", "isbndb"} {
		if !strings.Contains(logs.String(), "service="+service) {
			t.Fatalf("missing %s in %s", service, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "cause=dns") || !strings.Contains(logs.String(), "request_id=test-request") || strings.Contains(logs.String(), "private-") {
		t.Fatal("missing diagnostic or private data leaked", logs.String())
	}
}

func TestLookupNetworkResponseDiagnostics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status               int
		body, outcome, phase string
	}{
		{200, `{}`, "success", "response_json"},
		{404, "private-title", "not_found", "http_status"},
		{429, "private-key", "failed", "http_status"},
		{200, "private-key private-title", "failed", "response_json"},
	} {
		var logs bytes.Buffer
		h := newLookupHTTP(&http.Client{Transport: hardcoverTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})}, "https://example.invalid")
		var out any
		_ = h.getJSON(diagnosticContext(&logs), "/private-title", url.Values{"key": {"private-key"}}, &out)
		if !strings.Contains(logs.String(), "outcome="+tc.outcome) || !strings.Contains(logs.String(), "phase="+tc.phase) || strings.Contains(logs.String(), "private-") {
			t.Fatal(logs.String())
		}
	}
}
