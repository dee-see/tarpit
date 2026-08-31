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
			Location: "a\xff.js",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var url, location string
	if err := db.DB().QueryRow(`
		SELECT u.url, o.location
		FROM url_occurrences o JOIN urls u ON u.id = o.url_id`).
		Scan(&url, &location); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"url": url, "location": location} {
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

// attempts is a retry budget for genuine failures. Claiming a package - which
// happens again after every crash, interruption or restart - must not spend it.
func TestAttemptsCountFailuresNotClaims(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "corpus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Seed(ctx, "npm", []string{"thing"}); err != nil {
		t.Fatal(err)
	}

	attempts := func() int {
		var n int
		if err := db.DB().QueryRow(`SELECT attempts FROM frontier WHERE name='thing'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	status := func() string {
		var s string
		if err := db.DB().QueryRow(`SELECT status FROM frontier WHERE name='thing'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Five interrupted runs: claimed, then released without ever failing.
	for i := 0; i < 5; i++ {
		item, err := db.Claim(ctx, "npm", -1)
		if err != nil || item == nil {
			t.Fatalf("claim %d: %v (item %v)", i, err, item)
		}
		if _, err := db.ReleaseClaimed(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := attempts(); got != 0 {
		t.Errorf("after 5 interrupted claims attempts = %d, want 0", got)
	}
	if got := status(); got != "pending" {
		t.Errorf("status = %q, want pending", got)
	}

	// Now two real failures against a budget of three.
	for i := 1; i <= 2; i++ {
		item, err := db.Claim(ctx, "npm", -1)
		if err != nil || item == nil {
			t.Fatalf("claim: %v", err)
		}
		if err := db.Fail(ctx, item.ID, "boom", 3); err != nil {
			t.Fatal(err)
		}
		if got := attempts(); got != i {
			t.Errorf("after %d failure(s) attempts = %d", i, got)
		}
		if got := status(); got != "pending" {
			t.Errorf("after %d failure(s) status = %q, want pending", i, got)
		}
	}

	// The third exhausts the budget and parks it.
	item, err := db.Claim(ctx, "npm", -1)
	if err != nil || item == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := db.Fail(ctx, item.ID, "boom", 3); err != nil {
		t.Fatal(err)
	}
	if got, st := attempts(), status(); got != 3 || st != "failed" {
		t.Errorf("attempts=%d status=%q, want 3/failed", got, st)
	}

	// Seeding it explicitly is the way back in.
	if err := db.Seed(ctx, "npm", []string{"thing"}); err != nil {
		t.Fatal(err)
	}
	if got, st := attempts(), status(); got != 0 || st != "pending" {
		t.Errorf("after reseed attempts=%d status=%q, want 0/pending", got, st)
	}
}
