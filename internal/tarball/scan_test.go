package tarball

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/dee-see/tarpit/internal/extract"
)

func makeArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestScanClassifiesAndLocates(t *testing.T) {
	archive := makeArchive(t, map[string]string{
		"package/scripts/install.js": "#!/usr/bin/env node\nconst url = 'https://bin.example.com/v1/node.tar.gz';\n",
		"package/README.md":          "# thing\n\n[![ci](https://badge.example.org/x.svg)](https://ci.example.org/j)\n",
		"package/binding.gyp":        "{ 'sources': ['https://build.example.net/dep.tgz'] }\n",
		"package/test/fixture.js":    "fetch('https://mock.example.io/data.json')\n",
	})

	findings, err := Scan(bytes.NewReader(archive), Options{
		InstallScripts: map[string]bool{"scripts/install.js": true},
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	byHost := map[string]Finding{}
	for _, f := range findings {
		byHost[f.URL.Host] = f
	}

	want := map[string]struct {
		kind extract.SourceKind
		path string
		line int
	}{
		"bin.example.com":   {extract.FileInstallScript, "scripts/install.js", 2},
		"badge.example.org": {extract.FileDocs, "README.md", 3},
		"ci.example.org":    {extract.FileDocs, "README.md", 3},
		"build.example.net": {extract.FileBuildConfig, "binding.gyp", 1},
		"mock.example.io":   {extract.FileTest, "test/fixture.js", 1},
	}

	for host, w := range want {
		got, ok := byHost[host]
		if !ok {
			t.Errorf("no finding for %s", host)
			continue
		}
		if got.Kind != w.kind {
			t.Errorf("%s: kind = %s, want %s", host, got.Kind, w.kind)
		}
		if got.Path != w.path {
			t.Errorf("%s: path = %s, want %s", host, got.Path, w.path)
		}
		if got.Line != w.line {
			t.Errorf("%s: line = %d, want %d", host, got.Line, w.line)
		}
	}
	if len(findings) != len(want) {
		t.Errorf("got %d findings, want %d: %+v", len(findings), len(want), findings)
	}
}

// TestScanAcrossChunkBoundary is the case the chunked reader exists to get
// right: a URL that begins just before a chunk boundary and ends after it must
// be reported once, in full.
func TestScanAcrossChunkBoundary(t *testing.T) {
	const url = "https://straddle.example.com/very/long/path/to/a/binary.tar.gz"

	for _, startOffset := range []int{chunkSize - 10, chunkSize - len(url) + 1, chunkSize - 1, chunkSize} {
		body := strings.Repeat("x", startOffset) + url + "\n"
		archive := makeArchive(t, map[string]string{"package/big.js": body})

		findings, err := Scan(bytes.NewReader(archive), Options{})
		if err != nil {
			t.Fatalf("offset %d: Scan: %v", startOffset, err)
		}
		var hits []string
		for _, f := range findings {
			hits = append(hits, f.URL.Normalized)
		}
		if len(hits) != 1 || hits[0] != url {
			t.Errorf("offset %d: got %q, want exactly [%q]", startOffset, hits, url)
		}
	}
}

func TestScanDedupesWithinFile(t *testing.T) {
	body := strings.Repeat("see https://repeat.example.com/x\n", 50)
	archive := makeArchive(t, map[string]string{"package/a.js": body})

	findings, err := Scan(bytes.NewReader(archive), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	if findings[0].Line != 1 {
		t.Errorf("line = %d, want first occurrence (1)", findings[0].Line)
	}
	if findings[0].Snippet != "see https://repeat.example.com/x" {
		t.Errorf("snippet = %q", findings[0].Snippet)
	}
}

func TestScanEnforcesDecompressionCeiling(t *testing.T) {
	archive := makeArchive(t, map[string]string{
		"package/bomb.txt": strings.Repeat("a", 1<<20),
	})
	_, err := Scan(bytes.NewReader(archive), Options{MaxDecompressed: 4096})
	if err == nil {
		t.Fatal("expected ErrTooLarge, got nil")
	}
}
