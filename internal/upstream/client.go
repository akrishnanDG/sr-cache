package upstream

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

type Client struct {
	base        *url.URL
	authHeader  string
	http        *http.Client
	maxBodySize int64
}

// DefaultMaxBodySize bounds upstream response bodies we'll buffer in memory.
// Confluent Cloud SR responses are typically <100KB; 16MB is generous.
const DefaultMaxBodySize int64 = 16 << 20 // 16 MiB

func New(rawURL, key, secret string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream url: %w", err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(key + ":" + secret))
	return &Client{
		base:        u,
		authHeader:  "Basic " + auth,
		maxBodySize: DefaultMaxBodySize,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

// SetMaxBodySize overrides the default body cap. Must be called before any Do().
func (c *Client) SetMaxBodySize(n int64) { c.maxBodySize = n }

// CloseIdleConnections drains the upstream HTTP transport's pooled connections.
// Called during graceful shutdown.
func (c *Client) CloseIdleConnections() {
	if t, ok := c.http.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// Do sends a request to the upstream with the given method/path and an optional body.
// Headers from the inbound request can be passed through; auth is overwritten.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, headers http.Header, body io.Reader) (*Response, error) {
	target := *c.base
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(path, "/")
	if query != nil {
		target.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		for k, v := range headers {
			lk := strings.ToLower(k)
			// drop hop-by-hop and auth/host headers; we set our own auth and Host
			if lk == "authorization" || lk == "host" || lk == "connection" ||
				lk == "content-length" || lk == "accept-encoding" {
				continue
			}
			req.Header[k] = v
		}
	}
	req.Header.Set("Authorization", c.authHeader)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/vnd.schemaregistry.v1+json, application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// LimitReader caps memory at maxBodySize+1; if we read the +1 byte, we
	// know the body exceeded the cap and can reject cleanly.
	limit := c.maxBodySize
	if limit <= 0 {
		limit = DefaultMaxBodySize
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("upstream response exceeded %d bytes", limit)
	}
	return &Response{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        b,
	}, nil
}

// IsRateLimited reports whether the response should trip the breaker.
func (r *Response) IsRateLimited() bool {
	return r != nil && r.Status == http.StatusTooManyRequests
}
