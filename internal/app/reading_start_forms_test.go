package app

import (
	"net/url"
	"testing"
	"time"
)

func TestLocalReadingTransitionsDefaultStart(t *testing.T) {
	for _, action := range []string{"start button", "edit status", "restart through edit", "add as reading"} {
		t.Run(action, func(t *testing.T) {
			a := testApp(t, true)
			a.now = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }
			b := &browser{t: t, handler: a.Handler()}
			if action == "add as reading" {
				expect(t, b.form("/books/new", bookForm("New reader", "reading")), 303)
			} else {
				id := addBooks(t, a, Book{Title: "New reader", Author: "Author"})[0]
				if action == "restart through edit" {
					if _, err := a.store.DB.Exec("UPDATE books SET reading_status='abandoned',started_at='2026-10-01' WHERE id=?", id); err != nil {
						t.Fatal(err)
					}
				}
				if action == "start button" {
					expect(t, b.form(bookPath(id)+"/status", url.Values{"action": {"start"}}), 303)
				} else {
					fields := bookForm("New reader", "reading")
					if action != "restart through edit" {
						fields.Set("started_at", "")
					}
					expect(t, b.form(bookPath(id)+"/edit", fields), 303)
				}
			}
			book := bookNamed(t, a, "New reader")
			if book.Status != "reading" || book.StartedAt == nil || *book.StartedAt != "2026-11-17" {
				t.Fatalf("start not dated: %+v", book)
			}
		})
	}
}

func TestCurrentStartDateQuickSave(t *testing.T) {
	a := testApp(t, true)
	b := &browser{t: t, handler: a.Handler()}
	id := addBooks(t, a, Book{Title: "Reading dates", Author: "Author"})[0]
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='reading',audio_seconds=34560,progress_seconds=28380 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	path := bookPath(id) + "/start-date"
	expect(t, b.form(path, url.Values{"started_at": {"2026-11-01"}}), 303)
	book, _ := getBook(a.store.DB, id)
	if book.StartedAt == nil || *book.StartedAt != "2026-11-01" || !book.StartedManual || *book.AudioSeconds != 34560 || *book.ProgressSeconds != 28380 || book.Status != "reading" {
		t.Fatalf("date save changed other fields: %+v", book)
	}
	expect(t, b.form(path, url.Values{"started_at": {"not a date"}}), 400)
	book, _ = getBook(a.store.DB, id)
	if *book.StartedAt != "2026-11-01" {
		t.Fatal("invalid date changed start")
	}
	expect(t, b.form(path, url.Values{"started_at": {""}}), 303)
	book, _ = getBook(a.store.DB, id)
	if book.StartedAt != nil || !book.StartedManual {
		t.Fatal("clear not saved as local edit")
	}
	if dbValue[int](t, a, "SELECT count(*) FROM reading_log") != 0 {
		t.Fatal("saving start added a finished reading")
	}
	if _, err := a.store.DB.Exec("UPDATE books SET reading_status='to-read' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	expect(t, b.form(path, url.Values{"started_at": {"2026-11-01"}}), 400)
}
