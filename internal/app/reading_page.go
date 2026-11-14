package app

import (
	"database/sql"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// readingFilter narrows the reading history list: a finish year (or
// "undated"), a rating of at least Min, and only unrated readings. Filters
// combine, and they are plain query parameters so paging keeps them.
type readingFilter struct {
	Year    string
	Min     float64
	Unrated bool
}

var yearPattern = regexp.MustCompile(`^\d{4}$`)

func parseReadingFilter(v url.Values) readingFilter {
	f := readingFilter{}
	if y := v.Get("year"); yearPattern.MatchString(y) || y == "undated" {
		f.Year = y
	}
	if n, err := strconv.ParseFloat(v.Get("min"), 64); err == nil && n >= 0.25 && n <= 5 && n*4 == math.Trunc(n*4) {
		f.Min = n
	}
	f.Unrated = v.Get("unrated") == "1"
	return f
}

// Query is the filter as URL parameters, for links and paging.
func (f readingFilter) Query() url.Values {
	v := url.Values{}
	if f.Year != "" {
		v.Set("year", f.Year)
	}
	if f.Min > 0 {
		v.Set("min", ratingText(f.Min))
	}
	if f.Unrated {
		v.Set("unrated", "1")
	}
	return v
}

func (f readingFilter) Active() bool { return f.Year != "" || f.Min > 0 || f.Unrated }

func (f readingFilter) where() (string, []any) {
	conds, args := []string{}, []any{}
	switch {
	case f.Year == "undated":
		conds = append(conds, "l.finished_at IS NULL")
	case f.Year != "":
		conds, args = append(conds, "substr(l.finished_at,1,4)=?"), append(args, f.Year)
	}
	if f.Min > 0 {
		conds, args = append(conds, "l.rating>=?"), append(args, f.Min)
	}
	if f.Unrated {
		conds = append(conds, "l.rating IS NULL")
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

func countFilteredReadings(q queryer, f readingFilter) (int, error) {
	where, args := f.where()
	var n int
	err := q.QueryRow("SELECT count(*) FROM reading_log l "+where, args...).Scan(&n)
	return n, err
}

func pageFilteredReadings(q queryer, f readingFilter, limit, offset int) ([]readingEntry, error) {
	where, args := f.where()
	return queryReadings(q, where+readingOrder+" LIMIT ? OFFSET ?", append(args, limit, offset)...)
}

// yearSummary is one year's readings under the current filter. Year is empty
// for readings with no finish date. Average is over rated readings only.
type yearSummary struct {
	Year      string
	Readings  int
	Rated     int
	Average   float64
	Continued bool
	Items     []readingEntry
}

// yearSummaries aggregates the filtered readings by year in one query.
func yearSummaries(q queryer, f readingFilter) (map[string]yearSummary, error) {
	where, args := f.where()
	rows, err := q.Query("SELECT COALESCE(substr(l.finished_at,1,4),''), count(*), count(l.rating), avg(l.rating) FROM reading_log l "+where+" GROUP BY 1", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]yearSummary{}
	for rows.Next() {
		var s yearSummary
		var avg sql.NullFloat64
		if err = rows.Scan(&s.Year, &s.Readings, &s.Rated, &avg); err != nil {
			return nil, err
		}
		s.Average = avg.Float64
		out[s.Year] = s
	}
	return out, rows.Err()
}

// groupByYear splits one page of readings (newest year first, undated last)
// into year sections carrying each year's totals.
func groupByYear(items []readingEntry, totals map[string]yearSummary, page int) []yearSummary {
	var out []yearSummary
	for _, e := range items {
		year := ""
		if e.FinishedAt != nil && len(*e.FinishedAt) >= 4 {
			year = (*e.FinishedAt)[:4]
		}
		if len(out) == 0 || out[len(out)-1].Year != year {
			s := totals[year]
			s.Year, s.Items = year, nil
			s.Continued = len(out) == 0 && page > 1
			out = append(out, s)
		}
		out[len(out)-1].Items = append(out[len(out)-1].Items, e)
	}
	return out
}

// ratingBucket is the readings rated up to a whole star: Stars 4 holds
// 3.25, 3.5, 3.75, and 4. Quarters counts each of those four values.
type ratingBucket struct {
	Stars    int
	Count    int
	Quarters [4]int
}

type ratingDistribution struct {
	Buckets []ratingBucket
	Max     int
	Rated   int
	Unrated int
}

// loadDistribution counts every reading by rating in one query.
func loadDistribution(q queryer) (ratingDistribution, error) {
	d := ratingDistribution{Buckets: make([]ratingBucket, 5)}
	for i := range d.Buckets {
		d.Buckets[i].Stars = i + 1
	}
	rows, err := q.Query("SELECT rating,count(*) FROM reading_log GROUP BY rating")
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var r sql.NullFloat64
		var n int
		if err = rows.Scan(&r, &n); err != nil {
			return d, err
		}
		if !r.Valid {
			d.Unrated += n
			continue
		}
		star := min(5, max(1, int(math.Ceil(r.Float64))))
		quarter := int(math.Round((r.Float64-float64(star-1))*4)) - 1 // 0..3
		b := &d.Buckets[star-1]
		b.Count += n
		b.Quarters[min(3, max(0, quarter))] += n
		d.Rated += n
	}
	for _, b := range d.Buckets {
		d.Max = max(d.Max, b.Count)
	}
	return d, rows.Err()
}

func (a *App) readingHistory(w http.ResponseWriter, r *http.Request) error {
	filter := parseReadingFilter(r.URL.Query())
	stats, err := historyStats(a.store.DB)
	if err != nil {
		return err
	}
	dist, err := loadDistribution(a.store.DB)
	if err != nil {
		return err
	}
	total, err := countFilteredReadings(a.store.DB, filter)
	if err != nil {
		return err
	}
	pages := newPager(r, total, "/reading", filter.Query())
	readings, err := pageFilteredReadings(a.store.DB, filter, pages.PerPage, pages.offset())
	if err != nil {
		return err
	}
	totals, err := yearSummaries(a.store.DB, filter)
	if err != nil {
		return err
	}
	accuracy, err := loadAccuracy(a.store.DB, 0)
	if err != nil {
		return err
	}
	return a.render(w, r, "reading.html", page{"Title": pageTitle("Reading history", pages), "Readings": readings, "Years": groupByYear(readings, totals, pages.Page), "Stats": stats, "Dist": dist, "Filter": filter, "Pager": pages, "Accuracy": accuracy, "Total": total}, 200)
}
