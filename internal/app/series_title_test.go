package app

import (
	"database/sql"
	"testing"
)

func TestSplitSeriesTitle(t *testing.T) {
	t.Parallel()
	one, two, half := 1.0, 2.0, 0.5
	cases := []struct {
		in, title, series string
		number            *float64
		ok                bool
	}{
		{"Heated Rivalry (Game Changers, #2)", "Heated Rivalry", "Game Changers", &two, true},
		{"The Hobbit (The Lord of the Rings #1)", "The Hobbit", "The Lord of the Rings", &one, true},
		{"Prequel: A Story (Wind Cycle, #0.5)", "Prequel: A Story", "Wind Cycle", &half, true},
		{"  Spaced Out ( Big Series ,  # 2 )  ", "Spaced Out", "Big Series", &two, true},
		{"Title (With Parens) (Series, #1)", "Title (With Parens)", "Series", &one, true},
		{"Dune (A Novel)", "Dune (A Novel)", "", nil, false},
		{"Plain Title", "Plain Title", "", nil, false},
		{"Box Set (Series, #1-3)", "Box Set (Series, #1-3)", "", nil, false},
		{"Nothing (#2)", "Nothing (#2)", "", nil, false},
		{"(Series, #2)", "(Series, #2)", "", nil, false},
		{"Title (Series, #2) extra", "Title (Series, #2) extra", "", nil, false},
	}
	for _, c := range cases {
		title, series, number, ok := splitSeriesTitle(c.in)
		if ok != c.ok || title != c.title || series != c.series || (number == nil) != (c.number == nil) || (number != nil && *number != *c.number) {
			t.Errorf("%q: got %q, %q, %v, %v", c.in, title, series, number, ok)
		}
	}
}

func titledRows(t *testing.T, rows ...string) []Record {
	t.Helper()
	csv := "Book Id,Title,Author,Exclusive Shelf\n"
	for _, r := range rows {
		csv += r + "\n"
	}
	records, problems, err := ParseCSV([]byte(csv), "goodreads")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	return records
}

func TestImportReadsSeriesFromTitle(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	rows := titledRows(t, `101,"Heated Rivalry (Game Changers, #2)",Rachel Reid,to-read`, `102,"Dune (A Novel)",Frank Herbert,to-read`)
	if r := rows[0]; r.Title != "Heated Rivalry" || r.Series != "Game Changers" || r.SeriesNumber == nil || *r.SeriesNumber != 2 {
		t.Fatalf("%+v", r.Book)
	}
	importOnce(t, a, rows)
	got := bookByTitle(t, a, "Heated Rivalry")
	if got.Series != "Game Changers" || got.SeriesNumber == nil || *got.SeriesNumber != 2 || got.Author != "Rachel Reid" {
		t.Fatalf("%+v", got)
	}
	if dune := bookByTitle(t, a, "Dune (A Novel)"); dune.Series != "" {
		t.Fatalf("a subtitle became a series: %+v", dune)
	}
	// Importing again adds nothing and changes nothing.
	if res := importOnce(t, a, titledRows(t, `101,"Heated Rivalry (Game Changers, #2)",Rachel Reid,to-read`)); res.Added != 0 || res.Changed {
		t.Fatalf("%+v", res)
	}
}

func TestImportMatchesBooksStoredWithTheFullTitle(t *testing.T) {
	t.Parallel()
	a := testApp(t, false)
	// Imported before series were read from titles, with no Goodreads ID to match by.
	addRecords(t, a, []Record{{Book: Book{Title: "Heated Rivalry (Game Changers, #2)", Author: "Rachel Reid", Status: "to-read", Source: "manual", Interest: 4}}})
	existing := bookByTitle(t, a, "Heated Rivalry (Game Changers, #2)")
	res := importOnce(t, a, titledRows(t, `,"Heated Rivalry (Game Changers, #2)",Rachel Reid,to-read`))
	if res.Added != 0 {
		t.Fatalf("a duplicate was added: %+v", res)
	}
	all, _ := loadBooks(a.store.DB)
	if len(all) != 1 {
		t.Fatalf("%d books", len(all))
	}
	// The match is the same book; its series is filled and its title and want-to-read stay.
	got := all[0]
	if got.ID != existing.ID || got.Title != existing.Title || got.Interest != 4 || got.Series != "Game Changers" || got.SeriesNumber == nil || *got.SeriesNumber != 2 {
		t.Fatalf("%+v", got)
	}
	// A series the reader set is not replaced.
	if err := a.store.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE books SET series='My Series', series_number=7 WHERE id=?", got.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	importOnce(t, a, titledRows(t, `,"Heated Rivalry (Game Changers, #2)",Rachel Reid,to-read`))
	if again := bookByTitle(t, a, "Heated Rivalry (Game Changers, #2)"); again.Series != "My Series" || *again.SeriesNumber != 7 {
		t.Fatalf("%+v", again)
	}
}

func TestSeriesFromTitleSurvivesThePreviewStorage(t *testing.T) {
	t.Parallel()
	rec := titledRows(t, `101,"Heated Rivalry (Game Changers, #2)",Rachel Reid,to-read`)[0]
	raw, err := encodePlan(Plan{Records: []Record{rec}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodePlan(nil, "import", raw)
	if err != nil {
		t.Fatal(err)
	}
	got := back.Records[0]
	if got.Title != "Heated Rivalry" || got.Series != "Game Changers" || *got.SeriesNumber != 2 || got.OriginalTitle != rec.OriginalTitle || got.OriginalTitle == "" {
		t.Fatalf("%+v", got)
	}
}
