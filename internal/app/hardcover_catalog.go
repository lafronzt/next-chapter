package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Hardcover's catalog is community-curated data, used at your own risk. It is
// read with the read:catalog scope and carries nothing about any reader. It
// fills empty fields in a lookup the reader started, shows the complete
// series, and (when switched on) a cover and a community average. Ranking, the
// AI estimate, and reasons never read any of it.

// hardcoverImageHost is the one host an image may be loaded from, and the one
// host the Content-Security-Policy allows when covers are switched on.
const hardcoverImageHost = "assets.hardcover.app"

const hcCatalogFields = `id title slug description pages release_year rating ratings_count cached_image
  contributions { author { name } }
  book_series(where: {series: {canonical_id: {_is_null: true}}}) {
   position
   series { id name books_count
    book_series(where: {book: {book_status_id: {_eq: 1}}}, order_by: [{position: asc_nulls_last}, {book: {users_count: desc}}, {id: asc}], limit: 100) {
     position
     book { id title release_year pages contributions { author { name } } }
    }
   }
  }`

const hcCatalogByIDQuery = `query NextChapterCatalog($id: Int!) {
 books_by_pk(id: $id) {
  ` + hcCatalogFields + `
 }
}`

func hcCatalogByISBNQuery(isbn string) string {
	field := "isbn_13"
	if len(isbn) == 10 {
		field = "isbn_10"
	}
	return `query NextChapterCatalogISBN($isbn: String!) {
 editions(where: {` + field + `: {_eq: $isbn}}, limit: 3) {
  book {
   ` + hcCatalogFields + `
  }
 }
}`
}

// flexFloat reads a number sent as a number or a string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return err
	}
	*f = flexFloat(n)
	return nil
}

type hcSeriesJSON struct {
	ID      flexInt `json:"id"`
	Name    string  `json:"name"`
	Count   flexInt `json:"books_count"`
	Members []struct {
		Position *flexFloat  `json:"position"`
		Book     *hcBookJSON `json:"book"`
	} `json:"book_series"`
}

type hcCatalogBook struct {
	ID            flexInt         `json:"id"`
	Title         string          `json:"title"`
	Slug          string          `json:"slug"`
	Description   string          `json:"description"`
	Pages         flexInt         `json:"pages"`
	ReleaseYear   flexInt         `json:"release_year"`
	Rating        *flexFloat      `json:"rating"`
	RatingsCount  flexInt         `json:"ratings_count"`
	Image         json.RawMessage `json:"cached_image"`
	Contributions []struct {
		Author struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"contributions"`
	BookSeries []struct {
		Position *flexFloat    `json:"position"`
		Series   *hcSeriesJSON `json:"series"`
	} `json:"book_series"`
}

// imageURL is the cover URL, only when it is an https URL on the one allowed
// host; anything else is dropped, so a stored URL is always safe to load.
func (b hcCatalogBook) imageURL() string {
	var img struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(b.Image, &img) != nil {
		return ""
	}
	return hcImageURL(img.URL)
}

var hcImagePath = regexp.MustCompile(`^/[A-Za-z0-9/_.,%~+=@:-]{1,300}$`)

func hcImageURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 400 || u.Scheme != "https" || u.Host != hardcoverImageHost || u.User != nil || u.Fragment != "" || u.RawQuery != "" || !hcImagePath.MatchString(u.EscapedPath()) || strings.Contains(u.Path, "..") {
		return ""
	}
	return u.String()
}

func position(p *flexFloat) *float64 {
	if p == nil || *p < 0 || *p > 100000 {
		return nil
	}
	v := float64(*p)
	return &v
}

// details turns a catalog book into what a lookup can fill. Text is cleaned
// and capped here, like every other source's.
func (b hcCatalogBook) details() BookDetails {
	d := BookDetails{Description: cleanText(b.Description, maxDescription)}
	if b.Pages > 0 && b.Pages < 100000 {
		d.Pages = positive(int(b.Pages))
	}
	if y := int(b.ReleaseYear); y >= 1000 && y <= 2200 {
		d.PublishedYear = &y
	}
	for _, s := range b.BookSeries {
		if s.Series != nil && cleanText(s.Series.Name, 200) != "" {
			d.Series, d.SeriesNumber = cleanText(s.Series.Name, 200), position(s.Position)
			break
		}
	}
	return d
}

// ---- fetching ----

func (c *hardcoverClient) catalogByID(ctx context.Context, id int64) (*hcCatalogBook, error) {
	var out struct {
		Book *hcCatalogBook `json:"books_by_pk"`
	}
	if err := c.do(ctx, hcCatalogByIDQuery, map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	return out.Book, nil
}

// catalogByISBN returns the one book an ISBN names; two different books are
// ambiguous and none is not found.
func (c *hardcoverClient) catalogByISBN(ctx context.Context, isbn string) (*hcCatalogBook, error) {
	var out struct {
		Editions []struct {
			Book *hcCatalogBook `json:"book"`
		} `json:"editions"`
	}
	if err := c.do(ctx, hcCatalogByISBNQuery(isbn), map[string]any{"isbn": isbn}, &out); err != nil {
		return nil, err
	}
	var found *hcCatalogBook
	for _, e := range out.Editions {
		switch {
		case e.Book == nil || e.Book.ID < 1:
		case found == nil:
			found = e.Book
		case found.ID != e.Book.ID:
			return nil, errLookupAmbiguous
		}
	}
	return found, nil
}

// hardcoverSource is Hardcover as a BookSource. With the reader's linked id it
// asks for exactly that book; without one it tries the ISBN, then a title and
// author search that must name one book. Calls share the client's budget and
// the app's Hardcover lock.
type hardcoverSource struct {
	c  *hardcoverClient
	mu *sync.Mutex
	id int64
}

func (hardcoverSource) Name() string { return "Hardcover" }

func (s hardcoverSource) Lookup(ctx context.Context, q LookupQuery) (BookDetails, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var book *hcCatalogBook
	var err error
	switch {
	case s.id > 0:
		book, err = s.c.catalogByID(ctx, s.id)
	case q.ISBN != "":
		if book, err = s.c.catalogByISBN(ctx, q.ISBN); err == nil && book == nil {
			book, err = s.byText(ctx, q)
		}
	default:
		book, err = s.byText(ctx, q)
	}
	if err != nil {
		return BookDetails{}, err
	}
	if book == nil || book.ID < 1 {
		return BookDetails{}, errLookupNotFound
	}
	d := book.details()
	if d.empty() {
		return BookDetails{}, errLookupNotFound
	}
	return d, nil
}

// byText searches by title and author and accepts only one exact match.
func (s hardcoverSource) byText(ctx context.Context, q LookupQuery) (*hcCatalogBook, error) {
	cands, err := s.c.search(ctx, strings.TrimSpace(q.Title+" "+q.Author))
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, c := range cands {
		if sameBook(q, c.Title, []string{c.Author}) && !slices.Contains(ids, c.BookID) {
			ids = append(ids, c.BookID)
		}
	}
	switch len(ids) {
	case 0:
		return nil, nil
	case 1:
		return s.c.catalogByID(ctx, ids[0])
	}
	return nil, errLookupAmbiguous
}

// hardcoverLookupSource returns Hardcover as a source for one book's lookup, or
// nil when Hardcover is off, not configured, or the book is not allowed. A
// linked book is asked by its id and goes first, since an exact id beats a
// text search; otherwise Hardcover goes last.
func (a *App) hardcoverLookupSource(bookID int64) (src BookSource, first bool) {
	if a.hardcoverReady() != nil {
		return nil, false
	}
	l, err := getBookLink(a.store.DB, bookID)
	if err != nil {
		return nil, false
	}
	s := hardcoverSource{c: a.hardcover, mu: &a.hardcoverMu}
	if l != nil {
		s.id = l.HardcoverBookID
	}
	return s, l != nil
}

// ---- the local cache ----

// hcCachedCatalog is what the last refresh stored for one book.
type hcCachedCatalog struct {
	Rating       *float64
	RatingsCount int
	ImageURL     string
	FetchedAt    string
}

type hcSeriesMember struct {
	HardcoverBookID int64
	Position        *float64
	Title, Author   string
	Year, Pages     int
}

// storeCatalog replaces everything cached for a linked book. It is a cache of
// remote data: dropping it loses nothing, and it is rebuilt on request.
func storeCatalog(q queryer, bookID int64, b hcCatalogBook, at string) error {
	if err := dropCatalog(q, bookID); err != nil {
		return err
	}
	var rating *float64
	if b.Rating != nil && *b.Rating > 0 && *b.Rating <= 5 {
		v := math.Round(float64(*b.Rating)*100) / 100
		rating = &v
	}
	if _, err := q.Exec("INSERT INTO hardcover_catalog(book_id,rating,ratings_count,image_url,fetched_at) VALUES(?,?,?,?,?)", bookID, rating, max(0, int(b.RatingsCount)), b.imageURL(), at); err != nil {
		return err
	}
	for _, bs := range b.BookSeries {
		s := bs.Series
		if s == nil || s.ID < 1 || cleanText(s.Name, 200) == "" {
			continue
		}
		seen := map[int64]bool{}
		for _, m := range s.Members {
			if m.Book == nil || m.Book.ID < 1 || seen[int64(m.Book.ID)] || cleanText(m.Book.Title, 300) == "" {
				continue
			}
			seen[int64(m.Book.ID)] = true
			c := m.Book.candidate(0)
			if _, err := q.Exec(`INSERT INTO series_cache(source_book_id,series_id,series_name,series_total,hardcover_book_id,position,title,author,year,pages,fetched_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				bookID, int64(s.ID), cleanText(s.Name, 200), max(0, int(s.Count)), c.BookID, position(m.Position), cleanText(c.Title, 300), cleanText(c.Author, 300), positive(c.Year), positive(c.Pages), at); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropCatalog removes a book's cached Hardcover data.
func dropCatalog(q queryer, bookID int64) error {
	if _, err := q.Exec("DELETE FROM hardcover_catalog WHERE book_id=?", bookID); err != nil {
		return err
	}
	_, err := q.Exec("DELETE FROM series_cache WHERE source_book_id=?", bookID)
	return err
}

func loadCatalog(q queryer, bookID int64) (*hcCachedCatalog, error) {
	var c hcCachedCatalog
	var rating sql.NullFloat64
	err := q.QueryRow("SELECT rating,ratings_count,image_url,fetched_at FROM hardcover_catalog WHERE book_id=?", bookID).Scan(&rating, &c.RatingsCount, &c.ImageURL, &c.FetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if rating.Valid {
		c.Rating = &rating.Float64
	}
	// Only an address on the allowed host is ever handed to a page.
	c.ImageURL = hcImageURL(c.ImageURL)
	return &c, err
}

// seriesCatalog is the cached Hardcover members of one local series that the
// reader does not have.
type seriesCatalog struct {
	SourceBookID int64
	SeriesID     int64
	Name         string
	FetchedAt    string
	Total        int
	Missing      []hcSeriesMember
	// NextMissing is the first numbered volume the reader lacks that comes
	// after every volume they have, or nil.
	NextMissing *hcSeriesMember
	Truncated   bool
}

// catalogFor finds the cached members of the Hardcover series whose name is
// the local group's, from a linked book of that group, and drops those the
// reader already has (by link or by the same title and author) or deleted.
// It reads only the cache: no request.
func catalogFor(q queryer, g seriesGroup) (*seriesCatalog, error) {
	rows, err := q.Query(`SELECT sc.source_book_id,sc.series_id,sc.series_name,sc.series_total,sc.hardcover_book_id,sc.position,sc.title,sc.author,COALESCE(sc.year,0),COALESCE(sc.pages,0),sc.fetched_at
 FROM series_cache sc JOIN books b ON b.id=sc.source_book_id WHERE lower(trim(b.series))=lower(trim(?)) AND lower(trim(sc.series_name))=lower(trim(?))
 ORDER BY sc.series_id, sc.position IS NULL, sc.position, sc.hardcover_book_id`, g.Name, g.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cat *seriesCatalog
	var all []hcSeriesMember
	for rows.Next() {
		var src, sid int64
		var name, at string
		var total int
		var m hcSeriesMember
		if err = rows.Scan(&src, &sid, &name, &total, &m.HardcoverBookID, &m.Position, &m.Title, &m.Author, &m.Year, &m.Pages, &at); err != nil {
			return nil, err
		}
		if cat == nil {
			cat = &seriesCatalog{SourceBookID: src, SeriesID: sid, Name: name, FetchedAt: at, Total: total}
		}
		if sid == cat.SeriesID {
			all = append(all, m)
		}
	}
	if err = rows.Err(); err != nil || cat == nil {
		return nil, err
	}
	cat.Truncated = cat.Total > len(all)
	have := 0.0
	for _, v := range g.Numbered {
		if v.Number != nil {
			have = max(have, *v.Number)
		}
	}
	for _, m := range all {
		var n int
		if err = q.QueryRow(`SELECT (SELECT count(*) FROM book_hardcover WHERE hardcover_book_id=?)+(SELECT count(*) FROM hardcover_alias WHERE hardcover_book_id=?)+(SELECT count(*) FROM books WHERE identity=?)+(SELECT count(*) FROM book_aliases WHERE identity=?)`,
			m.HardcoverBookID, m.HardcoverBookID, identity(m.Title, m.Author), identity(m.Title, m.Author)).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			continue
		}
		cat.Missing = append(cat.Missing, m)
		if cat.NextMissing == nil && m.Position != nil && *m.Position > have && *m.Position == math.Trunc(*m.Position) {
			mm := m
			cat.NextMissing = &mm
		}
	}
	if len(cat.Missing) == 0 {
		cat.NextMissing = nil
	}
	return cat, nil
}

func (m hcSeriesMember) Label() string {
	if m.Position == nil {
		return "no number"
	}
	return "#" + formatNumber(*m.Position)
}

func (c hcCachedCatalog) RatingText() string {
	if c.Rating == nil {
		return ""
	}
	s := fmt.Sprintf("Hardcover average %.1f", *c.Rating)
	if c.RatingsCount > 0 {
		s += " (" + groupDigits(c.RatingsCount) + " ratings)"
	}
	return s
}

func groupDigits(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
