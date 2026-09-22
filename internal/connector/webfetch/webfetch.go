// Package webfetch is the polite GET the store connectors share: a descriptive
// User-Agent, a bounded body, and 429 handling that WAITS as the server asks
// (Retry-After, capped) instead of guessing -- obeying a rate limiter, never
// defeating one. It only GETs public URLs; callers own what they fetch.
package webfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultUserAgent identifies nagus to small winery hosts.
const DefaultUserAgent = "nagus/0.3 (+https://github.com/leftathome/nagus; personal deal tracker)"

// ErrRateLimited is returned when the host kept answering 429.
var ErrRateLimited = errors.New("rate limited by host")

// Client performs GETs. The zero value is usable.
type Client struct {
	HTTP       *http.Client
	UserAgent  string
	MaxRetries int           // retries after 429; 0 = 3, <0 = none
	MaxWait    time.Duration // cap on one Retry-After wait; 0 = 2m
	MaxBody    int64         // 0 = 32 MiB
	// Sleep waits between retries; nil = a context-aware sleep. Tests inject.
	Sleep func(context.Context, time.Duration) error
}

// Get fetches url with the given extra headers and returns the body of a 200.
// Any other status is an error naming it.
func (c *Client) Get(ctx context.Context, url string, header map[string]string) ([]byte, error) {
	retries := c.MaxRetries
	if retries == 0 {
		retries = 3
	} else if retries < 0 {
		retries = 0
	}
	for attempt := 0; ; attempt++ {
		body, status, retryAfter, err := c.once(ctx, url, header)
		if err != nil {
			return nil, err
		}
		switch {
		case status == http.StatusOK:
			return body, nil
		case status == http.StatusTooManyRequests && attempt < retries:
			wait := retryAfter
			if wait <= 0 {
				wait = time.Duration(attempt+1) * 10 * time.Second
			}
			if max := c.maxWait(); wait > max {
				wait = max
			}
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
		case status == http.StatusTooManyRequests:
			return nil, fmt.Errorf("%w: %s", ErrRateLimited, url)
		default:
			return nil, fmt.Errorf("GET %s: HTTP %d", url, status)
		}
	}
}

func (c *Client) once(ctx context.Context, url string, header map[string]string) ([]byte, int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	max := c.MaxBody
	if max <= 0 {
		max = 32 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("GET %s: reading body: %w", url, err)
	}
	var ra time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		ra = time.Duration(s) * time.Second
	}
	return body, resp.StatusCode, ra, nil
}

func (c *Client) maxWait() time.Duration {
	if c.MaxWait > 0 {
		return c.MaxWait
	}
	return 2 * time.Minute
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
