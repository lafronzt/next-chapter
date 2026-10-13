package app

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var (
	mainNavPattern = regexp.MustCompile(`(?s)<nav aria-label="Main navigation">(.*?)</nav>`)
	titlePattern   = regexp.MustCompile(`<title>([^<]*)</title>`)
	navLinkPattern = regexp.MustCompile(`<a href="([^"]*)" (?:class="active" aria-current="(page|true)")?>`)
)

// currentNav returns the sidebar links marked current, as "href=value".
func currentNav(t *testing.T, body string) []string {
	t.Helper()
	m := mainNavPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no main navigation")
	}
	current := []string{}
	for _, link := range navLinkPattern.FindAllStringSubmatch(m[1], -1) {
		if link[2] != "" {
			current = append(current, link[1]+"="+link[2])
		}
	}
	return current
}

// a11yLibrary is a demo library with a queue, a review item, a loan, and an
// import preview, for walking every page.
func a11yLibrary(t *testing.T) (*App, *browser, map[string]string) {
	t.Helper()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	b.request("GET", "/", nil)
	expect(t, b.request("POST", "/demo", nil), 303)
	addRecords(t, a, []Record{libbyLoan("Library Find")})
	if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,finished_at,rating,source,review_chars,fingerprint) VALUES(1,'2025-04-01',4,'goodreads',120,'f1')"); err != nil {
		t.Fatal(err)
	}
	b.request("GET", "/imports", nil)
	rr := uploadCSV(b, "goodreads", "Title,Author\nA New Book,Writer\n")
	expect(t, rr, 303)
	plans := map[string]string{"import": rr.Header().Get("Location")}
	b.request("GET", "/", nil)
	rr = b.request("POST", "/suggest", nil)
	plans["reorder"] = rr.Header().Get("Location")
	b.request("GET", "/?view=unreviewed", nil)
	rr = b.request("POST", "/triage", url.Values{"action": {"want"}})
	plans["triage"] = rr.Header().Get("Location")
	return a, b, plans
}

func TestNavigationMarksOneCurrentItem(t *testing.T) {
	t.Parallel()
	_, b, plans := a11yLibrary(t)
	cases := map[string]string{
		"/":                  "/=page",
		"/?view=queue&q=act": "/=page",
		"/?view=unreviewed":  "/?view=unreviewed=page",
		"/?view=all":         "/?view=all=page",
		"/?view=read":        "/=true",
		"/?view=reading":     "/=true",
		"/history":           "/history=page",
		"/history?page=2":    "/history=page",
		"/reading":           "/reading=page",
		"/imports":           "/imports=page",
		"/settings":          "/settings=page",
		"/books/new":         "/=true",
		"/books/1":           "/=true",
		"/books/1/delete":    "/=true",
		"/books/1/merge":     "/=true",
		plans["import"]:      "/imports=true",
		plans["reorder"]:     "/=true",
		plans["triage"]:      "/?view=unreviewed=true",
	}
	for path, want := range cases {
		got := currentNav(t, b.request("GET", path, nil).Body.String())
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: current %v, want [%s]", path, got, want)
		}
	}
	// Pages outside every section mark nothing.
	for _, path := range []string{"/login", "/books/999", "/books/x"} {
		if got := currentNav(t, b.request("GET", path, nil).Body.String()); len(got) != 0 {
			t.Errorf("%s: current %v", path, got)
		}
	}
	// The selected list tab is current too.
	body := b.request("GET", "/?view=read", nil).Body.String()
	if strings.Count(body, `class="selected" aria-current="page"`) != 1 || !strings.Contains(body, `<a href="/?view=read" class="selected" aria-current="page">`) {
		t.Fatal("selected tab")
	}
}

func TestDecorativeGlyphsAreHidden(t *testing.T) {
	t.Parallel()
	_, b, _ := a11yLibrary(t)
	body := b.request("GET", "/", nil).Body.String()
	nav := mainNavPattern.FindStringSubmatch(body)[1]
	for _, glyph := range []string{"≡", "✉", "▤", "◷", "↓", "⚙"} {
		if !strings.Contains(nav, `<span aria-hidden="true">`+glyph+`</span>`) {
			t.Errorf("nav glyph %s is announced", glyph)
		}
	}
	for _, glyph := range []string{"◆", "↗", "⌕", "＋", "✧"} {
		if strings.Contains(body, glyph) && !regexp.MustCompile(`aria-hidden="true">`+glyph+`<`).MatchString(body) {
			t.Errorf("%s is announced", glyph)
		}
	}
	if strings.Contains(body, `<span>↗</span>`) || strings.Contains(body, `>◆ Pinned<`) {
		t.Fatal("unhidden glyph")
	}
}

func TestRowActionsNameTheirBook(t *testing.T) {
	t.Parallel()
	_, b, _ := a11yLibrary(t)
	body := b.request("GET", "/", nil).Body.String()
	for _, want := range []string{
		`aria-label="Unpin The Creative Act"`,
		`aria-label="Pin Piranesi"`,
		`aria-label="Keep here: Piranesi"`,
		`aria-label="Find its place: Piranesi"`,
		// A pinned book explains why it cannot move.
		`aria-label="Move The Creative Act up" disabled title="Pinned books stay in place. Unpin it to move it."`,
		`aria-label="Find its place: The Creative Act" disabled title="Pinned books keep their place. Unpin it first."`,
		// The book after a pinned one cannot move above it.
		`aria-label="Move Braiding Sweetgrass up" disabled title="The book above is pinned."`,
		`aria-label="Move Piranesi down" disabled title="Already last in your queue."`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestErrorNoticesAreAlerts(t *testing.T) {
	t.Parallel()
	_, b, plans := a11yLibrary(t)
	// A library change makes the earlier previews stale.
	expect(t, b.request("POST", "/books/2/action", url.Values{"action": {"pin"}}), 303)
	if body := b.request("GET", plans["reorder"], nil).Body.String(); !strings.Contains(body, `<div class="notice error" role="alert">Your library has changed`) || !strings.Contains(body, `disabled title="Generate a fresh preview first."`) {
		t.Fatal("stale preview")
	}
	if body := b.request("GET", plans["import"], nil).Body.String(); !strings.Contains(body, `<div class="notice" role="status">1 rows from goodreads`) {
		t.Fatal("import notice role")
	}
	b.request("GET", "/imports", nil)
	body := uploadCSV(b, "goodreads", "Title,Author\nMissing,\n").Body.String()
	if !strings.Contains(body, `<div class="notice error" role="alert">`) || !strings.Contains(body, `<h2 id="import-errors">1 rows need attention.`) || !strings.Contains(body, `<ul aria-labelledby="import-errors">`) || !strings.Contains(body, `aria-invalid="true" aria-describedby="import-errors"`) {
		t.Fatal("import errors")
	}
	if body = b.request("GET", "/books/999", nil).Body.String(); !strings.Contains(body, `<p role="alert">That record or preview could not be found.</p>`) {
		t.Fatal("error page")
	}
	if body = b.request("GET", "/?notice=saved", nil).Body.String(); !strings.Contains(body, `<div class="notice" role="status">Saved to your library.</div>`) {
		t.Fatal("success notice")
	}
	if body = b.request("GET", "/books/2", nil).Body.String(); !strings.Contains(body, `<p class="notice" role="status">NYPL · print · borrowed`) {
		t.Fatal("loan notice")
	}
	p := browser{t: t, handler: testApp(t, false).Handler()}
	p.request("GET", "/login", nil)
	if body = p.request("POST", "/login", url.Values{"password": {"wrong"}}).Body.String(); !strings.Contains(body, `<div class="notice error" role="alert" id="login-error">`) || !strings.Contains(body, `aria-describedby="login-error"`) {
		t.Fatal("login error")
	}
}

func TestPagesHaveDistinctTitles(t *testing.T) {
	t.Parallel()
	_, b, plans := a11yLibrary(t)
	cases := map[string]string{
		"/":                 "Reading queue – Next Chapter",
		"/?q=act":           "Reading queue matching “act” – Next Chapter",
		"/?view=unreviewed": "To review – Next Chapter",
		"/?view=read":       "Finished – Next Chapter",
		"/?view=all":        "All books – Next Chapter",
		"/history":          "Library history – Next Chapter",
		"/reading":          "Reading history – Next Chapter",
		"/imports":          "Import books – Next Chapter",
		"/settings":         "Preferences &amp; sync – Next Chapter",
		"/books/new":        "Add a book – Next Chapter",
		"/books/5":          "Piranesi – Next Chapter",
		"/books/5/delete":   "Delete Piranesi – Next Chapter",
		"/books/5/merge":    "Merge Piranesi – Next Chapter",
		"/login":            "Sign in – Next Chapter",
		"/books/999":        "Not Found – Next Chapter",
		"/books/x":          "Bad Request – Next Chapter",
		plans["import"]:     "Import preview – Next Chapter",
		plans["reorder"]:    "Suggested order – Next Chapter",
		plans["triage"]:     "Confirm sorting – Next Chapter",
	}
	seen := map[string]string{}
	for path, want := range cases {
		m := titlePattern.FindStringSubmatch(b.request("GET", path, nil).Body.String())
		if m == nil || m[1] != want {
			t.Errorf("%s: title %v, want %q", path, m, want)
			continue
		}
		if other, dup := seen[m[1]]; dup {
			t.Errorf("%s and %s share a title", path, other)
		}
		seen[m[1]] = path
	}
	// Later pages say which page they are.
	_, pb := queueApp(t, 120)
	if m := titlePattern.FindStringSubmatch(pb.request("GET", "/?page=2", nil).Body.String()); m == nil || m[1] != "Reading queue, page 2 of 3 – Next Chapter" {
		t.Fatal(m)
	}
}

func TestSkipLinkAndMarkupStayCSPSafe(t *testing.T) {
	t.Parallel()
	_, b, plans := a11yLibrary(t)
	idPattern := regexp.MustCompile(`\sid="([^"]+)"`)
	for _, path := range []string{"/", "/?view=unreviewed", "/history", "/reading", "/imports", "/settings", "/books/new", "/books/1", "/books/1/merge", "/books/1/delete", "/login", "/books/999", plans["import"], plans["reorder"], plans["triage"]} {
		rr := b.request("GET", path, nil)
		body := rr.Body.String()
		skip := strings.Index(body, `<a class="skip-link" href="#main">Skip to main content</a>`)
		if skip < 0 || skip > strings.Index(body, `<aside class="sidebar">`) {
			t.Errorf("%s: skip link missing or after the sidebar", path)
		}
		ids := map[string]int{}
		for _, m := range idPattern.FindAllStringSubmatch(body, -1) {
			ids[m[1]]++
		}
		if ids["main"] != 1 || !strings.Contains(body, `<main id="main" tabindex="-1">`) {
			t.Errorf("%s: skip target", path)
		}
		for id, n := range ids {
			if n > 1 {
				t.Errorf("%s: duplicate id %q", path, id)
			}
		}
		for _, ref := range regexp.MustCompile(`aria-(?:describedby|labelledby)="([^"]+)"`).FindAllStringSubmatch(body, -1) {
			if ids[ref[1]] != 1 {
				t.Errorf("%s: aria reference to missing id %q", path, ref[1])
			}
		}
		if strings.Contains(body, " style=") || strings.Contains(body, "<style") || strings.Contains(body, "<script") {
			t.Errorf("%s: inline style or script", path)
		}
		if csp := rr.Header().Get("Content-Security-Policy"); csp != "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'; form-action 'self'; base-uri 'self'" {
			t.Errorf("%s: CSP changed: %s", path, csp)
		}
		if !strings.Contains(body, `<meta name="viewport" content="width=device-width,initial-scale=1">`) || strings.Contains(body, "user-scalable") {
			t.Errorf("%s: viewport", path)
		}
	}
	css, err := assets.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"@media (prefers-reduced-motion:reduce)", ".skip-link:focus", ".sr-only {"} {
		if !strings.Contains(string(css), rule) {
			t.Errorf("stylesheet lacks %s", rule)
		}
	}
}
