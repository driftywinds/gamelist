package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a small generic HTTP client for JSON APIs.
//
// Headers carries persistent headers (auth, content-type overrides) that are
// applied to every request. There is deliberately no "APIKey" magic: callers
// set exactly the auth scheme their API needs, e.g.:
//
//   - IGDB/Twitch:  {"Client-ID": id, "Authorization": "Bearer " + token}
//   - Steam:        none (the Web API key travels in the query string)
//   - Epic token:   {"Authorization": "Basic " + b64, "Content-Type": "application/x-www-form-urlencoded"}
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Headers    map[string]string
}

// NewClient creates a client. headers may be nil; it is copied defensively.
func NewClient(baseURL string, headers map[string]string) *Client {
	h := make(map[string]string, len(headers))
	for k, v := range headers {
		h[k] = v
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Headers:    h,
	}
}

// SetHeader sets or overrides one persistent header (safe between calls from
// a single goroutine).
func (c *Client) SetHeader(key, value string) {
	if c.Headers == nil {
		c.Headers = map[string]string{}
	}
	c.Headers[key] = value
}

// buildURL joins the base URL and path; a fully-qualified path is used as-is.
func (c *Client) buildURL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return c.BaseURL + path
}

// Do performs the request and returns the raw body and status code.
// Persistent Headers are applied first, then extra (which wins on conflict).
// A non-2xx status yields an *APIError.
func (c *Client) Do(method, path string, body io.Reader, extra map[string]string) ([]byte, int, error) {
	req, err := http.NewRequest(method, c.buildURL(path), body)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, resp.StatusCode, &APIError{
			Code:    resp.StatusCode,
			Message: strings.TrimSpace(string(data)),
		}
	}
	return data, resp.StatusCode, nil
}

// Request is the JSON convenience wrapper around Do: it defaults
// Content-Type to application/json and decodes the response body into dst.
// dst must be a pointer json.Unmarshal can write into.
func (c *Client) Request(method, path string, body io.Reader, dst any) error {
	data, _, err := c.Do(method, path, body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil // no content
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("decode response: %w (body: %.200s)", err, string(data))
	}
	return nil
}
