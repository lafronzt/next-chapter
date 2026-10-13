package app

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// seedLibrary adds n synthetic books: two of every three are in the queue,
// the rest finished, and every seventh has an active Libby loan.
func seedLibrary(t testing.TB, a *App, n int) {
	t.Helper()
	rs := make([]Record, 0, n)
	for i := range n {
		status := "to-read"
		if i%3 == 2 {
			status = "read"
		}
		r := Record{Book: Book{Title: fmt.Sprintf("Synthetic Book %05d", i), Author: fmt.Sprintf("Writer %03d", i%400), Tags: "fiction, synthetic", Status: status, Source: "goodreads"}}
		rs = append(rs, r)
		if i%7 == 0 {
			loan := r
			loan.Source = "libby"
			loan.Event = &Event{Format: "ebook", Status: "borrowed", Activity: "Borrowed", At: canonicalTime(time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC)), DueDate: "2026-12-01"}
			rs = append(rs, loan)
		}
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		if _, err := importRecords(tx, rs); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE books SET needs_placement=0")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// syntheticGoodreadsCSV is a Goodreads export with n rows, about 100 bytes each.
func syntheticGoodreadsCSV(n int) string {
	var b strings.Builder
	b.WriteString("Book Id,Title,Author,ISBN13,Number of Pages,Exclusive Shelf,Bookshelves\n")
	shelves := []string{"to-read", "read", "currently-reading"}
	for i := range n {
		fmt.Fprintf(&b, "%d,Imported Title %05d,Author %04d,978%010d,%d,%s,\"fiction, imported\"\n", 1000000+i, i, i%1000, i, 100+i%400, shelves[i%3])
	}
	return b.String()
}

func median(samples []time.Duration) time.Duration {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}

// TestMeasureScale reports the numbers quoted in the change description.
// NC_MEASURE=1 go test -run TestMeasureScale -v ./internal/app
func TestMeasureScale(t *testing.T) {
	if os.Getenv("NC_MEASURE") == "" {
		t.Skip("set NC_MEASURE=1 to measure")
	}
	a := testApp(t, true)
	seedLibrary(t, a, 5000)
	b := browser{t: t, handler: a.Handler()}
	for _, path := range []string{"/", "/?view=all", "/?view=all&q=writer+01"} {
		b.request("GET", path, nil)
		var samples []time.Duration
		size := 0
		for range 9 {
			start := time.Now()
			rr := b.request("GET", path, nil)
			samples = append(samples, time.Since(start))
			size = rr.Body.Len()
		}
		t.Logf("GET %-24s 5,000 books: median %8v, %9d bytes", path, median(samples).Round(time.Microsecond), size)
	}

	imp := testApp(t, true)
	ib := browser{t: t, handler: imp.Handler()}
	ib.request("GET", "/imports", nil)
	data := syntheticGoodreadsCSV(10_000)
	start := time.Now()
	rr := uploadCSV(&ib, "goodreads", data)
	post := time.Since(start)
	expect(t, rr, 303)
	planURL := rr.Header().Get("Location")
	var stored int
	if err := imp.store.DB.QueryRow("SELECT length(CAST(payload AS BLOB)) FROM plans").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	records, _, err := ParseCSV([]byte(data), "goodreads")
	if err != nil {
		t.Fatal(err)
	}
	var saves []time.Duration
	for range 5 {
		err := imp.store.Write(func(tx *sql.Tx) error {
			start := time.Now()
			_, err := savePlan(tx, "import", Plan{Records: records, Source: "goodreads"})
			saves = append(saves, time.Since(start))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var gets []time.Duration
	for range 5 {
		start := time.Now()
		expect(t, ib.request("GET", planURL, nil), 200)
		gets = append(gets, time.Since(start))
	}
	t.Logf("10,000-row import: CSV %d bytes, POST /imports %v, stored plan %d bytes, savePlan median %v, GET /plans/{id} median %v", len(data), post.Round(time.Millisecond), stored, median(saves).Round(time.Microsecond), median(gets).Round(time.Microsecond))
	start = time.Now()
	expect(t, ib.request("POST", planURL, nil), 303)
	t.Logf("applying the 10,000-row import: %v", time.Since(start).Round(time.Millisecond))
}
