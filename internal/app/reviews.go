package app

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/renderer/html"
)

// Raw HTML and unsafe URL schemes are disabled by Goldmark's default renderer.
// Hard wraps preserve the reader's deliberate line breaks.
var reviewMarkdown = goldmark.New(goldmark.WithRendererOptions(html.WithHardWraps()))

func reviewFormat(format string) string {
	if format == "markdown" {
		return format
	}
	return "plain"
}

func reviewField(r *http.Request) (string, error) {
	s := strings.TrimSpace(strings.ReplaceAll(r.FormValue("review"), "\r\n", "\n"))
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxReview {
		return "", bad("Keep your review within 20,000 characters.")
	}
	return s, nil
}

func (a *App) reviewPreview(w http.ResponseWriter, r *http.Request) error {
	s, err := reviewField(r)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err = w.Write([]byte((readingEntry{Review: &s, ReviewFormat: "markdown"}).ReviewHTML()))
	return err
}

func (e readingEntry) ReviewHTML() template.HTML {
	if e.Review == nil {
		return ""
	}
	if e.ReviewFormat != "markdown" {
		return template.HTML("<p>" + strings.ReplaceAll(template.HTMLEscapeString(*e.Review), "\n", "<br>\n") + "</p>")
	}
	var out bytes.Buffer
	if err := reviewMarkdown.Convert([]byte(*e.Review), &out); err != nil {
		return ""
	}
	return template.HTML(out.String())
}
