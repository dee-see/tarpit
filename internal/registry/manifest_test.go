package registry

import (
	"encoding/json"
	"reflect"
	"testing"
)

func parse(t *testing.T, raw string) *Manifest {
	t.Helper()
	m, err := ParseManifest(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	return m
}

func TestParseManifestLocatesURLs(t *testing.T) {
	m := parse(t, `{
		"name": "thing",
		"version": "1.2.3",
		"scripts": {
			"postinstall": "curl -sL https://bin.example.com/v1.tar.gz | tar xz",
			"test": "mocha --reporter https://docs.example.org/reporter"
		},
		"binary": { "host": "https://thing-binaries.s3.amazonaws.com", "remote_path": "./v1/" },
		"dependencies": { "legacy": "git+https://git.example.net/legacy.git", "normal": "^1.0.0" },
		"repository": { "type": "git", "url": "git+https://github.com/example/thing.git" },
		"homepage": "https://thing.example.io",
		"dist": { "tarball": "https://registry.npmjs.org/thing/-/thing-1.2.3.tgz", "shasum": "abc" }
	}`)

	if m.Version != "1.2.3" {
		t.Errorf("Version = %q", m.Version)
	}
	if m.TarballURL != "https://registry.npmjs.org/thing/-/thing-1.2.3.tgz" {
		t.Errorf("TarballURL = %q", m.TarballURL)
	}

	got := map[string]string{}
	for _, f := range m.URLs {
		got[f.URL.Normalized] = f.Location
	}

	want := map[string]string{
		"https://bin.example.com/v1.tar.gz":        "scripts.postinstall",
		"https://docs.example.org/reporter":        "scripts.test",
		"https://thing-binaries.s3.amazonaws.com":  "binary.host",
		"git+https://git.example.net/legacy.git":   "dependencies.legacy",
		"git+https://github.com/example/thing.git": "repository",
		"https://thing.example.io":                 "homepage",
	}
	for u, loc := range want {
		if got[u] != loc {
			t.Errorf("%s: location = %q, want %q", u, got[u], loc)
		}
	}

	// The registry's own tarball URL is noise, not attack surface.
	if _, ok := got[m.TarballURL]; ok {
		t.Error("dist.tarball should not be recorded as a finding")
	}
}

func TestParseManifestRecordsAllDepKinds(t *testing.T) {
	m := parse(t, `{
		"version": "1.0.0",
		"dependencies": { "a": "^1" },
		"optionalDependencies": { "b": "^2" },
		"devDependencies": { "c": "^3" },
		"peerDependencies": { "d": "^4" }
	}`)

	got := map[string]DepKind{}
	for _, d := range m.Deps {
		got[d.Name] = d.Kind
	}
	want := map[string]DepKind{"a": DepRuntime, "b": DepOptional, "c": DepDev, "d": DepPeer}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Deps = %v, want %v", got, want)
	}
}

// Old packages routinely violate the manifest schema. One bad field must not
// cost the whole version.
func TestParseManifestToleratesMalformedFields(t *testing.T) {
	m := parse(t, `{
		"version": "0.0.1",
		"scripts": ["this", "should", "be", "an", "object"],
		"homepage": { "web": "https://survivor.example.com" },
		"dependencies": { "ok": "^1.0.0", "weird": { "nested": true } }
	}`)

	if m.Version != "0.0.1" {
		t.Errorf("Version = %q, want 0.0.1", m.Version)
	}
	var found bool
	for _, f := range m.URLs {
		if f.URL.Host == "survivor.example.com" {
			found = true
		}
	}
	if !found {
		t.Error("URL in a well-formed field was lost because another field was malformed")
	}
	if len(m.Deps) != 1 || m.Deps[0].Name != "ok" {
		t.Errorf("Deps = %v, want only the well-formed entry", m.Deps)
	}
}
