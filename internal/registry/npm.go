// Package registry talks to the npm registry: version metadata and tarballs.
package registry

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
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

// ManifestRetainer decides, from a version string alone, whether that version's
// manifest is worth keeping. It is consulted while the registry document is
// still streaming, so that manifests which will not be sampled are never built.
type ManifestRetainer interface {
	// Offer reports whether to retain version, and names a previously retained
	// version it supersedes ("" if none).
	Offer(version string) (keep bool, evict string)
}

// Packument is the registry document for a package. Versions holds only the
// manifests a ManifestRetainer asked to keep; published lists every version
// string that went past, retained or not.
type Packument struct {
	Name      string
	DistTags  map[string]string
	Versions  map[string]json.RawMessage
	Time      map[string]string
	published []string
}

// VersionList returns every published version string, including those whose
// manifests were not retained.
func (p *Packument) VersionList() []string { return p.published }

// Retained returns the versions whose manifests are in hand.
func (p *Packument) Retained() []string {
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

// Packument fetches the registry document for a package, retaining only the
// version manifests retain asks for. A nil retain keeps everything.
//
// The full document is requested rather than the abbreviated "install-v1"
// variant, because the abbreviated form omits `scripts` - precisely where
// install-time fetches live. The cost of that is size: the largest documents in
// an npm dependency tree run 10-15 MB (aws-sdk 10.1, typescript 14.9), and
// decoding one whole allocates about 43 MB. Multiplied by the worker count that
// is worth avoiding, so the document is walked as a token stream and only the
// sampled manifests are ever materialised - about 20 MB with encoding/json,
// and 1 MB with jsontext, which is why this uses the latter.
func (c *Client) Packument(ctx context.Context, name string, retain ManifestRetainer) (*Packument, error) {
	resp, err := c.get(ctx, c.BaseURL+"/"+escapePackageName(name), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	p := &Packument{Versions: map[string]json.RawMessage{}}
	dec := jsontext.NewDecoder(resp.Body)

	if err := expectObjectStart(dec); err != nil {
		return nil, fmt.Errorf("decode packument for %s: %w", name, err)
	}
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("decode packument for %s: %w", name, err)
		}
		if tok.Kind() == '}' {
			break
		}
		key := tok.String()

		switch key {
		case "name":
			err = decodeInto(dec, &p.Name)
		case "dist-tags":
			err = decodeInto(dec, &p.DistTags)
		case "time":
			p.Time, err = decodeTimes(dec)
		case "versions":
			err = streamVersions(dec, p, retain)
		default:
			// Skipped without being built. The top-level readme alone is often
			// megabytes and nothing here reads it.
			err = dec.SkipValue()
		}
		if err != nil {
			return nil, fmt.Errorf("decode packument for %s at %q: %w", name, key, err)
		}
	}
	return p, nil
}

// decodeInto reads the next value and unmarshals it with encoding/json.
//
// The streaming walk uses jsontext because that is where the allocations were;
// individual values stay on encoding/json, whose looser behaviour around
// duplicate keys and type mismatches this corpus depends on - packages
// published in 2011 violate the schema constantly.
func decodeInto(dec *jsontext.Decoder, target any) error {
	val, err := dec.ReadValue()
	if err != nil {
		return err
	}
	return json.Unmarshal(val, target)
}

func expectObjectStart(dec *jsontext.Decoder) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != '{' {
		return fmt.Errorf("expected an object, got %q", tok.Kind())
	}
	return nil
}

// streamVersions walks the "versions" object, deciding what to keep from each
// key before its value is read.
func streamVersions(dec *jsontext.Decoder, p *Packument, retain ManifestRetainer) error {
	if err := expectObjectStart(dec); err != nil {
		return err
	}
	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		if tok.Kind() == '}' {
			return nil
		}
		version := tok.String()
		p.published = append(p.published, version)

		keep, evict := true, ""
		if retain != nil {
			keep, evict = retain.Offer(version)
		}
		if evict != "" {
			delete(p.Versions, evict)
		}
		if !keep {
			if err := dec.SkipValue(); err != nil {
				return err
			}
			continue
		}

		val, err := dec.ReadValue()
		if err != nil {
			return err
		}
		// ReadValue may hand back the decoder's own buffer, so this has to copy.
		p.Versions[version] = append(json.RawMessage(nil), val...)
	}
}

// VersionManifest fetches a single version's manifest directly. Used to
// reconcile the "latest" dist-tag, which may only be known after the versions
// have already streamed past.
func (c *Client) VersionManifest(ctx context.Context, name, version string) (json.RawMessage, error) {
	resp, err := c.get(ctx, c.BaseURL+"/"+escapePackageName(name)+"/"+url.PathEscape(version), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode %s@%s: %w", name, version, err)
	}
	return raw, nil
}

// decodeTimes reads the "time" object, keeping only entries whose value is a
// string.
//
// Manifests already get this tolerance because packages published in 2011
// routinely violate the schema; the packument itself needed it too. A handful
// of old packages carry an object where a timestamp belongs, and decoding
// strictly threw away the whole document - and with it every version of the
// package - over one malformed entry.
func decodeTimes(dec *jsontext.Decoder) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := decodeInto(dec, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
		}
	}
	return out, nil
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
