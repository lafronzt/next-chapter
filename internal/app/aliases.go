package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Remembered identities: the records a merge or a deletion leaves so that
// later imports follow the merge or skip the deleted book. The reader can see
// each one with the keys actually stored, and remove a single one after a
// preview. Nothing here retargets, bulk-clears, or merges automatically.

// aliasRow is one stored alias, with its target when there is one.
type aliasRow struct {
	Identity, GoodreadsID, ISBN string
	TargetID                    *int64
	TargetTitle                 string
	TargetMissing               bool
}

// Kind says whether the alias redirects to a book or records a deletion.
func (a aliasRow) Kind() string {
	if a.TargetID == nil {
		return "deleted"
	}
	return "merge"
}

// aliasPlan is a previewed removal of one alias.
type aliasPlan struct {
	Identity     string `json:"identity"`
	GoodreadsID  string `json:"goodreads_id"`
	ISBN         string `json:"isbn"`
	TargetBookID *int64 `json:"target_book_id"`
	TargetTitle  string `json:"target_title,omitempty"`
	Kind         string `json:"kind"`
	// Shared lists other remembered records that carry the same ISBN or
	// Goodreads ID, which removal would leave ambiguous or unambiguous.
	Shared []string `json:"shared,omitempty"`
}

func loadAlias(q queryer, identity string) (aliasRow, error) {
	var a aliasRow
	var title sql.NullString
	err := q.QueryRow(`SELECT a.identity,a.goodreads_id,a.isbn,a.target_book_id,b.title FROM book_aliases a LEFT JOIN books b ON b.id=a.target_book_id WHERE a.identity=?`, identity).Scan(&a.Identity, &a.GoodreadsID, &a.ISBN, &a.TargetID, &title)
	if err != nil {
		return a, err
	}
	a.TargetTitle = title.String
	a.TargetMissing = a.TargetID != nil && !title.Valid
	return a, nil
}

// aliasesPage lists the remembered merges and deletions.
func (a *App) aliasesPage(w http.ResponseWriter, r *http.Request) error {
	kind := r.URL.Query().Get("kind")
	where := ""
	switch kind {
	case "merge":
		where = " WHERE a.target_book_id IS NOT NULL"
	case "deleted":
		where = " WHERE a.target_book_id IS NULL"
	default:
		kind = ""
	}
	var total int
	if err := a.store.DB.QueryRow("SELECT count(*) FROM book_aliases a" + where).Scan(&total); err != nil {
		return err
	}
	pages := newPager(r, total, "/aliases", url.Values{"kind": {kind}})
	rows, err := a.store.DB.Query(`SELECT a.identity,a.goodreads_id,a.isbn,a.target_book_id,b.title FROM book_aliases a LEFT JOIN books b ON b.id=a.target_book_id`+where+` ORDER BY a.identity LIMIT ? OFFSET ?`, pages.PerPage, pages.offset())
	if err != nil {
		return err
	}
	list := []aliasRow{}
	for rows.Next() {
		var x aliasRow
		var title sql.NullString
		if err = rows.Scan(&x.Identity, &x.GoodreadsID, &x.ISBN, &x.TargetID, &title); err != nil {
			rows.Close()
			return err
		}
		x.TargetTitle = title.String
		x.TargetMissing = x.TargetID != nil && !title.Valid
		list = append(list, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return a.render(w, r, "aliases.html", page{"Title": pageTitle("Remembered merges and deletions", pages), "Aliases": list, "Pager": pages, "Kind": kind, "Nav": "imports"}, 200)
}

// previewAliasRemoval explains what removing one alias would change, and
// saves it as a plan to confirm. Removing never resurrects a book now.
func (a *App) previewAliasRemoval(w http.ResponseWriter, r *http.Request) error {
	identity := r.FormValue("identity")
	if identity == "" || len(identity) > 2000 {
		return bad("Choose a remembered record to remove.")
	}
	var planID string
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		x, err := loadAlias(tx, identity)
		if errors.Is(err, sql.ErrNoRows) {
			return conflict("This remembered record is already gone. Reload the list.")
		}
		if err != nil {
			return err
		}
		p := aliasPlan{Identity: x.Identity, GoodreadsID: x.GoodreadsID, ISBN: x.ISBN, TargetBookID: x.TargetID, TargetTitle: x.TargetTitle, Kind: x.Kind()}
		for _, k := range []struct{ column, value, label string }{{"isbn", x.ISBN, "ISBN"}, {"goodreads_id", x.GoodreadsID, "Goodreads ID"}} {
			if k.value == "" {
				continue
			}
			var n int
			if err = tx.QueryRow("SELECT count(*) FROM book_aliases WHERE "+k.column+"=? AND identity!=?", k.value, x.Identity).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				p.Shared = append(p.Shared, fmt.Sprintf("%d other remembered %s share this %s", n, plural(n, "record", "records"), k.label))
			}
		}
		planID, err = savePlan(tx, "alias-remove", Plan{Alias: &p})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+planID)
}

// applyAliasPlan is the apply branch for an alias removal. The caller has
// checked the revision; the row must still be exactly what was previewed.
func applyAliasPlan(q queryer, p *Plan) (string, error) {
	if p.Alias == nil {
		return "", errors.New("alias plan has no data")
	}
	pl := p.Alias
	x, err := loadAlias(q, pl.Identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", conflict("This remembered record is already gone. Reload the list.")
	}
	if err != nil {
		return "", err
	}
	same := x.GoodreadsID == pl.GoodreadsID && x.ISBN == pl.ISBN && ((x.TargetID == nil) == (pl.TargetBookID == nil)) && (x.TargetID == nil || *x.TargetID == *pl.TargetBookID)
	if !same {
		return "", conflict("This remembered record changed since the preview. Start again.")
	}
	if _, err = q.Exec("DELETE FROM book_aliases WHERE identity=?", pl.Identity); err != nil {
		return "", err
	}
	return "/aliases?notice=saved", nil
}

// stored describes the keys an alias actually holds.
func (a aliasRow) Stored() string {
	parts := []string{"normalized title and author: " + a.Identity}
	if a.ISBN != "" {
		parts = append(parts, "ISBN "+a.ISBN)
	}
	if a.GoodreadsID != "" {
		parts = append(parts, "Goodreads ID "+a.GoodreadsID)
	}
	return strings.Join(parts, " · ")
}
