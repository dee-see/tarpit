package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// Binaries are scanned rather than skipped, so URLs and snippets can carry
// bytes that are not valid UTF-8. Writing those raw into a TEXT column yields a
// database that ordinary clients cannot read - a Python sqlite3 cursor raises
// instead of returning the row - so they are repaired on the way in.
func TestSaveVersionSanitizesInvalidUTF8(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "corpus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pkgID, err := db.UpsertPackage(ctx, "npm", "thing")
	if err != nil {
		t.Fatal(err)
	}

	bad := "https://example.com/\xff\xfe"
	if err := db.SaveVersion(ctx, pkgID, VersionResult{
		Version: "1.0.0",
		Findings: []Finding{{
			URL: bad, Scheme: "https", Host: "example.com",
			RegistrableDomain: "example.com", SourceKind: "file_source",
			Location: "a\xff.js", Snippet: "x = '\xff\xfe'",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var url, location, snippet string
	if err := db.DB().QueryRow(`
		SELECT u.url, o.location, o.snippet
		FROM url_occurrences o JOIN urls u ON u.id = o.url_id`).
		Scan(&url, &location, &snippet); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"url": url, "location": location, "snippet": snippet} {
		if !utf8.ValidString(got) {
			t.Errorf("%s is not valid UTF-8: %q", name, got)
		}
	}
}

// The hosts table is the checker's worklist, so it should not fill up with
// residue. A dotless host is what remains after a truncated URL such as
// "https://www." + domain loses its trailing dot - but a bucket name has no
// dots either, and those are among the best targets, so the rule is
// scheme-aware.
func TestHostsTableSkipsUnresolvableHosts(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "corpus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pkgID, err := db.UpsertPackage(ctx, "npm", "thing")
	if err != nil {
		t.Fatal(err)
	}

	findings := []Finding{
		{URL: "https://www", Scheme: "https", Host: "www", SourceKind: "file_source", Location: "a.js"},
		{URL: "https://real.example.com", Scheme: "https", Host: "real.example.com",
			RegistrableDomain: "example.com", SourceKind: "file_source", Location: "b.js"},
		{URL: "s3://my-release-bucket", Scheme: "s3", Host: "my-release-bucket",
			SourceKind: "metadata_binary", Location: "binary.host"},
	}
	if err := db.SaveVersion(ctx, pkgID, VersionResult{Version: "1.0.0", Findings: findings}); err != nil {
		t.Fatal(err)
	}

	var hosts []string
	rows, err := db.DB().Query(`SELECT host FROM hosts ORDER BY host`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, h)
	}
	want := []string{"my-release-bucket", "real.example.com"}
	if fmt.Sprint(hosts) != fmt.Sprint(want) {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}

	// All three URLs are still recorded; only the worklist is filtered.
	var n int
	if err := db.DB().QueryRow(`SELECT count(*) FROM urls`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("urls = %d, want 3: filtering the worklist must not drop data", n)
	}
}
