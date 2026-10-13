package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
)

func (a *App) deleteBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		p, err := previewDelete(a.store.DB, id)
		if err != nil {
			return err
		}
		return a.render(w, r, "delete.html", page{"Title": "Delete " + p.Book.Title, "Preview": p}, 200)
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if err := deleteBook(tx, id); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/?view=all&notice=deleted")
}

func (a *App) mergeBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		if raw := r.URL.Query().Get("target"); raw != "" {
			target, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || target < 1 {
				return bad("Choose a valid book to merge into.")
			}
			preview, err := mergeBooks(a.store.DB, id, target, false)
			if err != nil {
				return err
			}
			return a.render(w, r, "merge.html", page{"Title": "Merge " + preview.Source.Title, "Preview": preview, "Book": preview.Source}, 200)
		}
		source, err := getBook(a.store.DB, id)
		if err != nil {
			return err
		}
		query := searchQuery(r.URL.Query().Get("q"))
		results := []Book{}
		if query != "" {
			where, args := bookFilter{Query: query}.where()
			if results, err = queryBooks(a.store.DB, where+" AND b.id!=? ORDER BY b.position,b.id LIMIT 50", append(args, id)...); err != nil {
				return err
			}
		}
		return a.render(w, r, "merge.html", page{"Title": "Merge " + source.Title, "Book": source, "Query": query, "Results": results}, 200)
	}
	target, err := strconv.ParseInt(r.FormValue("target"), 10, 64)
	if err != nil || target < 1 {
		return bad("Choose a valid book to merge into.")
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if _, err := mergeBooks(tx, id, target, true); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=merged", target))
}
