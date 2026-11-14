package app

import (
	"database/sql"
	"net/url"
	"strings"
	"testing"
)

func storedPrefs(t *testing.T, a *App) Preferences {
	t.Helper()
	p, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSavingEachSectionChangesOnlyItsOwnKeys(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	start := Preferences{LibraryWeight: 2, Length: "short", Mood: "hopeful", KeepReviews: true, UseEstimates: true}
	if err := a.store.Write(func(tx *sql.Tx) error { return setMeta(tx, "preferences", start) }); err != nil {
		t.Fatal(err)
	}
	// Saving Ranking, with no privacy field in the form, must not touch KeepReviews.
	rev := currentRevision(t, a)
	expect(t, b.form("/settings", url.Values{"section": {"ranking"}, "library_weight": {"0.5"}, "length": {"long"}, "mood": {"quiet"}}), 303)
	got := storedPrefs(t, a)
	want := Preferences{LibraryWeight: 0.5, Length: "long", Mood: "quiet", KeepReviews: true, UseEstimates: false}
	if got != want {
		t.Fatalf("ranking save: %+v, want %+v", got, want)
	}
	if currentRevision(t, a) != rev+1 {
		t.Fatal("revision should bump once")
	}
	// Saving Privacy changes only KeepReviews.
	if _, err := a.store.DB.Exec("DELETE FROM reading_log"); err != nil {
		t.Fatal(err)
	}
	expect(t, b.form("/settings", url.Values{"section": {"privacy"}}), 303)
	got = storedPrefs(t, a)
	want.KeepReviews = false
	if got != want {
		t.Fatalf("privacy save: %+v, want %+v", got, want)
	}
	// Invalid values still return 400 with the same messages, per section.
	for values, message := range map[string]string{"library_weight=9&length=any": "Choose a library timing preference.", "library_weight=1&length=huge": "Choose a length preference."} {
		v, _ := url.ParseQuery(values)
		v.Set("section", "ranking")
		rr := b.form("/settings", v)
		expect(t, rr, 400)
		if !strings.Contains(rr.Body.String(), message) {
			t.Errorf("%q: %s", values, rr.Body.String())
		}
	}
	expect(t, b.form("/settings", url.Values{"section": {"bogus"}, "library_weight": {"1"}, "length": {"any"}}), 400)
}

func TestTurningOffReviewTextNeedsConfirmation(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	addRecords(t, a, records("Reviewed"))
	id := bookNamed(t, a, "Reviewed").ID
	if err := a.store.Write(func(tx *sql.Tx) error {
		return setMeta(tx, "preferences", Preferences{LibraryWeight: 1, Length: "any", KeepReviews: true})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,review,review_chars,fingerprint) VALUES(?,?,?,'manual','Private words',13,'manual:r1')", id, "2025-01-01", 4.0); err != nil {
		t.Fatal(err)
	}
	reviewKept := func() bool {
		var n int
		a.store.DB.QueryRow("SELECT count(*) FROM reading_log WHERE review IS NOT NULL").Scan(&n)
		return n > 0
	}
	// Saving Ranking never deletes review text, even if it would be "off" in that form.
	expect(t, b.form("/settings", url.Values{"section": {"ranking"}, "library_weight": {"1"}, "length": {"any"}}), 303)
	if !reviewKept() || !storedPrefs(t, a).KeepReviews {
		t.Fatal("a ranking save touched review text")
	}
	// Turning it off asks first, and changes nothing yet.
	rr := b.form("/settings", url.Values{"section": {"privacy"}})
	expect(t, rr, 200)
	if !strings.Contains(rr.Body.String(), "Delete 1 stored review?") || !reviewKept() || !storedPrefs(t, a).KeepReviews {
		t.Fatal("no confirmation, or it deleted early")
	}
	// Confirming deletes and turns it off.
	expect(t, b.form("/settings", url.Values{"section": {"privacy"}, "confirm": {"yes"}}), 303)
	if reviewKept() || storedPrefs(t, a).KeepReviews {
		t.Fatal("confirmed deletion did not happen")
	}
}

func TestSettingsPageSectionsAndAnchors(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/settings", nil).Body.String()
	for _, want := range []string{`id="ranking" open>`, `<details class="panel settings-section" id="privacy" >`, `id="ai" >`, `name="section" value="ranking"`, `name="section" value="privacy"`, "About this library", "database schema version", `href="/export.json"`, `href="/backup.sqlite3"`, "Default: Any length."} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(body), ".sqlite3\"") && strings.Contains(body, "data/") {
		t.Error("the database path is shown")
	}
	if body = b.request("GET", "/settings?open=privacy", nil).Body.String(); !strings.Contains(body, `id="privacy" open>`) {
		t.Error("?open=privacy did not expand Privacy")
	}
	if body = b.request("GET", "/settings?notice=ai-on", nil).Body.String(); !strings.Contains(body, `id="ai" open>`) {
		t.Error("an AI notice did not expand the AI section")
	}
}
