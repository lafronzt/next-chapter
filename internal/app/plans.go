package app

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"time"
)

// Plans are stored as gzip-compressed JSON of storedPlan. Payloads written
// before this format are plain JSON of Plan; they expire within an hour and
// still decode until then.
var gzipMagic = []byte{0x1f, 0x8b}

// storedPlan is a Plan with its import records in their compact form.
type storedPlan struct {
	Plan
	Compact []planRecord `json:"compact_records,omitempty"`
}

// planRecord keeps only the Record fields that importing and the preview use.
type planRecord struct {
	Title        string     `json:"t"`
	Author       string     `json:"a"`
	ISBN         string     `json:"i,omitempty"`
	GoodreadsID  string     `json:"g,omitempty"`
	Pages        *int       `json:"p,omitempty"`
	Tags         string     `json:"tg,omitempty"`
	Status       string     `json:"s"`
	Series       string     `json:"sr,omitempty"`
	SeriesNumber *float64   `json:"sn,omitempty"`
	Source       string     `json:"src"`
	Rating       int        `json:"r,omitempty"`
	Finished     string     `json:"fd,omitempty"`
	ReviewChars  int        `json:"rc,omitempty"`
	Review       string     `json:"rv,omitempty"`
	Published    *int       `json:"py,omitempty"`
	Average      *float64   `json:"ar,omitempty"`
	Event        *planEvent `json:"e,omitempty"`
}
type planEvent struct {
	Format   string `json:"f"`
	Status   string `json:"s"`
	Activity string `json:"a"`
	At       string `json:"at"`
	DueDate  string `json:"d,omitempty"`
}

func compactRecord(r Record) planRecord {
	c := planRecord{Title: r.Title, Author: r.Author, ISBN: r.ISBN, GoodreadsID: r.GoodreadsID, Pages: r.Pages, Tags: r.Tags, Status: r.Status, Series: r.Series, SeriesNumber: r.SeriesNumber, Source: r.Source, Rating: r.Rating,
		Finished: r.FinishedAt, ReviewChars: r.ReviewChars, Review: r.Review, Published: r.PublishedYear, Average: r.AvgRating}
	if e := r.Event; e != nil {
		c.Event = &planEvent{e.Format, e.Status, e.Activity, e.At, e.DueDate}
	}
	return c
}

// record restores a Record as ParseCSV produced it.
func (c planRecord) record() Record {
	r := Record{Book: Book{Title: c.Title, Author: c.Author, ISBN: c.ISBN, GoodreadsID: c.GoodreadsID, Pages: c.Pages, Tags: c.Tags, Status: c.Status, Series: c.Series, SeriesNumber: c.SeriesNumber, Source: c.Source, Interest: 3, NeedsPlacement: true, PublishedYear: c.Published, AvgRating: c.Average},
		Rating: c.Rating, FinishedAt: c.Finished, ReviewChars: c.ReviewChars, Review: c.Review}
	if c.Rating >= 1 && c.Rating <= 5 {
		r.Interest = c.Rating
	}
	if e := c.Event; e != nil {
		r.Event = &Event{Format: e.Format, Status: e.Status, Activity: e.Activity, At: e.At, DueDate: e.DueDate}
	}
	return r
}

func encodePlan(p Plan) ([]byte, error) {
	s := storedPlan{Plan: p}
	s.Records = nil
	if len(p.Records) > 0 {
		s.Compact = make([]planRecord, len(p.Records))
		for i, r := range p.Records {
			s.Compact[i] = compactRecord(r)
		}
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(s); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodePlan reads either format. A legacy import plan has no stored Skipped
// count, so it is computed here.
func decodePlan(q queryer, kind string, raw []byte) (Plan, error) {
	if !bytes.HasPrefix(raw, gzipMagic) {
		var p Plan
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, err
		}
		var err error
		if kind == "import" {
			p.Skipped, err = countTombstoned(q, p.Records)
		}
		return p, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return Plan{}, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return Plan{}, err
	}
	var s storedPlan
	if err = json.Unmarshal(data, &s); err != nil {
		return Plan{}, err
	}
	p := s.Plan
	if len(s.Compact) > 0 {
		p.Records = make([]Record, len(s.Compact))
		for i, c := range s.Compact {
			p.Records[i] = c.record()
		}
	}
	return p, nil
}

// savePlan stores a preview for an hour. An import plan records how many rows
// match deleted books now, so showing the preview does not look them up again.
func savePlan(q queryer, kind string, p Plan) (string, error) {
	id := randomToken()
	if kind == "import" {
		var err error
		if p.Skipped, err = countTombstoned(q, p.Records); err != nil {
			return "", err
		}
	}
	raw, err := encodePlan(p)
	if err != nil {
		return "", err
	}
	rev, err := revision(q)
	if err != nil {
		return "", err
	}
	if _, err = q.Exec("DELETE FROM plans WHERE created_at<?", time.Now().Unix()-3600); err != nil {
		return "", err
	}
	_, err = q.Exec("INSERT INTO plans VALUES(?,?,?,?,?)", id, kind, raw, rev, time.Now().Unix())
	return id, err
}
func readPlan(q queryer, id string) (string, Plan, int64, error) {
	var kind string
	var raw []byte
	var rev int64
	err := q.QueryRow("SELECT kind,payload,revision FROM plans WHERE id=? AND created_at>?", id, time.Now().Unix()-3600).Scan(&kind, &raw, &rev)
	if err != nil {
		return "", Plan{}, 0, err
	}
	p, err := decodePlan(q, kind, raw)
	return kind, p, rev, err
}
