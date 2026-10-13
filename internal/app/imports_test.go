package app

import (
	"strings"
	"testing"
)

func TestCSVAndRSS(t *testing.T) {
	for _, source := range []string{"goodreads", "libby", "nypl"} {
		t.Run(source, func(t *testing.T) {
			data, err := assets.ReadFile("web/examples/" + source + ".csv")
			if err != nil {
				t.Fatal(err)
			}
			rows, problems, err := ParseCSV(data, source)
			if err != nil || len(problems) != 0 || len(rows) == 0 {
				t.Fatalf("%v %v %v", rows, problems, err)
			}
		})
	}
	_, problems, err := ParseCSV([]byte("Title,Author,Timestamp,Activity\nBook,Person,2026-01-01,unknown\n"), "libby")
	if err != nil || len(problems) != 1 {
		t.Fatalf("%v %v", problems, err)
	}
	for _, data := range []string{"Title,Title\nA,B", "Bad,Header\nA,B", strings.Repeat("x", maxUpload+1)} {
		if _, _, err := ParseCSV([]byte(data), "goodreads"); err == nil {
			t.Fatal("accepted invalid CSV")
		}
	}
	rss := `<rss><channel><item><book_title>A &amp; B</book_title><author_name>Person</author_name><book_id>42</book_id><book><num_pages>250</num_pages></book></item></channel></rss>`
	rows, err := ParseRSS([]byte(rss))
	if err != nil || len(rows) != 1 || rows[0].Title != "A & B" || rows[0].Pages == nil {
		t.Fatalf("%v %v", rows, err)
	}
	if _, err = ParseRSS([]byte(`<!DOCTYPE rss><rss/>`)); err == nil {
		t.Fatal("accepted DTD")
	}
}
