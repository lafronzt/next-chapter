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

func TestAudioProgressInput(t *testing.T) {
	for raw, want := range map[string]int{"0:00": 0, "2:30": 9000, "125:09": 450540, "2:30:15": 9015, "9999:59:59": maxProgressSeconds} {
		got, err := parseAudioPosition(raw)
		if err != nil || got != want {
			t.Fatalf("%s: %d %v", raw, got, err)
		}
		if n, err := parseAudioPosition(audioPosition(got)); err != nil || n != got {
			t.Fatalf("round trip %d: %d %v", got, n, err)
		}
	}
	for _, raw := range []string{"2", "-1:30", "1:60", "1:30:60", "1.5:00", "10000:00", "1:", "1:2:3:4", "999999999999999999999:00"} {
		if _, err := parseAudioPosition(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestAudioProgressSaveSyncHistoryRestore(t *testing.T) {
	f := &progFake{hasRead: true, ub: 70, readID: 80, status: 2, edPages: 300, edAudio: 36000}
	a, b, id := progApp(t, f, "reading")
	prefs, err := readMeta(a.store.DB, "preferences", defaultPreferences)
	if err != nil {
		t.Fatal(err)
	}
	prefs.ProgressHistory = true
	if err := setMeta(a.store.DB, "preferences", prefs); err != nil {
		t.Fatal(err)
	}
	optIn(t, b, id)
	rev := revisionOf(t, a)
	loc := saveProgress(t, a, b, id, "seconds", "2:30")
	if !strings.Contains(loc, "hc-progress-sent") || f.seconds != 9000 || f.pages != 0 {
		t.Fatalf("%s: %+v", loc, f)
	}
	book, _ := getBook(a.store.DB, id)
	if book.ProgressSeconds == nil || *book.ProgressSeconds != 9000 || book.ProgressPercent != nil || book.ProgressPage != nil || book.ProgressText() != "2:30 elapsed" || book.HasProgress() {
		t.Fatalf("%+v", book)
	}
	if _, ok := book.PagesLeft(); ok {
		t.Fatal("audio uses print pages")
	}
	if revisionOf(t, a) != rev {
		t.Fatal("progress moved library revision")
	}
	cps, err := loadAllCheckpoints(a.store.DB)
	if err != nil || len(cps) != 1 || cps[0].Unit != "seconds" || cps[0].Text() != "2:30 elapsed" {
		t.Fatalf("%+v %v", cps, err)
	}
	if rr := b.request("POST", bookPath(id)+"/progress", url.Values{"unit": {"seconds"}, "progress": {"2:60"}}); rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	f.mu.Lock()
	f.seconds = 10815
	f.mu.Unlock()
	b.request("GET", bookPath(id), nil)
	refill(a)
	pull := locationOf(t, b.form(bookPath(id)+"/hardcover/progress/pull", nil))
	b.request("GET", pull, nil)
	expect(t, b.form(pull, url.Values{"use": {"hardcover"}}), 303)
	book, _ = getBook(a.store.DB, id)
	if book.ProgressSeconds == nil || *book.ProgressSeconds != 10815 {
		t.Fatal(book.ProgressSeconds)
	}
	// Correct the newest audio checkpoint using the same hours:minutes input.
	attempt, err := openAttemptID(a.store.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	updates, err := loadCheckpoints(a.store.DB, attempt)
	if err != nil || len(updates) != 1 {
		t.Fatal(updates, err)
	}
	update := updates[0]
	expect(t, b.form(fmt.Sprintf("/checkpoints/%d", update.ID), url.Values{"action": {"save"}, "version": {fmt.Sprint(update.Version)}, "day": {update.Day}, "value": {"3:15:30"}}), 303)
	book, _ = getBook(a.store.DB, id)
	if book.ProgressSeconds == nil || *book.ProgressSeconds != 11730 {
		t.Fatal("checkpoint edit did not update current position")
	}
	raw := exportOf(t, a)
	var exported exportFile
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	tb.request("GET", "/", nil)
	preview := locationOf(t, uploadRestore(tb, raw))
	tb.request("GET", preview, nil)
	expect(t, tb.form(preview, nil), 303)
	if got := bookNamed(t, target, "Prog Book"); got.ProgressSeconds == nil || *got.ProgressSeconds != 11730 {
		t.Fatalf("restore: %+v", got)
	}
	// Switching to percent clears time and sends the equivalent fraction of audio.
	saveProgress(t, a, b, id, "percent", "25")
	book, _ = getBook(a.store.DB, id)
	if book.ProgressSeconds != nil || book.ProgressPercent == nil || *book.ProgressPercent != 25 || f.seconds != 9000 {
		t.Fatalf("switch: %+v / %d", book, f.seconds)
	}
	saveProgress(t, a, b, id, "seconds", "")
	book, _ = getBook(a.store.DB, id)
	if book.ProgressSeconds != nil || book.ProgressPercent != nil || book.ProgressPage != nil {
		t.Fatal("clear failed")
	}
}

func TestAudioProgressConversion(t *testing.T) {
	for _, tc := range []struct {
		remote hcOpenRead
		want   int
		skip   bool
	}{
		{hcOpenRead{EdAudio: 36000, EdPages: 300}, 9000, false},
		{hcOpenRead{EdAudio: 7200}, 7200, false},
		{hcOpenRead{}, 9000, false},
		{hcOpenRead{EdPages: 300}, 9000, true},
	} {
		out := progressToRemote(Book{ProgressSeconds: intp(9000)}, tc.remote)
		if (out.Skip != "") != tc.skip || !tc.skip && (!out.Audio || out.Seconds != tc.want) {
			t.Fatalf("%+v", out)
		}
	}
	zero := progressToRemote(Book{ProgressSeconds: intp(0)}, hcOpenRead{EdAudio: 36000})
	if v := progressVars(3, zero, "")["read"].(map[string]any); v["progress_seconds"] != 0 || len(v) != 1 {
		t.Fatal(v)
	}
	p := progressFromRemote(Book{ProgressSeconds: intp(9000)}, hcOpenRead{EdAudio: 36000})
	if p.Seconds == nil || *p.Seconds != 0 {
		t.Fatalf("zero pull: %+v", p)
	}
	cps := []progressCheckpoint{{Day: "2026-03-01", Unit: "seconds", Value: intp(3600)}, {Day: "2026-03-03", Unit: "seconds", Value: intp(7200)}}
	pace := estimatePace(cps, Book{ProgressSeconds: intp(7200)}, nil, time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC))
	if pace.Rate != "About 30 minutes of audio a day" || pace.FinishDate != "" {
		t.Fatalf("%+v", pace)
	}
}

func TestMigrateAudioAndNoteShares(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:22]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source,reading_status,progress_percent) VALUES(1,'Audio','A','audio|a',0,'x','manual','reading',25);
INSERT INTO reading_attempts(id,book_id,started_on) VALUES(1,1,'2026-01-01');
INSERT INTO progress_checkpoints(id,attempt_id,book_id,day,unit,value,version) VALUES(1,1,1,'2026-01-02','percent',25,4);
INSERT INTO book_notes(id,book_id,body,created_at,updated_at) VALUES(1,1,'Keep private','x','x');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	if scalar[int](t, path, "SELECT progress_percent FROM books") != 25 || scalar[int](t, path, "SELECT version FROM progress_checkpoints") != 4 || scalar[int](t, path, "SELECT hardcover_journal_id FROM book_notes") != 0 {
		t.Fatal("migration altered old data")
	}
	if got := backups(t, path); len(got) != 1 || got[0] != path+".pre-v23.bak" {
		t.Fatal(got)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE books SET progress_percent=NULL,progress_seconds=9000; UPDATE progress_checkpoints SET unit='seconds',value=9000"); err != nil {
		t.Fatal(err)
	}
}
