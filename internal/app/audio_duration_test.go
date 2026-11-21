package app

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAudioDurationMixedPace(t *testing.T) {
	today := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	cps := []progressCheckpoint{{Day: "2026-11-16", Unit: "percent", Value: intp(26)}, {Day: "2026-11-17", Unit: "seconds", Value: intp(25800)}}
	b := Book{ProgressSeconds: intp(25800), AudioSeconds: intp(72000)}
	got := estimatePace(cps, b, nil, today)
	if got.Rate != "About 118 minutes of audio a day" || got.FinishDate != "2026-11-24" || !strings.Contains(got.Basis, "using total audio 20:00") {
		t.Fatalf("%+v", got)
	}
	if cps[0].Unit != "percent" || *cps[0].Value != 26 {
		t.Fatal("pace rewrote history")
	}
	b.ProgressSeconds = nil
	b.ProgressPercent = intp(40)
	cps = append(cps, progressCheckpoint{Day: "2026-11-18", Unit: "percent", Value: intp(40), TotalSeconds: intp(72000)})
	if got := estimatePace(cps, b, nil, today.AddDate(0, 0, 1)); got.Rate == "" || got.FinishDate == "" {
		t.Fatalf("reverse switch: %+v", got)
	}
	for _, tc := range []struct {
		name        string
		checkpoints []progressCheckpoint
		book        Book
	}{
		{"unknown duration", cps, Book{ProgressPercent: intp(40)}},
		{"changed edition", []progressCheckpoint{{Day: "2026-11-16", Unit: "percent", Value: intp(26), TotalSeconds: intp(36000)}, cps[1]}, Book{ProgressSeconds: intp(25800), AudioSeconds: intp(72000)}},
		{"reset", []progressCheckpoint{{Day: "2026-11-16", Unit: "percent", Value: intp(50)}, cps[1]}, Book{ProgressSeconds: intp(25800), AudioSeconds: intp(72000)}},
		{"clear", []progressCheckpoint{{Day: "2026-11-16", Unit: "percent"}, cps[1]}, Book{ProgressSeconds: intp(25800), AudioSeconds: intp(72000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimatePace(tc.checkpoints, tc.book, nil, today); got.Rate != "" {
				t.Fatalf("%+v", got)
			}
		})
	}
	b = Book{ProgressSeconds: intp(9000), AudioSeconds: intp(36000), Pages: intp(300)}
	if !b.HasProgress() || b.Percent() != 25 || b.ProgressText() != "2:30 of 10:00 · 25% · 7:30 left" {
		t.Fatal(b.ProgressText())
	}
	b.ProgressSeconds = intp(40000)
	if b.Percent() != 100 || !strings.Contains(b.ProgressText(), "0:00 left") {
		t.Fatal(b.ProgressText())
	}
}

func TestAudioDurationSaveRestore(t *testing.T) {
	a, b, _, id := readingApp(t)
	prefs, _ := readMeta(a.store.DB, "preferences", defaultPreferences)
	prefs.ProgressHistory = true
	if err := setMeta(a.store.DB, "preferences", prefs); err != nil {
		t.Fatal(err)
	}
	path := bookPath(id) + "/progress"
	revision := revisionOf(t, a)
	expect(t, b.form(path, url.Values{"unit": {"seconds"}, "progress": {"2:30"}, "audio_duration": {"10:00"}}), 303)
	book, _ := getBook(a.store.DB, id)
	if book.Percent() != 25 || !book.HasProgress() || book.Status != "reading" || revisionOf(t, a) != revision {
		t.Fatalf("%+v", book)
	}
	for _, page := range []string{bookPath(id), "/"} {
		body := b.request("GET", page, nil).Body.String()
		if !strings.Contains(body, "2:30 of 10:00 · 25%") || !strings.Contains(body, `class="progress-meter"`) {
			t.Fatalf("missing audio progress on %s", page)
		}
	}
	cps, err := loadAllCheckpoints(a.store.DB)
	if err != nil || len(cps) != 1 || cps[0].TotalSeconds == nil || *cps[0].TotalSeconds != 36000 {
		t.Fatalf("%+v %v", cps, err)
	}
	for _, raw := range []string{"0:00", "2:60", "bad", "10000:00"} {
		expect(t, b.form(path, url.Values{"unit": {"seconds"}, "progress": {"3:00"}, "audio_duration": {raw}}), 400)
	}
	book, _ = getBook(a.store.DB, id)
	if *book.ProgressSeconds != 9000 || *book.AudioSeconds != 36000 {
		t.Fatal("invalid update partially saved")
	}
	// Quick saves on Home omit duration and must retain it.
	expect(t, b.form(path, url.Values{"unit": {"percent"}, "progress": {"30"}}), 303)
	book, _ = getBook(a.store.DB, id)
	if book.AudioSeconds == nil || book.ProgressSeconds != nil || *book.ProgressPercent != 30 {
		t.Fatalf("%+v", book)
	}
	target := testApp(t, true)
	tb := &browser{t: t, handler: target.Handler()}
	tb.request("GET", "/", nil)
	preview := locationOf(t, uploadRestore(tb, exportOf(t, a)))
	tb.request("GET", preview, nil)
	expect(t, tb.form(preview, nil), 303)
	restored := bookNamed(t, target, "Reading")
	if restored.AudioSeconds == nil || *restored.AudioSeconds != 36000 {
		t.Fatalf("%+v", restored)
	}
	updates, err := loadAllCheckpoints(target.store.DB)
	if err != nil || len(updates) != 1 || updates[0].TotalSeconds == nil || *updates[0].TotalSeconds != 36000 {
		t.Fatalf("%+v %v", updates, err)
	}
	expect(t, b.form(path, url.Values{"unit": {"seconds"}, "progress": {"2:30"}, "audio_duration": {""}}), 303)
	book, _ = getBook(a.store.DB, id)
	if book.AudioSeconds != nil || book.HasProgress() {
		t.Fatal("duration not cleared")
	}
}

func TestMigrateAudioDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.sqlite3")
	if err := migrateFile(path, migrations[:24]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO books(id,title,author,identity,position,added_at,source,reading_status,progress_seconds) VALUES(1,'Audio','A','audio|a',0,'x','manual','reading',9000);
 INSERT INTO reading_attempts(id,book_id,started_on) VALUES(1,1,'2026-11-01');
 INSERT INTO progress_checkpoints(id,attempt_id,book_id,day,unit,value,version) VALUES(1,1,1,'2026-11-16','seconds',9000,4);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateFile(path, migrations); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{"SELECT progress_seconds FROM books": 9000, "SELECT value FROM progress_checkpoints": 9000, "SELECT version FROM progress_checkpoints": 4, "SELECT count(*) FROM books WHERE audio_seconds IS NULL": 1, "SELECT count(*) FROM progress_checkpoints WHERE total_seconds IS NULL": 1} {
		if got := scalar[int](t, path, query); got != want {
			t.Fatalf("%s: %d", query, got)
		}
	}
	if got := backups(t, path); len(got) != 1 || got[0] != path+".pre-v25.bak" {
		t.Fatal(got)
	}
}

func TestAudioDueDatePace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		book Book
		days int
		want string
	}{
		{"position", Book{AudioSeconds: intp(36000), ProgressSeconds: intp(9000)}, 5, "7 hours 30 minutes of audio left · about 90 minutes a day to finish in time"},
		{"percentage", Book{AudioSeconds: intp(36000), ProgressPercent: intp(25)}, 5, "7 hours 30 minutes of audio left · about 90 minutes a day to finish in time"},
		{"not started", Book{AudioSeconds: intp(36000), Pages: intp(300)}, 5, "10 hours of audio left · about 120 minutes a day to finish in time"},
		{"round up seconds", Book{AudioSeconds: intp(36000), ProgressSeconds: intp(35939)}, 2, "2 minutes of audio left · about 1 minute a day to finish in time"},
		{"due today", Book{AudioSeconds: intp(36000), ProgressSeconds: intp(9000)}, 0, "7 hours 30 minutes of audio left"},
		{"overdue", Book{AudioSeconds: intp(36000), ProgressSeconds: intp(9000)}, -1, "7 hours 30 minutes of audio left"},
		{"finished", Book{AudioSeconds: intp(36000), ProgressSeconds: intp(36000)}, 5, ""},
		{"past total", Book{AudioSeconds: intp(36000), ProgressSeconds: intp(36001)}, 5, ""},
		{"unknown total", Book{ProgressSeconds: intp(9000), Pages: intp(300)}, 5, ""},
		{"zero total", Book{AudioSeconds: intp(0), ProgressSeconds: intp(9000)}, 5, ""},
		{"percent without total", Book{ProgressPercent: intp(25)}, 5, "75% left · about 15% a day to finish in time"},
		{"page tracking", Book{AudioSeconds: intp(36000), Pages: intp(300), ProgressPage: intp(100)}, 5, "200 pages left · about 40 pages a day to finish in time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (dueSoon{Book: tc.book, Days: tc.days}).Pace(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAudioDueDatePaceOnHomeAndCalendar(t *testing.T) {
	t.Parallel()
	a, b := calendarApp(t)
	today := time.Now()
	a.now = func() time.Time { return today }
	addRecords(t, a, records("Audio Due"))
	id := bookIDs(t, a)["Audio Due"]
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='reading',pages=300,audio_seconds=36000,progress_seconds=9000 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	setLoan(t, a, "Audio Due", "libby", "audiobook", "borrowed", today.AddDate(0, 0, 5).Format(time.DateOnly), "")
	revision, snapshot := revisionOf(t, a), queueSnapshot(t, a)
	for _, path := range []string{"/", "/calendar"} {
		rr := b.request("GET", path, nil)
		expect(t, rr, 200)
		if !strings.Contains(rr.Body.String(), "7 hours 30 minutes of audio left · about 90 minutes a day to finish in time") {
			t.Fatalf("missing audio deadline guidance on %s", path)
		}
	}
	if revisionOf(t, a) != revision || queueSnapshot(t, a) != snapshot {
		t.Fatal("viewing audio deadline guidance changed the library")
	}
}
