package app

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeKeepsAuthoredReviewOnMatchingReading(t *testing.T) {
	a := testApp(t, true)
	ids := addBooks(t, a, Book{Title: "Source", Author: "Writer"}, Book{Title: "Target", Author: "Writer"})
	b := &browser{t: t, handler: a.Handler()}
	for i, id := range ids {
		review := ""
		if i == 0 {
			review = "**Keep my review**"
		}
		expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), url.Values{"finished_at": {"2026-10-01"}, "review": {review}}), 303)
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		preview := mergePreview{}
		return mergeReadings(tx, &preview, ids[0], ids[1], true)
	})
	if err != nil {
		t.Fatal(err)
	}
	rs := readingsOf(t, a, "Target")
	if len(rs) != 2 {
		t.Fatal("merge discarded authored review", rs)
	}
	for _, e := range rs {
		if e.Review != nil && *e.Review == "**Keep my review**" {
			return
		}
	}
	t.Fatal("merge lost authored review text")
}

func TestReaderReviewPersistenceAndRestore(t *testing.T) {
	a := testApp(t, true)
	id := addBooks(t, a, Book{Title: "Review Book", Author: "Writer", Status: "to-read", Interest: 3})[0]
	b := &browser{t: t, handler: a.Handler()}
	review := "**Wonderful** café\n\n- First thought\n- Second thought"
	expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), url.Values{"rating": {"4.25"}, "review": {review}}), 303)
	e := readingsOf(t, a, "Review Book")[0]
	if e.Review == nil || *e.Review != review || e.ReviewFormat != "markdown" || e.ReviewChars != len([]rune(review)) {
		t.Fatal(e)
	}
	before := bookNamed(t, a, "Review Book")
	expect(t, b.form(fmt.Sprintf("/readings/%d", e.ID), url.Values{"action": {"save"}, "rating": {"3.75"}, "review": {"*Reconsidered*"}}), 303)
	// Saving unrelated privacy preferences must never remove authored reviews.
	expect(t, b.form("/settings", url.Values{"section": {"privacy"}}), 303)
	e = readingsOf(t, a, "Review Book")[0]
	if e.Review == nil || *e.Review != "*Reconsidered*" {
		t.Fatal(e)
	}
	after := bookNamed(t, a, "Review Book")
	if before.Status != after.Status || before.Position != after.Position || before.Interest != after.Interest {
		t.Fatal("review changed reading intent or queue")
	}
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, exportOf(t, a))
	expect(t, rr, 303)
	expect(t, tb.form(rr.Header().Get("Location"), nil), 303)
	restored := readingsOf(t, target, "Review Book")[0]
	if restored.Review == nil || *restored.Review != *e.Review || restored.ReviewFormat != "markdown" {
		t.Fatal("authored review lost with import storage disabled", restored)
	}
	expect(t, b.form(fmt.Sprintf("/readings/%d", e.ID), url.Values{"action": {"save"}, "review": {""}}), 303)
	if readingsOf(t, a, "Review Book")[0].Review != nil {
		t.Fatal("clear did not persist")
	}
	expect(t, b.form(fmt.Sprintf("/books/%d/readings", id), url.Values{"review": {strings.Repeat("é", maxReview+1)}}), 400)
}

func TestReviewFormattingAndSafePreview(t *testing.T) {
	s := "**Bold** and *italic*\n\n- item\n\n> quote\n\n[link](https://example.com)\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1))"
	e := readingEntry{Review: &s, ReviewFormat: "markdown"}
	html := string(e.ReviewHTML())
	for _, want := range []string{"<strong>Bold</strong>", "<em>italic</em>", "<li>item</li>", "<blockquote>", `href="https://example.com"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %s in %s", want, html)
		}
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "javascript:") {
		t.Fatal(html)
	}
	e.ReviewFormat = "plain"
	if html := string(e.ReviewHTML()); strings.Contains(html, "<strong>") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal(html)
	}
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	rev := currentRevision(t, a)
	rr := b.form("/reviews/preview", url.Values{"review": {s}})
	expect(t, rr, 200)
	if rr.Body.String() != string((readingEntry{Review: &s, ReviewFormat: "markdown"}).ReviewHTML()) || currentRevision(t, a) != rev {
		t.Fatal("preview differs or mutated library")
	}
}

func TestFinishBookKeepsReview(t *testing.T) {
	a := testApp(t, true)
	id := addBooks(t, a, Book{Title: "Finished Review", Author: "Writer", Status: "reading", Interest: 3})[0]
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.form(fmt.Sprintf("/books/%d/finish", id), url.Values{"finished_at": {"2026-11-01"}, "review": {"**Loved** it"}, "rating": {"4.75"}}), 303)
	e := readingsOf(t, a, "Finished Review")[0]
	if e.Review == nil || *e.Review != "**Loved** it" || *e.Rating != 4.75 {
		t.Fatal(e)
	}
}

func TestMigrateReaderReviews(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:26]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source) VALUES(1,'Review','A','review|a',0,'x','manual');
 INSERT INTO reading_log(book_id,rating,source,review,review_chars,fingerprint) VALUES(1,4.25,'manual','**Original**',12,'manual:migration');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[string](t, path, "SELECT review_format FROM reading_log") != "plain" || scalar[string](t, path, "SELECT review FROM reading_log") != "**Original**" || scalar[float64](t, path, "SELECT rating FROM reading_log") != 4.25 {
		t.Fatal("migration changed existing review or rating")
	}
	if got := backups(t, path); len(got) != 1 || got[0] != path+".pre-v27.bak" {
		t.Fatal(got)
	}
}
