package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// A failed POST can already have committed remotely. Recovery belongs to the
// proposal workflow, which reads the branch and PR before attempting another write.
func (c *client) postJSON(ctx context.Context, endpoint string, body, out any) error {
	if !c.writeEnabled {
		return ErrWriteDisabled
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := c.reserveRequest(false); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.headers(req)
	httpClient := *c.http
	// Redirecting a write could forward private source code to another origin.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.observeRateLimit(resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := readErrorMessage(resp.Body)
		for _, secret := range c.secrets {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[REDACTED]")
			}
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			message = "write redirects are not followed"
		}
		return &HTTPError{Status: resp.StatusCode, Message: message,
			RateLimited: resp.StatusCode == 429 || resp.StatusCode == 403 && rateLimitExhausted(resp.Header)}
	}
	if _, err := decodeJSONResponse(resp, out); err != nil {
		return fmt.Errorf("write may have succeeded remotely: %w", err)
	}
	return nil
}
