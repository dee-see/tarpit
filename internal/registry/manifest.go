package registry

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/dee-see/tarpit/internal/extract"
)

// DepKind distinguishes the dependency lists. Every kind is parsed and stored
// regardless of what the crawler is configured to follow, so that enabling
// --dev later needs no re-fetching.
type DepKind string

const (
	DepRuntime  DepKind = "runtime"
	DepOptional DepKind = "optional"
	DepDev      DepKind = "dev"
	DepPeer     DepKind = "peer"
)

// Dependency is one edge out of a package version.
type Dependency struct {
	Name  string
	Range string
	Kind  DepKind
}

// MetaFinding is a URL found in the registry manifest rather than in a file.
type MetaFinding struct {
	Kind     extract.SourceKind
	Location string // dotted manifest path, e.g. "scripts.postinstall"
	URL      extract.URL
	Snippet  string
}

// Manifest is the useful content of one published version's package.json.
type Manifest struct {
	Version    string
	TarballURL string
	Shasum     string
	Deps       []Dependency
	// InstallScriptFiles are archive-relative paths that install-time hooks
	// invoke. The scanner uses these to classify those files as
	// FileInstallScript, which is the highest-severity file kind.
	InstallScriptFiles map[string]bool
	URLs               []MetaFinding
}

// installLifecycle are the scripts npm runs when a package is installed as a
// dependency. prepublish is included because in npm 4 and earlier it ran on
// plain `npm install` too, and this corpus is mostly old versions.
var installLifecycle = map[string]bool{
	"preinstall": true, "install": true, "postinstall": true,
	"prepare": true, "preprepare": true, "postprepare": true, "prepublish": true,
}

var scriptExts = map[string]bool{
	".js": true, ".cjs": true, ".mjs": true, ".sh": true, ".bash": true,
	".py": true, ".rb": true, ".ps1": true, ".bat": true, ".cmd": true,
}

// ParseManifest reads one version entry out of a packument.
//
// Fields are decoded one at a time from a raw map rather than into a single
// struct, because this corpus reaches back to packages published in 2011 whose
// manifests routinely violate the schema - a `scripts` array, a `homepage`
// object. A strict decode would throw away the whole version over one bad
// field; here a malformed field costs only that field.
func ParseManifest(raw json.RawMessage) (*Manifest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("manifest is not an object: %w", err)
	}

	m := &Manifest{
		Version:            decodeString(fields["version"]),
		InstallScriptFiles: map[string]bool{},
	}

	if dist, ok := fields["dist"]; ok {
		d := decodeStringMap(dist)
		m.TarballURL = d["tarball"]
		m.Shasum = d["shasum"]
	}

	seen := map[string]bool{}
	add := func(kind extract.SourceKind, location, candidate string) {
		for _, match := range extract.Find([]byte(candidate)) {
			u, ok := extract.Normalize(match.Raw)
			if !ok || seen[u.Normalized] {
				continue
			}
			seen[u.Normalized] = true
			m.URLs = append(m.URLs, MetaFinding{
				Kind: kind, Location: location, URL: u, Snippet: truncate(candidate),
			})
		}
	}

	// Lifecycle scripts. These are the crown jewels: a URL here is fetched with
	// the installing user's privileges.
	for name, cmd := range decodeStringMap(fields["scripts"]) {
		kind := extract.MetadataOther
		if installLifecycle[name] {
			kind = extract.MetadataScript
			for _, ref := range scriptFileRefs(cmd) {
				m.InstallScriptFiles[ref] = true
			}
		}
		add(kind, "scripts."+name, cmd)
	}

	// node-pre-gyp / prebuild-install binary hosting, which is usually a bucket.
	for key, val := range decodeStringMap(fields["binary"]) {
		add(extract.MetadataBinary, "binary."+key, val)
	}

	// Dependency specs. npm fetches http(s) and git specs directly at install
	// time, so a lapsed host here is as good as an install hook.
	for field, kind := range map[string]DepKind{
		"dependencies":         DepRuntime,
		"optionalDependencies": DepOptional,
		"devDependencies":      DepDev,
		"peerDependencies":     DepPeer,
	} {
		for name, spec := range decodeStringMap(fields[field]) {
			m.Deps = append(m.Deps, Dependency{Name: name, Range: spec, Kind: kind})
			add(extract.MetadataDepSpec, field+"."+name, spec)
		}
	}

	// Project links. Low severity on their own, but a lapsed repository host is
	// a strong hint that the rest of the package's infrastructure went with it.
	for _, field := range []string{"repository", "homepage", "bugs", "funding", "author", "contributors"} {
		for _, s := range collectStrings(fields[field]) {
			add(extract.MetadataRepo, field, s)
		}
	}

	// Finally sweep the raw manifest for anything the typed fields missed:
	// custom keys, publisher tooling residue, fields npm no longer documents.
	// Everything already recorded is skipped by `seen`.
	seen[m.TarballURL] = true
	add(extract.MetadataOther, "manifest", string(raw))

	return m, nil
}

// scriptFileRefs pulls script paths out of a shell command, so that
// "postinstall": "node scripts/install.js" tells the tarball scanner which file
// to treat as install-time code. This is the reason tarballs are downloaded at
// all: the manifest proves a fetch happens, but hides where.
func scriptFileRefs(cmd string) []string {
	var out []string
	fields := strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '|' || r == '&' || r == '"' || r == '\''
	})
	for _, tok := range fields {
		tok = strings.TrimPrefix(tok, "./")
		if tok == "" || strings.Contains(tok, "://") || strings.HasPrefix(tok, "-") {
			continue
		}
		if !scriptExts[strings.ToLower(path.Ext(tok))] {
			continue
		}
		out = append(out, path.Clean(tok))
	}
	return out
}

func decodeString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// decodeStringMap decodes an object of strings, skipping entries whose value is
// not a string rather than failing the whole map.
func decodeStringMap(raw json.RawMessage) map[string]string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s := decodeString(v); s != "" {
			out[k] = s
		}
	}
	return out
}

// collectStrings walks any JSON value and returns every string in it, which
// lets one code path handle fields npm allows in several shapes - `repository`
// is a string in old packages and an object in new ones, `funding` can be either
// or an array of both.
func collectStrings(raw json.RawMessage) []string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(node any) {
		switch t := node.(type) {
		case string:
			out = append(out, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
