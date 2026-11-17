package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HardcoverConfig contains operator-only credentials. Never serialize it.
type HardcoverConfig struct {
	Token string `json:"-"`
}

func LoadHardcoverConfig(getenv func(string) string) (HardcoverConfig, error) {
	token := strings.TrimSpace(getenv("HARDCOVER_API_TOKEN"))
	if strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 || len(token) > 8192 {
		return HardcoverConfig{}, errors.New("HARDCOVER_API_TOKEN must be a single bearer token")
	}
	return HardcoverConfig{Token: token}, nil
}

type hardcoverError string

func (e hardcoverError) Error() string { return string(e) }

const (
	errHardcoverAuth        = hardcoverError("Hardcover rejected the token. Check its expiry or reset it at hardcover.app/account/api.")
	errHardcoverUnavailable = hardcoverError("Hardcover is unavailable. Nothing changed; try again later.")
	errHardcoverTimeout     = hardcoverError("Hardcover timed out. Nothing changed; try again later.")
	errHardcoverCapacity    = hardcoverError("Hardcover request exceeds the burst capacity. Use a smaller batch.")
	errHardcoverResponse    = hardcoverError("Hardcover returned an unexpected response. Nothing changed.")
)

type hardcoverScopeError struct{ Need string }

func (e hardcoverScopeError) Error() string {
	return "Hardcover needs scope " + e.Need + ". Check hardcover.app/account/api."
}

type hardcoverRateError struct{ RetryAfter time.Duration }

func (e hardcoverRateError) Error() string {
	return fmt.Sprintf("Hardcover rate limit reached. Try again in %d seconds.", int64(e.RetryAfter.Seconds())+1)
}

// All calls share one budget. Reserve before sending, and never hold a database
// transaction during HTTP. A 429 blocks subsequent calls; it is never retried.
type hardcoverClient struct {
	http                          *http.Client
	endpoint, token               string
	mu                            sync.Mutex
	now                           func() time.Time
	tokens                        float64
	minuteAt, dayAt, blockedUntil time.Time
	daily                         int
	remaining                     int
	serverBudgets                 []hardcoverBudget
	// searchTimeout bounds a search request; tests lengthen it so a loaded
	// machine cannot turn a fake server's slow answer into a timeout.
	searchTimeout time.Duration
	// scopes and failure are what this process has seen, for the status block.
	scopes  map[string]hcScopeSeen
	failure *hcFailure
}

// hcScopeSeen says whether a scope worked, or was refused, and when.
type hcScopeSeen struct {
	OK bool
	At time.Time
}

// hcFailure is the last request that failed, cleared by the next success.
type hcFailure struct {
	Err error
	At  time.Time
}

// scopeFor names the scope a query needs, from its text. Queries belong to
// this package, so the match is on what they read or write.
func scopeFor(query string) string {
	switch {
	case strings.Contains(query, "mutation"):
		return "write:library"
	case strings.Contains(query, "NextChapterConnection"):
		return "read:me"
	case strings.Contains(query, "user_books"):
		return "read:library"
	}
	return "read:catalog"
}

// note records the outcome of one call for the status block.
func (c *hardcoverClient) note(query string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.scopes == nil {
		c.scopes = map[string]hcScopeSeen{}
	}
	now := c.now()
	var scope hardcoverScopeError
	switch {
	case err == nil:
		c.scopes[scopeFor(query)] = hcScopeSeen{true, now}
		c.failure = nil
	case errors.As(err, &scope):
		c.scopes[scopeFor(query)] = hcScopeSeen{false, now}
		c.failure = &hcFailure{err, now}
	default:
		c.failure = &hcFailure{err, now}
	}
}

// Status is a copy of what the client has seen, safe to read without the lock.
func (c *hardcoverClient) Status() (scopes map[string]hcScopeSeen, failure *hcFailure, blockedUntil time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	scopes = make(map[string]hcScopeSeen, len(c.scopes))
	for k, v := range c.scopes {
		scopes[k] = v
	}
	if c.failure != nil {
		f := *c.failure
		failure = &f
	}
	return scopes, failure, c.blockedUntil
}

func newHardcoverClient(c HardcoverConfig, transport *http.Client, endpoint string) *hardcoverClient {
	if c.Token == "" {
		return nil
	}
	if endpoint == "" {
		endpoint = "https://api.hardcover.app/v1/graphql"
	}
	h := http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 30 * time.Second}}
	if transport != nil {
		h = *transport
	}
	h.Timeout = 30 * time.Second
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := time.Now()
	return &hardcoverClient{http: &h, endpoint: endpoint, token: c.Token, now: time.Now, tokens: 10, minuteAt: now, dayAt: now, daily: 5000, remaining: -1, searchTimeout: 3 * time.Second}
}

// Queries belong to this package. Count aliases independently and nested fields
// for free. Reject fragments/directives instead of undercounting their cost.
var hardcoverTokens = regexp.MustCompile(`"(?:\\.|[^"\\])*"|#[^\n]*|[_A-Za-z][_0-9A-Za-z]*|[{}():@]|\.\.\.`)

func hardcoverCost(query string) (int, bool) {
	depth, parens, cost := 0, 0, 0
	alias := false
	search := false
	for _, s := range hardcoverTokens.FindAllString(query, -1) {
		if strings.HasPrefix(s, "#") || strings.HasPrefix(s, `"`) {
			continue
		}
		switch s {
		case "...", "@":
			return 0, false
		case "(":
			parens++
		case ")":
			parens--
		case "{":
			if parens == 0 {
				depth++
			}
		case "}":
			if parens == 0 {
				depth--
			}
		case ":":
			if depth == 1 && parens == 0 {
				alias = true
			}
		default:
			if depth == 1 && parens == 0 {
				if !alias {
					cost++
				}
				alias = false
				if s == "search" {
					search = true
				}
			}
		}
	}
	if depth != 0 || parens != 0 {
		return 0, false
	}
	return cost, search
}

type hardcoverBudget struct {
	remaining int
	until     time.Time
}

func (c *hardcoverClient) reserve(cost int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if now.Before(c.blockedUntil) {
		return hardcoverRateError{c.blockedUntil.Sub(now)}
	}
	if now.Sub(c.dayAt) >= 24*time.Hour {
		c.daily = 5000
		c.dayAt = now
	}
	c.tokens = min(10, c.tokens+max(0, now.Sub(c.minuteAt).Seconds()))
	c.minuteAt = now
	if c.daily < cost {
		return hardcoverRateError{max(time.Second, c.dayAt.Add(24*time.Hour).Sub(now))}
	}
	if c.tokens < float64(cost) {
		return hardcoverRateError{time.Duration(float64(cost)-c.tokens)*time.Second + time.Second}
	}
	for _, b := range c.serverBudgets {
		if now.Before(b.until) && b.remaining < cost {
			return hardcoverRateError{b.until.Sub(now)}
		}
	}
	for i := range c.serverBudgets {
		if now.Before(c.serverBudgets[i].until) {
			c.serverBudgets[i].remaining -= cost
		}
	}
	c.tokens -= float64(cost)
	c.daily -= cost
	return nil
}
func hardcoverRetry(raw string, now time.Time) time.Duration {
	if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 86400 {
		return max(time.Second, time.Duration(n)*time.Second)
	}
	if at, err := http.ParseTime(raw); err == nil {
		return min(24*time.Hour, max(time.Second, at.Sub(now)))
	}
	return time.Minute
}

var hardcoverHeaderValue = regexp.MustCompile(`(?:^|[,; ])(r|remaining|t|reset)=([0-9]+)`)

func (c *hardcoverClient) observe(h http.Header, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Multiple policies can appear in one header, each with its own reset.
	// Do not mistake a per-minute remaining count for the daily allowance.
	var budgets []hardcoverBudget
	remaining := -1
	for _, policy := range strings.Split(h.Get("RateLimit"), ",") {
		count, reset := -1, 60
		for _, m := range hardcoverHeaderValue.FindAllStringSubmatch(policy, -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil {
				continue
			}
			if m[1] == "r" || m[1] == "remaining" {
				count = n
			} else {
				reset = n
			}
		}
		if count >= 0 {
			budgets = append(budgets, hardcoverBudget{count, c.now().Add(time.Duration(min(max(reset, 1), 86400)) * time.Second)})
			if remaining < 0 || count < remaining {
				remaining = count
			}
		}
	}
	if len(budgets) == 0 {
		if count, err := strconv.Atoi(h.Get("X-RateLimit-Remaining")); err == nil && count >= 0 {
			reset := time.Minute
			if epoch, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				reset = min(24*time.Hour, max(time.Second, time.Unix(epoch, 0).Sub(c.now())))
			}
			budgets = append(budgets, hardcoverBudget{count, c.now().Add(reset)})
			remaining = count
		}
	}
	if len(budgets) > 0 {
		c.serverBudgets = budgets
		c.remaining = remaining
	}
	if status == 429 {
		c.blockedUntil = c.now().Add(hardcoverRetry(h.Get("Retry-After"), c.now()))
	}
}
func (c *hardcoverClient) knownRemaining() int { c.mu.Lock(); defer c.mu.Unlock(); return c.remaining }

func (c *hardcoverClient) do(ctx context.Context, query string, vars any, out any) (err error) {
	if c != nil {
		defer func() { c.note(query, err) }()
	}
	if c == nil {
		return hardcoverError("Hardcover is not configured.")
	}
	cost, search := hardcoverCost(query)
	if cost < 1 || cost > 10 {
		return errHardcoverCapacity
	}
	if err := c.reserve(cost); err != nil {
		return err
	}
	timeout := 30 * time.Second
	if search {
		timeout = c.searchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return errHardcoverResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return errHardcoverResponse
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "NextChapter/"+Version+" (+self-hosted)")
	resp, err := c.http.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errHardcoverTimeout
		}
		return errHardcoverUnavailable
	}
	defer resp.Body.Close()
	c.observe(resp.Header, resp.StatusCode)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1_048_577))
	if err != nil || len(data) > 1_048_576 {
		return errHardcoverResponse
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code  string `json:"code"`
				Scope string `json:"required_scope"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	// Never propagate provider text: it can echo credentials or arbitrary markup.
	decodeErr := json.Unmarshal(data, &envelope)
	switch resp.StatusCode {
	case 401:
		return errHardcoverAuth
	case 403:
		if bytes.Contains(data, []byte("request_exceeds_capacity")) {
			return errHardcoverCapacity
		}
		return hardcoverScopeError{hardcoverNeededScope(data)}
	case 408:
		return errHardcoverTimeout
	case 429:
		return hardcoverRateError{hardcoverRetry(resp.Header.Get("Retry-After"), c.now())}
	case 503, 502, 504:
		return errHardcoverUnavailable
	}
	if resp.StatusCode != 200 || decodeErr != nil {
		return errHardcoverResponse
	}
	if len(envelope.Errors) > 0 {
		for _, e := range envelope.Errors {
			switch e.Extensions.Code {
			case "invalid-jwt", "access-denied", "invalid-headers":
				return errHardcoverAuth
			case "insufficient_scope":
				return hardcoverScopeError{hardcoverNeededScope(data)}
			}
		}
		return errHardcoverResponse
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, out) != nil {
		return errHardcoverResponse
	}
	return nil
}
func hardcoverNeededScope(data []byte) string {
	for _, s := range []string{"read:me", "read:library", "write:library", "read:catalog", "read:lists", "write:lists"} {
		if bytes.Contains(data, []byte(s)) {
			return s
		}
	}
	return "required by this operation (not reported)"
}

type hardcoverLink struct {
	UserID   int64  `json:"hardcover_user_id"`
	Username string `json:"username"`
	LinkedAt string `json:"linked_at"`
}

func loadHardcoverLink(q queryer) (*hardcoverLink, error) {
	var l hardcoverLink
	err := q.QueryRow("SELECT hardcover_user_id,username,linked_at FROM hardcover_link WHERE id=1").Scan(&l.UserID, &l.Username, &l.LinkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &l, err
}
