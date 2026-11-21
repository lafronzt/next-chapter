package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Following a series and checking it for new books is reader-initiated end to
// end. A follow is a Hardcover series id the reader chose; a check reads that
// one series' members (read:catalog, no reader data) and remembers what it
// saw. Nothing here adds a book, changes the queue, or sends anything to
// Hardcover: adding goes through the existing previewed series plan.
//
// What Hardcover's published schema and guides (read 2026-11-21 from the
// hardcoverapp/hardcover-docs repository) allow, and what is not assumed:
//   - series are identified by a stable integer id; a duplicate series has
//     canonical_id set to the series it was merged into;
//   - book_series gives each member a float position (null allowed), a text
//     "details" form, and a compilation flag; a book has canonical_id when it
//     was merged, book_status_id 1 for a normal record, and is_partial_book;
//   - books.release_date is a date described as the original publication
//     date, with no precision field, and release_year is a separate integer;
//   - list queries are capped (book_series 997 rows), so members are read in
//     pages by limit and offset.
//
// A release date is shown as Hardcover lists it, never as proof of when a
// book was or will be published, and a missing date is unknown, not "soon".

const (
	hcSeriesPageSize      = 200 // rows per request; Hardcover's cap is 997
	hcSeriesPagesPerCheck = 5   // requests per series per press of the button
	hcSeriesCheckMaxBatch = 5   // series checked by one press
	maxSeriesFollows      = 100
	maxSeriesMembers      = 2000 // members kept per series
	seriesStaleDays       = 30
)

const hcSeriesCheckQuery = `query NextChapterSeriesCheck($id: Int!, $limit: Int!, $offset: Int!) {
 series_by_pk(id: $id) {
  id name slug canonical_id books_count
  book_series(where: {book: {canonical_id: {_is_null: true}, book_status_id: {_eq: 1}, is_partial_book: {_eq: false}}}, order_by: [{position: asc_nulls_last}, {book_id: asc}], limit: $limit, offset: $offset) {
   position details compilation
   book { id title slug release_date release_year pages contributions { author { name } } }
  }
 }
}`

type seriesFollow struct {
	SeriesID       int64  `json:"series_id"`
	Name           string `json:"name"`
	Slug           string `json:"slug,omitempty"`
	LocalSeries    string `json:"local_series,omitempty"`
	FollowedAt     string `json:"followed_at"`
	CanonicalID    int64  `json:"canonical_id,omitempty"`
	Gone           bool   `json:"gone,omitempty"`
	Total          int    `json:"total,omitempty"`
	Run            int    `json:"run,omitempty"`
	CompleteChecks int    `json:"complete_checks,omitempty"`
	NextOffset     int    `json:"next_offset,omitempty"`
	LastCheckAt    string `json:"last_check_at,omitempty"`
	LastCompleteAt string `json:"last_complete_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// Partial reports that the latest check stopped before the end of the series.
func (f seriesFollow) Partial() bool { return f.NextOffset > 0 }

// URL is the series page on Hardcover.
func (f seriesFollow) URL() string {
	if f.Slug == "" {
		return ""
	}
	return "https://hardcover.app/series/" + url.PathEscape(f.Slug)
}

// EstimatedRequests is the number of requests the next check would make: one
// per page of members, from where a partial check stopped, bounded by the
// per-press budget. It is an estimate from the last known size.
func (f seriesFollow) EstimatedRequests() int {
	left := f.Total - f.NextOffset
	pages := (left + hcSeriesPageSize - 1) / hcSeriesPageSize
	return min(max(pages, 1), hcSeriesPagesPerCheck)
}

type seriesDiscovery struct {
	SeriesID        int64    `json:"series_id"`
	HardcoverBookID int64    `json:"hardcover_book_id"`
	Title           string   `json:"title"`
	Author          string   `json:"author,omitempty"`
	Slug            string   `json:"slug,omitempty"`
	Position        *float64 `json:"position"`
	Details         string   `json:"details,omitempty"`
	Compilation     bool     `json:"compilation,omitempty"`
	ReleaseDate     string   `json:"release_date,omitempty"`
	Year            *int     `json:"year,omitempty"`
	Pages           *int     `json:"pages,omitempty"`
	FirstSeenAt     string   `json:"first_seen_at"`
	Baseline        bool     `json:"baseline,omitempty"`
	SeenRun         int      `json:"seen_run,omitempty"`
	Listed          bool     `json:"listed"`
	State           string   `json:"state"`
	ResolvedAt      string   `json:"resolved_at,omitempty"`
	Version         int      `json:"version"`
}

const followColumns = "series_id,name,slug,local_series,followed_at,canonical_id,gone,total,run,complete_checks,next_offset,last_check_at,last_complete_at,last_error"

func (f *seriesFollow) scan() []any {
	return []any{&f.SeriesID, &f.Name, &f.Slug, &f.LocalSeries, &f.FollowedAt, &f.CanonicalID, &f.Gone, &f.Total, &f.Run, &f.CompleteChecks, &f.NextOffset, &f.LastCheckAt, &f.LastCompleteAt, &f.LastError}
}

const discoveryColumns = "series_id,hardcover_book_id,title,author,slug,position,details,compilation,release_date,year,pages,first_seen_at,baseline,seen_run,listed,state,resolved_at,version"

func (d *seriesDiscovery) scan() []any {
	return []any{&d.SeriesID, &d.HardcoverBookID, &d.Title, &d.Author, &d.Slug, &d.Position, &d.Details, &d.Compilation, &d.ReleaseDate, &d.Year, &d.Pages, &d.FirstSeenAt, &d.Baseline, &d.SeenRun, &d.Listed, &d.State, &d.ResolvedAt, &d.Version}
}

func loadFollows(q queryer) ([]seriesFollow, error) {
	rows, err := q.Query("SELECT " + followColumns + " FROM series_follows ORDER BY lower(name),series_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []seriesFollow{}
	for rows.Next() {
		var f seriesFollow
		if err = rows.Scan(f.scan()...); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func getFollow(q queryer, id int64) (*seriesFollow, error) {
	var f seriesFollow
	err := q.QueryRow("SELECT "+followColumns+" FROM series_follows WHERE series_id=?", id).Scan(f.scan()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

// loadDiscoveries returns one series' members in a deterministic order:
// numbered by position, then those with no position, each by title then id.
func loadDiscoveries(q queryer, seriesID int64) ([]seriesDiscovery, error) {
	return queryDiscoveries(q, "WHERE series_id=?", seriesID)
}

func queryDiscoveries(q queryer, tail string, args ...any) ([]seriesDiscovery, error) {
	rows, err := q.Query("SELECT "+discoveryColumns+" FROM series_discoveries "+tail+" ORDER BY series_id, position IS NULL, position, lower(title), hardcover_book_id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []seriesDiscovery{}
	for rows.Next() {
		var d seriesDiscovery
		if err = rows.Scan(d.scan()...); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- reading the library once, so classifying costs no per-book query ----

type localRef struct {
	ID            int64
	Title, Author string
}

type libraryIndex struct {
	linked     map[int64]int64       // Hardcover book id -> live local book
	hcAlias    map[int64]int64       // remembered Hardcover id -> live target, 0 when deleted
	identities map[string]int64      // identity -> live local book
	aliases    map[string]int64      // remembered identity -> live target, 0 when deleted
	byTitle    map[string][]localRef // normalized title -> local books
}

func loadLibraryIndex(q queryer) (*libraryIndex, error) {
	ix := &libraryIndex{linked: map[int64]int64{}, hcAlias: map[int64]int64{}, identities: map[string]int64{}, aliases: map[string]int64{}, byTitle: map[string][]localRef{}}
	collect := func(query string, each func(*sql.Rows) error) error {
		rows, err := q.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err = each(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := collect("SELECT hardcover_book_id,book_id FROM book_hardcover", func(r *sql.Rows) error {
		var hc, id int64
		if err := r.Scan(&hc, &id); err != nil {
			return err
		}
		ix.linked[hc] = id
		return nil
	}); err != nil {
		return nil, err
	}
	if err := collect("SELECT id,identity,title,author FROM books", func(r *sql.Rows) error {
		var ref localRef
		var ident string
		if err := r.Scan(&ref.ID, &ident, &ref.Title, &ref.Author); err != nil {
			return err
		}
		ix.identities[ident] = ref.ID
		key := normalized(ref.Title)
		ix.byTitle[key] = append(ix.byTitle[key], ref)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := collect("SELECT identity,COALESCE(target_book_id,0),(SELECT count(*) FROM books b WHERE b.id=target_book_id) FROM book_aliases", func(r *sql.Rows) error {
		var ident string
		var target int64
		var live int
		if err := r.Scan(&ident, &target, &live); err != nil {
			return err
		}
		if live == 0 {
			target = 0
		}
		ix.aliases[ident] = target
		return nil
	}); err != nil {
		return nil, err
	}
	if err := collect("SELECT h.hardcover_book_id,h.identity FROM hardcover_alias h", func(r *sql.Rows) error {
		var hc int64
		var ident string
		if err := r.Scan(&hc, &ident); err != nil {
			return err
		}
		ix.hcAlias[hc] = ix.aliases[ident]
		return nil
	}); err != nil {
		return nil, err
	}
	return ix, nil
}

// ---- classification ----

const (
	catNew       = "new"
	catUpcoming  = "upcoming"
	catKnown     = "known"
	catLibrary   = "library"
	catPossible  = "possible"
	catRemoved   = "removed"
	catUnlisted  = "unlisted"
	catDismissed = "dismissed"
)

// discoveryView is one member as the results page shows it.
type discoveryView struct {
	seriesDiscovery
	Category   string
	LocalID    int64
	LocalTitle string
	Upcoming   bool
	// Offerable members can be chosen for the add preview.
	Offerable    bool
	ReleaseText  string
	Why          string
	SamePosition bool
}

// Fractional reports a number such as 2.5, listed in order but never a
// prerequisite for the next whole-numbered volume.
func (d discoveryView) Fractional() bool {
	return d.Position != nil && *d.Position != float64(int64(*d.Position))
}

func (d discoveryView) Label() string {
	if d.Position == nil {
		return "no number"
	}
	s := "#" + formatNumber(*d.Position)
	if d.Details != "" && d.Details != formatNumber(*d.Position) {
		s += " (" + d.Details + ")"
	}
	return s
}

// Link is the book's page on Hardcover.
func (d discoveryView) Link() string {
	if d.Slug == "" {
		return ""
	}
	return "https://hardcover.app/books/" + url.PathEscape(d.Slug)
}

// classify decides where a member belongs. It reads the library index only.
func classify(d seriesDiscovery, ix *libraryIndex, today time.Time) discoveryView {
	v := discoveryView{seriesDiscovery: d}
	ident := identity(d.Title, d.Author)
	if t, err := time.Parse(time.DateOnly, d.ReleaseDate); err == nil && t.After(today) {
		v.Upcoming = true
	}
	switch {
	case d.ReleaseDate != "":
		v.ReleaseText = "Release date listed on Hardcover: " + d.ReleaseDate
	case d.Year != nil:
		v.ReleaseText = fmt.Sprintf("Only a year is listed: %d", *d.Year)
	default:
		v.ReleaseText = "No release date listed"
	}
	if id, ok := ix.linked[d.HardcoverBookID]; ok {
		return v.in(catLibrary, id, "Linked to a book in your library.")
	}
	if target, ok := ix.hcAlias[d.HardcoverBookID]; ok && target > 0 {
		return v.in(catLibrary, target, "A book you merged into one in your library.")
	}
	if id, ok := ix.identities[ident]; ok {
		return v.in(catLibrary, id, "The same title and author is already in your library.")
	}
	if target, ok := ix.aliases[ident]; ok && target > 0 {
		return v.in(catLibrary, target, "A book you merged into one in your library.")
	}
	if d.State == "dismissed" {
		return v.in(catDismissed, 0, "You dismissed it. Restore it to offer it again.")
	}
	if target, ok := ix.hcAlias[d.HardcoverBookID]; ok && target == 0 {
		return v.in(catRemoved, 0, "You deleted this book earlier, so it is not offered again.")
	}
	if target, ok := ix.aliases[ident]; ok && target == 0 {
		return v.in(catRemoved, 0, "You deleted this book earlier, so it is not offered again.")
	}
	if !d.Listed {
		return v.in(catUnlisted, 0, "Hardcover's list no longer includes it as of the last complete check. It is kept here, not deleted.")
	}
	v.Offerable = true
	for _, ref := range ix.byTitle[normalized(d.Title)] {
		v.LocalID, v.LocalTitle = ref.ID, ref.Title
		v.Category = catPossible
		v.Why = "You have a book with this title but a different author. It may be the same book or a different one; review before adding."
		return v
	}
	switch {
	case v.Upcoming:
		v.Category = catUpcoming
		v.Why = "Hardcover lists a release date that has not happened yet. It does not say how precise the date is."
	case d.State == "open" && !d.Baseline:
		v.Category = catNew
		v.Why = "Hardcover's list for this series gained it after your previous complete check. That says when it was cataloged, not when it was published."
	case d.State == "acknowledged":
		v.Category = catKnown
		v.Why = "You acknowledged it. It is not in your library."
	default:
		v.Category = catKnown
		v.Why = "It was already in Hardcover's list when you started following. It is not in your library."
	}
	return v
}

func (v discoveryView) in(category string, local int64, why string) discoveryView {
	v.Category, v.LocalID, v.Why = category, local, why
	return v
}

// seriesCounts is what a follow's summary line shows.
type seriesCounts struct {
	New, Upcoming, Known, Dismissed, Library, Possible int
}

func countViews(views []discoveryView) seriesCounts {
	var c seriesCounts
	for _, v := range views {
		switch v.Category {
		case catNew:
			c.New++
		case catUpcoming:
			c.Upcoming++
		case catKnown:
			c.Known++
		case catDismissed:
			c.Dismissed++
		case catLibrary:
			c.Library++
		case catPossible:
			c.Possible++
		}
	}
	return c
}

// followSummary is a follow with its counts, for lists.
type followSummary struct {
	seriesFollow
	seriesCounts
	Stale bool
}

// Stale reports an old last check, with now passed in.
func (f seriesFollow) staleAt(now time.Time) bool {
	t, err := time.Parse(time.RFC3339Nano, f.LastCompleteAt)
	return err != nil || now.Sub(t) > seriesStaleDays*24*time.Hour
}

// summarizeFollows counts every follow's members with one read of the library
// and one of the members.
func summarizeFollows(q queryer, follows []seriesFollow, today time.Time) ([]followSummary, error) {
	if len(follows) == 0 {
		return nil, nil
	}
	ix, err := loadLibraryIndex(q)
	if err != nil {
		return nil, err
	}
	all, err := queryDiscoveries(q, "")
	if err != nil {
		return nil, err
	}
	views := map[int64][]discoveryView{}
	for _, d := range all {
		views[d.SeriesID] = append(views[d.SeriesID], classify(d, ix, today))
	}
	out := make([]followSummary, len(follows))
	for i, f := range follows {
		out[i] = followSummary{seriesFollow: f, seriesCounts: countViews(views[f.SeriesID])}
	}
	return out, nil
}

// ---- reading Hardcover ----

type hcSeriesMemberJSON struct {
	Position    *flexFloat `json:"position"`
	Details     string     `json:"details"`
	Compilation bool       `json:"compilation"`
	Book        *struct {
		ID            flexInt `json:"id"`
		Title         string  `json:"title"`
		Slug          string  `json:"slug"`
		ReleaseDate   *string `json:"release_date"`
		ReleaseYear   flexInt `json:"release_year"`
		Pages         flexInt `json:"pages"`
		Contributions []struct {
			Author struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"contributions"`
	} `json:"book"`
}

type hcSeriesInfo struct {
	ID          flexInt              `json:"id"`
	Name        string               `json:"name"`
	Slug        string               `json:"slug"`
	CanonicalID *flexInt             `json:"canonical_id"`
	Count       flexInt              `json:"books_count"`
	Members     []hcSeriesMemberJSON `json:"book_series"`
}

// seriesRead is what one press of Check read from one series.
type seriesRead struct {
	StartOffset int
	Info        *hcSeriesInfo
	Gone        bool
	Members     []hcSeriesMemberJSON
	// NextOffset is where to continue, 0 when the whole series was read.
	NextOffset int
	Complete   bool
	Err        error
	Requests   int
}

// readSeries fetches pages from offset, within the per-press budget. A failed
// page keeps what earlier pages returned and says where to continue. It never
// holds a database transaction.
func (a *App) readSeries(ctx context.Context, id int64, offset int) seriesRead {
	res := seriesRead{StartOffset: offset}
	for range hcSeriesPagesPerCheck {
		var out struct {
			Series *hcSeriesInfo `json:"series_by_pk"`
		}
		res.Requests++
		if err := a.hardcover.do(ctx, hcSeriesCheckQuery, map[string]any{"id": id, "limit": hcSeriesPageSize, "offset": offset}, &out); err != nil {
			res.Err, res.NextOffset = err, offset
			return res
		}
		if out.Series == nil || int64(out.Series.ID) != id {
			res.Gone, res.Complete = true, true
			return res
		}
		if res.Info == nil {
			res.Info = out.Series
		}
		if out.Series.CanonicalID != nil && *out.Series.CanonicalID > 0 {
			res.Info, res.Complete = out.Series, true
			return res
		}
		res.Members = append(res.Members, out.Series.Members...)
		if len(out.Series.Members) < hcSeriesPageSize {
			res.Complete = true
			return res
		}
		offset += hcSeriesPageSize
		res.NextOffset = offset
	}
	return res
}

// seriesErrorClass names a failure without any text from Hardcover.
func seriesErrorClass(err error) string {
	var rate hardcoverRateError
	var scope hardcoverScopeError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &rate):
		return "rate-limit"
	case errors.As(err, &scope):
		return "scope"
	case errors.Is(err, errHardcoverAuth):
		return "auth"
	case errors.Is(err, errHardcoverTimeout):
		return "timeout"
	case errors.Is(err, errHardcoverUnavailable):
		return "unavailable"
	case errors.Is(err, errHardcoverResponse):
		return "response"
	}
	return "error"
}

func seriesErrorText(class string) string {
	switch class {
	case "":
		return ""
	case "rate-limit":
		return "Hardcover's rate limit was reached."
	case "scope":
		return "The Hardcover token is missing the read:catalog scope."
	case "auth":
		return "Hardcover rejected the token."
	case "timeout":
		return "Hardcover timed out."
	case "unavailable":
		return "Hardcover was unavailable."
	case "response":
		return "Hardcover answered in a shape this app does not understand."
	}
	return "The check failed."
}

// seriesOutcome is what recording a check changed, for the notice.
type seriesOutcome struct {
	Name        string
	Added, Seen int
	Complete    bool
	Baseline    bool
	Merged      bool
	Gone        bool
	Error       string
}

// recordSeriesCheck stores one press's result. It runs in the caller's
// transaction and makes no request. A partial or failed press keeps what it
// read and never advances the complete-check count, and a member Hardcover
// did not return is never deleted: after a complete read it is only marked as
// no longer listed.
func recordSeriesCheck(tx queryer, f seriesFollow, res seriesRead, now string) (seriesOutcome, error) {
	out := seriesOutcome{Name: f.Name, Error: seriesErrorClass(res.Err)}
	run := f.Run
	if res.StartOffset == 0 {
		run++
	}
	set := func(sets string, args ...any) error {
		_, err := tx.Exec("UPDATE series_follows SET "+sets+" WHERE series_id=?", append(args, f.SeriesID)...)
		return err
	}
	switch {
	case res.Gone:
		out.Gone = true
		return out, set("gone=1,last_check_at=?,last_error='',next_offset=0", now)
	case res.Info != nil && res.Info.CanonicalID != nil && *res.Info.CanonicalID > 0:
		out.Merged = true
		return out, set("canonical_id=?,last_check_at=?,last_error='',next_offset=0", int64(*res.Info.CanonicalID), now)
	case res.Info == nil:
		// The first request failed: remember only that it did.
		return out, set("last_check_at=?,last_error=?", now, out.Error)
	}
	var kept int
	if err := tx.QueryRow("SELECT count(*) FROM series_discoveries WHERE series_id=?", f.SeriesID).Scan(&kept); err != nil {
		return out, err
	}
	seen := map[int64]bool{}
	for _, m := range res.Members {
		if m.Book == nil || m.Book.ID < 1 || seen[int64(m.Book.ID)] {
			continue
		}
		title := cleanText(m.Book.Title, 300)
		if title == "" {
			continue
		}
		seen[int64(m.Book.ID)] = true
		author := ""
		if len(m.Book.Contributions) > 0 {
			author = cleanText(m.Book.Contributions[0].Author.Name, 300)
		}
		date := ""
		if m.Book.ReleaseDate != nil {
			if t, err := time.Parse(time.DateOnly, strings.TrimSpace(*m.Book.ReleaseDate)); err == nil {
				date = t.Format(time.DateOnly)
			}
		}
		var year *int
		if y := int(m.Book.ReleaseYear); y >= 1000 && y <= 2200 {
			year = &y
		}
		var existing int
		if err := tx.QueryRow("SELECT count(*) FROM series_discoveries WHERE series_id=? AND hardcover_book_id=?", f.SeriesID, int64(m.Book.ID)).Scan(&existing); err != nil {
			return out, err
		}
		if existing == 0 && kept >= maxSeriesMembers {
			continue
		}
		baseline := f.CompleteChecks == 0
		if _, err := tx.Exec(`INSERT INTO series_discoveries(series_id,hardcover_book_id,title,author,slug,position,details,compilation,release_date,year,pages,first_seen_at,baseline,seen_run,listed)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)
 ON CONFLICT(series_id,hardcover_book_id) DO UPDATE SET
  version=version+CASE WHEN title IS NOT excluded.title OR author IS NOT excluded.author OR position IS NOT excluded.position OR release_date IS NOT excluded.release_date OR listed=0 OR slug IS NOT excluded.slug THEN 1 ELSE 0 END,
  title=excluded.title,author=excluded.author,slug=excluded.slug,position=excluded.position,details=excluded.details,compilation=excluded.compilation,
  release_date=excluded.release_date,year=excluded.year,pages=excluded.pages,seen_run=excluded.seen_run,listed=1`,
			f.SeriesID, int64(m.Book.ID), title, author, cleanText(m.Book.Slug, 200), position(m.Position), cleanText(m.Details, 40), m.Compilation, date, year, positive(int(m.Book.Pages)), now, baseline, run); err != nil {
			return out, err
		}
		if existing == 0 {
			kept++
			out.Added++
		}
		out.Seen++
	}
	name := cleanText(res.Info.Name, 200)
	if name == "" {
		name = f.Name
	}
	slug := cleanText(res.Info.Slug, 200)
	complete := res.Complete && res.Err == nil
	if complete {
		out.Complete, out.Baseline = true, f.CompleteChecks == 0
		// Members this complete read did not return are only marked.
		if _, err := tx.Exec("UPDATE series_discoveries SET listed=0,version=version+1 WHERE series_id=? AND seen_run<? AND listed=1", f.SeriesID, run); err != nil {
			return out, err
		}
		return out, set("name=?,slug=?,total=?,run=?,complete_checks=complete_checks+1,next_offset=0,last_check_at=?,last_complete_at=?,last_error='',canonical_id=0,gone=0",
			name, slug, int(res.Info.Count), run, now, now)
	}
	next := res.NextOffset
	if next == 0 {
		next = res.StartOffset
	}
	return out, set("name=?,slug=?,total=?,run=?,next_offset=?,last_check_at=?,last_error=?,gone=0", name, slug, int(res.Info.Count), run, next, now, out.Error)
}

// byDiscoveryOrder sorts views the way the page lists them.
func byDiscoveryOrder(a, b discoveryView) int {
	switch {
	case a.Position == nil && b.Position != nil:
		return 1
	case a.Position != nil && b.Position == nil:
		return -1
	case a.Position != nil && *a.Position != *b.Position:
		return cmp.Compare(*a.Position, *b.Position)
	}
	return cmp.Or(cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)), cmp.Compare(a.HardcoverBookID, b.HardcoverBookID))
}
