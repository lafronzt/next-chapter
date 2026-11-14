package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Book lookup asks global book databases for facts about one book. It is plain
// HTTPS and JSON, with no AI: nothing here decides what is true. The reader
// starts every lookup, and only a title, an author, and an ISBN leave the app.
// Whatever comes back is untrusted text (see cleanText) and only ever fills
// fields that are empty.

const (
	maxDescription   = 2000
	lookupTimeout    = 8 * time.Second
	lookupBodyLimit  = 1 << 20
	lookupMaxTags    = 6
	openLibraryHost  = "https://openlibrary.org"
	googleBooksHost  = "https://www.googleapis.com"
	isbndbHost       = "https://api2.isbndb.com"
	lookupBatchLimit = 25 // books per "look up these books" click
)

// LookupConfig holds the optional operator keys. Open Library needs none, and
// the app works fully without any of these.
type LookupConfig struct {
	GoogleBooksKey, ISBNdbKey string
}

// LoadLookupConfig reads GOOGLE_BOOKS_API_KEY and ISBNDB_API_KEY.
func LoadLookupConfig(getenv func(string) string) LookupConfig {
	return LookupConfig{GoogleBooksKey: strings.TrimSpace(getenv("GOOGLE_BOOKS_API_KEY")), ISBNdbKey: strings.TrimSpace(getenv("ISBNDB_API_KEY"))}
}

// LookupQuery is everything a lookup sends: title, author, and ISBN.
type LookupQuery struct {
	Title  string `json:"title"`
	Author string `json:"author"`
	ISBN   string `json:"isbn,omitempty"`
}

// sent lists the fields that leave the app for this query, for the preview.
func (q LookupQuery) sent() string {
	if q.ISBN != "" {
		return "title, author, and ISBN"
	}
	return "title and author"
}

// BookDetails is what a lookup found. Zero values mean "not found".
type BookDetails struct {
	Description       string   `json:"description,omitempty"`
	DescriptionSource string   `json:"description_source,omitempty"`
	Pages             *int     `json:"pages,omitempty"`
	PublishedYear     *int     `json:"published_year,omitempty"`
	Series            string   `json:"series,omitempty"`
	SeriesNumber      *float64 `json:"series_number,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	// Sources names every database that had the book.
	Sources []string `json:"sources,omitempty"`
}

func (d BookDetails) empty() bool {
	return d.Description == "" && d.Pages == nil && d.PublishedYear == nil && d.Series == "" && len(d.Tags) == 0
}

// BookSource is one global book database.
type BookSource interface {
	Name() string
	Lookup(ctx context.Context, q LookupQuery) (BookDetails, error)
}

var (
	errLookupNotFound  = errors.New("no match")
	errLookupAmbiguous = errors.New("more than one book matches")
)

// lookupHTTP is the one small client every source uses: fixed hosts, a short
// timeout, no redirects, a size limit, and an honest User-Agent.
type lookupHTTP struct {
	client *http.Client
	base   string
	header http.Header
}

func newLookupHTTP(client *http.Client, base string) lookupHTTP {
	c := http.Client{Timeout: lookupTimeout}
	if client != nil {
		c = *client
		if c.Timeout == 0 {
			c.Timeout = lookupTimeout
		}
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return lookupHTTP{client: &c, base: strings.TrimRight(base, "/")}
}

func (h lookupHTTP) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	target := h.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "NextChapter/"+Version+" (self-hosted reading queue)")
	req.Header.Set("Accept", "application/json")
	for k, v := range h.header {
		req.Header[k] = v
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return errors.New("the request failed")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errLookupNotFound
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return errors.New("the database redirected the request, which is not followed")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("the database answered with status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, lookupBodyLimit+1))
	if err != nil {
		return errors.New("the response could not be read")
	}
	if len(data) > lookupBodyLimit {
		return errors.New("the response was too large")
	}
	if err = json.Unmarshal(data, out); err != nil {
		return errors.New("the response was not valid JSON")
	}
	return nil
}

var (
	htmlTagPattern   = regexp.MustCompile(`<[^>]*>`)
	htmlBlockPattern = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	spacePattern     = regexp.MustCompile(`[ \t\f\v]+`)
	blankLines       = regexp.MustCompile(`\n{3,}`)
	bookYearPattern  = regexp.MustCompile(`\b(1[0-9]{3}|20[0-9]{2})\b`)
)

// cleanText makes text from a database or a form safe to store: plain text
// only (markup stripped, entities decoded, control characters dropped),
// tidy whitespace, and at most limit characters. It is still rendered escaped
// and sent only inside <library_data>.
func cleanText(s string, limit int) string {
	s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n\n", "</P>", "\n\n").Replace(s)
	s = htmlBlockPattern.ReplaceAllString(s, "")
	s = htmlTagPattern.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = htmlTagPattern.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r':
			return -1
		case r == '\n':
			return r
		case unicode.IsControl(r) || r == ' ' || r == ' ':
			return ' '
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(spacePattern.ReplaceAllString(line, " "))
	}
	s = blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(truncate(strings.TrimSpace(s), limit))
}

// cleanTag makes one subject into a short, comma-free, lowercase tag, or "".
func cleanTag(s string) string {
	s = strings.ToLower(cleanText(s, 200))
	s = strings.Join(strings.Fields(strings.NewReplacer(",", " ", "\n", " ").Replace(s)), " ")
	if s == "" || len([]rune(s)) > 40 || strings.Contains(s, ":") {
		return ""
	}
	for _, junk := range []string{"open library", "in library", "accessible book", "protected daisy", "overdrive", "large type", "lending library"} {
		if strings.Contains(s, junk) {
			return ""
		}
	}
	return s
}

func addTags(tags []string, more ...string) []string {
	for _, t := range more {
		if t = cleanTag(t); t == "" || t == "fiction" || t == "books" || t == "general" || len(tags) >= lookupMaxTags {
			continue
		}
		dup := false
		for _, have := range tags {
			dup = dup || have == t
		}
		if !dup {
			tags = append(tags, t)
		}
	}
	return tags
}

func yearIn(s string) *int {
	if m := bookYearPattern.FindString(s); m != "" {
		n, _ := strconv.Atoi(m)
		return &n
	}
	return nil
}

func positive(n int) *int {
	if n < 1 || n > 99999 {
		return nil
	}
	return &n
}

func titleCase(s string) string {
	words := strings.Fields(strings.ReplaceAll(s, "_", " "))
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// sameBook reports whether a database's title and authors are the query's.
func sameBook(q LookupQuery, title string, authors []string) bool {
	if normalized(title) != normalized(q.Title) {
		return false
	}
	if normalized(q.Author) == "" {
		return true
	}
	want := normalized(q.Author)
	for _, a := range authors {
		if got := normalized(a); got != "" && (got == want || strings.Contains(got, want) || strings.Contains(want, got)) {
			return true
		}
	}
	return false
}

// --- Open Library ---

type openLibrary struct{ http lookupHTTP }

func (openLibrary) Name() string { return "Open Library" }

var workKeyPattern = regexp.MustCompile(`^/works/OL[0-9]+W$`)

type olText struct{ text string }

// UnmarshalJSON reads a description given as a string or as {"value": "…"}.
func (t *olText) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		t.text = s
		return nil
	}
	var v struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(data, &v) == nil {
		t.text = v.Value
	}
	return nil
}

func (s openLibrary) Lookup(ctx context.Context, q LookupQuery) (BookDetails, error) {
	var d BookDetails
	workKey := ""
	if q.ISBN != "" {
		// /isbn/{isbn}.json redirects, and redirects are not followed, so use
		// the lookup endpoint that answers directly.
		var res map[string]struct {
			Details struct {
				Works []struct {
					Key string `json:"key"`
				} `json:"works"`
				Pages       int    `json:"number_of_pages"`
				PublishDate string `json:"publish_date"`
			} `json:"details"`
		}
		key := "ISBN:" + q.ISBN
		err := s.http.getJSON(ctx, "/api/books", url.Values{"bibkeys": {key}, "format": {"json"}, "jscmd": {"details"}}, &res)
		if err != nil {
			return d, err
		}
		entry, ok := res[key]
		if !ok {
			return d, errLookupNotFound
		}
		d.Pages, d.PublishedYear = positive(entry.Details.Pages), yearIn(entry.Details.PublishDate)
		if len(entry.Details.Works) > 0 {
			workKey = entry.Details.Works[0].Key
		}
	} else {
		var res struct {
			Docs []struct {
				Key       string   `json:"key"`
				Title     string   `json:"title"`
				Authors   []string `json:"author_name"`
				FirstYear int      `json:"first_publish_year"`
				Pages     int      `json:"number_of_pages_median"`
				Subjects  []string `json:"subject"`
				Editions  int      `json:"edition_count"`
			} `json:"docs"`
		}
		query := url.Values{"title": {q.Title}, "limit": {"10"}, "fields": {"key,title,author_name,first_publish_year,number_of_pages_median,subject,edition_count"}}
		if q.Author != "" {
			query.Set("author", q.Author)
		}
		if err := s.http.getJSON(ctx, "/search.json", query, &res); err != nil {
			return d, err
		}
		// Adaptations and sequels carry the author second; the book itself has
		// the author first. Several works can remain (a reissue, a graphic
		// novel): one with far more editions than the rest is the book.
		matches := []int{}
		for i, doc := range res.Docs {
			if sameBook(q, doc.Title, doc.Authors[:min(1, len(doc.Authors))]) {
				matches = append(matches, i)
			}
		}
		slices.SortStableFunc(matches, func(a, b int) int { return res.Docs[b].Editions - res.Docs[a].Editions })
		switch {
		case len(matches) == 0:
			return d, errLookupNotFound
		case len(matches) > 1 && res.Docs[matches[0]].Editions < 5*max(res.Docs[matches[1]].Editions, 1):
			return d, errLookupAmbiguous
		}
		first := matches[0]
		doc := res.Docs[first]
		d.Pages, d.PublishedYear, workKey = positive(doc.Pages), positive(doc.FirstYear), doc.Key
		if d.PublishedYear != nil && *d.PublishedYear > 2200 {
			d.PublishedYear = nil
		}
		d.Tags = addTags(d.Tags, doc.Subjects...)
	}
	if !workKeyPattern.MatchString(workKey) {
		if d.empty() {
			return d, errLookupNotFound
		}
		return d, nil
	}
	var work struct {
		Description  olText   `json:"description"`
		Subjects     []string `json:"subjects"`
		FirstPublish string   `json:"first_publish_date"`
	}
	if err := s.http.getJSON(ctx, workKey+".json", nil, &work); err != nil {
		if errors.Is(err, errLookupNotFound) {
			return d, nil
		}
		return d, err
	}
	d.Description = cleanText(work.Description.text, maxDescription)
	if y := yearIn(work.FirstPublish); y != nil {
		d.PublishedYear = y
	}
	for _, subject := range work.Subjects {
		if name, ok := strings.CutPrefix(strings.ToLower(subject), "series:"); ok && d.Series == "" {
			d.Series = truncate(titleCase(cleanText(name, 200)), 200)
		}
	}
	d.Tags = addTags(d.Tags, work.Subjects...)
	if d.empty() {
		return d, errLookupNotFound
	}
	return d, nil
}

// --- Google Books ---

type googleBooks struct {
	http lookupHTTP
	key  string
}

func (googleBooks) Name() string { return "Google Books" }

func (s googleBooks) Lookup(ctx context.Context, q LookupQuery) (BookDetails, error) {
	var d BookDetails
	query := url.Values{"maxResults": {"10"}, "printType": {"books"}}
	if q.ISBN != "" {
		query.Set("q", "isbn:"+q.ISBN)
	} else {
		term := `intitle:"` + strings.ReplaceAll(q.Title, `"`, "") + `"`
		if q.Author != "" {
			term += ` inauthor:"` + strings.ReplaceAll(q.Author, `"`, "") + `"`
		}
		query.Set("q", term)
	}
	if s.key != "" {
		query.Set("key", s.key)
	}
	var res struct {
		Items []struct {
			VolumeInfo struct {
				Title       string   `json:"title"`
				Authors     []string `json:"authors"`
				Description string   `json:"description"`
				PageCount   int      `json:"pageCount"`
				Categories  []string `json:"categories"`
				Published   string   `json:"publishedDate"`
			} `json:"volumeInfo"`
		} `json:"items"`
	}
	if err := s.http.getJSON(ctx, "/books/v1/volumes", query, &res); err != nil {
		return d, err
	}
	found := false
	for i, item := range res.Items {
		v := item.VolumeInfo
		// An ISBN names one edition; a search must agree on title and author.
		// Editions of the same book then fill gaps in the order Google ranks them.
		if q.ISBN != "" && i > 0 || q.ISBN == "" && !sameBook(q, v.Title, v.Authors) {
			continue
		}
		found = true
		if d.Description == "" {
			d.Description = cleanText(v.Description, maxDescription)
		}
		if d.Pages == nil {
			d.Pages = positive(v.PageCount)
		}
		if y := yearIn(v.Published); y != nil && (d.PublishedYear == nil || *y < *d.PublishedYear) {
			d.PublishedYear = y
		}
		for _, c := range v.Categories {
			d.Tags = addTags(d.Tags, strings.Split(c, "/")...)
		}
	}
	if !found || d.empty() {
		return d, errLookupNotFound
	}
	return d, nil
}

// --- ISBNdb (only when the operator sets a key) ---

type isbndb struct{ http lookupHTTP }

func (isbndb) Name() string { return "ISBNdb" }

func (s isbndb) Lookup(ctx context.Context, q LookupQuery) (BookDetails, error) {
	var d BookDetails
	if q.ISBN == "" {
		return d, errLookupNotFound
	}
	var res struct {
		Book struct {
			Synopsis  string   `json:"synopsis"`
			Overview  string   `json:"overview"`
			Pages     int      `json:"pages"`
			Published string   `json:"date_published"`
			Subjects  []string `json:"subjects"`
		} `json:"book"`
	}
	if err := s.http.getJSON(ctx, "/book/"+q.ISBN, nil, &res); err != nil {
		return d, err
	}
	b := res.Book
	if b.Synopsis != "" {
		d.Description = cleanText(b.Synopsis, maxDescription)
	} else {
		d.Description = cleanText(b.Overview, maxDescription)
	}
	d.Pages, d.PublishedYear = positive(b.Pages), yearIn(b.Published)
	d.Tags = addTags(nil, b.Subjects...)
	if d.empty() {
		return d, errLookupNotFound
	}
	return d, nil
}

// --- the finder ---

// Finder asks each source in order and merges what they found.
type Finder struct{ sources []BookSource }

// lookupHosts are the base URLs of the databases. Production uses the
// defaults; tests point them at httptest servers.
type lookupHosts struct{ OpenLibrary, GoogleBooks, ISBNdb string }

func newFinder(cfg LookupConfig, hosts lookupHosts, client *http.Client) *Finder {
	pick := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	f := &Finder{sources: []BookSource{
		openLibrary{newLookupHTTP(client, pick(hosts.OpenLibrary, openLibraryHost))},
		googleBooks{newLookupHTTP(client, pick(hosts.GoogleBooks, googleBooksHost)), cfg.GoogleBooksKey},
	}}
	if cfg.ISBNdbKey != "" {
		h := newLookupHTTP(client, pick(hosts.ISBNdb, isbndbHost))
		h.header = http.Header{"Authorization": {cfg.ISBNdbKey}}
		f.sources = append(f.sources, isbndb{h})
	}
	return f
}

// Find looks the book up everywhere. It reports errLookupAmbiguous or
// errLookupNotFound when no source found it, and another error when a source
// failed and none found it.
func (f *Finder) Find(ctx context.Context, q LookupQuery) (BookDetails, error) {
	q.Title, q.Author, q.ISBN = strings.TrimSpace(q.Title), strings.TrimSpace(q.Author), cleanISBN(q.ISBN)
	if q.Title == "" {
		return BookDetails{}, errLookupNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 3*lookupTimeout)
	defer cancel()
	var found []BookDetails
	var names []string
	var ambiguous bool
	var failure error
	for _, s := range f.sources {
		d, err := s.Lookup(ctx, q)
		if errors.Is(err, errLookupNotFound) && q.ISBN != "" {
			// The ISBN may be unknown to this database; the title may not be.
			byTitle := q
			byTitle.ISBN = ""
			d, err = s.Lookup(ctx, byTitle)
		}
		switch {
		case err == nil:
			d.DescriptionSource = ""
			if d.Description != "" {
				d.DescriptionSource = s.Name()
			}
			found, names = append(found, d), append(names, s.Name())
		case errors.Is(err, errLookupAmbiguous):
			ambiguous = true
		case errors.Is(err, errLookupNotFound):
		default:
			failure = fmt.Errorf("%s: %w", s.Name(), err)
		}
	}
	if len(found) > 0 {
		merged := mergeDetails(found)
		merged.Sources = names
		return merged, nil
	}
	switch {
	case failure != nil:
		return BookDetails{}, failure
	case ambiguous:
		return BookDetails{}, errLookupAmbiguous
	}
	return BookDetails{}, errLookupNotFound
}

// mergeDetails combines results in source order. The description, series, and
// tags come from the first source that has them; pages and year take the value
// most sources agree on, the earliest source winning a tie.
func mergeDetails(results []BookDetails) BookDetails {
	var m BookDetails
	for _, d := range results {
		if m.Description == "" && d.Description != "" {
			m.Description, m.DescriptionSource = d.Description, d.DescriptionSource
		}
		if m.Series == "" {
			m.Series, m.SeriesNumber = d.Series, d.SeriesNumber
		}
		m.Tags = addTags(m.Tags, d.Tags...)
	}
	m.Pages, m.PublishedYear = agreed(results, func(d BookDetails) *int { return d.Pages }), agreed(results, func(d BookDetails) *int { return d.PublishedYear })
	return m
}

func agreed(results []BookDetails, get func(BookDetails) *int) *int {
	counts := map[int]int{}
	var order []int
	for _, d := range results {
		if v := get(d); v != nil {
			if counts[*v] == 0 {
				order = append(order, *v)
			}
			counts[*v]++
		}
	}
	best, bestCount := 0, 0
	for _, v := range order {
		if counts[v] > bestCount {
			best, bestCount = v, counts[v]
		}
	}
	if bestCount == 0 {
		return nil
	}
	return &best
}

// lookupMessage explains a failed lookup to the reader.
func lookupMessage(err error) string {
	switch {
	case errors.Is(err, errLookupNotFound):
		return "No book database had this book. Check the title and author, or add an ISBN."
	case errors.Is(err, errLookupAmbiguous):
		return "More than one book matches this title and author. Add an ISBN to the book and try again."
	}
	return "The book database could not be reached (" + err.Error() + "). Try again later."
}
