package crawl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/dee-see/tarpit/internal/registry"
	"github.com/dee-see/tarpit/internal/sample"
	"github.com/dee-see/tarpit/internal/store"
)

type fakePkg struct {
	deps    map[string]string
	devDeps map[string]string
	scripts map[string]string
	files   map[string]string
}

// fakeRegistry serves packuments and tarballs, and counts requests so tests can
// assert that a re-run does not refetch work it already did.
type fakeRegistry struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newFakeRegistry(t *testing.T, pkgs map[string]fakePkg) *fakeRegistry {
	return newFakeRegistryWith(t, pkgs, nil)
}

// newFakeRegistryWith allows extra manifest fields, so a test can make the
// registry's copy of a manifest differ from the one inside the tarball - which
// is what npm's publish-time normalization actually does.
func newFakeRegistryWith(t *testing.T, pkgs map[string]fakePkg, extra map[string]any) *fakeRegistry {
	t.Helper()
	fr := &fakeRegistry{hits: map[string]int{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")

		fr.mu.Lock()
		fr.hits[r.URL.Path]++
		fr.mu.Unlock()

		if tgz, ok := strings.CutPrefix(name, "tarballs/"); ok {
			pkgName := strings.TrimSuffix(tgz, ".tgz")
			p, ok := pkgs[pkgName]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(makeTarball(t, p.files))
			return
		}

		p, ok := pkgs[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		manifest := map[string]any{
			"name":    name,
			"version": "1.0.0",
			"dist":    map[string]string{"tarball": fr.URL + "/tarballs/" + name + ".tgz"},
		}
		if p.deps != nil {
			manifest["dependencies"] = p.deps
		}
		if p.devDeps != nil {
			manifest["devDependencies"] = p.devDeps
		}
		if p.scripts != nil {
			manifest["scripts"] = p.scripts
		}
		if name == "root" {
			maps.Copy(manifest, extra)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"name":      name,
			"dist-tags": map[string]string{"latest": "1.0.0"},
			"versions":  map[string]any{"1.0.0": manifest},
			"time":      map[string]string{"1.0.0": "2016-03-22T12:00:00.000Z"},
		})
	})

	fr.Server = httptest.NewServer(mux)
	t.Cleanup(fr.Close)
	return fr
}

func (f *fakeRegistry) hitsFor(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[p]
}

func makeTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		hdr := &tar.Header{Name: path.Join("package", name), Mode: 0o644,
			Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func runCrawl(t *testing.T, fr *fakeRegistry, dbPath string, kinds []string) Result {
	t.Helper()
	return runCrawlDepth(t, fr, dbPath, kinds, "root", -1)
}

func runCrawlDepth(t *testing.T, fr *fakeRegistry, dbPath string, kinds []string, seed string, depth int) Result {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	client := registry.NewClient(1000)
	client.BaseURL = fr.URL

	res, err := Run(context.Background(), Config{
		Ecosystem:   "npm",
		Client:      client,
		Store:       db,
		Sample:      sample.Options{Strategy: sample.Minor},
		FollowKinds: kinds,
		MaxDepth:    depth,
		Concurrency: 2,
		MaxAttempts: 2,
		Logf:        func(string, ...any) {},
	}, []string{seed})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func query[T any](t *testing.T, dbPath, q string, args ...any) []T {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.DB().Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []T
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func fixture() map[string]fakePkg {
	return map[string]fakePkg{
		"root": {
			deps:    map[string]string{"runtimedep": "^1.0.0"},
			devDeps: map[string]string{"devdep": "^1.0.0"},
			scripts: map[string]string{"postinstall": "node scripts/install.js"},
			files: map[string]string{
				"scripts/install.js": "download('https://bin.lapsed-example.com/v1/node.tar.gz')\n",
				"README.md":          "badge: https://badge.example.org/x.svg\n",
			},
		},
		"runtimedep": {files: map[string]string{"index.js": "// nothing here\n"}},
		"devdep": {
			files: map[string]string{"index.js": "fetch('https://devonly.example.net/a')\n"},
		},
	}
}

// TestCrawlStoresAllDepKindsButFollowsOnlySelected is the invariant the
// incremental re-run depends on.
func TestCrawlStoresAllDepKindsButFollowsOnlySelected(t *testing.T) {
	fr := newFakeRegistry(t, fixture())
	dbPath := path.Join(t.TempDir(), "corpus.db")

	runCrawl(t, fr, dbPath, []string{"runtime"})

	scanned := query[string](t, dbPath, `SELECT name FROM packages ORDER BY name`)
	want := []string{"root", "runtimedep"}
	if fmt.Sprint(scanned) != fmt.Sprint(want) {
		t.Errorf("scanned packages = %v, want %v (devdep must not be followed)", scanned, want)
	}

	// ...but the dev edge is on disk regardless.
	devEdges := query[string](t, dbPath, `SELECT dep_name FROM dependencies WHERE kind = 'dev'`)
	if fmt.Sprint(devEdges) != "[devdep]" {
		t.Errorf("dev edges = %v, want [devdep] stored even though --dev was off", devEdges)
	}
}

// TestRerunWithDevIsIncremental is the requirement that shaped the schema:
// enabling --dev must pick up newly reachable packages without rescanning
// anything already done.
func TestRerunWithDevIsIncremental(t *testing.T) {
	fr := newFakeRegistry(t, fixture())
	dbPath := path.Join(t.TempDir(), "corpus.db")

	first := runCrawl(t, fr, dbPath, []string{"runtime"})
	if first.Versions != 2 {
		t.Fatalf("first run scanned %d versions, want 2", first.Versions)
	}

	rootTarballHits := fr.hitsFor("/tarballs/root.tgz")
	rootPackumentHits := fr.hitsFor("/root")
	doneBefore := query[int](t, dbPath, `SELECT count(*) FROM package_versions WHERE extract_status = 'done'`)[0]

	second := runCrawl(t, fr, dbPath, []string{"runtime", "dev"})

	if second.Versions != 1 {
		t.Errorf("second run scanned %d versions, want exactly 1 (devdep only)", second.Versions)
	}
	// Naming root as a seed resets its frontier row, so it is reprocessed - by
	// design, since that is what re-expands its dependencies at depth 1 relative
	// to this crawl. The cost is bounded to a single packument request:
	if got := fr.hitsFor("/root"); got != rootPackumentHits+1 {
		t.Errorf("root packument fetched %d times, want exactly one more (%d)",
			got, rootPackumentHits+1)
	}
	// ...and crucially the artifact is not refetched, because version-level
	// scan state lives in package_versions and the frontier reset does not
	// touch it.
	if got := fr.hitsFor("/tarballs/root.tgz"); got != rootTarballHits {
		t.Errorf("root tarball refetched (%d -> %d); resetting the frontier row "+
			"must not invalidate scanned versions", rootTarballHits, got)
	}
	if second.Skipped != 1 {
		t.Errorf("skipped %d versions, want 1: root is reprocessed but already scanned",
			second.Skipped)
	}

	doneAfter := query[int](t, dbPath, `SELECT count(*) FROM package_versions WHERE extract_status = 'done'`)[0]
	if doneAfter != doneBefore+1 {
		t.Errorf("done versions %d -> %d, want exactly one more", doneBefore, doneAfter)
	}

	names := query[string](t, dbPath, `SELECT name FROM packages ORDER BY name`)
	if fmt.Sprint(names) != "[devdep root runtimedep]" {
		t.Errorf("packages = %v, want devdep to have been picked up", names)
	}
	hosts := query[string](t, dbPath, `SELECT host FROM urls WHERE host = 'devonly.example.net'`)
	if len(hosts) != 1 {
		t.Error("URL reachable only through a dev edge was not extracted")
	}
}

func TestCrawlClassifiesInstallScriptURLs(t *testing.T) {
	fr := newFakeRegistry(t, fixture())
	dbPath := path.Join(t.TempDir(), "corpus.db")
	runCrawl(t, fr, dbPath, []string{"runtime"})

	kinds := query[string](t, dbPath, `
		SELECT o.source_kind FROM url_occurrences o
		JOIN urls u ON u.id = o.url_id
		WHERE u.host = 'bin.lapsed-example.com'`)
	if fmt.Sprint(kinds) != "[file_install_script]" {
		t.Errorf("kind = %v, want [file_install_script]: a URL inside a file named by "+
			"postinstall is install-time code", kinds)
	}

	badge := query[string](t, dbPath, `
		SELECT o.source_kind FROM url_occurrences o
		JOIN urls u ON u.id = o.url_id
		WHERE u.host = 'badge.example.org'`)
	if fmt.Sprint(badge) != "[file_docs]" {
		t.Errorf("README URL kind = %v, want [file_docs]", badge)
	}
}

// TestReprocessedPackageSkipsScannedVersions covers the other half of
// resumability: when a package is claimed again - after a failure, or after an
// interrupted run left it pending - the versions already on disk are skipped
// rather than redownloaded.
func TestReprocessedPackageSkipsScannedVersions(t *testing.T) {
	fr := newFakeRegistry(t, fixture())
	dbPath := path.Join(t.TempDir(), "corpus.db")

	runCrawl(t, fr, dbPath, []string{"runtime"})
	tarballHits := fr.hitsFor("/tarballs/root.tgz")

	// Put root back on the queue, as an interrupted or failed run would.
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE frontier SET status = 'pending' WHERE name = 'root'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	second := runCrawl(t, fr, dbPath, []string{"runtime"})

	if second.Skipped != 1 {
		t.Errorf("skipped %d versions, want 1: root was reclaimed but is already scanned", second.Skipped)
	}
	if second.Versions != 0 {
		t.Errorf("rescanned %d versions, want 0", second.Versions)
	}
	if got := fr.hitsFor("/tarballs/root.tgz"); got != tarballHits {
		t.Errorf("root tarball refetched (%d -> %d) despite being already scanned", tarballHits, got)
	}
}

// TestRootPackageJSONDropsOnlyExactDuplicates guards the narrow claim the
// dedupe rests on. The archive's root package.json largely repeats the
// manifest, but npm rewrites repository.url on publish, so the two are not
// interchangeable and only identical URLs may be dropped.
func TestRootPackageJSONDropsOnlyExactDuplicates(t *testing.T) {
	pkgs := map[string]fakePkg{
		"root": {
			files: map[string]string{
				// First URL matches the manifest homepage exactly; second is the
				// un-normalized repository URL that only exists in the tarball.
				"package.json": `{"homepage":"https://dup.example.com",` +
					`"repository":{"url":"https://github.com/x/y.git"}}`,
				"nested/package.json": `{"homepage":"https://vendored.example.com"}`,
			},
		},
	}
	// The manifest carries the normalized form, as npm would publish it.
	fr := newFakeRegistryWith(t, pkgs, map[string]any{
		"homepage":   "https://dup.example.com",
		"repository": map[string]string{"url": "git+https://github.com/x/y.git"},
	})
	dbPath := path.Join(t.TempDir(), "corpus.db")
	runCrawl(t, fr, dbPath, []string{"runtime"})

	kindsFor := func(host string) []string {
		return query[string](t, dbPath, `
			SELECT o.source_kind || '@' || o.location
			FROM url_occurrences o JOIN urls u ON u.id = o.url_id
			WHERE u.host = ? ORDER BY 1`, host)
	}

	if got := kindsFor("dup.example.com"); fmt.Sprint(got) != "[metadata_repo@homepage]" {
		t.Errorf("duplicate = %v, want only the manifest sighting", got)
	}
	if got := kindsFor("vendored.example.com"); fmt.Sprint(got) != "[file_source@nested/package.json]" {
		t.Errorf("nested package.json = %v, want kept: it is a different document", got)
	}
	got := kindsFor("github.com")
	if len(got) != 2 {
		t.Errorf("github.com sightings = %v, want both the manifest's git+https form "+
			"and the tarball's plain https form", got)
	}
}

// TestSeedingResetsDepth covers the bug this reset exists for: a package first
// reached as a dependency keeps the depth it was discovered at, so seeding it
// under a depth limit claimed nothing and reported no work.
func TestSeedingResetsDepth(t *testing.T) {
	pkgs := map[string]fakePkg{
		"root": {deps: map[string]string{"mid": "^1"}},
		"mid":  {deps: map[string]string{"leaf": "^1"}},
		"leaf": {files: map[string]string{"index.js": "fetch('https://leaf.example.com/x')\n"}},
	}
	fr := newFakeRegistry(t, pkgs)
	dbPath := path.Join(t.TempDir(), "corpus.db")

	// Crawl root one hop: mid is scanned at depth 1, leaf is never enqueued.
	runCrawlDepth(t, fr, dbPath, []string{"runtime"}, "root", 1)
	if got := query[string](t, dbPath, `SELECT name FROM packages ORDER BY name`); fmt.Sprint(got) != "[mid root]" {
		t.Fatalf("after first crawl packages = %v, want [mid root]", got)
	}

	// Now seed mid itself, one hop. Before the reset this claimed nothing,
	// because mid sat at depth 1 and the limit was 1.
	runCrawlDepth(t, fr, dbPath, []string{"runtime"}, "mid", 1)

	names := query[string](t, dbPath, `SELECT name FROM packages ORDER BY name`)
	if fmt.Sprint(names) != "[leaf mid root]" {
		t.Errorf("packages = %v, want leaf reached by seeding mid", names)
	}
	if got := query[string](t, dbPath, `SELECT host FROM urls WHERE host='leaf.example.com'`); len(got) != 1 {
		t.Error("URL behind the newly seeded package was not extracted")
	}
	if got := query[int](t, dbPath, `SELECT depth FROM frontier WHERE name='mid'`)[0]; got != 0 {
		t.Errorf("mid depth = %d, want 0 after being seeded", got)
	}
}
