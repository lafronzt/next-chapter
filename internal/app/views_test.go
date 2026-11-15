package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// viewLibrary is a small library with every property the filters use.
func viewLibrary(t *testing.T) *App {
	t.Helper()
	a := testApp(t, true)
	p := func(n int) *int { return &n }
	rs := []Record{
		{Book: Book{Title: "Short Fantasy", Author: "A", Status: "to-read", Source: "manual", Interest: 3, Tags: "fantasy, Science Fiction", Pages: p(200), Series: "Élan Saga"}},
		{Book: Book{Title: "Long Fantasy", Author: "A", Status: "to-read", Source: "manual", Interest: 3, Tags: "fantasy", Pages: p(700), Series: "élan saga"}},
		{Book: Book{Title: "Fan Fiction", Author: "A", Status: "to-read", Source: "manual", Interest: 3, Tags: "fan", Pages: p(250)}},
		{Book: Book{Title: "Unknown Pages", Author: "A", Status: "read", Source: "manual", Interest: 3, Tags: "fantasy"}},
		{Book: Book{Title: "Percent 100%", Author: "A", Status: "to-read", Source: "manual", Interest: 3, Tags: "100%, a_b", Pages: p(300)}},
		{Book: Book{Title: "Plain", Author: "B", Status: "reading", Source: "manual", Interest: 3, Pages: p(150)}},
	}
	addRecords(t, a, rs)
	loan := func(title, source, format, status, at string) Record {
		r := libbyEvent(title, status, at)
		r.Author = map[bool]string{true: "B", false: "A"}[title == "Plain"]
		r.Source, r.Event.Format = source, format
		return r
	}
	addRecords(t, a, []Record{
		loan("Short Fantasy", "libby", "ebook", "borrowed", "2026-10-01T00:00:00.000000000+00:00"),
		loan("Short Fantasy", "libby", "audiobook", "borrowed", "2026-10-02T00:00:00.000000000+00:00"),
		loan("Long Fantasy", "nypl", "print", "hold", "2026-10-03T00:00:00.000000000+00:00"),
		loan("Plain", "libby", "ebook", "returned", "2026-10-04T00:00:00.000000000+00:00"),
	})
	if _, err := a.store.DB.Exec("UPDATE books SET owned=1 WHERE title IN ('Short Fantasy','Fan Fiction','Plain')"); err != nil {
		t.Fatal(err)
	}
	return a
}

func titlesFor(t *testing.T, a *App, query string) []string {
	t.Helper()
	v, _ := url.ParseQuery(query)
	f, _ := parseListFilter(v)
	n, err := countBooks(a.store.DB, f)
	if err != nil {
		t.Fatal(err)
	}
	items, err := listBooks(a.store.DB, f, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, it := range items {
		out = append(out, it.Title)
	}
	if n != len(out) {
		t.Fatalf("%q: count %d but %d listed", query, n, len(out))
	}
	return out
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	a := viewLibrary(t)
	for _, c := range []struct {
		query string
		want  string
	}{
		{"", "Short Fantasy,Long Fantasy,Fan Fiction,Unknown Pages,Percent 100%,Plain"},
		// Exact tags: "fan" is not "fantasy", spaces and case are ignored.
		{"tag=fan", "Fan Fiction"},
		{"tag=fantasy", "Short Fantasy,Long Fantasy,Unknown Pages"},
		{"tag=%20SCIENCE%20fiction%20", "Short Fantasy"},
		{"tag=fant", ""},
		{"tag=%25", ""},
		{"tag=100%25", "Percent 100%"},
		{"tag=a_b", "Percent 100%"},
		{"tag=a%25", ""},
		// Series follow ranking's case rule, including non-ASCII.
		{"series=%C3%89LAN%20SAGA", "Short Fantasy,Long Fantasy"},
		{"series=elan%20saga", ""},
		{"series=%25", ""},
		// Page ranges leave out unknown pages unless asked.
		{"pages_max=250", "Short Fantasy,Fan Fiction,Plain"},
		{"pages_max=250&pages_unknown=1", "Short Fantasy,Fan Fiction,Unknown Pages,Plain"},
		{"pages_min=250", "Long Fantasy,Fan Fiction,Percent 100%"},
		{"pages_min=250&pages_max=300", "Fan Fiction,Percent 100%"},
		{"pages_min=300&pages_max=250", ""},
		{"pages_unknown=1", "Unknown Pages"},
		// One loan must satisfy every borrowing choice.
		{"source=libby", "Short Fantasy,Plain"},
		{"source=libby&format=ebook", "Short Fantasy,Plain"},
		{"source=libby&format=print", ""},
		{"source=nypl&loan_status=hold", "Long Fantasy"},
		{"format=audiobook&loan_status=borrowed", "Short Fantasy"},
		{"format=audiobook&loan_status=hold", ""},
		// Ownership.
		{"owned=yes", "Short Fantasy,Fan Fiction,Plain"},
		{"owned=no", "Long Fantasy,Unknown Pages,Percent 100%"},
		// Everything combines with AND, with status, search, and sort.
		{"owned=yes&pages_max=250&status=to-read", "Short Fantasy,Fan Fiction"},
		{"tag=fantasy&owned=yes&source=libby", "Short Fantasy"},
		{"status=to-read&tag=fantasy&sort=title", "Long Fantasy,Short Fantasy"},
		{"q=%25", "Percent 100%"},
		{"q=%5F%5F", ""},
		{"q=percent", "Percent 100%"},
		// Unknown values are ignored.
		{"status=nope&owned=maybe&format=vhs&pages_min=abc", "Short Fantasy,Long Fantasy,Fan Fiction,Unknown Pages,Percent 100%,Plain"},
	} {
		got := strings.Join(titlesFor(t, a, c.query), ",")
		if got != c.want {
			t.Errorf("%q:\n got %s\nwant %s", c.query, got, c.want)
		}
	}
	// A book with two matching loans is listed and counted once.
	if got := titlesFor(t, a, "source=libby&loan_status=borrowed"); len(got) != 1 || got[0] != "Short Fantasy" {
		t.Fatal(got)
	}
	_, notes := parseListFilter(url.Values{"pages_min": {"0"}, "pages_max": {"100000"}, "owned": {"maybe"}, "source": {"x"}, "status": {"y"}, "sort": {"z"}})
	if len(notes) != 6 {
		t.Fatalf("notes %v", notes)
	}
}

func TestListFilterPaginationAndPositions(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	rs := records()
	for i := range 25 {
		r := records(fmt.Sprintf("Book %02d", i))[0]
		if i%2 == 0 {
			r.Tags = "even, all"
		} else {
			r.Tags = "odd, all"
		}
		rs = append(rs, r)
	}
	addRecords(t, a, rs)
	if _, err := a.store.DB.Exec("UPDATE books SET needs_placement=0; UPDATE books SET pinned=1 WHERE title='Book 02'"); err != nil {
		t.Fatal(err)
	}
	positions := func() string {
		var out []string
		rows, _ := a.store.DB.Query("SELECT title,position,pinned,needs_placement,reading_status FROM books ORDER BY id")
		defer rows.Close()
		for rows.Next() {
			var title, status string
			var pos, pin, needs int
			rows.Scan(&title, &pos, &pin, &needs, &status)
			out = append(out, fmt.Sprint(title, pos, pin, needs, status))
		}
		return strings.Join(out, ";")
	}
	before, rev := positions(), revisionOf(t, a)
	b := &browser{t: t, handler: a.Handler()}
	seen := map[string]bool{}
	next := "/?view=all&tag=even&per_page=5&sort=title"
	var total string
	for next != "" {
		rr := b.request("GET", next, nil)
		expect(t, rr, 200)
		for _, m := range regexp.MustCompile(`Book \d\d`).FindAllString(rr.Body.String(), -1) {
			seen[m] = true
		}
		if total == "" {
			total = regexp.MustCompile(`of (\d+)`).FindString(rr.Body.String())
		}
		link := regexp.MustCompile(`href="(/\?[^"]*)"[^>]*rel="next"`).FindStringSubmatch(rr.Body.String())
		next = ""
		if link != nil {
			next = strings.ReplaceAll(link[1], "&amp;", "&")
			if !strings.Contains(next, "tag=even") || !strings.Contains(next, "sort=title") {
				t.Fatalf("pagination lost a filter: %s", next)
			}
		}
	}
	if len(seen) != 13 {
		t.Fatalf("saw %d books across pages (%s)", len(seen), total)
	}
	for title := range seen {
		var tags string
		a.store.DB.QueryRow("SELECT tags FROM books WHERE title=?", title).Scan(&tags)
		if !strings.Contains(tags, "even") {
			t.Fatalf("%s is not even", title)
		}
	}
	if positions() != before || revisionOf(t, a) != rev {
		t.Fatal("filtering changed positions, pins, status, or the revision")
	}
	// A filtered, unsorted list numbers books by their true queue place and
	// keeps the move limits (Book 02 is pinned).
	f, _ := parseListFilter(url.Values{"tag": {"even"}, "per_page": {"5"}})
	items, err := listBooks(a.store.DB, f, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Title != "Book 00" || items[0].QueuePos != 1 || items[1].Title != "Book 02" || items[1].QueuePos != 3 || !items[0].CanDown || items[1].CanUp || items[1].CanDown {
		t.Fatalf("%+v %+v", items[0], items[1])
	}
	// A sorted list turns moves off.
	f.Sort = "title"
	items, _ = listBooks(a.store.DB, f, 5, 0)
	for _, it := range items {
		if it.CanUp || it.CanDown {
			t.Fatal("a sorted list allows moves")
		}
	}
}

func TestSavedViewLifecycle(t *testing.T) {
	t.Parallel()
	a := viewLibrary(t)
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.request("GET", "/?view=all", nil), 200)
	rev := revisionOf(t, a)
	save := func(name string, extra url.Values) *httpResponse {
		v := url.Values{"name": {name}, "owned": {"yes"}, "pages_max": {"250"}}
		for k, x := range extra {
			v[k] = x
		}
		rr := b.request("POST", "/views", v)
		return &httpResponse{rr.Code, rr.Header().Get("Location"), rr.Body.String()}
	}
	for name, v := range map[string]string{"empty": "  ", "long": strings.Repeat("n", maxViewName+1)} {
		if r := save(v, nil); r.Code != 400 {
			t.Fatalf("%s: %d", name, r.Code)
		}
	}
	if r := save("Name", url.Values{"owned": {"maybe"}}); r.Code != 400 {
		t.Fatal(r.Code)
	}
	created := save("  Owned,   under 250 pages ", nil)
	if created.Code != 303 || !strings.HasPrefix(created.Location, "/?") || !strings.Contains(created.Location, "saved=1") {
		t.Fatalf("%+v", created)
	}
	if r := save("owned, UNDER 250 pages", nil); r.Code != 409 {
		t.Fatalf("duplicate name: %d", r.Code)
	}
	if r := save("Élan", nil); r.Code != 303 {
		t.Fatal(r.Code)
	}
	if r := save("éLAN", nil); r.Code != 409 {
		t.Fatalf("non-ASCII duplicate: %d", r.Code)
	}
	views, _ := loadSavedViews(a.store.DB)
	if len(views) != 2 || views[0].Name != "Owned, under 250 pages" || views[0].Version != 1 || views[0].Definition.Owned != "yes" || *views[0].Definition.PagesMax != 250 {
		t.Fatalf("%+v", views)
	}
	id := views[0].ID

	// The stable address redirects to the full filter, and the page says
	// which view it is and that the filters match.
	rr := b.request("GET", fmt.Sprintf("/views/%d", id), nil)
	expect(t, rr, 303)
	page := b.request("GET", rr.Header().Get("Location"), nil).Body.String()
	if !strings.Contains(page, "Saved view: Owned, under 250 pages") || strings.Contains(page, "Filters changed") || !strings.Contains(page, "Short Fantasy") || strings.Contains(page, "Long Fantasy") {
		t.Fatal("saved view page wrong")
	}
	// A temporary change is flagged, and not saved until asked.
	changed := rr.Header().Get("Location") + "&tag=fantasy"
	page = b.request("GET", changed, nil).Body.String()
	if !strings.Contains(page, "Filters changed") || !strings.Contains(page, "Update this view") {
		t.Fatal("temporary filters not flagged")
	}
	if v, _ := getSavedView(a.store.DB, id); v.Definition.Tag != "" || v.Version != 1 {
		t.Fatal("viewing changed the view")
	}
	expect(t, b.request("GET", "/views/999", nil), 404)
	expect(t, b.request("GET", "/views", nil), 200)

	// Rename and update need the loaded version.
	path := fmt.Sprintf("/views/%d", id)
	expect(t, b.request("POST", path, url.Values{"action": {"rename"}, "name": {"Owned and short"}, "version": {"1"}}), 303)
	expect(t, b.request("POST", path, url.Values{"action": {"rename"}, "name": {"Stale"}, "version": {"1"}}), 409)
	expect(t, b.request("POST", path, url.Values{"action": {"rename"}, "name": {"élan"}, "version": {"2"}}), 409)
	expect(t, b.request("POST", path, url.Values{"action": {"update"}, "tag": {"fantasy"}, "owned": {"yes"}, "version": {"1"}}), 409)
	expect(t, b.request("POST", path, url.Values{"action": {"update"}, "tag": {"fantasy"}, "owned": {"yes"}, "version": {"2"}}), 303)
	v, _ := getSavedView(a.store.DB, id)
	if v.Name != "Owned and short" || v.Definition.Tag != "fantasy" || v.Definition.PagesMax != nil || v.Version != 3 {
		t.Fatalf("%+v", v)
	}
	// A view with no matches stays.
	expect(t, b.request("POST", path, url.Values{"action": {"update"}, "tag": {"nothing-like-this"}, "version": {"3"}}), 303)
	page = b.request("GET", "/views", nil).Body.String()
	if !strings.Contains(page, "No books match right now. The view is kept.") {
		t.Fatal("empty view not explained")
	}
	expect(t, b.request("POST", path, url.Values{"action": {"explode"}, "version": {"4"}}), 400)
	expect(t, b.request("POST", path, url.Values{"action": {"delete"}}), 400)
	expect(t, b.request("POST", path, url.Values{"action": {"delete"}, "version": {"4"}}), 303)
	expect(t, b.request("POST", path, url.Values{"action": {"delete"}, "version": {"4"}}), 409)
	expect(t, b.request("GET", path, nil), 404)
	if revisionOf(t, a) != rev {
		t.Fatal("saved views changed the library revision")
	}
	// The cap.
	for i := range maxSavedViews {
		r := save(fmt.Sprintf("View %d", i), nil)
		if i < maxSavedViews-1 && r.Code != 303 {
			t.Fatalf("view %d: %d", i, r.Code)
		}
	}
	if r := save("Overflow", nil); r.Code != 400 {
		t.Fatalf("over the cap: %d", r.Code)
	}
}

func TestOwnershipIsTheReadersOwn(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.form("/books/new", bookForm("Mine", "unreviewed", "owned", "1")), 303)
	if !bookNamed(t, a, "Mine").Owned {
		t.Fatal("the add form did not set ownership")
	}
	id := bookNamed(t, a, "Mine").ID
	// Imports and library events neither infer nor clear it.
	addRecords(t, a, []Record{libbyEvent("Mine", "returned", "2026-10-01T00:00:00.000000000+00:00"), {Book: Book{Title: "Mine", Author: "Author", Status: "to-read", Source: "goodreads", Interest: 4, Tags: "x"}}, {Book: Book{Title: "Theirs", Author: "Author", Status: "to-read", Source: "goodreads", Interest: 4}}})
	expect(t, b.form(fmt.Sprintf("/books/%d/loan", id), url.Values{"source": {"libby"}, "format": {"ebook"}, "status": {"returned"}}), 303)
	if !bookNamed(t, a, "Mine").Owned || bookNamed(t, a, "Theirs").Owned {
		t.Fatal("an import or loan changed ownership")
	}
	if !strings.Contains(b.request("GET", fmt.Sprintf("/books/%d", id), nil).Body.String(), `class="badge">Owned`) {
		t.Fatal("the book page does not show ownership")
	}
	// Unchecking is the reader's choice.
	expect(t, b.form(fmt.Sprintf("/books/%d/edit", id), bookForm("Mine", "unreviewed")), 303)
	if bookNamed(t, a, "Mine").Owned {
		t.Fatal("ownership stayed after unchecking")
	}
	// A merge keeps ownership if either book is owned.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.store.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE books SET owned=1 WHERE title='Theirs'")
	ids := bookIDs(t, a)
	rr := b.request("GET", fmt.Sprintf("/books/%d/merge?target=%d", ids["Theirs"], ids["Mine"]), nil)
	if !strings.Contains(rr.Body.String(), "becomes owned too") {
		t.Fatal("merge preview does not say so")
	}
	if err := a.store.Write(func(tx *sql.Tx) error { _, err := mergeBooks(tx, ids["Theirs"], ids["Mine"], true); return err }); err != nil {
		t.Fatal(err)
	}
	if !bookNamed(t, a, "Mine").Owned {
		t.Fatal("the merge dropped ownership")
	}
}

func TestSavedViewsExportRestore(t *testing.T) {
	t.Parallel()
	source := viewLibrary(t)
	b := &browser{t: t, handler: source.Handler()}
	expect(t, b.request("GET", "/?view=all", nil), 200)
	expect(t, b.request("POST", "/views", url.Values{"name": {"Owned"}, "owned": {"yes"}, "sort": {"title"}}), 303)
	expect(t, b.request("POST", "/views", url.Values{"name": {"Borrowed ebooks"}, "source": {"libby"}, "format": {"ebook"}, "pages_unknown": {"1"}, "tag": {"fantasy"}, "series": {"Élan Saga"}}), 303)
	original := exportOf(t, source)
	var f exportFile
	if err := json.Unmarshal(original, &f); err != nil || len(f.SavedViews) != 2 || f.SchemaVersion != maxRestoreSchema {
		t.Fatal(err, len(f.SavedViews), f.SchemaVersion)
	}
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	expect(t, tb.request("GET", "/imports", nil), 200)
	rr := uploadRestore(tb, original)
	expect(t, rr, 303)
	plan := tb.request("GET", rr.Header().Get("Location"), nil).Body.String()
	if !strings.Contains(plan, "2 saved views come along") {
		t.Fatal("preview does not mention saved views")
	}
	expect(t, tb.request("POST", rr.Header().Get("Location"), nil), 303)
	if !equalJSON(normalizedExport(t, exportOf(t, target)), normalizedExport(t, original)) {
		t.Fatal("saved views or ownership did not round trip")
	}
	if !bookNamed(t, target, "Short Fantasy").Owned || bookNamed(t, target, "Long Fantasy").Owned {
		t.Fatal("ownership lost")
	}
	// Older exports restore with safe defaults.
	var raw map[string]any
	json.Unmarshal(original, &raw)
	delete(raw, "saved_views")
	raw["schema_version"] = 13
	for _, book := range raw["books"].([]any) {
		delete(book.(map[string]any), "owned")
	}
	older, _ := json.Marshal(raw)
	fresh := testApp(t, true)
	fb := &browser{t: t, handler: fresh.Handler()}
	expect(t, fb.request("GET", "/imports", nil), 200)
	rr = uploadRestore(fb, older)
	expect(t, rr, 303)
	expect(t, fb.request("POST", rr.Header().Get("Location"), nil), 303)
	if views, _ := loadSavedViews(fresh.store.DB); len(views) != 0 || bookNamed(t, fresh, "Short Fantasy").Owned {
		t.Fatal("older export got views or ownership")
	}
	// Invalid definitions and names are refused, and nothing is written.
	for name, edit := range map[string]func(map[string]any){
		"unknown filter value": func(m map[string]any) { view(m, 0)["definition"].(map[string]any)["owned"] = "maybe" },
		"bad version":          func(m map[string]any) { view(m, 0)["definition"].(map[string]any)["v"] = 9 },
		"negative pages":       func(m map[string]any) { view(m, 0)["definition"].(map[string]any)["pages_min"] = -4 },
		"empty name":           func(m map[string]any) { view(m, 0)["name"] = "" },
		"repeated name":        func(m map[string]any) { view(m, 1)["name"] = "borrowed EBOOKS" },
		"bad date":             func(m map[string]any) { view(m, 0)["created_at"] = "x" },
		"unknown field":        func(m map[string]any) { view(m, 0)["definition"].(map[string]any)["sql"] = "1=1" },
	} {
		var m map[string]any
		json.Unmarshal(original, &m)
		edit(m)
		data, _ := json.Marshal(m)
		other := testApp(t, true)
		ob := &browser{t: t, handler: other.Handler()}
		expect(t, ob.request("GET", "/imports", nil), 200)
		if rr := uploadRestore(ob, data); rr.Code != 400 {
			t.Fatalf("%s: status %d", name, rr.Code)
		}
		if countRows(t, other)["books"] != 0 {
			t.Fatalf("%s wrote books", name)
		}
	}
}

func view(m map[string]any, i int) map[string]any {
	return m["saved_views"].([]any)[i].(map[string]any)
}

func TestMigrateSavedViewsFromVersionThirteen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lib.sqlite3")
	if err := migrateFile(path, migrations[:13]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source) VALUES(1,'Old','A','old|a',0,'x','manual')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if v, _ := versionOf(t, path); v != 14 {
		t.Fatal(v)
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM books WHERE owned=0 AND title='Old'"); n != 1 {
		t.Fatal("existing books changed")
	}
	if n := scalar[int](t, path, "SELECT count(*) FROM saved_views"); n != 0 {
		t.Fatal(n)
	}
	if b := backups(t, path); len(b) != 1 || b[0] != path+".pre-v14.bak" {
		t.Fatalf("backups %v", b)
	}
}
