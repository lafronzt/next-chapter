package app

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRanking(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestScoreBoundaries(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)
	pages := func(n int) *int { return &n }
	due := func(days int) []Loan {
		return []Loan{{Status: "borrowed", DueDate: today.AddDate(0, 0, days).Format(time.DateOnly)}}
	}
	cases := []struct {
		name   string
		book   Book
		prefs  Preferences
		want   float64
		reason string
	}{
		{"interest only", Book{Interest: 3}, defaultPreferences, 30, "Interest 3/5"},
		{"overdue", Book{Interest: 1, Loans: due(-1)}, defaultPreferences, 10 + 15 + 20, "Deadline has passed"},
		{"due today", Book{Interest: 1, Loans: due(0)}, defaultPreferences, 10 + 15 + 20, "Deadline within 7 days"},
		{"due in 7 days", Book{Interest: 1, Loans: due(7)}, defaultPreferences, 10 + 15 + 20, "Deadline within 7 days"},
		{"due in 8 days", Book{Interest: 1, Loans: due(8)}, defaultPreferences, 10 + 15, "Already borrowed"},
		{"bad due date", Book{Interest: 1, Loans: []Loan{{Status: "borrowed", DueDate: "soon"}}}, defaultPreferences, 10 + 15, "Already borrowed"},
		{"ready", Book{Interest: 1, Loans: []Loan{{Status: "ready"}}}, defaultPreferences, 10 + 25, "Ready for pickup"},
		{"available", Book{Interest: 1, Loans: []Loan{{Status: "available"}}}, defaultPreferences, 10 + 5, "Interest 1/5"},
		{"hold", Book{Interest: 1, Loans: []Loan{{Status: "hold"}}}, defaultPreferences, 10, "Interest 1/5"},
		{"best loan only", Book{Interest: 1, Loans: []Loan{{Status: "available"}, {Status: "ready"}}}, defaultPreferences, 10 + 25, "Ready for pickup"},
		{"library weight 0", Book{Interest: 1, Loans: []Loan{{Status: "ready"}}}, Preferences{LibraryWeight: 0, Length: "any"}, 10, "Ready for pickup"},
		{"library weight 2", Book{Interest: 1, Loans: []Loan{{Status: "ready"}}}, Preferences{LibraryWeight: 2, Length: "any"}, 10 + 50, "Ready for pickup"},
		{"mood ignores case and spaces", Book{Interest: 1, Tags: "fiction, HOPEFUL "}, Preferences{LibraryWeight: 1, Length: "any", Mood: " hopeful"}, 10 + 15, "Matches your hopeful mood"},
		{"mood needs a whole tag", Book{Interest: 1, Tags: "hopefulness"}, Preferences{LibraryWeight: 1, Length: "any", Mood: "hopeful"}, 10, "Interest 1/5"},
		{"short at 250", Book{Interest: 1, Pages: pages(250)}, Preferences{LibraryWeight: 1, Length: "short"}, 20, "Matches your preferred length"},
		{"short at 251", Book{Interest: 1, Pages: pages(251)}, Preferences{LibraryWeight: 1, Length: "short"}, 10, "Interest 1/5"},
		{"long at 400", Book{Interest: 1, Pages: pages(400)}, Preferences{LibraryWeight: 1, Length: "long"}, 20, "Matches your preferred length"},
		{"long at 399", Book{Interest: 1, Pages: pages(399)}, Preferences{LibraryWeight: 1, Length: "long"}, 10, "Interest 1/5"},
		{"length unknown", Book{Interest: 1}, Preferences{LibraryWeight: 1, Length: "short"}, 10, "Interest 1/5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reasons := score(c.book, c.prefs, today)
			if got != c.want || !slices.Contains(reasons, c.reason) {
				t.Fatalf("score %v %q, want %v with %q", got, reasons, c.want, c.reason)
			}
		})
	}
}

// The deadline window counts calendar days in the location of today; callers
// pass UTC, so it turns over at UTC midnight.
func TestScoreDeadlineAtUTCMidnight(t *testing.T) {
	t.Parallel()
	b := Book{Interest: 1, Loans: []Loan{{Status: "borrowed", DueDate: "2026-10-17"}}}
	for _, c := range []struct {
		today time.Time
		want  float64
	}{
		{time.Date(2026, 10, 9, 23, 59, 59, 0, time.UTC), 25},
		{time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), 45},
		{time.Date(2026, 10, 10, 23, 59, 59, 0, time.UTC), 45},
		// 21:00 on the 9th in New York is already the 10th in UTC.
		{time.Date(2026, 10, 9, 21, 0, 0, 0, time.FixedZone("EDT", -4*3600)).UTC(), 45},
	} {
		if got, _ := score(b, defaultPreferences, c.today); got != c.want {
			t.Errorf("today %v: score %v, want %v", c.today, got, c.want)
		}
	}
}

func cloneBooks(books []Book) []Book {
	out := make([]Book, len(books))
	for i, b := range books {
		if b.Pages != nil {
			p := *b.Pages
			b.Pages = &p
		}
		if b.SeriesNumber != nil {
			n := *b.SeriesNumber
			b.SeriesNumber = &n
		}
		b.Loans = slices.Clone(b.Loans)
		out[i] = b
	}
	return out
}

// validOrder reports whether order (indexes into books) keeps every pin in its
// slot and every earlier series number ahead of later ones.
func validOrder(books []Book, order []int) bool {
	for slot, j := range order {
		if books[j].Pinned && j != slot {
			return false
		}
		for _, k := range order[slot+1:] {
			if prerequisite(books[k], books[j]) {
				return false
			}
		}
	}
	return true
}

// anyValidOrder brute-forces every permutation of a small queue.
func anyValidOrder(books []Book) bool {
	order := make([]int, len(books))
	used := make([]bool, len(books))
	var place func(slot int) bool
	place = func(slot int) bool {
		if slot == len(books) {
			return validOrder(books, order)
		}
		for j := range books {
			if used[j] {
				continue
			}
			// Prune: a pin may only sit in its own slot.
			if (books[j].Pinned && j != slot) || (books[slot].Pinned && j != slot) {
				continue
			}
			used[j], order[slot] = true, j
			if place(slot + 1) {
				return true
			}
			used[j] = false
		}
		return false
	}
	return place(0)
}

// insertionOrders lists every queue that moves only the candidate, as indexes.
func insertionOrders(books []Book, c int) [][]int {
	rest := []int{}
	for j := range books {
		if j != c {
			rest = append(rest, j)
		}
	}
	out := [][]int{}
	for slot := 0; slot <= len(rest); slot++ {
		out = append(out, slices.Insert(slices.Clone(rest), slot, c))
	}
	return out
}

// validInsertion checks the constraints insertion promises: pins stay put and
// the candidate respects its own series order. Existing disorder among the
// other books is left alone.
func validInsertion(books []Book, order []int, c int) bool {
	at := slices.Index(order, c)
	for slot, j := range order {
		if books[j].Pinned && j != slot {
			return false
		}
		if slot < at && prerequisite(books[c], books[j]) || slot > at && prerequisite(books[j], books[c]) {
			return false
		}
	}
	return true
}

func TestProposeInvariants(t *testing.T) {
	t.Parallel()
	prefs := []Preferences{defaultPreferences, {LibraryWeight: 2, Length: "short", Mood: "hopeful"}, {LibraryWeight: 0, Length: "long"}}
	outcomes := map[string]int{}
	for seed := range uint64(400) {
		rng := rand.New(rand.NewPCG(seed, seed))
		n := 1 + rng.IntN(7)
		if seed%4 == 0 {
			n = 8 + rng.IntN(40) // larger queues: no brute force, other invariants only
		}
		books := randomQueue(rng, n)
		p := prefs[rng.IntN(len(prefs))]
		candidate := int64(0)
		if rng.IntN(2) == 0 {
			candidate = books[rng.IntN(n)].ID
		}
		before := cloneBooks(books)
		rows, err := Propose(books, p, candidate, proposeToday)
		if !reflect.DeepEqual(books, before) {
			t.Fatalf("seed %d: Propose mutated its input", seed)
		}
		again, againErr := Propose(books, p, candidate, proposeToday)
		if !reflect.DeepEqual(rows, again) || (err == nil) != (againErr == nil) {
			t.Fatalf("seed %d: not deterministic", seed)
		}
		index := map[int64]int{}
		for i, b := range books {
			index[b.ID] = i
		}
		c := -1
		if candidate != 0 {
			c = index[candidate]
		}
		if err != nil {
			outcomes["error"]++
			switch {
			case c >= 0 && books[c].Pinned:
				outcomes["pinned candidate"]++
			case c >= 0:
				outcomes["insertion proven impossible"]++
				for _, order := range insertionOrders(books, c) {
					if validInsertion(books, order, c) {
						t.Fatalf("seed %d: insertion failed (%v) but %v is valid\n%+v", seed, err, order, books)
					}
				}
			case n <= 7:
				outcomes["reorder proven impossible"]++
				if anyValidOrder(books) {
					t.Fatalf("seed %d: reorder failed (%v) but a valid order exists\n%+v", seed, err, books)
				}
			}
			continue
		}
		outcomes["ok"]++
		order := make([]int, len(rows))
		for i, row := range rows {
			j, ok := index[row.ID]
			if !ok {
				t.Fatalf("seed %d: unknown ID %d", seed, row.ID)
			}
			order[i] = j
			if row.New != i+1 || row.Old != j+1 || row.Pinned != books[j].Pinned {
				t.Fatalf("seed %d: row %d is %+v", seed, i, row)
			}
		}
		sorted := slices.Sorted(slices.Values(order))
		for i := range sorted {
			if sorted[i] != i {
				t.Fatalf("seed %d: %v is not a permutation of %d books", seed, order, n)
			}
		}
		if c >= 0 {
			if !validInsertion(books, order, c) {
				t.Fatalf("seed %d: invalid insertion %v\n%+v", seed, order, books)
			}
			rest := slices.DeleteFunc(slices.Clone(order), func(j int) bool { return j == c })
			if !slices.IsSorted(rest) {
				t.Fatalf("seed %d: insertion moved other books: %v", seed, order)
			}
		} else if !validOrder(books, order) {
			t.Fatalf("seed %d: invalid order %v\n%+v", seed, order, books)
		}
	}
	if outcomes["ok"] < 100 || outcomes["insertion proven impossible"] == 0 || outcomes["reorder proven impossible"] == 0 {
		t.Fatalf("generator coverage: %v", outcomes)
	}
	t.Logf("outcomes: %v", outcomes)
}

func TestProposeEmptyAndUnknownCandidate(t *testing.T) {
	t.Parallel()
	if rows, err := Propose(nil, defaultPreferences, 0, proposeToday); err != nil || len(rows) != 0 {
		t.Fatalf("%v %v", rows, err)
	}
	_, err := Propose([]Book{{ID: 1, Interest: 3}}, defaultPreferences, 2, proposeToday)
	if err == nil || !strings.Contains(err.Error(), "unpinned book in your queue") {
		t.Fatal(err)
	}
}
