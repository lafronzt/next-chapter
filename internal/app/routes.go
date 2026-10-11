package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func bookFields(r *http.Request) (Book, error) {
	b := Book{Title: strings.TrimSpace(r.FormValue("title")), Author: strings.TrimSpace(r.FormValue("author")), Tags: strings.TrimSpace(r.FormValue("tags")), Status: r.FormValue("reading_status"), Series: strings.TrimSpace(r.FormValue("series")), Source: "manual"}
	if b.Title == "" || b.Author == "" || len([]rune(b.Title)) > 500 || len([]rune(b.Author)) > 300 {
		return b, bad("Enter a title and author within the field limits.")
	}
	if !in(b.Status, readingStates) {
		return b, bad("Choose a reading status.")
	}
	var err error
	b.Interest, err = strconv.Atoi(r.FormValue("interest"))
	if err != nil || b.Interest < 1 || b.Interest > 5 {
		return b, bad("Interest must be between 1 and 5.")
	}
	if raw := r.FormValue("pages"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 99999 {
			return b, bad("Pages must be between 1 and 99,999.")
		}
		b.Pages = &n
	}
	if raw := r.FormValue("series_number"); raw != "" {
		n, e := strconv.ParseFloat(raw, 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9999 {
			return b, bad("Enter a valid series number.")
		}
		b.SeriesNumber = &n
	}
	if len([]rune(b.Tags)) > 500 || len([]rune(b.Series)) > 200 {
		return b, bad("Tags or series name are too long.")
	}
	return b, nil
}
func savePlan(q queryer, kind string, p Plan) (string, error) {
	id := randomToken()
	raw, err := json.Marshal(p)
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
	_, err = q.Exec("INSERT INTO plans VALUES(?,?,?,?,?)", id, kind, string(raw), rev, time.Now().Unix())
	return id, err
}
func makeSuggestion(q queryer, candidate int64) (string, error) {
	all, err := loadBooks(q)
	if err != nil {
		return "", err
	}
	prefs, err := readMeta(q, "preferences", defaultPreferences)
	if err != nil {
		return "", err
	}
	rows, err := Propose(queueOnly(all), prefs, candidate, time.Now().UTC())
	if err != nil {
		return "", bad(err.Error())
	}
	kind := "reorder"
	if candidate != 0 {
		kind = "insert"
	}
	return savePlan(q, kind, Plan{Rows: rows, CandidateID: candidate})
}
func (a *App) newBook(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodGet {
		return a.render(w, r, "book.html", page{"Book": Book{Interest: 3, Status: "to-read"}}, 200)
	}
	b, err := bookFields(r)
	if err != nil {
		return err
	}
	location := "/?notice=saved"
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		id, err := findBook(tx, Record{Book: b})
		if err != nil {
			return err
		}
		if id != 0 {
			return conflict("This book is already in your library. Edit its existing entry.")
		}
		// Adding a deleted book again by hand is an explicit choice that wins.
		if err = clearTombstones(tx, Record{Book: b}); err != nil {
			return err
		}
		if _, err = importRecords(tx, []Record{{Book: b}}); err != nil {
			return err
		}
		id, err = findBook(tx, Record{Book: b})
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE books SET interest=? WHERE id=?", b.Interest, id); err != nil {
			return err
		}
		if err = bump(tx); err != nil {
			return err
		}
		if b.Status == "to-read" {
			plan, err := makeSuggestion(tx, id)
			if err != nil {
				return err
			}
			location = "/plans/" + plan
		}
		return nil
	})
	if err != nil {
		return err
	}
	return redirect(w, r, location)
}
func (a *App) editBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		b, err := bookByID(a.store.DB, id)
		if err != nil {
			return err
		}
		events, err := loadEvents(a.store.DB, id, 500)
		if err != nil {
			return err
		}
		return a.render(w, r, "book.html", page{"Book": b, "Editing": true, "Events": events}, 200)
	}
	b, err := bookFields(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		old, err := bookByID(tx, id)
		if err != nil {
			return err
		}
		// Title and author must be unique among live books only.
		match, _, err := lookupBook(tx, Record{Book: b}, false)
		if err != nil {
			return err
		}
		if match != 0 && match != id {
			return conflict("Another book already has this title and author.")
		}
		pin, needs, position := old.Pinned, old.NeedsPlacement, old.Position
		if old.Status != b.Status {
			pin = false
			needs = b.Status == "to-read"
			if needs {
				if err = tx.QueryRow("SELECT COALESCE(MAX(position),-1)+1 FROM books").Scan(&position); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(`UPDATE books SET title=?,author=?,identity=?,interest=?,reading_status=?,pages=?,tags=?,series=?,series_number=?,pinned=?,needs_placement=?,position=? WHERE id=?`, b.Title, b.Author, identity(b.Title, b.Author), b.Interest, b.Status, b.Pages, b.Tags, b.Series, b.SeriesNumber, pin, needs, position, id)
		if err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=saved", id))
}
func (a *App) bookAction(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		all, err := loadBooks(tx)
		if err != nil {
			return err
		}
		queue := queueOnly(all)
		idx := -1
		for i, b := range queue {
			if b.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			return bad("Choose a book in your reading queue.")
		}
		b := queue[idx]
		switch r.FormValue("action") {
		case "pin":
			_, err = tx.Exec("UPDATE books SET pinned=? WHERE id=?", !b.Pinned, id)
		case "placed":
			_, err = tx.Exec("UPDATE books SET needs_placement=0 WHERE id=?", id)
		case "up", "down":
			target := idx - 1
			if r.FormValue("action") == "down" {
				target = idx + 1
			}
			if target < 0 || target >= len(queue) || b.Pinned || queue[target].Pinned {
				return bad("This move would cross a pinned book or the end of the queue.")
			}
			queue[idx], queue[target] = queue[target], queue[idx]
			ids := []int64{}
			for _, b := range queue {
				ids = append(ids, b.ID)
			}
			err = reorder(tx, ids)
		default:
			return bad("Choose a valid action.")
		}
		if err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/")
}
func (a *App) loan(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	source, format, status, due := r.FormValue("source"), r.FormValue("format"), r.FormValue("status"), r.FormValue("due_date")
	if !in(source, []string{"nypl", "libby"}) || !in(format, []string{"print", "ebook", "audiobook", "digital"}) || !in(status, libraryStates) {
		return bad("Choose a library source, format, and activity.")
	}
	if due != "" {
		if _, err = time.Parse(time.DateOnly, due); err != nil {
			return bad("Enter a valid due date.")
		}
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		b, err := bookByID(tx, id)
		if err != nil {
			return err
		}
		b.Source = source
		event := &Event{Source: source, Format: format, Status: status, Activity: status, At: canonicalTime(time.Now()), DueDate: due}
		if _, err = importRecords(tx, []Record{{Book: b, Event: event}}); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=loan", id))
}
func (a *App) suggest(w http.ResponseWriter, r *http.Request) error {
	candidate := int64(0)
	if raw := r.FormValue("candidate_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			return bad("Choose a valid book.")
		}
		candidate = id
	}
	var id string
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = makeSuggestion(tx, candidate)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+id)
}

type undoBook struct {
	ID             int64 `json:"id"`
	NeedsPlacement bool  `json:"needs_placement"`
}

func readPlan(q queryer, id string) (string, Plan, int64, error) {
	var kind, raw string
	var rev int64
	err := q.QueryRow("SELECT kind,payload,revision FROM plans WHERE id=? AND created_at>?", id, time.Now().Unix()-3600).Scan(&kind, &raw, &rev)
	if err != nil {
		return "", Plan{}, 0, err
	}
	var p Plan
	err = json.Unmarshal([]byte(raw), &p)
	return kind, p, rev, err
}
func (a *App) plan(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		kind, p, rev, err := readPlan(a.store.DB, id)
		if err != nil {
			return err
		}
		preview := p.Records
		if len(preview) > 100 {
			preview = preview[:100]
		}
		skipped := 0
		if kind == "import" {
			if skipped, err = countTombstoned(a.store.DB, p.Records); err != nil {
				return err
			}
		}
		return a.render(w, r, "plan.html", page{"Kind": kind, "Plan": p, "PreviewRecords": preview, "Skipped": skipped, "Stale": rev != state(r).Revision}, 200)
	}
	notice := "ordered"
	skipped := 0
	err := a.store.Write(func(tx *sql.Tx) error {
		kind, p, rev, err := readPlan(tx, id)
		if err != nil {
			return err
		}
		now, err := revision(tx)
		if err != nil {
			return err
		}
		if now != rev {
			return conflict("Your library changed. Generate a fresh preview before applying it.")
		}
		if kind == "import" {
			summary, err := importRecords(tx, p.Records)
			if err != nil {
				return err
			}
			skipped = summary.Skipped
			if summary.Changed {
				if err = bump(tx); err != nil {
					return err
				}
			}
			notice = "imported"
		} else {
			all, err := loadBooks(tx)
			if err != nil {
				return err
			}
			previous := []undoBook{}
			for _, b := range queueOnly(all) {
				previous = append(previous, undoBook{b.ID, b.NeedsPlacement})
			}
			raw, err := json.Marshal(previous)
			if err != nil {
				return err
			}
			ids := []int64{}
			for _, row := range p.Rows {
				ids = append(ids, row.ID)
			}
			if err = reorder(tx, ids); err != nil {
				return err
			}
			if p.CandidateID != 0 {
				_, err = tx.Exec("UPDATE books SET needs_placement=0 WHERE id=?", p.CandidateID)
			} else {
				_, err = tx.Exec("UPDATE books SET needs_placement=0 WHERE reading_status='to-read'")
			}
			if err != nil {
				return err
			}
			if err = bump(tx); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT OR REPLACE INTO undo_queue VALUES(1,?,?)", string(raw), now+1); err != nil {
				return err
			}
		}
		_, err = tx.Exec("DELETE FROM plans WHERE id=?", id)
		return err
	})
	if err != nil {
		return err
	}
	if skipped > 0 {
		return redirect(w, r, fmt.Sprintf("/?notice=%s&skipped=%d", notice, skipped))
	}
	return redirect(w, r, "/?notice="+notice)
}
func (a *App) undo(w http.ResponseWriter, r *http.Request) error {
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var raw string
		var rev int64
		if err := tx.QueryRow("SELECT payload,revision FROM undo_queue WHERE id=1").Scan(&raw, &rev); err != nil {
			return err
		}
		now, err := revision(tx)
		if err != nil {
			return err
		}
		if rev != now {
			return conflict("Undo expired after another library change.")
		}
		var previous []undoBook
		if err = json.Unmarshal([]byte(raw), &previous); err != nil {
			return err
		}
		for i, b := range previous {
			if _, err = tx.Exec("UPDATE books SET position=?,needs_placement=? WHERE id=?", i, b.NeedsPlacement, b.ID); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("DELETE FROM undo_queue"); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/?notice=undone")
}
func (a *App) importPage(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodGet {
		return a.render(w, r, "imports.html", nil, 200)
	}
	source := r.FormValue("source")
	if !in(source, []string{"goodreads", "libby", "nypl"}) {
		return bad("Choose an import source.")
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		return bad("Choose a CSV file.")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil {
		return err
	}
	records, problems, parseErr := ParseCSV(data, source)
	if parseErr != nil {
		return bad(parseErr.Error())
	}
	if len(problems) > 0 {
		count := len(problems)
		if count > 30 {
			problems = problems[:30]
		}
		return a.render(w, r, "imports.html", page{"Errors": problems, "ErrorCount": count}, 400)
	}
	var id string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		id, err = savePlan(tx, "import", Plan{Records: records, Source: source})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+id)
}
func (a *App) example(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimSuffix(r.PathValue("name"), ".csv")
	if !in(name, []string{"goodreads", "nypl", "libby"}) {
		return sql.ErrNoRows
	}
	data, err := assets.ReadFile("web/examples/" + name + ".csv")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	_, err = w.Write(data)
	return err
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodPost {
		weight, err := strconv.ParseFloat(r.FormValue("library_weight"), 64)
		if err != nil || (weight != 0 && weight != 0.5 && weight != 1 && weight != 2) {
			return bad("Choose a library timing preference.")
		}
		length := r.FormValue("length")
		if !in(length, []string{"any", "short", "long"}) {
			return bad("Choose a length preference.")
		}
		prefs := Preferences{weight, length, truncate(strings.TrimSpace(r.FormValue("mood")), 100)}
		conn := Connection{strings.TrimSpace(r.FormValue("user_id")), strings.TrimSpace(r.FormValue("shelf")), r.FormValue("enabled") != ""}
		if err = validateConnection(conn); err != nil {
			return bad(err.Error())
		}
		err = a.store.Write(func(tx *sql.Tx) error {
			if err := current(tx, r); err != nil {
				return err
			}
			if err := setMeta(tx, "preferences", prefs); err != nil {
				return err
			}
			if err := setMeta(tx, "sync", conn); err != nil {
				return err
			}
			return bump(tx)
		})
		if err != nil {
			return err
		}
		return redirect(w, r, "/settings?notice=saved")
	}
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		return err
	}
	conn, err := readMeta(a.store.DB, "sync", Connection{Shelf: "to-read"})
	if err != nil {
		return err
	}
	last, err := readMeta(a.store.DB, "last_sync", SyncResult{})
	if err != nil {
		return err
	}
	poll, err := readMeta(a.store.DB, "poll_state", PollState{})
	if err != nil {
		return err
	}
	version, _, err := schemaVersion(a.store.DB)
	if err != nil {
		return err
	}
	return a.render(w, r, "settings.html", page{"Prefs": prefs, "Connection": conn, "LastSync": last, "Poll": poll, "SchemaVersion": version}, 200)
}
func (a *App) syncNow(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.safeSync(r.Context(), a.now()); err != nil {
		var s *syncError
		if !errors.As(err, &s) {
			return err
		}
		status := 400
		if errors.Is(err, errSyncBusy) {
			status = 409
		} else if errors.Is(err, errFeedUnavailable) {
			log.Printf("Goodreads check failed (class=%s)", s.class)
		}
		return httpError{status, syncMessage(err, true)}
	}
	return redirect(w, r, "/settings?notice=synced")
}
func (a *App) history(w http.ResponseWriter, r *http.Request) error {
	events, err := loadEvents(a.store.DB, 0, 500)
	if err != nil {
		return err
	}
	return a.render(w, r, "history.html", page{"Events": events}, 200)
}
func (a *App) export(w http.ResponseWriter, r *http.Request) error {
	var data []byte
	err := a.store.Write(func(tx *sql.Tx) error {
		books, err := loadBooks(tx)
		if err != nil {
			return err
		}
		events, err := loadEvents(tx, 0, 0)
		if err != nil {
			return err
		}
		prefs, err := readMeta(tx, "preferences", defaultPreferences)
		if err != nil {
			return err
		}
		version, _, err := schemaVersion(tx)
		if err != nil {
			return err
		}
		aliases, err := loadAliases(tx)
		if err != nil {
			return err
		}
		data, err = json.MarshalIndent(map[string]any{"schema_version": version, "aliases": aliases, "exported_at": canonicalTime(time.Now()), "books": books, "events": events, "preferences": prefs}, "", "  ")
		return err
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="next-chapter.json"`)
	_, err = w.Write(data)
	return err
}
func (a *App) backup(w http.ResponseWriter, r *http.Request) error {
	data, err := a.store.Backup()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="next-chapter.sqlite3"`)
	_, err = w.Write(data)
	return err
}
func (a *App) demo(w http.ResponseWriter, r *http.Request) error {
	if !a.config.Demo {
		return sql.ErrNoRows
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		all, err := loadBooks(tx)
		if err != nil {
			return err
		}
		if len(all) != 0 {
			return bad("Example books can only be added to an empty demo library.")
		}
		titles := [][3]string{{"The Creative Act", "Rick Rubin", "creative, reflective"}, {"Braiding Sweetgrass", "Robin Wall Kimmerer", "nature, hopeful"}, {"Tomorrow, and Tomorrow, and Tomorrow", "Gabrielle Zevin", "fiction, friendship"}, {"A Psalm for the Wild-Built", "Becky Chambers", "hopeful, fiction"}, {"Piranesi", "Susanna Clarke", "mystery, fiction"}}
		records := []Record{}
		for _, t := range titles {
			records = append(records, Record{Book: Book{Title: t[0], Author: t[1], Tags: t[2], Status: "to-read", Source: "demo"}})
		}
		if _, err = importRecords(tx, records); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE books SET needs_placement=0; UPDATE books SET pinned=1 WHERE id=1; UPDATE books SET interest=5,needs_placement=1 WHERE id=5"); err != nil {
			return err
		}
		r := records[1]
		r.Source = "nypl"
		r.Event = &Event{Format: "print", Status: "borrowed", Activity: "borrowed", At: canonicalTime(time.Now()), DueDate: time.Now().UTC().AddDate(0, 0, 4).Format(time.DateOnly)}
		if _, err = importRecords(tx, []Record{r}); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/")
}

func (a *App) deleteBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		p, err := previewDelete(a.store.DB, id)
		if err != nil {
			return err
		}
		return a.render(w, r, "delete.html", page{"Preview": p}, 200)
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if err := deleteBook(tx, id); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/?view=all&notice=deleted")
}
func (a *App) mergeBook(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if r.Method == http.MethodGet {
		if raw := r.URL.Query().Get("target"); raw != "" {
			target, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || target < 1 {
				return bad("Choose a valid book to merge into.")
			}
			preview, err := mergeBooks(a.store.DB, id, target, false)
			if err != nil {
				return err
			}
			return a.render(w, r, "merge.html", page{"Preview": preview, "Book": preview.Source}, 200)
		}
		all, err := loadBooks(a.store.DB)
		if err != nil {
			return err
		}
		var source Book
		query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
		results := []Book{}
		for _, b := range all {
			if b.ID == id {
				source = b
			} else if query != "" && matchesQuery(b, query) && len(results) < 50 {
				results = append(results, b)
			}
		}
		if source.ID == 0 {
			return sql.ErrNoRows
		}
		return a.render(w, r, "merge.html", page{"Book": source, "Query": query, "Results": results}, 200)
	}
	target, err := strconv.ParseInt(r.FormValue("target"), 10, 64)
	if err != nil || target < 1 {
		return bad("Choose a valid book to merge into.")
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		if _, err := mergeBooks(tx, id, target, true); err != nil {
			return err
		}
		return bump(tx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=merged", target))
}
