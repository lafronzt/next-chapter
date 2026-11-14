package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
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
func (v listView) filter(query string) bookFilter { return bookFilter{Status: v.Status, Query: query} }

func (a *App) index(w http.ResponseWriter, r *http.Request) error {
	view := findView(r.URL.Query().Get("view"))
	query := searchQuery(r.URL.Query().Get("q"))
	stats, err := loadStats(a.store.DB)
	if err != nil {
		return err
	}
	filter := view.filter(query)
	status, sortKey := "", ""
	if view.Key == "all" {
		// Only All books filters by status and sorts; both are plain query
		// parameters and unknown values fall back to the default.
		if s := r.URL.Query().Get("status"); in(s, readingStates) {
			status, filter.Status = s, s
		}
		for _, o := range sortOptions {
			if r.URL.Query().Get("sort") == o.Key {
				sortKey, filter.Sort = o.Key, o.Key
			}
		}
	}
	total, err := countBooks(a.store.DB, filter)
	if err != nil {
		return err
	}
	pages := newPager(r, total, "/", url.Values{"view": {view.Key}, "q": {query}, "status": {status}, "sort": {sortKey}})
	items, err := listBooks(a.store.DB, filter, pages.PerPage, pages.offset())
	if err != nil {
		return err
	}
	undoRev, undoLabel, err := readUndo(a.store.DB)
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
	data := page{"AI": ai, "Title": pageTitle(title, pages), "Items": items, "Pager": pages, "Stats": stats, "View": view.Key, "ViewInfo": view, "Views": views, "Query": query, "CanUndo": canUndo, "UndoLabel": undoLabel, "Status": status, "Sort": sortKey, "SortOptions": sortOptions, "ReadingStates": readingStates}
	if view.Key == "queue" && query == "" && pages.Page == 1 {
		home, err := loadHome(a.store.DB, time.Now())
		if err != nil {
			return err
		}
		data["Home"] = home
	}
	return a.render(w, r, "index.html", data, 200)
}
