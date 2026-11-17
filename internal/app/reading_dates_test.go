package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadingStartDatesFinishAndRestore(t *testing.T) {
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	id := addBooks(t, a, Book{Title: "Dates", Author: "Writer"})[0]
	a.now = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }
	expect(t, b.form(bookPath(id)+"/status", url.Values{"action": {"start"}}), 303)
	book, _ := getBook(a.store.DB, id)
	if book.StartedAt == nil || *book.StartedAt != "2026-11-17" {
		t.Fatalf("start not recorded: %+v", book)
	}
	expect(t, b.form("/undo", nil), 303)
	book, _ = getBook(a.store.DB, id)
	if book.Status != "to-read" || book.StartedAt != nil {
		t.Fatalf("undo left the start: %+v", book)
	}
	expect(t, b.form(bookPath(id)+"/status", url.Values{"action": {"start"}}), 303)
	fields := url.Values{"title": {"Dates"}, "author": {"Writer"}, "interest": {"3"}, "reading_status": {"reading"}, "started_at": {"2026-11-01"}, "audio_duration": {"12:30"}}
	expect(t, b.form(bookPath(id)+"/edit", fields), 303)
	book, _ = getBook(a.store.DB, id)
	if book.StartedAt == nil || *book.StartedAt != "2026-11-01" || !book.StartedManual || *book.AudioSeconds != 45000 {
		t.Fatalf("edit: %+v", book)
	}
	expect(t, b.form(bookPath(id)+"/finish", url.Values{"started_at": {"2026-11-19"}, "finished_at": {"2026-11-18"}}), 400)
	if dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "reading" {
		t.Fatal("invalid date partially saved")
	}
	expect(t, b.form(bookPath(id)+"/finish", url.Values{"finished_at": {"2026-11-18"}, "rating": {"4"}}), 303)
	rs := readingsOf(t, a, "Dates")
	if len(rs) != 1 || rs[0].StartedAt == nil || *rs[0].StartedAt != "2026-11-01" {
		t.Fatalf("finish lost start: %+v", rs)
	}
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	tb.request("GET", "/", nil)
	loc := locationOf(t, uploadRestore(tb, exportOf(t, a)))
	expect(t, tb.form(loc, nil), 303)
	restored := readingsOf(t, target, "Dates")
	if len(restored) != 1 || restored[0].StartedAt == nil || *restored[0].StartedAt != "2026-11-01" {
		t.Fatalf("restore lost start: %+v", restored)
	}
	book = bookNamed(t, target, "Dates")
	if book.AudioSeconds == nil || *book.AudioSeconds != 45000 {
		t.Fatal("restore lost duration")
	}
}

func TestHardcoverImportStartDatesAndAudioDuration(t *testing.T) {
	a := testApp(t, true)
	importResultFor(t, a, []Record{finished("From Goodreads", "2026-10-31", 4)})
	item := hcPullItem{HardcoverBookID: 500, Title: "From Goodreads", Author: "Author", StatusID: 3, AudioSeconds: 36000, Reads: []hcPullRead{{ID: 801, StartedAt: "2026-10-03", FinishedAt: "2026-10-31", Precision: 1}}}
	pull := func() pullResult {
		t.Helper()
		var result pullResult
		if err := a.store.Write(func(tx *sql.Tx) error {
			var err error
			result, err = applyPull(tx, []hcPullItem{item}, pullOptions{}, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	pull()
	rs := readingsOf(t, a, "From Goodreads")
	if len(rs) != 1 || rs[0].StartedAt == nil || *rs[0].StartedAt != "2026-10-03" || rs[0].HardcoverReadID == nil {
		t.Fatalf("same-day reading not linked/filled: %+v", rs)
	}
	if bookNamed(t, a, "From Goodreads").AudioSeconds == nil {
		t.Fatal("duration not imported")
	}
	if pull().Changed {
		t.Fatal("repeat pull changed data")
	}
	b := &browser{t: t, handler: a.Handler()}
	expect(t, b.form(fmt.Sprintf("/readings/%d", rs[0].ID), url.Values{"action": {"save"}, "started_at": {"2026-10-02"}, "finished_at": {"2026-10-31"}, "rating": {"4"}}), 303)
	item.Reads[0].StartedAt = "2026-10-05"
	item.AudioSeconds = 72000
	pull()
	rs = readingsOf(t, a, "From Goodreads")
	if *rs[0].StartedAt != "2026-10-02" || *bookNamed(t, a, "From Goodreads").AudioSeconds != 36000 {
		t.Fatal("pull overwrote local dates or existing duration")
	}
	// A future Goodreads export keeps the manual start and original reading.
	importResultFor(t, a, []Record{finished("From Goodreads", "2026-10-31", 5)})
	rs = readingsOf(t, a, "From Goodreads")
	if len(rs) != 1 || *rs[0].StartedAt != "2026-10-02" || *rs[0].Rating != 4 {
		t.Fatalf("Goodreads overwrote edit: %+v", rs)
	}
}

func TestHardcoverSelectedEditionDuration(t *testing.T) {
	for _, tc := range []struct {
		name, edition, reads string
		want                 int
	}{
		{"shelf audio", `{"id":2,"audio_seconds":36000}`, `[]`, 36000},
		{"read audio overrides shelf", `{"id":2,"audio_seconds":36000}`, `[{"id":1,"edition":{"audio_seconds":72000}}]`, 72000},
		{"read print overrides audio", `{"id":2,"audio_seconds":36000}`, `[{"id":1,"edition":{"pages":300}}]`, 0},
		{"unknown read edition", `{"id":2,"audio_seconds":36000}`, `[{"id":1,"edition":{}}]`, 0},
		{"bad duration", `{"id":2,"audio_seconds":-1}`, `[]`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var u hcUserBookJSON
			raw := fmt.Sprintf(`{"book":%s,"edition":%s,"user_book_reads":%s}`, hcBook(500, "Audio", "Writer", 2000), tc.edition, tc.reads)
			if err := json.Unmarshal([]byte(raw), &u); err != nil {
				t.Fatal(err)
			}
			item, ok := u.item()
			if !ok || item.AudioSeconds != tc.want {
				t.Fatalf("%+v", item)
			}
		})
	}
}

func TestHardcoverStartDateSyncAndRemoteStaleness(t *testing.T) {
	f := &progFake{status: 3, ub: 65, finished: []fakeRead{{801, "2026-10-31", 1}}, starts: map[int]string{801: "2026-10-02"}}
	a, b, id := progApp(t, f, "read")
	if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,started_at,finished_at,source,fingerprint,hardcover_read_id) VALUES(?,'2026-10-01','2026-10-31','manual','manual:dates',801)", id); err != nil {
		t.Fatal(err)
	}
	rid := dbValue[int64](t, a, "SELECT id FROM reading_log")
	preview := func() string {
		t.Helper()
		refill(a)
		return locationOf(t, b.form(fmt.Sprintf("/readings/%d/hardcover/send", rid), nil))
	}
	loc := preview()
	body := b.request("GET", loc, nil).Body.String()
	if !strings.Contains(body, "2026-10-01") || !strings.Contains(body, "2026-10-02") || !strings.Contains(body, `value="keep" checked`) {
		t.Fatal("start conflict not previewed")
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"theirs"}}), 303)
	rs := readingsOf(t, a, "Prog Book")
	if *rs[0].StartedAt != "2026-10-02" {
		t.Fatal("remote start not taken")
	}
	if _, err := a.store.DB.Exec("UPDATE reading_log SET started_at='2026-10-03' WHERE id=?", rid); err != nil {
		t.Fatal(err)
	}
	loc = preview()
	expect(t, b.request("POST", loc, url.Values{"use": {"mine"}}), 303)
	f.mu.Lock()
	if f.starts[801] != "2026-10-03" {
		t.Fatal("local start not sent")
	}
	f.mu.Unlock()
	if _, err := a.store.DB.Exec("UPDATE reading_log SET started_at='2026-10-04' WHERE id=?", rid); err != nil {
		t.Fatal(err)
	}
	loc = preview()
	f.mu.Lock()
	f.starts[801] = "2026-10-05"
	f.mu.Unlock()
	expect(t, b.request("POST", loc, url.Values{"use": {"mine"}}), 409)
	if len(f.mutations) != 1 {
		t.Fatal("stale preview sent")
	}
	// Clearing an explicitly remembered start sends null, without creating a reread.
	if _, err := a.store.DB.Exec("UPDATE reading_log SET started_at=NULL WHERE id=?", rid); err != nil {
		t.Fatal(err)
	}
	loc = preview()
	expect(t, b.request("POST", loc, url.Values{"use": {"mine"}}), 303)
	if len(f.finished) != 1 || f.starts[801] != "" {
		t.Fatal("clearing start duplicated or retained it")
	}
}

func TestHardcoverCurrentStartSync(t *testing.T) {
	f := &progFake{status: 2, ub: 65, hasRead: true, readID: 801, starts: map[int]string{801: "2026-11-02"}}
	a, b, id := progApp(t, f, "reading")
	if _, err := a.store.DB.Exec("UPDATE books SET started_at='2026-11-01' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	refill(a)
	loc := locationOf(t, b.form(bookPath(id)+"/hardcover/dates", nil))
	expect(t, b.request("GET", loc, nil), 200)
	expect(t, b.request("POST", loc, url.Values{"use": {"mine"}}), 303)
	if f.starts[801] != "2026-11-01" || len(f.mutations) != 1 {
		t.Fatal("current start not sent")
	}
	raw, _ := json.Marshal(f.mutations[0])
	if strings.Contains(string(raw), "finished_at") || strings.Contains(string(raw), "status") {
		t.Fatalf("current start touched other fields: %s", raw)
	}
	if dbValue[int](t, a, "SELECT count(*) FROM reading_log") != 0 || dbValue[string](t, a, "SELECT reading_status FROM books WHERE id=?", id) != "reading" {
		t.Fatal("current start created finished reading or changed status")
	}
}

func TestReadingStartPartialFinish(t *testing.T) {
	a := testApp(t, true)
	id := addBooks(t, a, Book{Title: "Partial", Author: "Writer"})[0]
	if _, err := a.store.DB.Exec("INSERT INTO reading_log(book_id,started_at,finished_at,finished_precision,source,fingerprint) VALUES(?,'2026-06-15','2026-06-01',2,'hardcover','hardcover:read:801')", id); err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, handler: a.Handler()}
	rid := dbValue[int64](t, a, "SELECT id FROM reading_log")
	expect(t, b.form(fmt.Sprintf("/readings/%d", rid), url.Values{"action": {"save"}, "started_at": {"2026-06-20"}, "finished_at": {"2026-06-01"}, "rating": {"4"}}), 303)
	if dbValue[int](t, a, "SELECT finished_precision FROM reading_log") != 2 {
		t.Fatal("edit invented a precise finish")
	}
	expect(t, b.form(fmt.Sprintf("/readings/%d", rid), url.Values{"action": {"save"}, "started_at": {"2026-07-01"}, "finished_at": {"2026-06-01"}}), 400)
}

func TestMigrateReadingStartDates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:25]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source,reading_status,audio_seconds) VALUES(1,'Audio','A','audio|a',0,'x','manual','reading',36000);
 INSERT INTO reading_log(book_id,finished_at,source,fingerprint) VALUES(1,'2026-10-01','manual','manual:migration');
 INSERT INTO books(id,title,author,identity,position,added_at,source,reading_status) VALUES(2,'Known','B','known|b',1,'x','manual','reading');
 INSERT INTO reading_attempts(book_id,started_on) VALUES(2,'2026-11-01');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[int](t, path, "SELECT audio_seconds FROM books WHERE id=1") != 36000 || scalar[int](t, path, "SELECT count(*) FROM reading_log WHERE started_at IS NULL") != 1 || scalar[int](t, path, "SELECT count(*) FROM books WHERE started_at IS NULL AND started_manual=0") != 1 || scalar[string](t, path, "SELECT started_at FROM books WHERE id=2") != "2026-11-01" {
		t.Fatal("migration lost data or invented starts")
	}
	if got := backups(t, path); len(got) != 1 || got[0] != path+".pre-v26.bak" {
		t.Fatal(got)
	}
}

func TestCurrentStartHistoryAndRestore(t *testing.T) {
	a, b, _, id := readingApp(t)
	prefs, _ := readMeta(a.store.DB, "preferences", defaultPreferences)
	prefs.ProgressHistory = true
	if err := setMeta(a.store.DB, "preferences", prefs); err != nil {
		t.Fatal(err)
	}
	// Turning progress history on after entering the start should keep that
	// known date, without inventing an earlier progress update.
	if err := a.store.Write(func(tx *sql.Tx) error { return saveCurrentStart(tx, id, nullable("2026-11-02"), true) }); err != nil {
		t.Fatal(err)
	}
	expect(t, b.form(bookPath(id)+"/progress", url.Values{"unit": {"percent"}, "progress": {"10"}}), 303)
	if dbValue[string](t, a, "SELECT started_on FROM reading_attempts WHERE book_id=? AND ended_on IS NULL", id) != "2026-11-02" {
		t.Fatal("progress history lost known start")
	}
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	tb.request("GET", "/", nil)
	loc := locationOf(t, uploadRestore(tb, exportOf(t, a)))
	expect(t, tb.form(loc, nil), 303)
	restored := bookNamed(t, target, "Reading")
	if restored.StartedAt == nil || *restored.StartedAt != "2026-11-02" || !restored.StartedManual {
		t.Fatal("restore lost current start or protection")
	}
	// Hardcover must not refill an explicitly cleared local current start.
	if err := a.store.Write(func(tx *sql.Tx) error { return saveCurrentStart(tx, id, nil, true) }); err != nil {
		t.Fatal(err)
	}
	it := hcPullItem{HardcoverBookID: 500, Title: "Reading", Author: bookNamed(t, a, "Reading").Author, StatusID: 2, Reads: []hcPullRead{{ID: 801, StartedAt: "2026-11-05"}}}
	if err := a.store.Write(func(tx *sql.Tx) error { _, err := applyPull(tx, []hcPullItem{it}, pullOptions{}, nil); return err }); err != nil {
		t.Fatal(err)
	}
	if bookNamed(t, a, "Reading").StartedAt != nil || dbValue[string](t, a, "SELECT started_on FROM reading_attempts WHERE book_id=? AND ended_on IS NULL", id) != "" {
		t.Fatal("import undid explicit clear")
	}
}

func TestHardcoverProgressPullFillsDuration(t *testing.T) {
	f := &progFake{status: 2, ub: 65, hasRead: true, readID: 801, seconds: 9000, edAudio: 36000}
	a, b, id := progApp(t, f, "reading")
	if _, err := a.store.DB.Exec("UPDATE books SET progress_seconds=9000 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	refill(a)
	loc := locationOf(t, b.form(bookPath(id)+"/hardcover/progress/pull", nil))
	if body := b.request("GET", loc, nil).Body.String(); !strings.Contains(body, "Total audiobook duration (10:00)") {
		t.Fatal("duration missing from preview")
	}
	expect(t, b.request("POST", loc, url.Values{"use": {"hardcover"}}), 303)
	book, _ := getBook(a.store.DB, id)
	if book.AudioSeconds == nil || *book.AudioSeconds != 36000 || *book.ProgressSeconds != 9000 || book.Status != "reading" {
		t.Fatal("pull did not fill duration safely")
	}
}
