package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// listView is one tab of the book list. views is the only list of tabs; the
// index handler and templates both use it.
type listView struct {
	Key, Label, Status, Heading, Intro string
}

const defaultIntro = "Your list should follow your curiosity, not the date you added a book."

var views = []listView{
	{"queue", "Up next", "to-read", "A good book. Then another.", defaultIntro},
	{"reading", "Reading", "reading", "In good company.", defaultIntro},
	{"read", "Finished", "read", "Chapters completed.", defaultIntro},
	{"unreviewed", "To review", "unreviewed", "Borrowed, but not yet decided.", "Library loans arrive here so borrowing never counts as wanting to read. Tell us what each one means to you."},
	{"abandoned", "Set aside", "abandoned", "Not every book is for now.", defaultIntro},
	{"archived", "Archived", "archived", "Kept for the record.", defaultIntro},
	{"all", "All books", "", "Your books, together.", defaultIntro},
}

func findView(key string) listView {
	for _, v := range views {
		if v.Key == key {
			return v
		}
	}
	return views[0]
}
func searchQuery(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// Title names the view's page; Up next is the reading queue.
func (v listView) Title() string {
	if v.Key == "queue" {
		return "Reading queue"
	}
	return v.Label
}

// filter selects this view's books for a normalized query.
func (v listView) filter(query string) bookFilter { return bookFilter{v.Status, query} }

func (a *App) index(w http.ResponseWriter, r *http.Request) error {
	view := findView(r.URL.Query().Get("view"))
	query := searchQuery(r.URL.Query().Get("q"))
	stats, err := loadStats(a.store.DB)
	if err != nil {
		return err
	}
	total, err := countBooks(a.store.DB, view.filter(query))
	if err != nil {
		return err
	}
	pages := newPager(r, total, "/", url.Values{"view": {view.Key}, "q": {query}})
	items, err := listBooks(a.store.DB, view.filter(query), pages.PerPage, pages.offset())
	if err != nil {
		return err
	}
	var undoRev int64
	err = a.store.DB.QueryRow("SELECT revision FROM undo_queue WHERE id=1").Scan(&undoRev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	canUndo := err == nil && undoRev == state(r).Revision
	title := view.Title()
	if query != "" {
		title = fmt.Sprintf("%s matching “%s”", title, query)
	}
	ai, err := a.aiStatus(a.store.DB)
	if err != nil {
		return err
	}
	return a.render(w, r, "index.html", page{"AI": ai, "Title": pageTitle(title, pages), "Items": items, "Pager": pages, "Stats": stats, "View": view.Key, "ViewInfo": view, "Views": views, "Query": query, "CanUndo": canUndo}, 200)
}
