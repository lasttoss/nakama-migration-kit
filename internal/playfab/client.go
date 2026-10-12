// Package playfab reads players from a live PlayFab title's Admin API.
//
// It is the transport half of the kit: listing, paging, retrying, and the error taxonomy. It
// knows nothing about what a particular game means by "energy" - that is internal/profile - so
// the same code works against a title whose user data is keyed "energy" and one keyed
// "player_energy".
//
// Three things about this API decide the shape of the code, and all three are load bearing:
//
//   - PlayFab answers HTTP 200 with a non-200 `code` inside the envelope for many errors. A
//     client that only looks at the HTTP status treats a rate limit as a success and then
//     unmarshals an empty data object, which imports nothing and reports no error. Both codes
//     are checked here.
//   - A 429 is normal on a busy title, not exceptional. It is retried with the Retry-After the
//     title sent, because ignoring that header is how an importer turns a slow title into a
//     title that is rate limited for everyone.
//   - A 4xx that is not 429 will never succeed on retry. Retrying it wastes the wall clock the
//     rate limit is already spending, so it fails immediately with the title's own message.
//
// The client is safe for concurrent use: the importer runs several players at once, and
// Stats is what the plan and the report read.
package playfab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lasttoss/nakama-migration-kit/internal/source"
)

// Config configures a client. Only TitleID and SecretKey are required for a live title.
type Config struct {
	// BaseURL is the Admin API root. Empty means PlayFab's; a test points it at a fake.
	BaseURL string
	// TitleID is the title the Admin API calls are scoped to.
	TitleID string
	// SecretKey is the title's admin secret. It is read from the environment, never logged.
	SecretKey string
	// HTTPClient is the transport. Empty means a client with the timeouts below.
	HTTPClient *http.Client
	// Timeout applies to one request. Defaults to 30s.
	Timeout time.Duration
	// PageSize is how many ids one listing page asks for. Defaults to 100.
	PageSize int
	// MaxRetries is how many extra attempts a retryable failure gets. Defaults to 5.
	MaxRetries int
	// BaseBackoff is the first retry delay; it doubles per attempt up to MaxBackoff. Defaults
	// to 500ms, and tests set it to a microsecond.
	BaseBackoff time.Duration
	// MaxBackoff caps the delay. Defaults to 30s.
	MaxBackoff time.Duration
	// SegmentID is the PlayFab segment the listing walks. Defaults to "All Players".
	SegmentID string
	// OnRetry, if set, is called before each retry wait. It exists so a run can say out loud
	// that it is being rate limited instead of looking hung.
	OnRetry func(attempt int, wait time.Duration, err error)
}

// Stats is what the client has done, for the plan estimate and the run report.
type Stats struct {
	Requests    int `json:"requests"`
	Retries     int `json:"retries"`
	RateLimited int `json:"rate_limited"`
	ServerError int `json:"server_errors"`
	Permanent   int `json:"permanent_errors"`
}

// Client is an Admin API client.
type Client struct {
	cfg  Config
	http *http.Client

	mu    sync.Mutex
	stats Stats
}

// New builds a client.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://" + cfg.TitleID + ".playfabapi.com"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 100
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 5
	}
	if cfg.BaseBackoff == 0 {
		cfg.BaseBackoff = 500 * time.Millisecond
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.SegmentID == "" {
		cfg.SegmentID = "All Players"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{cfg: cfg, http: hc}
}

// Name identifies the backend, and is part of every derived custom id.
func (c *Client) Name() string { return "playfab" }

// Stats returns a copy of the counters.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// envelope is the part of every PlayFab response that matters: the code, and the message that
// explains it when it is not 200.
type envelope struct {
	Code         int    `json:"code"`
	Status       string `json:"status"`
	ErrorMessage string `json:"errorMessage"`
}

// call performs one Admin API request with retries, and unmarshals the envelope plus the data.
func (c *Client) call(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("playfab: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := c.backoff(attempt, lastErr)
			if c.cfg.OnRetry != nil {
				c.cfg.OnRetry(attempt, wait, lastErr)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("playfab: %w", ctx.Err())
			case <-time.After(wait):
			}
		}

		status, raw, header, err := c.do(ctx, path, payload)
		c.count(func(s *Stats) { s.Requests++ })
		if err != nil {
			// A transport error is retryable: it says nothing about the request.
			lastErr = fmt.Errorf("%s", c.redact(err.Error()))
			c.count(func(s *Stats) { s.Retries++ })
			continue
		}

		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			lastErr = fmt.Errorf("playfab: %s: response is not JSON (%s): %w", path, c.redact(preview(raw)), err)
			c.count(func(s *Stats) { s.Retries++ })
			continue
		}

		// The HTTP status and the envelope code can disagree, and PlayFab uses both.
		code := env.Code
		if code == 0 {
			code = status
		}
		if code == 200 {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("playfab: %s: decode data: %w", path, err)
			}
			return nil
		}

		apiErr := &APIError{
			StatusCode: status,
			Code:       code,
			Name:       env.Status,
			// A title's error text is echoed back to us and then written to a log on a machine
			// somebody else can read. It can quote the key we sent (an auth failure frequently
			// does), so the key is removed before the error travels any further.
			Message:    c.redact(env.ErrorMessage),
			RetryAfter: header.Get("Retry-After"),
			Method:     path,
		}
		if apiErr.Retryable() {
			lastErr = apiErr
			c.count(func(s *Stats) {
				s.Retries++
				if apiErr.RateLimited() {
					s.RateLimited++
				}
				if apiErr.ServerError() {
					s.ServerError++
				}
			})
			continue
		}
		c.count(func(s *Stats) { s.Permanent++ })
		return apiErr
	}
	return fmt.Errorf("playfab: %s: giving up after %d attempts: %w", path, c.cfg.MaxRetries+1, lastErr)
}

func (c *Client) do(ctx context.Context, path string, payload []byte) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("playfab: %s: build request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.SecretKey != "" {
		req.Header.Set("X-SecretKey", c.cfg.SecretKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("playfab: %s: %w", path, err)
	}
	defer resp.Body.Close()
	// A playfab response for one player is a few kilobytes; the cap is here so a broken or
	// hostile endpoint cannot exhaust memory on a long run.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("playfab: %s: read body: %w", path, err)
	}
	return resp.StatusCode, raw, resp.Header, nil
}

// backoff returns the wait before the given attempt. A Retry-After header wins, because it is
// the title telling us when it will answer again; the exponential delay is the fallback, with
// jitter so several workers do not retry in lockstep and turn a rate limit into a stampede.
func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	if apiErr, ok := asAPIError(lastErr); ok && apiErr.RetryAfter != "" {
		if seconds, err := strconv.Atoi(apiErr.RetryAfter); err == nil && seconds >= 0 {
			d := time.Duration(seconds) * time.Second
			if d > c.cfg.MaxBackoff {
				return c.cfg.MaxBackoff
			}
			return d
		}
		if at, err := http.ParseTime(apiErr.RetryAfter); err == nil {
			if d := time.Until(at); d > 0 && d <= c.cfg.MaxBackoff {
				return d
			}
		}
	}
	d := c.cfg.BaseBackoff << uint(attempt-1)
	if d > c.cfg.MaxBackoff || d <= 0 {
		d = c.cfg.MaxBackoff
	}
	// rand/v2's top-level functions are safe for concurrent use, and the importer retries from
	// several goroutines at once.
	jitter := time.Duration(rand.Int64N(int64(d/4 + 1)))
	return d + jitter
}

func (c *Client) count(f func(*Stats)) {
	c.mu.Lock()
	f(&c.stats)
	c.mu.Unlock()
}

// PlayerIDs returns one page of the segment, in PlayFab's continuation-token style.
func (c *Client) PlayerIDs(ctx context.Context, cursor string, limit int) ([]string, string, error) {
	if limit <= 0 {
		limit = c.cfg.PageSize
	}
	req := map[string]any{
		"SegmentId":    c.cfg.SegmentID,
		"MaxBatchSize": limit,
	}
	if cursor != "" {
		req["ContinuationToken"] = cursor
	}
	var resp struct {
		Data struct {
			ProfilesInSegment []struct {
				PlayerID string `json:"PlayerId"`
			} `json:"ProfilesInSegment"`
			ContinuationToken string `json:"ContinuationToken"`
		} `json:"data"`
	}
	if err := c.call(ctx, "/Admin/GetPlayersInSegment", req, &resp); err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(resp.Data.ProfilesInSegment))
	for _, p := range resp.Data.ProfilesInSegment {
		if p.PlayerID != "" {
			ids = append(ids, p.PlayerID)
		}
	}
	return ids, resp.Data.ContinuationToken, nil
}

// Player reads everything the kit imports for one player.
//
// The five reads are sequential on purpose: they share one rate limit with every other worker,
// so issuing them in parallel multiplies the number of in-flight requests without making the
// player arrive sooner, which is what turns a rate limit into 429s.
func (c *Client) Player(ctx context.Context, id string) (*source.Player, error) {
	p := &source.Player{ID: id, UserData: map[string]string{}, Currency: map[string]int64{}, Statistics: map[string]int64{}}

	var profile struct {
		Data struct {
			PlayerProfile struct {
				DisplayName string `json:"DisplayName"`
				Created     string `json:"Created"`
				LastLogin   string `json:"LastLogin"`
			} `json:"PlayerProfile"`
		} `json:"data"`
	}
	if err := c.call(ctx, "/Admin/GetPlayerProfile", map[string]any{"PlayFabId": id}, &profile); err != nil {
		return nil, err
	}
	p.DisplayName = profile.Data.PlayerProfile.DisplayName
	p.CreatedAt = parseTime(profile.Data.PlayerProfile.Created)
	p.UpdatedAt = parseTime(profile.Data.PlayerProfile.LastLogin)

	var userData struct {
		Data struct {
			Data map[string]string `json:"Data"`
		} `json:"data"`
	}
	if err := c.call(ctx, "/Admin/GetUserData", map[string]any{"PlayFabId": id}, &userData); err != nil {
		return nil, err
	}
	for k, v := range userData.Data.Data {
		p.UserData[k] = v
	}

	var inventory struct {
		Data struct {
			Inventory []struct {
				ItemInstanceID string `json:"ItemInstanceId"`
				ItemID         string `json:"ItemId"`
				RemainingUses  int64  `json:"RemainingUses"`
			} `json:"Inventory"`
		} `json:"data"`
	}
	if err := c.call(ctx, "/Admin/GetUserInventory", map[string]any{"PlayFabId": id}, &inventory); err != nil {
		return nil, err
	}
	for _, it := range inventory.Data.Inventory {
		count := it.RemainingUses
		if count == 0 {
			// PlayFab reports a permanent item as RemainingUses 0, which is one item, not none.
			count = 1
		}
		p.Inventory = append(p.Inventory, source.Item{InstanceID: it.ItemInstanceID, CatalogID: it.ItemID, Count: count})
	}

	var currency struct {
		Data struct {
			VirtualCurrency map[string]int64 `json:"VirtualCurrency"`
		} `json:"data"`
	}
	if err := c.call(ctx, "/Admin/GetUserVirtualCurrency", map[string]any{"PlayFabId": id}, &currency); err != nil {
		return nil, err
	}
	for code, amount := range currency.Data.VirtualCurrency {
		p.Currency[code] = amount
	}

	var stats struct {
		Data struct {
			Statistics []struct {
				StatisticName string `json:"StatisticName"`
				Value         int64  `json:"Value"`
			} `json:"Statistics"`
		} `json:"data"`
	}
	if err := c.call(ctx, "/Admin/GetPlayerStatistics", map[string]any{"PlayFabId": id}, &stats); err != nil {
		return nil, err
	}
	for _, s := range stats.Data.Statistics {
		p.Statistics[s.StatisticName] = s.Value
	}

	if err := source.Validate(p); err != nil {
		return nil, err
	}
	return p, nil
}

// redact removes the secret key from anything that is about to be logged or returned.
func (c *Client) redact(s string) string {
	if c.cfg.SecretKey == "" || !strings.Contains(s, c.cfg.SecretKey) {
		return s
	}
	return strings.ReplaceAll(s, c.cfg.SecretKey, "[redacted]")
}

// preview is the first bytes of a body, for an error message that says what arrived rather
// than only that it was wrong. A secret key is never in a response body, so there is nothing
// to redact here.
func preview(raw []byte) string {
	const max = 120
	if len(raw) <= max {
		return strconv.Quote(string(raw))
	}
	return strconv.Quote(string(raw[:max])) + fmt.Sprintf("... (%d bytes)", len(raw))
}

// parseTime reads PlayFab's RFC3339 timestamps, and returns the zero time for anything else: a
// timestamp the kit cannot read is not a reason to fail an import, and the mapping treats the
// zero time as "the backend did not say".
func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
