package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// hardcoverReady is the gate for every action that contacts Hardcover: the
// operator set a token and the reader switched Hardcover on.
func (a *App) hardcoverReady() error {
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	if !prefs.Hardcover {
		return bad("Enable Hardcover in Preferences → Privacy first.")
	}
	if a.hardcover == nil {
		return bad("Set HARDCOVER_API_TOKEN in the server environment first.")
	}
	return nil
}

// annotateOwners marks candidates another local book already holds, so the
// preview shows the conflict instead of failing on apply.
func annotateOwners(q queryer, items []hcLinkItem) error {
	for i := range items {
		for j := range items[i].Match.Candidates {
			c := &items[i].Match.Candidates[j]
			owner, title, err := linkOwner(q, c.BookID, items[i].BookID)
			if err != nil {
				return err
			}
			c.LinkedToID, c.LinkedTo = owner, title
			if items[i].Current != nil && items[i].Current.HardcoverBookID == c.BookID {
				c.IsCurrentLink = true
			}
		}
	}
	return nil
}

// saveLinkPlan stores a link plan inside a write that checks the form's revision.
func (a *App) saveLinkPlan(r *http.Request, plan hcLinkPlan) (string, error) {
	var id string
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = savePlan(tx, "hc-link", Plan{HardcoverLinks: &plan})
		return err
	})
	return id, err
}

// findBookLink looks up one book. It is a POST so no page load contacts Hardcover.
func (a *App) hardcoverFindBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if err = a.hardcoverReady(); err != nil {
		return err
	}
	book, err := getBook(a.store.DB, id)
	if err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, id)
	if err != nil {
		return err
	}
	a.hardcoverMu.Lock()
	items, stopped, err := a.hardcover.findMatches(r.Context(), []Book{book})
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	if stopped != nil {
		return hardcoverHTTPError(stopped)
	}
	plan := hcLinkPlan{Items: items}
	plan.Items[0].Current = link
	if err = annotateOwners(a.store.DB, plan.Items); err != nil {
		return err
	}
	planID, err := a.saveLinkPlan(r, plan)
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/links/"+planID)
}

// hardcoverUnlinkBook previews removing a book's link. It makes no request.
func (a *App) hardcoverUnlinkBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	book, err := getBook(a.store.DB, id)
	if err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, id)
	if err != nil {
		return err
	}
	if link == nil {
		return bad("This book is not linked to Hardcover.")
	}
	planID, err := a.saveLinkPlan(r, hcLinkPlan{Items: []hcLinkItem{{BookID: id, Title: book.Title, Author: book.Author, Action: "unlink", Current: link}}})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/links/"+planID)
}

// hardcoverMatchBatch looks for matches for the next few unlinked books after
// the cursor. Each run stays inside the request burst; the reader continues.
func (a *App) hardcoverMatchBatch(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	after, _ := strconv.ParseInt(r.FormValue("after"), 10, 64)
	books, err := queryBooks(a.store.DB, "WHERE b.id>? AND NOT EXISTS (SELECT 1 FROM book_hardcover h WHERE h.book_id=b.id) ORDER BY b.id LIMIT ?", after, hardcoverRunLimit)
	if err != nil {
		return err
	}
	if len(books) == 0 {
		return bad("Every book has been checked. Nothing is waiting to be linked.")
	}
	a.hardcoverMu.Lock()
	items, stopped, err := a.hardcover.findMatches(r.Context(), books)
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	plan := hcLinkPlan{Items: items}
	last := after
	if n := len(items); n > 0 {
		last = items[n-1].BookID
	}
	if stopped != nil {
		if len(items) == 0 {
			return hardcoverHTTPError(stopped)
		}
		plan.Stopped = stopped.Error() + " Books not reached are untouched; continue later."
	}
	if stopped == nil {
		last = books[len(books)-1].ID
	}
	plan.NextAfter = last
	if err = a.store.DB.QueryRow("SELECT count(*) FROM books b WHERE b.id>? AND NOT EXISTS (SELECT 1 FROM book_hardcover h WHERE h.book_id=b.id)", last).Scan(&plan.Remaining); err != nil {
		return err
	}
	if err = annotateOwners(a.store.DB, plan.Items); err != nil {
		return err
	}
	planID, err := a.saveLinkPlan(r, plan)
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/links/"+planID)
}

func (a *App) hardcoverLinkPlan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		kind, p, rev, err := readPlan(a.store.DB, id)
		if err != nil {
			return err
		}
		if kind != "hc-link" || p.HardcoverLinks == nil {
			return sql.ErrNoRows
		}
		return a.render(w, r, "hardcover_links.html", page{"Title": "Link to Hardcover", "Nav": "hardcover", "PlanID": id, "Links": p.HardcoverLinks, "Stale": rev != state(r).Revision}, 200)
	}
	var linked, unlinked int
	err := a.store.Write(func(tx *sql.Tx) error {
		kind, p, rev, err := readPlan(tx, id)
		if err != nil {
			return err
		}
		if kind != "hc-link" || p.HardcoverLinks == nil {
			return sql.ErrNoRows
		}
		if now, err := revision(tx); err != nil {
			return err
		} else if now != rev {
			return conflict("Your library changed. Generate a fresh preview before applying it.")
		}
		at := canonicalTime(a.now())
		for _, item := range p.HardcoverLinks.Items {
			if _, err = getBook(tx, item.BookID); errors.Is(err, sql.ErrNoRows) {
				return conflict("A book in this preview no longer exists. Generate a fresh preview.")
			} else if err != nil {
				return err
			}
			if item.Action == "unlink" {
				if _, err = tx.Exec("DELETE FROM book_hardcover WHERE book_id=?", item.BookID); err != nil {
					return err
				}
				if err = resetSync(tx, item.BookID); err != nil {
					return err
				}
				unlinked++
				continue
			}
			raw := r.FormValue("choice_" + strconv.FormatInt(item.BookID, 10))
			if raw == "" {
				continue
			}
			want, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return bad("Choose one of the listed Hardcover books.")
			}
			// Only a candidate this preview showed can be linked.
			var chosen *hcCandidate
			for i := range item.Match.Candidates {
				if item.Match.Candidates[i].BookID == want {
					chosen = &item.Match.Candidates[i]
				}
			}
			if chosen == nil {
				return bad("Choose one of the listed Hardcover books.")
			}
			if err = saveBookLink(tx, item.BookID, *chosen, at); err != nil {
				return err
			}
			linked++
		}
		if linked+unlinked > 0 {
			if err = bump(tx); err != nil {
				return err
			}
		}
		_, err = tx.Exec("DELETE FROM plans WHERE id=?", id)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/hardcover?notice=hardcover-links&linked=%d&unlinked=%d", linked, unlinked))
}
