package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

type Connection struct {
	UserID  string `json:"user_id"`
	Shelf   string `json:"shelf"`
	Enabled bool   `json:"enabled"`
}
type SyncResult struct {
	At      string `json:"at"`
	Message string `json:"message"`
	OK      bool   `json:"ok"`
}

var userIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var shelfPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)

func validateConnection(c Connection) error {
	if c.Enabled && c.UserID == "" {
		return fmt.Errorf("enter your Goodreads user ID before enabling polling")
	}
	if c.UserID != "" && (!userIDPattern.MatchString(c.UserID) || !shelfPattern.MatchString(c.Shelf)) {
		return fmt.Errorf("enter a numeric Goodreads user ID and a shelf with letters, digits, hyphens or underscores")
	}
	return nil
}
func FetchGoodreads(ctx context.Context, c Connection) ([]Record, error) {
	if c.UserID == "" {
		return nil, fmt.Errorf("save your Goodreads user ID first")
	}
	if err := validateConnection(c); err != nil {
		return nil, err
	}
	target := "https://www.goodreads.com/review/list_rss/" + c.UserID + "?" + url.Values{"shelf": {c.Shelf}, "sort": {"date_added"}, "order": {"d"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "NextChapter/0.2 (personal reading queue)")
	req.Header.Set("Accept", "application/rss+xml")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Goodreads redirected the feed; use CSV import")
	}}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Goodreads returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxUpload+1))
	if err != nil {
		return nil, err
	}
	return ParseRSS(data)
}
func (a *App) syncOnce(ctx context.Context) (int, error) {
	// Serialize manual and scheduled fetches; network I/O never holds a DB transaction.
	if !a.syncMu.TryLock() {
		return 0, fmt.Errorf("a Goodreads check is already running")
	}
	defer a.syncMu.Unlock()
	conn, err := readMeta(a.store.DB, "sync", Connection{Shelf: "to-read"})
	if err != nil {
		return 0, err
	}
	records, err := a.fetch(ctx, conn)
	if err != nil {
		return 0, err
	}
	added := 0
	err = a.store.Write(func(tx *sql.Tx) error {
		current, err := readMeta(tx, "sync", Connection{Shelf: "to-read"})
		if err != nil {
			return err
		}
		if current != conn {
			return fmt.Errorf("connection settings changed; try again")
		}
		result, err := importRecords(tx, records)
		if err != nil {
			return err
		}
		added = result.Added
		if result.Changed {
			if err = bump(tx); err != nil {
				return err
			}
		}
		message := fmt.Sprintf("Found %d feed entries; added %d books.", len(records), added)
		if moved := result.upgradedTotal(); moved > 0 {
			message = fmt.Sprintf("Found %d feed entries; added %d books; moved %d library-only books out of To review.", len(records), added, moved)
		}
		return setMeta(tx, "last_sync", SyncResult{canonicalTime(time.Now()), message, true})
	})
	return added, err
}
func (a *App) Poll(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			conn, err := readMeta(a.store.DB, "sync", Connection{})
			if err != nil {
				log.Printf("read sync settings: %v", err)
				continue
			}
			last, err := readMeta(a.store.DB, "last_poll", float64(0))
			if err != nil || !conn.Enabled || float64(time.Now().Unix())-last < 3600 {
				continue
			}
			if err = setMeta(a.store.DB, "last_poll", time.Now().Unix()); err != nil {
				log.Printf("record poll: %v", err)
				continue
			}
			if _, err = a.syncOnce(ctx); err != nil && ctx.Err() == nil {
				log.Print("Goodreads sync failed; CSV import remains available.")
				if err = setMeta(a.store.DB, "last_sync", SyncResult{canonicalTime(time.Now()), "Feed unavailable. Check that your shelf is public or use CSV import.", false}); err != nil {
					log.Printf("record sync status: %v", err)
				}
			}
		}
	}
}
