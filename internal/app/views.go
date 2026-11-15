package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxViewName     = 80
	maxSavedViews   = 100
	maxFilterText   = 200
	maxRestoreViews = maxSavedViews
	// viewDefinitionVersion is stored in every definition, so a later release
	// can read older ones.
	viewDefinitionVersion = 1
	maxPagesFilter        = 99999
)

var loanFormats = []string{"print", "ebook", "audiobook", "digital"}
var loanSources = []string{"nypl", "libby"}

// filterKeys are the query parameters that make up a list filter, in the
// order they are written.
var filterKeys = []string{"status", "q", "sort", "tag", "series", "pages_min", "pages_max", "pages_unknown", "source", "loan_status", "format", "owned"}

// parseListFilter reads the All books filter from query parameters. Values it
// does not recognize are ignored, and each one is reported in notes so the page
// can say so. It is the single reader for URLs, saved views, and forms.
func parseListFilter(v url.Values) (f bookFilter, notes []string) {
	f.Query = searchQuery(v.Get("q"))
	if s := v.Get("status"); in(s, readingStates) {
		f.Status = s
	} else if s != "" {
		notes = append(notes, "Ignored an unknown status.")
	}
	for _, o := range sortOptions {
		if v.Get("sort") == o.Key {
			f.Sort = o.Key
		}
	}
	if v.Get("sort") != "" && f.Sort == "" {
		notes = append(notes, "Ignored an unknown sort.")
	}
	f.Tag = cleanFilterText(v.Get("tag"))
	f.Series = cleanFilterText(v.Get("series"))
	for _, p := range []struct {
		key  string
		dest **int
	}{{"pages_min", &f.PagesMin}, {"pages_max", &f.PagesMax}} {
		raw := strings.TrimSpace(v.Get(p.key))
		if raw == "" {
			continue
		}
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= maxPagesFilter {
			*p.dest = &n
		} else {
			notes = append(notes, fmt.Sprintf("Ignored the page limit %q: enter a whole number from 1 to %d.", truncate(raw, 20), maxPagesFilter))
		}
	}
	f.PagesUnknown = v.Get("pages_unknown") == "1"
	f.LoanSource = allowed(v.Get("source"), loanSources, &notes)
	f.LoanStatus = allowed(v.Get("loan_status"), libraryStates, &notes)
	f.LoanFormat = allowed(v.Get("format"), loanFormats, &notes)
	f.Owned = allowed(v.Get("owned"), []string{"yes", "no"}, &notes)
	return f, notes
}

func allowed(value string, options []string, notes *[]string) string {
	if value == "" || in(value, options) {
		return value
	}
	*notes = append(*notes, fmt.Sprintf("Ignored an unknown choice %q.", truncate(value, 20)))
	return ""
}

// cleanFilterText trims a tag or series and drops control characters.
func cleanFilterText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), maxFilterText)
}

// values is the canonical query form of a filter: only the keys that are set,
// so equal filters produce equal URLs and definitions.
func (f bookFilter) values() url.Values {
	v := url.Values{}
	set := func(key, value string) {
		if value != "" {
			v.Set(key, value)
		}
	}
	set("status", f.Status)
	set("q", f.Query)
	set("sort", f.Sort)
	set("tag", f.Tag)
	set("series", f.Series)
	if f.PagesMin != nil {
		set("pages_min", strconv.Itoa(*f.PagesMin))
	}
	if f.PagesMax != nil {
		set("pages_max", strconv.Itoa(*f.PagesMax))
	}
	if f.PagesUnknown {
		set("pages_unknown", "1")
	}
	set("source", f.LoanSource)
	set("loan_status", f.LoanStatus)
	set("format", f.LoanFormat)
	set("owned", f.Owned)
	return v
}

// active reports whether any filter or sort is set.
func (f bookFilter) active() bool { return len(f.values()) > 0 }

type fieldPair struct{ Name, Value string }

// hiddenFields lists a filter's values for hidden form inputs, skipping the
// named keys, in a fixed order.
func (f bookFilter) hiddenFields(skip ...string) []fieldPair {
	v := f.values()
	out := []fieldPair{}
	for _, k := range filterKeys {
		if val := v.Get(k); val != "" && !slices.Contains(skip, k) {
			out = append(out, fieldPair{k, val})
		}
	}
	return out
}

// viewDefinition is what a saved view stores: a versioned, validated filter
// and sort. It is data, never a query: nothing in it is run as SQL.
type viewDefinition struct {
	Version      int    `json:"v"`
	Status       string `json:"status,omitempty"`
	Query        string `json:"q,omitempty"`
	Sort         string `json:"sort,omitempty"`
	Tag          string `json:"tag,omitempty"`
	Series       string `json:"series,omitempty"`
	PagesMin     *int   `json:"pages_min,omitempty"`
	PagesMax     *int   `json:"pages_max,omitempty"`
	PagesUnknown bool   `json:"pages_unknown,omitempty"`
	LoanSource   string `json:"source,omitempty"`
	LoanStatus   string `json:"loan_status,omitempty"`
	LoanFormat   string `json:"format,omitempty"`
	Owned        string `json:"owned,omitempty"`
}

func definitionOf(f bookFilter) viewDefinition {
	return viewDefinition{viewDefinitionVersion, f.Status, f.Query, f.Sort, f.Tag, f.Series, f.PagesMin, f.PagesMax, f.PagesUnknown, f.LoanSource, f.LoanStatus, f.LoanFormat, f.Owned}
}

func (d viewDefinition) filter() bookFilter {
	return bookFilter{Status: d.Status, Query: d.Query, Sort: d.Sort, Tag: d.Tag, Series: d.Series, PagesMin: d.PagesMin, PagesMax: d.PagesMax, PagesUnknown: d.PagesUnknown, LoanSource: d.LoanSource, LoanStatus: d.LoanStatus, LoanFormat: d.LoanFormat, Owned: d.Owned}
}

// check returns why a definition is invalid, or "". A stored or restored
// definition must survive parseListFilter unchanged.
func (d viewDefinition) check() string {
	if d.Version != viewDefinitionVersion {
		return fmt.Sprintf("the definition version must be %d", viewDefinitionVersion)
	}
	f, notes := parseListFilter(d.filter().values())
	if len(notes) > 0 || definitionOf(f).filter().values().Encode() != d.filter().values().Encode() {
		return "the definition has a value this release does not accept"
	}
	return ""
}

// Summary describes a definition in words for lists.
func (d viewDefinition) Summary() string {
	f := d.filter()
	parts := []string{}
	if f.Status != "" {
		parts = append(parts, statusLabel(f.Status))
	}
	if f.Query != "" {
		parts = append(parts, "matching “"+f.Query+"”")
	}
	if f.Tag != "" {
		parts = append(parts, "tagged “"+f.Tag+"”")
	}
	if f.Series != "" {
		parts = append(parts, "in series “"+f.Series+"”")
	}
	switch {
	case f.PagesMin != nil && f.PagesMax != nil:
		parts = append(parts, fmt.Sprintf("%d–%d pages", *f.PagesMin, *f.PagesMax))
	case f.PagesMin != nil:
		parts = append(parts, fmt.Sprintf("%d+ pages", *f.PagesMin))
	case f.PagesMax != nil:
		parts = append(parts, fmt.Sprintf("up to %d pages", *f.PagesMax))
	}
	if f.PagesUnknown {
		if f.PagesMin != nil || f.PagesMax != nil {
			parts = append(parts, "or pages unknown")
		} else {
			parts = append(parts, "pages unknown")
		}
	}
	loan := strings.TrimSpace(strings.Join([]string{strings.ToUpper(f.LoanSource), f.LoanFormat, f.LoanStatus}, " "))
	if loan != "" {
		parts = append(parts, "borrowing: "+loan)
	}
	switch f.Owned {
	case "yes":
		parts = append(parts, "owned")
	case "no":
		parts = append(parts, "not owned")
	}
	if f.Sort != "" {
		for _, o := range sortOptions {
			if o.Key == f.Sort {
				parts = append(parts, "sorted by "+strings.ToLower(o.Label))
			}
		}
	}
	if len(parts) == 0 {
		return "All books"
	}
	return strings.Join(parts, " · ")
}

// savedView is one named, versioned view.
type savedView struct {
	ID         int64
	Name       string
	Definition viewDefinition
	Version    int
	CreatedAt  string
	UpdatedAt  string
}

// URL opens the view: the stable /views/ID address redirects to the full filter.
func (s savedView) URL() string { return fmt.Sprintf("/views/%d", s.ID) }

func openURL(id int64, f bookFilter) string {
	v := f.values()
	v.Set("view", "all")
	v.Set("saved", strconv.FormatInt(id, 10))
	return "/?" + v.Encode()
}

func loadSavedViews(q queryer) ([]savedView, error) {
	rows, err := q.Query("SELECT id,name,definition,version,created_at,updated_at FROM saved_views ORDER BY nc_lower(name),id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []savedView{}
	for rows.Next() {
		var s savedView
		var raw string
		if err = rows.Scan(&s.ID, &s.Name, &raw, &s.Version, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &s.Definition); err != nil {
			s.Definition = viewDefinition{Version: viewDefinitionVersion}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func getSavedView(q queryer, id int64) (savedView, error) {
	var s savedView
	var raw string
	err := q.QueryRow("SELECT id,name,definition,version,created_at,updated_at FROM saved_views WHERE id=?", id).Scan(&s.ID, &s.Name, &raw, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal([]byte(raw), &s.Definition); err != nil {
		s.Definition = viewDefinition{Version: viewDefinitionVersion}
	}
	return s, nil
}

// cleanViewName trims a name and drops control characters.
func cleanViewName(s string) (string, error) {
	name := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
	switch {
	case name == "":
		return "", bad("Give the view a name.")
	case len([]rune(name)) > maxViewName:
		return "", bad(fmt.Sprintf("Keep a view name under %d characters.", maxViewName))
	}
	return name, nil
}

// nameTaken reports whether another saved view has this name, ignoring case.
func nameTaken(q queryer, name string, exceptID int64) (bool, error) {
	views, err := loadSavedViews(q)
	if err != nil {
		return false, err
	}
	for _, v := range views {
		if v.ID != exceptID && foldKey(v.Name) == foldKey(name) {
			return true, nil
		}
	}
	return false, nil
}

// filterFromForm reads the filter a view form carries in hidden fields.
func filterFromForm(r *http.Request) (bookFilter, error) {
	f, notes := parseListFilter(r.Form)
	if len(notes) > 0 {
		return f, bad(notes[0])
	}
	return f, nil
}

func requestViewID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, bad("Choose a valid view.")
	}
	return id, nil
}

// savedViewsPage lists the saved views with how many books each matches now.
func (a *App) savedViewsPage(w http.ResponseWriter, r *http.Request) error {
	views, err := loadSavedViews(a.store.DB)
	if err != nil {
		return err
	}
	type row struct {
		savedView
		Count int
	}
	rows := make([]row, len(views))
	for i, v := range views {
		n, err := countBooks(a.store.DB, v.Definition.filter())
		if err != nil {
			return err
		}
		rows[i] = row{v, n}
	}
	return a.render(w, r, "views.html", page{"Title": "Saved views", "Views": rows, "Nav": "all", "Max": maxSavedViews}, 200)
}

// openSavedView redirects the stable address of a view to its filters.
func (a *App) openSavedView(w http.ResponseWriter, r *http.Request) error {
	id, err := requestViewID(r)
	if err != nil {
		return err
	}
	s, err := getSavedView(a.store.DB, id)
	if err != nil {
		return err
	}
	return redirect(w, r, openURL(s.ID, s.Definition.filter()))
}

// createSavedView saves the filter and sort the form carries under a name.
// Like notes, it never changes the library revision or Undo.
func (a *App) createSavedView(w http.ResponseWriter, r *http.Request) error {
	r.ParseForm()
	name, err := cleanViewName(r.FormValue("name"))
	if err != nil {
		return err
	}
	f, err := filterFromForm(r)
	if err != nil {
		return err
	}
	var id int64
	err = a.store.Write(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM saved_views").Scan(&n); err != nil {
			return err
		}
		if n >= maxSavedViews {
			return bad(fmt.Sprintf("You can save at most %d views. Delete one first.", maxSavedViews))
		}
		if taken, err := nameTaken(tx, name, 0); err != nil {
			return err
		} else if taken {
			return conflict("You already have a view with that name.")
		}
		raw, _ := json.Marshal(definitionOf(f))
		now := canonicalTime(a.now())
		res, err := tx.Exec("INSERT INTO saved_views(name,definition,created_at,updated_at) VALUES(?,?,?,?)", name, string(raw), now, now)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, openURL(id, f))
}

// editSavedView renames, updates, or deletes a view. The form carries the
// version it was loaded with; a save from an older page is refused.
func (a *App) editSavedView(w http.ResponseWriter, r *http.Request) error {
	r.ParseForm()
	id, err := requestViewID(r)
	if err != nil {
		return err
	}
	version, err := strconv.Atoi(r.FormValue("version"))
	if err != nil {
		return bad("Reload the page and try again.")
	}
	action := r.FormValue("action")
	var name string
	var f bookFilter
	switch action {
	case "rename":
		if name, err = cleanViewName(r.FormValue("name")); err != nil {
			return err
		}
	case "update":
		if f, err = filterFromForm(r); err != nil {
			return err
		}
	case "delete":
	default:
		return bad("Choose a valid action.")
	}
	dest := "/views"
	err = a.store.Write(func(tx *sql.Tx) error {
		s, err := getSavedView(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return conflict("This view was already deleted. Reload the page.")
		}
		if err != nil {
			return err
		}
		if s.Version != version {
			return conflict("This view changed since the page loaded. Reload it before editing.")
		}
		now := canonicalTime(a.now())
		switch action {
		case "delete":
			_, err = tx.Exec("DELETE FROM saved_views WHERE id=?", id)
		case "rename":
			if taken, e := nameTaken(tx, name, id); e != nil {
				return e
			} else if taken {
				return conflict("You already have a view with that name.")
			}
			_, err = tx.Exec("UPDATE saved_views SET name=?,version=version+1,updated_at=? WHERE id=?", name, now, id)
		case "update":
			raw, _ := json.Marshal(definitionOf(f))
			_, err = tx.Exec("UPDATE saved_views SET definition=?,version=version+1,updated_at=? WHERE id=?", string(raw), now, id)
			dest = openURL(id, f)
		}
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, dest)
}
