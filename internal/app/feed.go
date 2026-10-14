package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
)

type Connection struct {
	UserID  string `json:"user_id"`
	Shelf   string `json:"shelf"`
	Enabled bool   `json:"enabled"`
	// ReadShelf is polled for reading history; empty turns that off.
	ReadShelf string `json:"read_shelf"`
}

var defaultConnection = Connection{Shelf: "to-read", ReadShelf: "read"}

var userIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var shelfPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)

func validateConnection(c Connection) error {
	if c.Enabled && c.UserID == "" {
		return fmt.Errorf("enter your Goodreads user ID before enabling polling")
	}
	if c.UserID != "" && (!userIDPattern.MatchString(c.UserID) || !shelfPattern.MatchString(c.Shelf)) {
		return fmt.Errorf("enter a numeric Goodreads user ID and a shelf with letters, digits, hyphens or underscores")
	}
	if c.ReadShelf != "" && !shelfPattern.MatchString(c.ReadShelf) {
		return fmt.Errorf("enter a read shelf with letters, digits, hyphens or underscores, or leave it blank")
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
	req.Header.Set("User-Agent", "NextChapter/"+Version+" (personal reading queue)")
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
		//lint:ignore ST1005 Goodreads is a proper noun
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
	// The feed asks for the newest additions first, so a busy shelf's latest
	// books are never cut off. New books join the end of the queue in the
	// order they are imported, so apply them oldest first, as Goodreads
	// numbers a shelf.
	slices.Reverse(records)
	return records, nil
}
