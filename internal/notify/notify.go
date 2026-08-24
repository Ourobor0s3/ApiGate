// Package notify posts best-effort JSON webhooks; failures are logged, never
// returned to callers.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Client posts webhook payloads to a single URL. Enabled reports whether a URL
// is configured, so a Client built from an empty env var is a safe no-op.
type Client struct {
	url        string
	httpClient *http.Client
	logger     *slog.Logger
}

// New returns a Client for url; an empty url yields a disabled no-op client.
func New(url string) *Client {
	return &Client{
		url:        url,
		httpClient: &http.Client{Timeout: 8 * time.Second},
		logger:     slog.Default(),
	}
}

// Enabled reports whether a webhook URL is configured.
func (c *Client) Enabled() bool { return c != nil && c.url != "" }

// Post serializes payload as JSON and sends it to the webhook URL.
func (c *Client) Post(ctx context.Context, payload any) {
	if !c.Enabled() {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		c.logger.Warn("webhook: marshal payload", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		c.logger.Warn("webhook: build request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Warn("webhook: send failed", "err", err)
		return
	}
	// Drain the body so the connection can be reused by keep-alive.
	_, _ = io.Copy(io.Discard, resp.Body)
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.logger.Warn("webhook: non-2xx response", "status", resp.StatusCode)
	}
}
