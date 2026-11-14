package app

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

func TestManifestIsPublicValidAndListsRealIcons(t *testing.T) {
	t.Parallel()
	a := testApp(t, false) // a password is set, and there is no session
	b := &browser{t: t, handler: a.Handler()}
	rr := b.request("GET", "/static/manifest.webmanifest", nil)
	expect(t, rr, 200)
	if ct := rr.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Fatalf("content type %q", ct)
	}
	var m struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		BackgroundColor string `json:"background_color"`
		ThemeColor      string `json:"theme_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name == "" || m.ShortName == "" || m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" || m.BackgroundColor == "" || m.ThemeColor == "" {
		t.Fatalf("%+v", m)
	}
	sizes, maskable := map[string]bool{}, false
	for _, icon := range m.Icons {
		path, ok := strings.CutPrefix(icon.Src, "/static/")
		if !ok {
			t.Fatalf("icon %s outside /static/", icon.Src)
		}
		if _, err := fs.Stat(assets, "web/static/"+path); err != nil {
			t.Fatalf("icon %s: %v", icon.Src, err)
		}
		expect(t, b.request("GET", icon.Src, nil), 200)
		sizes[icon.Sizes], maskable = true, maskable || icon.Purpose == "maskable"
	}
	if !sizes["192x192"] || !sizes["512x512"] || !maskable {
		t.Fatalf("icons %+v", m.Icons)
	}
	expect(t, b.request("GET", "/static/apple-touch-icon.png", nil), 200)
	// The manifest carries nothing about the library.
	if strings.Contains(strings.ToLower(rr.Body.String()), "password") || strings.Contains(rr.Body.String(), "csrf") {
		t.Fatal("manifest leaks")
	}
}

func TestLoginPageLinksManifestAndThemeColor(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	b := &browser{t: t, handler: a.Handler()}
	body := b.request("GET", "/login", nil).Body.String()
	for _, want := range []string{
		`<link rel="manifest" href="/static/manifest.webmanifest">`,
		`<link rel="apple-touch-icon" href="/static/apple-touch-icon.png">`,
		`<meta name="theme-color" content="#294f46" media="(prefers-color-scheme: light)">`,
		`<meta name="theme-color" content="#1b2f2a" media="(prefers-color-scheme: dark)">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`viewport-fit=cover`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}
