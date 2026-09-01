package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/dee-see/tarpit/internal/sample"
)

// packumentJSON builds a document shaped like npm's, with the large fields that
// make the real thing expensive and with dist-tags placed last so that the
// retainer cannot have seen it while the versions streamed past.
func packumentJSON(versions []string, latest string) string {
	var b strings.Builder
	b.WriteString(`{"name":"thing",`)
	b.WriteString(`"readme":"` + strings.Repeat("x", 200000) + `",`)
	b.WriteString(`"_attachments":{"junk":{"nested":[1,2,3],"deep":{"deeper":true}}},`)
	b.WriteString(`"time":{`)
	for i, v := range versions {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `%q:"2016-03-22T12:00:00.000Z"`, v)
	}
	b.WriteString(`},"versions":{`)
	for i, v := range versions {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `%q:{"name":"thing","version":%q,"readme":%q,"dist":{"tarball":"http://x/t.tgz"}}`,
			v, v, strings.Repeat("y", 5000))
	}
	b.WriteString(`},"dist-tags":{"latest":"` + latest + `"}}`)
	return b.String()
}

func serve(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Count(strings.Trim(r.URL.Path, "/"), "/") == 1 { // /pkg/version
			version := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			fmt.Fprintf(w, `{"name":"thing","version":%q,"dist":{"tarball":"http://x/t.tgz"}}`, version)
			return
		}
		io := w
		io.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(1000)
	c.BaseURL = srv.URL
	return c
}

func TestPackumentRetainsOnlySampledManifests(t *testing.T) {
	all := []string{"1.0.0", "1.0.1", "1.0.2", "1.1.0", "1.1.5", "2.0.0", "2.0.3"}
	c := serve(t, packumentJSON(all, "2.0.3"))

	p, err := c.Packument(context.Background(), "thing",
		sample.NewRetainer(sample.Options{Strategy: sample.Minor}))
	if err != nil {
		t.Fatal(err)
	}

	// Only the sampled versions had their manifests built.
	retained := p.Retained()
	sort.Strings(retained)
	if strings.Join(retained, ",") != "1.0.2,1.1.5,2.0.3" {
		t.Errorf("Retained = %v, want [1.0.2 1.1.5 2.0.3]", retained)
	}
	if p.Name != "thing" || p.Latest() != "2.0.3" {
		t.Errorf("Name=%q Latest=%q", p.Name, p.Latest())
	}
	if _, ok := p.PublishedAt("1.0.0"); !ok {
		t.Error("time map was not parsed")
	}
	// The retained manifests must still be intact after all that skipping.
	var m map[string]any
	if err := json.Unmarshal(p.Versions["2.0.3"], &m); err != nil {
		t.Fatalf("retained manifest is not valid JSON: %v", err)
	}
	if m["version"] != "2.0.3" {
		t.Errorf("retained manifest is for %v, want 2.0.3", m["version"])
	}
}

// A version that is not the highest in its line but is tagged latest must still
// be scannable, even though dist-tags is only seen after the versions.
func TestLatestOutsideTheSampleIsFetchedSeparately(t *testing.T) {
	all := []string{"1.0.0", "1.0.1", "1.0.2"}
	c := serve(t, packumentJSON(all, "1.0.0"))

	p, err := c.Packument(context.Background(), "thing",
		sample.NewRetainer(sample.Options{Strategy: sample.Minor}))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := p.Versions["1.0.0"]; held {
		t.Fatal("precondition: 1.0.0 should not have been retained by sampling")
	}

	raw, err := c.VersionManifest(context.Background(), "thing", "1.0.0")
	if err != nil {
		t.Fatalf("VersionManifest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["version"] != "1.0.0" {
		t.Errorf("fetched manifest is for %v, want 1.0.0", m["version"])
	}
}

// Four packages in the react crawl were lost entirely because their "time"
// object held a value that was not a timestamp string. One bad entry must not
// cost the whole package.
func TestPackumentToleratesMalformedTimeEntries(t *testing.T) {
	body := `{"name":"thing",
	  "time":{"created":"2011-01-01T00:00:00.000Z",
	          "1.0.0":"2016-03-22T12:00:00.000Z",
	          "1.1.0":{"ts":1234,"_id":"junk"}},
	  "versions":{"1.0.0":{"version":"1.0.0"},"1.1.0":{"version":"1.1.0"}},
	  "dist-tags":{"latest":"1.1.0"}}`
	c := serve(t, body)

	p, err := c.Packument(context.Background(), "thing",
		sample.NewRetainer(sample.Options{Strategy: sample.All}))
	if err != nil {
		t.Fatalf("one malformed time entry lost the whole packument: %v", err)
	}
	if len(p.Retained()) != 2 {
		t.Errorf("retained %v, want both versions", p.Retained())
	}
	if _, ok := p.PublishedAt("1.0.0"); !ok {
		t.Error("the well-formed timestamp was dropped too")
	}
	if _, ok := p.PublishedAt("1.1.0"); ok {
		t.Error("the malformed entry should be skipped, not invented")
	}
}
