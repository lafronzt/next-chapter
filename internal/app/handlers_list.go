package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
	var filterNotes []string
	keep := url.Values{"view": {view.Key}, "q": {query}}
	if view.Key == "all" {
		// Only All books filters and sorts. Every filter is a plain query
		// parameter read by parseListFilter; unknown values are ignored and
		// reported.
		filter, filterNotes = parseListFilter(r.URL.Query())
		keep = filter.values()
		keep.Set("view", "all")
		if id := r.URL.Query().Get("saved"); id != "" {
			keep.Set("saved", id)
		}
	}
	status, sortKey := filter.Status, filter.Sort
	total, err := countBooks(a.store.DB, filter)
	if err != nil {
		return err
	}
	pages := newPager(r, total, "/", keep)
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
	if view.Key == "all" {
		if err = a.listViewData(r, data, filter, filterNotes); err != nil {
			return err
		}
	}
	if view.Key == "queue" && query == "" && pages.Page == 1 {
		home, err := loadHome(a.store.DB, time.Now())
		if err != nil {
			return err
		}
		data["Home"] = home
	}
	return a.render(w, r, "index.html", data, 200)
}

// listViewData adds what the All books filters show: the saved views, whether
// the filters match the saved view being opened, and how many otherwise
// matching books have no page count.
func (a *App) listViewData(r *http.Request, data page, filter bookFilter, notes []string) error {
	saved, err := loadSavedViews(a.store.DB)
	if err != nil {
		return err
	}
	data["SavedViews"], data["FilterNotes"], data["Filter"] = saved, notes, filter
	data["FilterFields"], data["SearchFields"] = filter.hiddenFields(), filter.hiddenFields("q")
	data["Active"] = filter.active()
	data["Loans"], data["Sources"], data["Formats"] = libraryStates, loanSources, loanFormats
	if id, e := strconv.ParseInt(r.URL.Query().Get("saved"), 10, 64); e == nil {
		for _, s := range saved {
			if s.ID == id {
				data["Saved"] = s
				data["SavedDiffers"] = s.Definition.filter().values().Encode() != filter.values().Encode()
				data["SavedURL"] = openURL(s.ID, s.Definition.filter())
			}
		}
	}
	if (filter.PagesMin != nil || filter.PagesMax != nil) && !filter.PagesUnknown {
		unknown := filter
		unknown.PagesMin, unknown.PagesMax, unknown.PagesUnknown = nil, nil, true
		if data["UnknownPages"], err = countBooks(a.store.DB, unknown); err != nil {
			return err
		}
	}
	return nil
}
