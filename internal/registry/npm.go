// Package registry talks to the npm registry: version metadata and tarballs.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// DefaultBaseURL is the public npm registry.
const DefaultBaseURL = "https://registry.npmjs.org"

// ErrNotFound reports a package or version that the registry does not have.
// Unpublished packages are common enough in old dependency trees that callers
// need to distinguish this from a transport failure.
var ErrNotFound = errors.New("not found in registry")

// Client is a rate-limited npm registry client.
type Client struct {
	HTTP       *http.Client
	Limiter    *rate.Limiter
	BaseURL    string
	UserAgent  string
	MaxRetries int
}

// NewClient returns a client limited to rps requests per second.
//
// The User-Agent is deliberately identifying: this crawler makes a lot of
// requests to someone else's infrastructure, and they should be able to tell
// who is doing it and ask us to stop.
func NewClient(rps float64) *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: 5 * time.Minute},
		Limiter:    rate.NewLimiter(rate.Limit(rps), 1),
		BaseURL:    DefaultBaseURL,
		UserAgent:  "tarpit/0.1 (+https://github.com/dee-see/tarpit) supply-chain security research",
		MaxRetries: 4,
	}
}

// Packument is the registry document for a package: every published version's
// manifest in a single response.
type Packument struct {
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
	Time     map[string]string          `json:"time"`
}

// VersionList returns every published version string.
func (p *Packument) VersionList() []string {
	out := make([]string, 0, len(p.Versions))
	for v := range p.Versions {
		out = append(out, v)
	}
	return out
}

// Latest returns the version behind the "latest" dist-tag.
func (p *Packument) Latest() string { return p.DistTags["latest"] }

// PublishedAt returns the publish time of a version, if the registry recorded one.
func (p *Packument) PublishedAt(version string) (time.Time, bool) {
	raw, ok := p.Time[version]
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Packument fetches the full registry document for a package.
//
// The full document is requested explicitly rather than the abbreviated
// "install-v1" variant, because the abbreviated form omits `scripts` - which is
// precisely where install-time fetches live.
func (c *Client) Packument(ctx context.Context, name string) (*Packument, error) {
	resp, err := c.get(ctx, c.BaseURL+"/"+escapePackageName(name), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var p Packument
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode packument for %s: %w", name, err)
	}
	return &p, nil
}

// Tarball opens a version's artifact for streaming. The caller must close it.
func (c *Client) Tarball(ctx context.Context, tarballURL string) (io.ReadCloser, error) {
	resp, err := c.get(ctx, tarballURL, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// get issues a rate-limited request, retrying transient failures with
// exponential backoff and honouring Retry-After on 429.
func (c *Client) get(ctx context.Context, target, accept string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if err := c.Limiter.Wait(ctx); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept", accept)
		// Accept-Encoding is left to the transport: setting it by hand would
		// disable Go's transparent decompression, and a tarball served with
		// Content-Encoding: gzip would then arrive double-compressed.

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if !sleepBackoff(ctx, attempt, 0) {
				return nil, err
			}
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil

		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			resp.Body.Close()
			return nil, fmt.Errorf("%s: %w", target, ErrNotFound)

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			wait := retryAfter(resp)
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: registry returned %d", target, resp.StatusCode)
			if !sleepBackoff(ctx, attempt, wait) {
				return nil, lastErr
			}

		default:
			resp.Body.Close()
			return nil, fmt.Errorf("%s: registry returned %d", target, resp.StatusCode)
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", c.MaxRetries+1, lastErr)
}

// sleepBackoff waits before the next attempt, reporting false when the caller
// should stop retrying.
func sleepBackoff(ctx context.Context, attempt int, hint time.Duration) bool {
	delay := hint
	if delay <= 0 {
		delay = time.Duration(1<<attempt) * time.Second
	}
	if delay > 60*time.Second {
		delay = 60 * time.Second
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// escapePackageName encodes a package name for a registry path. This matters
// for scoped names, whose slash must not be read as a path separator:
// @scope/name is requested as @scope%2Fname.
func escapePackageName(name string) string {
	return url.PathEscape(name)
}
