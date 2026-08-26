package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRetryDelay = 30 * time.Second
const maxDuration = time.Duration(1<<63 - 1)

type HTTPError struct {
	Status      int
	Message     string
	RetryAfter  time.Duration
	RateLimited bool
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("remote API returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("remote API returned HTTP %d: %s", e.Status, e.Message)
}

type client struct {
	http    *http.Client
	headers func(*http.Request)
	secrets []string

	mu           sync.Mutex
	requests     int
	retries      int
	requestLimit int
	rateLimit    *RateLimit
}

func newClient(options Options, headers func(*http.Request), secrets ...string) *client {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = min(timeout, 30*time.Second)
	c := &client{
		headers:      headers,
		secrets:      secrets,
		requestLimit: options.RequestLimit,
	}
	c.http = &http.Client{Transport: transport, CheckRedirect: c.checkRedirect}
	return c
}

func (c *client) get(ctx context.Context, endpoint, accept string) (*http.Response, error) {
	return c.getWithEncoding(ctx, endpoint, accept, false)
}

func (c *client) getRaw(ctx context.Context, endpoint, accept string) (*http.Response, error) {
	return c.getWithEncoding(ctx, endpoint, accept, true)
}

func (c *client) getWithEncoding(ctx context.Context, endpoint, accept string, identityEncoding bool) (*http.Response, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.reserveRequest(attempt > 0); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if identityEncoding {
			req.Header.Set("Accept-Encoding", "identity")
		}
		c.headers(req)
		resp, err := c.http.Do(req)
		if err != nil {
			last = err
			var budgetErr *RequestBudgetError
			if errors.As(err, &budgetErr) {
				return nil, budgetErr
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < 2 {
				if err := waitRetry(ctx, time.Duration(1<<attempt)*200*time.Millisecond); err != nil {
					return nil, err
				}
			}
			continue
		}
		c.observeRateLimit(resp.Header)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		message := readErrorMessage(resp.Body)
		for _, secret := range c.secrets {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[REDACTED]")
			}
		}
		resp.Body.Close()
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		if retryAfter <= 0 && rateLimitExhausted(resp.Header) {
			retryAfter = resetDelay(resp.Header)
		}
		rateLimited := resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusForbidden && (rateLimitExhausted(resp.Header) || retryAfter > 0)
		httpErr := &HTTPError{Status: resp.StatusCode, Message: message, RetryAfter: retryAfter, RateLimited: rateLimited}
		last = httpErr
		if !rateLimited && resp.StatusCode < 500 {
			return nil, httpErr
		}
		if attempt == 2 {
			break
		}
		delay := retryAfter
		if delay <= 0 {
			delay = time.Duration(1<<attempt) * 500 * time.Millisecond
		}
		if delay > maxRetryDelay {
			return nil, httpErr
		}
		if err := waitRetry(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, last
}

func (c *client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 5 {
		return errors.New("too many redirects")
	}
	if req.Response != nil {
		c.observeRateLimit(req.Response.Header)
	}
	origin := via[0].URL
	if origin.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refusing HTTPS downgrade redirect")
	}
	if !strings.EqualFold(req.URL.Host, origin.Host) {
		if req.URL.Scheme != "https" {
			return errors.New("refusing insecure cross-host redirect")
		}
		req.Header.Del("Authorization")
		req.Header.Del("Private-Token")
		req.Header.Del("Cookie")
	}
	return c.reserveRequest(false)
}

func (c *client) reserveRequest(retry bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.requestLimit > 0 && c.requests >= c.requestLimit {
		return &RequestBudgetError{Limit: c.requestLimit}
	}
	c.requests++
	if retry {
		c.retries++
	}
	return nil
}

func (c *client) stats() RequestStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	stats := RequestStats{Requests: c.requests, Retries: c.retries, RequestLimit: c.requestLimit}
	if c.rateLimit != nil {
		copy := *c.rateLimit
		stats.RateLimit = &copy
	}
	return stats
}

func (c *client) observeRateLimit(headers http.Header) {
	limit, limitOK := headerInt(headers, "X-RateLimit-Limit", "RateLimit-Limit")
	remaining, remainingOK := headerInt(headers, "X-RateLimit-Remaining", "RateLimit-Remaining")
	if !limitOK && !remainingOK {
		return
	}
	reset, _ := headerInt64(headers, "X-RateLimit-Reset", "RateLimit-Reset")
	observed := &RateLimit{
		Limit:     limit,
		Remaining: remaining,
		Reset:     reset,
		Resource:  headers.Get("X-RateLimit-Resource"),
		Name:      headers.Get("RateLimit-Name"),
	}
	c.mu.Lock()
	c.rateLimit = observed
	c.mu.Unlock()
}

func (c *client) getJSON(ctx context.Context, endpoint string, out any) (http.Header, error) {
	resp, err := c.get(ctx, endpoint, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxJSONResponseSize {
		return nil, &ResourceLimitError{Resource: "remote API response bytes", Limit: maxJSONResponseSize}
	}
	limited := &io.LimitedReader{R: resp.Body, N: maxJSONResponseSize + 1}
	dec := json.NewDecoder(limited)
	if err := dec.Decode(out); err != nil {
		if limited.N == 0 {
			return nil, &ResourceLimitError{Resource: "remote API response bytes", Limit: maxJSONResponseSize}
		}
		return nil, fmt.Errorf("decode remote API response: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode remote API response: trailing JSON value")
		}
		return nil, fmt.Errorf("decode remote API response: %w", err)
	}
	if limited.N == 0 {
		return nil, &ResourceLimitError{Resource: "remote API response bytes", Limit: maxJSONResponseSize}
	}
	return resp.Header, nil
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > uint64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if allDecimal(value) {
		return maxDuration
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}

func allDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func rateLimitExhausted(headers http.Header) bool {
	remaining, ok := headerInt(headers, "X-RateLimit-Remaining", "RateLimit-Remaining")
	return ok && remaining == 0
}

func resetDelay(headers http.Header) time.Duration {
	reset, ok := headerInt64(headers, "X-RateLimit-Reset", "RateLimit-Reset")
	if !ok {
		return 0
	}
	return max(time.Until(time.Unix(reset, 0)), 0)
}

func headerInt(headers http.Header, names ...string) (int, bool) {
	value, ok := headerInt64(headers, names...)
	return int(value), ok
}

func headerInt64(headers http.Header, names ...string) (int64, bool) {
	for _, name := range names {
		raw := headers.Get(name)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

func readErrorMessage(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	var body struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil {
		if body.Message != "" {
			return body.Message
		}
		if body.Error != "" {
			return body.Error
		}
	}
	return strings.TrimSpace(string(data))
}
