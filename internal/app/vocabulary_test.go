package app

import (
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"
	"testing"
)

var tagPattern = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]*>`)

func visibleText(body string) string {
	return html.UnescapeString(tagPattern.ReplaceAllString(body, " "))
}

func TestEveryStatusHasOneDisplayLabel(t *testing.T) {
	t.Parallel()
	want := map[string]string{"to-read": "Queue", "reading": "Reading", "read": "Read", "abandoned": "Set aside", "archived": "Set aside", "unreviewed": "To review"}
	for _, s := range readingStates {
		if statusLabel(s) != want[s] {
			t.Errorf("%s labelled %q", s, statusLabel(s))
		}
	}
}

func TestPagesNeverPrintRawStatusKeys(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	addRecords(t, a, []Record{shelf("Queued", "to-read"), shelf("Going", "reading"), shelf("Done", "read"), shelf("Stopped", "archived"), libbyLoan("Held")})
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='abandoned' WHERE title='Stopped'"); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	paths := []string{"/", "/?view=all", "/?view=reading", "/?view=unreviewed", "/?view=abandoned", "/?view=archived", "/reading", "/history", "/books/new"}
	for _, title := range []string{"Queued", "Going", "Done", "Stopped", "Held"} {
		id := bookNamed(t, a, title).ID
		paths = append(paths, fmt.Sprintf("/books/%d", id), fmt.Sprintf("/books/%d/delete", id), fmt.Sprintf("/books/%d/merge", id))
	}
	for _, path := range paths {
		text := visibleText(b.request("GET", path, nil).Body.String())
		for _, raw := range []string{"to-read", "unreviewed", "abandoned"} {
			// "want-to-read" is the vocabulary, not a raw key.
			if regexp.MustCompile(`(^|[^a-z-])` + raw + `\b`).MatchString(text) {
				t.Errorf("%s prints %q", path, raw)
			}
		}
		if strings.Contains(strings.ToLower(text), "interest ") && !strings.Contains(text, "not interested") && !strings.Contains(text, "Not interested") {
			t.Errorf("%s still says interest", path)
		}
	}
}

func TestPipsCountAndStarsStayForRatings(t *testing.T) {
	t.Parallel()
	pips := templateFuncs()["pips"].(func(int) template.HTML)
	for n := 1; n <= 5; n++ {
		got := string(pips(n))
		if strings.Count(got, "●") != n || strings.Count(got, "○") != 5-n || !strings.Contains(got, fmt.Sprintf("Want-to-read %d of 5", n)) || strings.Contains(got, "★") {
			t.Errorf("%d: %s", n, got)
		}
	}
	r := 4.25
	if got := string(templateFuncs()["stars"].(func(*float64) template.HTML)(&r)); !strings.Contains(got, "★★★★") || !strings.Contains(got, "4.25 out of 5") {
		t.Fatal(got)
	}
}
