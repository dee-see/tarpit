package registry

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/dee-see/tarpit/internal/extract"
)

func parse(t *testing.T, raw string) *Manifest {
	t.Helper()
	m, err := ParseManifest(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	return m
}

func TestParseManifestClassifiesURLs(t *testing.T) {
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

	got := map[string]extract.SourceKind{}
	for _, f := range m.URLs {
		got[f.URL.Normalized] = f.Kind
	}

	want := map[string]extract.SourceKind{
		"https://bin.example.com/v1.tar.gz":        extract.MetadataScript,
		"https://docs.example.org/reporter":        extract.MetadataOther,
		"https://thing-binaries.s3.amazonaws.com":  extract.MetadataBinary,
		"git+https://git.example.net/legacy.git":   extract.MetadataDepSpec,
		"git+https://github.com/example/thing.git": extract.MetadataRepo,
		"https://thing.example.io":                 extract.MetadataRepo,
	}
	for u, kind := range want {
		if got[u] != kind {
			t.Errorf("%s: kind = %q, want %q", u, got[u], kind)
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

func TestScriptFileRefs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"node scripts/install.js", []string{"scripts/install.js"}},
		{"node ./install.js && sh ./bin/setup.sh", []string{"install.js", "bin/setup.sh"}},
		{"curl -sL https://x.example.com/a.sh | sh", nil},
		{"node-gyp rebuild", nil},
		{`node "scripts/post install.js"`, []string{"install.js"}},
	}
	for _, tc := range tests {
		got := scriptFileRefs(tc.in)
		sort.Strings(got)
		want := append([]string(nil), tc.want...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("scriptFileRefs(%q) = %v, want %v", tc.in, got, want)
		}
	}
}

func TestInstallScriptFilesFeedTheScanner(t *testing.T) {
	m := parse(t, `{"version":"1.0.0","scripts":{"postinstall":"node scripts/install.js","build":"node scripts/build.js"}}`)
	if !m.InstallScriptFiles["scripts/install.js"] {
		t.Error("install-time script file not recorded")
	}
	if m.InstallScriptFiles["scripts/build.js"] {
		t.Error("build script is not install-time and must not be flagged as such")
	}
}
