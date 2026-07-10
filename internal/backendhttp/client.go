// Package backendhttp proxies HTTP requests to a picam-orchestrator
// backend on behalf of a browser request handled by picam-frontend.
//
// The C++ original hand-rolled raw sockets for this (connect-with-timeout,
// manual request-line construction, CRLF-injection sanitizing of every
// forwarded query value). None of that is needed here: net/http already
// resolves hostnames, applies connection/read timeouts via context, and
// safely encodes query parameters — a crafted value like
// "main\r\nX-Injected: evil" is just a literal, percent-encoded query
// value to net/url, never a header injection.
package backendhttp

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"picam-frontend/internal/config"
)

// Client is a shared http.Client used for every proxied request to every
// configured Pi.
type Client struct {
	http *http.Client
}

func New(timeout time.Duration) *Client {
	return &Client{http: &http.Client{Timeout: timeout}}
}

func backendURL(b config.Backend, path string, query url.Values) string {
	u := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(b.Host, strconv.Itoa(b.Port)),
		Path:     path,
		RawQuery: query.Encode(),
	}
	return u.String()
}

// Get performs a GET against backend b and returns its status code and
// body verbatim, capped at 1MB (matches the sanity cap the C++ original
// applied to its own hand-rolled response reader).
func (c *Client) Get(ctx context.Context, b config.Backend, path string, query url.Values) (status int, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, backendURL(b, path, query), nil)
	if err != nil {
		return 0, nil, err
	}
	return c.do(req)
}

// PostJSON POSTs jsonBody against backend b and returns its status code
// and body verbatim.
func (c *Client) PostJSON(ctx context.Context, b config.Backend, path string, query url.Values, jsonBody []byte) (status int, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, backendURL(b, path, query), bytes.NewReader(jsonBody))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req)
}

func (c *Client) do(req *http.Request) (status int, body []byte, err error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}
