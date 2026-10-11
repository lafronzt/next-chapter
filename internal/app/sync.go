package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strings"
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

// PollState is the scheduled checker's failure and backoff state.
type PollState struct {
	Failures    int    `json:"failures"`
	LastErrorAt string `json:"last_error_at"`
	NextAttempt string `json:"next_attempt"`
}

const (
	pollInterval   = time.Hour
	maxPollBackoff = 24 * time.Hour
	feedTimeout    = 15 * time.Second
	maxRedirects   = 3
	feedHost       = "www.goodreads.com"
	feedOutage     = "Goodreads could not be checked. Verify your public shelf and saved user ID, or use CSV import."
	pollOutage     = "Feed unavailable. Check that your shelf is public or use CSV import."
)

// Sync failures fall into three kinds: settings the reader must fix, a check that
// is already running, and a feed that could not be fetched or read.
var (
	errSyncConfig      = errors.New("sync configuration")
	errSyncBusy        = errors.New("sync already running")
	errFeedUnavailable = errors.New("feed unavailable")
	errRedirect        = errors.New("redirect rejected")
)

// syncError carries a reader-facing message (never a URL or user ID) and, for
// feed errors, a short class such as "timeout" or "status-503" for logs.
type syncError struct {
	kind    error
	class   string
	message string
	cause   error
}

func (e *syncError) Error() string {
	if e.cause != nil {
		return e.message + ": " + e.cause.Error()
	}
	return e.message
}
func (e *syncError) Unwrap() []error { return []error{e.kind, e.cause} }

func configError(message string) error {
	return &syncError{kind: errSyncConfig, class: "config", message: message}
}
func feedError(class string, cause error) error {
	return &syncError{kind: errFeedUnavailable, class: class, message: feedOutage, cause: cause}
}

// syncMessage returns the text to show for err; manual selects the wording used
// by the Check now button rather than the scheduled poller's status line.
func syncMessage(err error, manual bool) string {
	var s *syncError
	if errors.As(err, &s) && !errors.Is(err, errFeedUnavailable) {
		return s.message
	}
	if manual {
		return feedOutage
	}
	return pollOutage
}
func errorClass(err error) string {
	var s *syncError
	if errors.As(err, &s) {
		return s.class
	}
	return "internal"
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

// feedClient fetches a public Goodreads shelf feed. Tests replace the transport;
// the base URL and redirect policy keep requests on https://www.goodreads.com.
type feedClient struct {
	client *http.Client
	base   string
}

func newFeedClient(transport http.RoundTripper) *feedClient {
	return &feedClient{base: "https://" + feedHost, client: &http.Client{Transport: transport, Timeout: feedTimeout, CheckRedirect: checkFeedRedirect}}
}

// checkFeedRedirect follows at most maxRedirects hops, and only to https on the
// exact Goodreads host (no other port, no credentials), preserving SSRF safety.
func checkFeedRedirect(req *http.Request, via []*http.Request) error {
	u := req.URL
	if len(via) > maxRedirects || u.Scheme != "https" || u.Host != feedHost || u.User != nil {
		return errRedirect
	}
	return nil
}

var defaultFeed = newFeedClient(nil)

func FetchGoodreads(ctx context.Context, c Connection) ([]Record, error) {
	return defaultFeed.Fetch(ctx, c)
}
func (f *feedClient) Fetch(ctx context.Context, c Connection) ([]Record, error) {
	if c.UserID == "" {
		return nil, configError("Save your Goodreads user ID first.")
	}
	if err := validateConnection(c); err != nil {
		return nil, configError("Check your Goodreads settings: " + err.Error() + ".")
	}
	target := f.base + "/review/list_rss/" + c.UserID + "?" + url.Values{"shelf": {c.Shelf}, "sort": {"date_added"}, "order": {"d"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "NextChapter/0.2 (personal reading queue)")
	req.Header.Set("Accept", "application/rss+xml")
	response, err := f.client.Do(req)
	if err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, errRedirect):
			return nil, feedError("redirect", err)
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
			return nil, feedError("timeout", err)
		case errors.Is(err, context.Canceled):
			return nil, err
		}
		return nil, feedError("network", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, feedError(fmt.Sprintf("status-%d", response.StatusCode), fmt.Errorf("Goodreads returned status %d", response.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxUpload+1))
	if err != nil {
		return nil, feedError("network", err)
	}
	if len(data) > maxUpload {
		return nil, feedError("too-large", fmt.Errorf("feed exceeds %d bytes", maxUpload))
	}
	records, err := ParseRSS(data)
	if err != nil {
		return nil, feedError("parse", err)
	}
	return records, nil
}

// safeSync runs syncOnce, turning a panic in fetching, parsing, or importing into
// an ordinary feed error so neither the poller nor a request takes down the app.
func (a *App) safeSync(ctx context.Context, now time.Time) (added int, err error) {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("Goodreads sync panicked: %v\n%s", v, debug.Stack())
			err = feedError("panic", fmt.Errorf("panic: %v", v))
		}
	}()
	return a.syncOnce(ctx, now)
}
func (a *App) syncOnce(ctx context.Context, now time.Time) (int, error) {
	// Serialize manual and scheduled fetches; network I/O never holds a DB transaction.
	if !a.syncMu.TryLock() {
		return 0, &syncError{kind: errSyncBusy, class: "busy", message: "A Goodreads check is already running. Try again in a moment."}
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
			return configError("Your Goodreads settings changed during the check. Try again.")
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
			message = strings.TrimSuffix(message, ".") + fmt.Sprintf("; moved %d library-only books out of To review.", moved)
		}
		if result.Skipped > 0 {
			message = strings.TrimSuffix(message, ".") + fmt.Sprintf("; skipped %d you deleted.", result.Skipped)
		}
		if err = setMeta(tx, "last_sync", SyncResult{canonicalTime(now), message, true}); err != nil {
			return err
		}
		// Any success resets the scheduled checker's backoff.
		if err = setMeta(tx, "poll_state", PollState{NextAttempt: canonicalTime(now.Add(pollInterval))}); err != nil {
			return err
		}
		return setMeta(tx, "last_poll", now.Unix())
	})
	return added, err
}

// pollDue reports whether a scheduled check should run at now. Without a stored
// next attempt (databases from before backoff), it falls back to last_poll.
func pollDue(q queryer, now time.Time) (bool, PollState, error) {
	state, err := readMeta(q, "poll_state", PollState{})
	if err != nil {
		return false, state, err
	}
	if state.NextAttempt != "" {
		next, err := time.Parse(time.RFC3339Nano, state.NextAttempt)
		if err != nil {
			return true, state, nil
		}
		return !now.Before(next), state, nil
	}
	last, err := readMeta(q, "last_poll", float64(0))
	if err != nil {
		return false, state, err
	}
	return float64(now.Unix())-last >= pollInterval.Seconds(), state, nil
}

func pollBackoff(failures int) time.Duration {
	delay := pollInterval
	for i := 1; i < failures && delay < maxPollBackoff; i++ {
		delay *= 2
	}
	return min(delay, maxPollBackoff)
}

// pollIfDue performs everything one poller tick does. State is written only after
// the outcome is known, so a crash mid-attempt retries on the next tick.
func (a *App) pollIfDue(ctx context.Context, now time.Time) {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("Goodreads poller recovered from panic: %v\n%s", v, debug.Stack())
		}
	}()
	conn, err := readMeta(a.store.DB, "sync", Connection{})
	if err != nil {
		log.Printf("read sync settings: %v", err)
		return
	}
	if !conn.Enabled {
		return
	}
	due, state, err := pollDue(a.store.DB, now)
	if err != nil {
		log.Printf("read poll state: %v", err)
		return
	}
	if !due {
		return
	}
	_, err = a.safeSync(ctx, now)
	if err == nil || ctx.Err() != nil || errors.Is(err, errSyncBusy) {
		// A busy skip is not a failure; the next tick tries again.
		return
	}
	state.Failures++
	delay := pollBackoff(state.Failures)
	state.LastErrorAt = canonicalTime(now)
	state.NextAttempt = canonicalTime(now.Add(delay))
	log.Printf("Goodreads poll failed (class=%s, consecutive=%d); next attempt in %s. CSV import remains available.", errorClass(err), state.Failures, delay)
	status := SyncResult{canonicalTime(now), syncMessage(err, false), false}
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := setMeta(tx, "last_sync", status); err != nil {
			return err
		}
		return setMeta(tx, "poll_state", state)
	})
	if err != nil {
		log.Printf("record sync status: %v", err)
	}
}

func (a *App) Poll(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.pollIfDue(ctx, a.now())
		}
	}
}
