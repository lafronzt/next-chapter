package app

import (
	"reflect"
	"testing"
	"time"
)

func TestRanking(t *testing.T) {
	today := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	books := []Book{{ID: 1, Interest: 1}, {ID: 2, Interest: 4}, {ID: 3, Interest: 2}, {ID: 4, Interest: 5}}
	rows, err := Propose(books, defaultPreferences, 4, today)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(rows), []int64{4, 1, 2, 3}) {
		t.Fatal(rows)
	}
	books[0].Pinned = true
	rows, err = Propose(books, defaultPreferences, 4, today)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(rows), []int64{1, 4, 2, 3}) {
		t.Fatal(rows)
	}
	rows, err = Propose(books, defaultPreferences, 0, today)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(rows), []int64{1, 4, 2, 3}) {
		t.Fatal(rows)
	}
	books[2].Loans = []Loan{{Status: "borrowed", DueDate: "2026-10-12"}}
	rows, err = Propose(books, defaultPreferences, 0, today)
	if err != nil {
		t.Fatal(err)
	}
	if rows[1].ID != 3 {
		t.Fatal(rows)
	}
	// Adding a second format must not double the library bonus.
	score1, _ := score(books[2], defaultPreferences, today)
	books[2].Loans = append(books[2].Loans, books[2].Loans[0])
	score2, _ := score(books[2], defaultPreferences, today)
	if score1 != score2 {
		t.Fatal("stacked formats")
	}
	if books[0].ID != 1 || books[3].ID != 4 {
		t.Fatal("mutated input")
	}
}
func TestSeriesPinsReservePrerequisites(t *testing.T) {
	one, two := 1.0, 2.0
	books := []Book{{ID: 1, Interest: 1, Series: "A", SeriesNumber: &one}, {ID: 2, Interest: 5}, {ID: 3, Interest: 4, Series: "A", SeriesNumber: &two, Pinned: true}}
	rows, err := Propose(books, defaultPreferences, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids(rows), []int64{2, 1, 3}) {
		t.Fatal(rows)
	}
	books[0], books[2] = books[2], books[0]
	if _, err = Propose(books, defaultPreferences, 0, time.Now()); err == nil {
		t.Fatal("allowed impossible pin")
	}
}
