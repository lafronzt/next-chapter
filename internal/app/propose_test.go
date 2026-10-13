package app

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"
)

var proposeToday = time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)

// randomQueue builds a small queue that exercises pins, series with duplicate
// numbers and case-variant names, library signals, tags, and page counts.
func randomQueue(rng *rand.Rand, n int) []Book {
	series := []string{"Saga", "SAGA", "saga", "ſaga", "Kelvin", "Kelvin", "Other"}
	statuses := []string{"borrowed", "ready", "available", "hold", "returned"}
	books := make([]Book, n)
	for i := range books {
		b := Book{ID: int64(i*3 + 7), Interest: 1 + rng.IntN(5), Tags: []string{"", "hopeful", "mystery, Hopeful"}[rng.IntN(3)]}
		if rng.IntN(4) == 0 {
			pages := []int{120, 300, 520}[rng.IntN(3)]
			b.Pages = &pages
		}
		if rng.IntN(3) == 0 {
			b.Loans = []Loan{{Status: statuses[rng.IntN(len(statuses))], DueDate: proposeToday.AddDate(0, 0, rng.IntN(20)-5).Format(time.DateOnly)}}
		}
		if rng.IntN(2) == 0 {
			b.Series = series[rng.IntN(len(series))]
			if rng.IntN(6) != 0 {
				number := float64(rng.IntN(5)) / []float64{1, 2}[rng.IntN(2)]
				b.SeriesNumber = &number
			}
		}
		b.Pinned = rng.IntN(6) == 0
		books[i] = b
	}
	rng.Shuffle(len(books), func(i, j int) { books[i], books[j] = books[j], books[i] })
	return books
}

func TestProposeMatchesReference(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	prefs := []Preferences{defaultPreferences, {LibraryWeight: 2, Length: "short", Mood: "hopeful"}, {LibraryWeight: 0, Length: "long"}}
	errorsSeen, okSeen := map[bool]int{}, map[bool]int{}
	iterations := 20000
	if raceEnabled {
		iterations = 3000
	}
	for iteration := range iterations {
		books := randomQueue(rng, 5+rng.IntN(36))
		p := prefs[rng.IntN(len(prefs))]
		var candidate int64
		switch rng.IntN(5) {
		case 0:
			candidate = 0
		case 1:
			candidate = 999999
		default:
			candidate = books[rng.IntN(len(books))].ID
		}
		want, wantErr := proposeReference(books, p, candidate, proposeToday)
		got, gotErr := Propose(books, p, candidate, proposeToday)
		if fmt.Sprint(wantErr) != fmt.Sprint(gotErr) || !reflect.DeepEqual(want, got) {
			t.Fatalf("iteration %d candidate %d\nbooks %+v\nwant %v %v\ngot  %v %v", iteration, candidate, books, ids(want), wantErr, ids(got), gotErr)
		}
		if wantErr != nil {
			errorsSeen[candidate == 0]++
		} else {
			okSeen[candidate == 0]++
		}
	}
	// Guard against a generator that never reaches one of the interesting outcomes.
	if errorsSeen[true] == 0 || errorsSeen[false] == 0 || okSeen[true] == 0 || okSeen[false] == 0 {
		t.Fatalf("generator coverage: errors %v ok %v", errorsSeen, okSeen)
	}
}

// syntheticQueue mirrors the measurements in prompts/01: 10% of books in series,
// no pins.
func syntheticQueue(n int) []Book {
	rng := rand.New(rand.NewPCG(uint64(n), 7))
	books := make([]Book, n)
	for i := range books {
		books[i] = Book{ID: int64(i + 1), Interest: 1 + rng.IntN(5), Loans: []Loan{}}
		if i%10 == 0 {
			number := float64(i / 10 % 7)
			books[i].Series = fmt.Sprintf("Series %d", i/70)
			books[i].SeriesNumber = &number
		}
	}
	return books
}

func TestProposeScale(t *testing.T) {
	books := syntheticQueue(2000)
	for _, candidate := range []int64{books[len(books)/2].ID, 0} {
		start := time.Now()
		if _, err := Propose(books, defaultPreferences, candidate, proposeToday); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
			t.Fatalf("candidate %d took %v for 2,000 books", candidate, elapsed)
		}
	}
}

func benchmarkPropose(b *testing.B, propose func([]Book, Preferences, int64, time.Time) ([]Suggestion, error), insert bool) {
	for _, n := range []int{200, 500, 1000, 2000, 5000} {
		books := syntheticQueue(n)
		candidate := int64(0)
		if insert {
			candidate = books[n/2].ID
		}
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				if _, err := propose(books, defaultPreferences, candidate, proposeToday); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkProposeInsert(b *testing.B)           { benchmarkPropose(b, Propose, true) }
func BenchmarkProposeReorder(b *testing.B)          { benchmarkPropose(b, Propose, false) }
func BenchmarkProposeReferenceInsert(b *testing.B)  { benchmarkPropose(b, proposeReference, true) }
func BenchmarkProposeReferenceReorder(b *testing.B) { benchmarkPropose(b, proposeReference, false) }
