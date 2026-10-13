package app

import (
	"database/sql"
	"net/http"
	"time"
)

func (a *App) demo(w http.ResponseWriter, r *http.Request) error {
	if !a.config.Demo {
		return sql.ErrNoRows
	}
	err := a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var count int
		err := tx.QueryRow("SELECT count(*) FROM books").Scan(&count)
		if err != nil {
			return err
		}
		if count != 0 {
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
