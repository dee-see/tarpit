// Package tarball streams a gzipped tar archive and extracts URLs from every
// file inside it, without ever writing the archive to disk.
package tarball

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/dee-see/tarpit/internal/extract"
)

const (
	// chunkSize bounds how much of any single file is held in memory at once.
	// Nothing is skipped for being large; it is just read in pieces.
	chunkSize = 1 << 20 // 1 MiB
	// overlap is carried between chunks so that a URL straddling a chunk
	// boundary is still matched in full.
	overlap = 8 << 10 // 8 KiB
	// maxSnippet caps the stored context line.
	maxSnippet = 200
)

var newline = []byte{'\n'}

// ErrTooLarge reports that an archive exceeded the decompression ceiling. This
// is a decompression-bomb guard, not a size filter: the default ceiling is far
// above any legitimate package.
var ErrTooLarge = errors.New("archive exceeded decompression ceiling")

// Finding is one URL sighting inside an archive.
type Finding struct {
	Path    string
	Line    int
	Kind    extract.SourceKind
	URL     extract.URL
	Snippet string
}

// Options configures a scan.
type Options struct {
	// MaxDecompressed aborts the scan once this many bytes have been
	// decompressed. Zero means 2 GiB.
	MaxDecompressed int64
	// InstallScripts holds archive-relative paths referenced by the package's
	// install hooks. Files in this set are classified FileInstallScript, the
	// highest-severity file kind, because they run at install time.
	InstallScripts map[string]bool
}

// Scan reads a gzipped tar archive from r and returns every distinct URL found,
// deduplicated per (file, URL) so that a URL repeated throughout one file
// produces a single finding anchored at its first occurrence.
func Scan(r io.Reader, opts Options) ([]Finding, error) {
	maxBytes := opts.MaxDecompressed
	if maxBytes <= 0 {
		maxBytes = 2 << 30
	}

	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer zr.Close()

	counted := &countingReader{r: zr, limit: maxBytes}
	tr := tar.NewReader(counted)

	var findings []Finding
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				return findings, ErrTooLarge
			}
			return findings, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := stripPackagePrefix(hdr.Name)
		kind := classify(name, opts.InstallScripts)

		found, err := scanFile(tr, name, kind)
		findings = append(findings, found...)
		if err != nil {
			if errors.Is(err, ErrTooLarge) {
				return findings, ErrTooLarge
			}
			return findings, fmt.Errorf("read %s: %w", name, err)
		}
	}
	return findings, nil
}

// scanFile reads one archive entry in bounded chunks. Binary files are scanned
// too rather than skipped: a URL compiled into a checked-in binary is
// strings-visible and just as claimable as one in source.
func scanFile(r io.Reader, name string, kind extract.SourceKind) ([]Finding, error) {
	var (
		findings []Finding
		seen     = map[string]bool{}
		carry    []byte
		lineAt   = 1
	)

	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			window := append(carry, buf[:n]...)
			final := false
			if err == io.EOF {
				final = true
			}
			emit(window, name, kind, lineAt, final, seen, &findings)

			keep := overlap
			if len(window) < keep {
				keep = len(window)
			}
			consumed := window[:len(window)-keep]
			lineAt += bytes.Count(consumed, newline)
			carry = append([]byte(nil), window[len(window)-keep:]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return findings, err
		}
	}
	// Flush whatever is left in the carry as a final window.
	if len(carry) > 0 {
		emit(carry, name, kind, lineAt, true, seen, &findings)
	}
	return findings, nil
}

// emit records matches from one window. Unless this is the final window,
// matches touching the very end are dropped: they may be truncated mid-URL and
// will be seen whole in the next window, which overlaps this one.
func emit(window []byte, name string, kind extract.SourceKind, lineAt int, final bool, seen map[string]bool, out *[]Finding) {
	for _, m := range extract.Find(window) {
		if !final && m.Offset+len(m.Raw) == len(window) {
			continue
		}
		u, ok := extract.Normalize(m.Raw)
		if !ok || seen[u.Normalized] {
			continue
		}
		seen[u.Normalized] = true
		*out = append(*out, Finding{
			Path:    name,
			Line:    lineAt + bytes.Count(window[:m.Offset], newline),
			Kind:    kind,
			URL:     u,
			Snippet: snippet(window, m.Offset),
		})
	}
}

// snippet returns the line containing offset, trimmed and length-capped, to give
// a human triaging the corpus enough context to judge a finding at a glance.
func snippet(b []byte, offset int) string {
	start := 0
	if i := bytes.LastIndexByte(b[:offset], '\n'); i >= 0 {
		start = i + 1
	}
	end := len(b)
	if i := bytes.IndexByte(b[offset:], '\n'); i >= 0 {
		end = offset + i
	}
	s := strings.TrimSpace(string(b[start:end]))
	if len(s) > maxSnippet {
		s = s[:maxSnippet]
	}
	return s
}

// classify maps an archive path to a source kind. Install scripts are checked
// first: they are the reason tarballs are downloaded at all, since the common
// pattern "postinstall": "node scripts/install.js" hides the URL in here rather
// than in the registry metadata.
func classify(name string, installScripts map[string]bool) extract.SourceKind {
	if installScripts[name] {
		return extract.FileInstallScript
	}

	lower := strings.ToLower(name)
	base := path.Base(lower)
	ext := path.Ext(lower)

	switch base {
	case "binding.gyp", "makefile", "gnumakefile", "cmakelists.txt", "dockerfile", "webpack.config.js":
		return extract.FileBuildConfig
	}
	switch ext {
	case ".sh", ".bash", ".zsh", ".ps1", ".bat", ".cmd", ".gyp", ".gypi", ".mk", ".cmake":
		return extract.FileBuildConfig
	case ".md", ".markdown", ".rst", ".txt", ".adoc":
		return extract.FileDocs
	}
	for _, seg := range strings.Split(lower, "/") {
		switch seg {
		case "test", "tests", "spec", "specs", "__tests__", "fixtures", "e2e":
			return extract.FileTest
		case ".github", ".circleci", ".travis.yml", "ci":
			return extract.FileBuildConfig
		case "doc", "docs", "examples", "example":
			return extract.FileDocs
		}
	}
	return extract.FileSource
}

// stripPackagePrefix removes the "package/" directory that npm wraps every
// tarball in, so recorded paths match what a reader sees in the repository.
func stripPackagePrefix(name string) string {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if rest, ok := strings.CutPrefix(name, "package/"); ok {
		return rest
	}
	return name
}

// countingReader enforces the decompression ceiling as bytes are pulled.
type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n >= c.limit {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > c.limit-c.n {
		p = p[:c.limit-c.n]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
