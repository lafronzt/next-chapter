package app

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An AI request runs in the background: the reader sees a waiting page, can
// browse meanwhile with a note on every page, cannot start a second request,
// and is sent to the result once it arrives.
func TestAIRequestRunsInBackground(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	a, _ := aiApp(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		replyFor("anthropic", `{"estimate":4.2,"confidence":"medium","reasons":["You like this author."]}`)(w, r)
	})
	id := libraryFixture(t, a, 4)
	b := &browser{t: t, handler: a.Handler(), background: true}
	b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil)
	in, _ := loadEstimateInputs(a.store.DB, id)
	r := b.request("POST", fmt.Sprintf("/books/%d/estimate/ai", id), url.Values{"fingerprint": {in.Fingerprint}})
	expect(t, r, 303)
	job := r.Header().Get("Location")
	if !strings.HasPrefix(job, "/ai/jobs/") {
		t.Fatalf("redirected to %s", job)
	}

	waiting := b.request("GET", job, nil)
	expect(t, waiting, 200)
	body := waiting.Body.String()
	if !strings.Contains(body, "Asking the AI") || !strings.Contains(body, `http-equiv="refresh"`) || !strings.Contains(body, "an estimated rating for The Candidate") || strings.Contains(body, "The AI is working on") {
		t.Fatalf("waiting page is wrong: %s", body)
	}
	queue := b.request("GET", "/", nil).Body.String()
	if !strings.Contains(queue, "The AI is working on an estimated rating for The Candidate") || strings.Contains(queue, `http-equiv="refresh"`) {
		t.Fatal("other pages lack the running note, or reload themselves")
	}
	expect(t, b.request("POST", fmt.Sprintf("/books/%d/estimate/ai", id), url.Values{"fingerprint": {in.Fingerprint}}), 409)
	expect(t, b.request("POST", "/ai/test", nil), 409)

	close(release)
	b.background = false
	done := b.awaitJob(job)
	expect(t, done, 303)
	if loc := done.Header().Get("Location"); loc != fmt.Sprintf("/books/%d/estimate?notice=estimated", id) {
		t.Fatalf("result at %s", loc)
	}
	if saved, _ := readEstimate(a.store.DB, id); saved == nil || saved.AI == nil || *saved.AI != 4.2 {
		t.Fatalf("saved %+v", saved)
	}
	if strings.Contains(b.request("GET", "/", nil).Body.String(), "ai-job") {
		t.Fatal("note still shown after the result was seen")
	}
	if calls.Load() != 1 {
		t.Fatalf("%d calls", calls.Load())
	}
	expect(t, b.request("GET", "/ai/jobs/not-a-job", nil), 404)
}

// A failure is kept for the reader, with the provider error's status, until
// they open it.
func TestAIRequestFailureIsReported(t *testing.T) {
	a, _ := aiApp(t, "anthropic", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	b := &browser{t: t, handler: a.Handler(), background: true, gateway: true}
	b.request("GET", "/settings", nil)
	r := b.request("POST", "/ai/test", nil)
	expect(t, r, 303)
	job := r.Header().Get("Location")
	b.background = false
	failed := b.awaitJob(job)
	expect(t, failed, 502)
	if body := failed.Body.String(); !strings.Contains(body, "did not finish") || !strings.Contains(body, `href="/settings#ai"`) {
		t.Fatalf("failure page is wrong: %s", body)
	}
	if strings.Contains(b.request("GET", "/", nil).Body.String(), "ai-job") {
		t.Fatal("note still shown after the failure was seen")
	}
}

// A request still running when the app closes is cancelled, and Close waits
// for it.
func TestCloseCancelsAIRequest(t *testing.T) {
	a, _ := aiApp(t, "anthropic", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	b := &browser{t: t, handler: a.Handler(), background: true}
	b.request("GET", "/settings", nil)
	expect(t, b.request("POST", "/ai/test", nil), 303)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if job := a.currentAIJob(); job == nil || !job.Done || job.Err == nil {
		t.Fatalf("job %+v", job)
	}
}

// The optional exact-prompt view shows what the provider receives, including
// a self-hosted server's schema hint.
func TestPromptPreview(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			a, _ := aiApp(t, provider, func(w http.ResponseWriter, r *http.Request) { t.Error("preview sent a request") })
			id := libraryFixture(t, a, 4)
			b := &browser{t: t, handler: a.Handler()}
			page := html.UnescapeString(b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil).Body.String())
			in, _ := loadEstimateInputs(a.store.DB, id)
			req, err := estimateRequest(buildEstimatePayload(in.Book, in.History, in.Baseline, false))
			if err != nil {
				t.Fatal(err)
			}
			want := a.previewPrompt(req)
			if !strings.Contains(page, "Show the exact prompt") || !strings.Contains(page, want.System) || !strings.Contains(page, want.User) || !strings.Contains(page, want.Schema) || !strings.Contains(page, `"your_review"`) {
				t.Fatal("estimate page lacks the exact prompt")
			}
			if hinted := strings.Contains(want.System, estimateLocalSchema); hinted != (provider == "ollama") {
				t.Fatalf("schema hint in system prompt: %v", hinted)
			}
			b.request("GET", "/", nil)
			r := b.request("POST", "/suggest", nil)
			plan := html.UnescapeString(b.request("GET", r.Header().Get("Location"), nil).Body.String())
			if !strings.Contains(plan, "Show the exact prompt") || !strings.Contains(plan, reorderTask) {
				t.Fatal("plan page lacks the exact prompt")
			}
		})
	}
}

func TestWaited(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "a moment", 12 * time.Second: "12 seconds", 61 * time.Second: "1 minute 1 seconds", 185 * time.Second: "3 minutes 5 seconds"} {
		if got := waited(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
