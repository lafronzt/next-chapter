package app

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// The paste-back flow works with no provider configured at all.
func TestManualEstimateWithoutProvider(t *testing.T) {
	a := testApp(t, true)
	id := libraryFixture(t, a, 12)
	b := &browser{t: t, handler: a.Handler(), gateway: true}
	page := b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil).Body.String()
	if !strings.Contains(page, "Use your own AI") || !strings.Contains(page, "&lt;library_data&gt;") || strings.Contains(page, "Send and estimate") {
		t.Fatal("manual offer missing or provider send shown")
	}
	in, _ := loadEstimateInputs(a.store.DB, id)
	post := func(answer string) int {
		return b.request("POST", fmt.Sprintf("/books/%d/estimate/paste", id), url.Values{"fingerprint": {in.Fingerprint}, "answer": {answer}}).Code
	}
	if code := post("no json here"); code != 400 {
		t.Fatalf("no JSON: %d", code)
	}
	if code := post(`{"estimate":9,"confidence":"high","reasons":["x"]}`); code != 502 {
		t.Fatalf("bad answer: %d", code)
	}
	if saved, _ := readEstimate(a.store.DB, id); saved != nil {
		t.Fatalf("saved %+v from a bad answer", saved)
	}
	fenced := "Sure!\n```json\n{\"estimate\":4.26,\"confidence\":\"medium\",\"reasons\":[\"You like this.\"]}\n```"
	if code := post(fenced); code != 303 {
		t.Fatalf("good answer: %d", code)
	}
	saved, _ := readEstimate(a.store.DB, id)
	if saved == nil || saved.AI == nil || *saved.AI != 4.3 || saved.Model != manualModel {
		t.Fatalf("saved %+v", saved)
	}
	if u, _ := usageFor(a.store.DB, a.now()); u.Requests != 0 {
		t.Fatalf("manual answer counted as %d requests", u.Requests)
	}
}

func TestManualRejectsStaleFingerprint(t *testing.T) {
	a := testApp(t, true)
	id := libraryFixture(t, a, 4)
	b := &browser{t: t, handler: a.Handler(), gateway: true}
	b.request("GET", fmt.Sprintf("/books/%d/estimate", id), nil)
	r := b.request("POST", fmt.Sprintf("/books/%d/estimate/paste", id), url.Values{"fingerprint": {"stale"}, "answer": {`{"estimate":4,"confidence":"low","reasons":["x"]}`}})
	if r.Code != 409 {
		t.Fatalf("code %d", r.Code)
	}
}

func TestExtractAnswerJSON(t *testing.T) {
	got, err := extractAnswerJSON("here:\n```\n{\"a\":1}\n```\nthanks")
	if err != nil || got != `{"a":1}` {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"", "nothing", "} {", strings.Repeat("x", maxManualAnswer+1)} {
		if _, err := extractAnswerJSON(bad); err == nil {
			t.Errorf("accepted %.20q", bad)
		}
	}
}
