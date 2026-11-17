package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strconv"
)

// hcSeriesPlan is a previewed addition of series volumes the reader does not
// have. Nothing is added until the reader confirms it.
type hcSeriesPlan struct {
	SourceBookID int64            `json:"source_book_id"`
	LocalSeries  string           `json:"local_series"`
	Items        []hcSeriesMember `json:"items"`
}

const hcSeriesAddLimit = 50

// hardcoverRefresh reads one linked book's catalog entry (one request) and
// caches its community average, cover URL, and series members. It fills no
// field and changes no book, so it does not count as a library change.
func (a *App) hardcoverRefresh(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	id, err := requestID(r)
	if err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, id)
	if err != nil {
		return err
	}
	if link == nil {
		return bad("Link this book to Hardcover first.")
	}
	a.hardcoverMu.Lock()
	book, err := a.hardcover.catalogByID(r.Context(), link.HardcoverBookID)
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	if book == nil {
		return conflict("Hardcover no longer has this book.")
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		now, err := getBookLink(tx, id)
		if err != nil {
			return err
		}
		if now == nil || now.HardcoverBookID != link.HardcoverBookID {
			return conflict("This book's Hardcover link changed. Refresh it again.")
		}
		return storeCatalog(tx, id, *book, canonicalTime(a.now()))
	})
	if err != nil {
		return err
	}
	if r.FormValue("back") == "series" {
		return redirect(w, r, "/series?notice=hc-refreshed")
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=hc-refreshed", id))
}

// seriesAddPreview previews adding chosen cached series members. It reads the
// cache only, so it costs no request.
func (a *App) seriesAddPreview(w http.ResponseWriter, r *http.Request) error {
	src, err := strconv.ParseInt(r.FormValue("source"), 10, 64)
	if err != nil || src < 1 {
		return bad("Choose a series.")
	}
	b, err := getBook(a.store.DB, src)
	if err != nil {
		return err
	}
	cat, err := catalogFor(a.store.DB, seriesGroup{Name: b.Series})
	if err != nil {
		return err
	}
	if cat == nil || cat.SourceBookID != src {
		return bad("Refresh this series from Hardcover first.")
	}
	var chosen []int64
	for _, raw := range r.Form["member"] {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 && !slices.Contains(chosen, id) {
			chosen = append(chosen, id)
		}
	}
	plan := hcSeriesPlan{SourceBookID: src, LocalSeries: b.Series}
	for _, m := range cat.Missing {
		if slices.Contains(chosen, m.HardcoverBookID) {
			plan.Items = append(plan.Items, m)
		}
	}
	switch {
	case len(plan.Items) == 0:
		return bad("Choose at least one volume to add.")
	case len(plan.Items) > hcSeriesAddLimit:
		return bad(fmt.Sprintf("Add at most %d volumes at a time.", hcSeriesAddLimit))
	}
	var id string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = savePlan(tx, "hc-series", Plan{HardcoverSeries: &plan})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/series/"+id)
}

func (a *App) seriesAddPlan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		kind, p, rev, err := readPlan(a.store.DB, id)
		if err != nil {
			return err
		}
		if kind != "hc-series" || p.HardcoverSeries == nil {
			return sql.ErrNoRows
		}
		return a.render(w, r, "hardcover_series.html", page{"Title": "Add series volumes", "Nav": "hardcover", "PlanID": id, "Plan": p.HardcoverSeries, "Stale": rev != state(r).Revision}, 200)
	}
	var added, skipped int
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		kind, p, rev, err := readPlan(tx, id)
		if err != nil {
			return err
		}
		if kind != "hc-series" || p.HardcoverSeries == nil {
			return sql.ErrNoRows
		}
		if now, err := revision(tx); err != nil {
			return err
		} else if now != rev {
			return conflict("Your library changed. Preview the volumes again.")
		}
		plan := p.HardcoverSeries
		cat, err := catalogFor(tx, seriesGroup{Name: plan.LocalSeries})
		if err != nil {
			return err
		}
		if cat == nil {
			return conflict("The cached series is gone. Refresh it from Hardcover and preview again.")
		}
		still := map[int64]bool{}
		for _, m := range cat.Missing {
			still[m.HardcoverBookID] = true
		}
		at := canonicalTime(a.now())
		for _, m := range plan.Items {
			if !still[m.HardcoverBookID] {
				skipped++
				continue
			}
			rec := Record{Book: Book{Title: m.Title, Author: m.Author, Status: "to-read", Series: plan.LocalSeries, SeriesNumber: m.Position, Source: "hardcover-series", Interest: 3, NeedsPlacement: true, Pages: positive(m.Pages), PublishedYear: positive(m.Year)}}
			res, err := importRecords(tx, []Record{rec})
			if err != nil {
				return err
			}
			if res.Added == 0 {
				skipped++
				continue
			}
			var bookID int64
			if err = tx.QueryRow("SELECT id FROM books WHERE identity=?", identity(m.Title, m.Author)).Scan(&bookID); err != nil {
				return err
			}
			if err = saveBookLink(tx, bookID, hcCandidate{BookID: m.HardcoverBookID}, at); err != nil {
				return err
			}
			added++
		}
		if _, err = tx.Exec("DELETE FROM plans WHERE id=?", id); err != nil {
			return err
		}
		if added > 0 {
			return bump(tx)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/series?notice=hc-series-added&added=%d&skipped=%d", added, skipped))
}
