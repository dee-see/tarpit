package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf8"

	_ "modernc.org/sqlite"
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
			RegistrableDomain: "example.com",
			Location:          "a\xff.js",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var url, location string
	if err := db.DB().QueryRow(`
		SELECT u.url, l.path
		FROM url_occurrences o
		JOIN urls u      ON u.id = o.url_id
		JOIN locations l ON l.id = o.location_id`).
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
		{URL: "https://www", Scheme: "https", Host: "www", Location: "a.js"},
		{URL: "https://real.example.com", Scheme: "https", Host: "real.example.com",
			RegistrableDomain: "example.com", Location: "b.js"},
		{URL: "s3://my-release-bucket", Scheme: "s3", Host: "my-release-bucket",
			Location: "binary.host"},
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
	for i := range 5 {
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

// A corpus written before the collapse stores one row per version, with the
// file path and a source kind repeated in every one. Migrating must preserve
// which package cited which URL in which file, and must reconstruct the version
// span that the per-version rows made implicit.
func TestMigrateCollapsesLegacyOccurrences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE packages (
		  id INTEGER PRIMARY KEY, ecosystem TEXT NOT NULL, name TEXT NOT NULL,
		  first_seen_at TEXT NOT NULL, UNIQUE(ecosystem, name));
		CREATE TABLE package_versions (
		  id INTEGER PRIMARY KEY, package_id INTEGER NOT NULL, version TEXT NOT NULL,
		  published_at TEXT, tarball_url TEXT, tarball_bytes INTEGER,
		  tarball_sha256 TEXT, extract_status TEXT NOT NULL DEFAULT 'pending',
		  extract_error TEXT, extracted_at TEXT, UNIQUE(package_id, version));
		CREATE TABLE urls (
		  id INTEGER PRIMARY KEY, url TEXT NOT NULL UNIQUE, scheme TEXT NOT NULL,
		  host TEXT NOT NULL, port TEXT NOT NULL, path TEXT NOT NULL,
		  registrable_domain TEXT NOT NULL, has_placeholder INTEGER NOT NULL,
		  first_seen_at TEXT NOT NULL);
		CREATE TABLE url_occurrences (
		  url_id INTEGER NOT NULL, version_id INTEGER NOT NULL,
		  source_kind TEXT NOT NULL, location TEXT NOT NULL, line INTEGER NOT NULL,
		  UNIQUE(url_id, version_id, source_kind, location));
		CREATE TABLE hosts (
		  host TEXT PRIMARY KEY, registrable_domain TEXT NOT NULL,
		  first_seen_at TEXT NOT NULL, occurrence_count INTEGER NOT NULL DEFAULT 0,
		  last_checked_at TEXT);

		INSERT INTO packages VALUES (1, 'npm', 'thing', '2020-01-01T00:00:00Z');
		-- Deliberately out of publication order, and with one version whose
		-- date is unknown, because both occur in the real corpus.
		INSERT INTO package_versions (id, package_id, version, published_at) VALUES
		  (1, 1, '2.0.0', '2022-01-01T00:00:00Z'),
		  (2, 1, '1.0.0', '2020-01-01T00:00:00Z'),
		  (3, 1, '1.5.0', '2021-01-01T00:00:00Z'),
		  (4, 1, '0.9.0', NULL);
		INSERT INTO urls VALUES
		  (1, 'https://lapsed.example.com/b.tgz', 'https', 'lapsed.example.com', '',
		   '/b.tgz', 'example.com', 0, '2020-01-01T00:00:00Z');
		INSERT INTO hosts VALUES ('lapsed.example.com', 'example.com',
		   '2020-01-01T00:00:00Z', 99, NULL);

		-- Same URL, same file, four versions: one row after the collapse.
		INSERT INTO url_occurrences VALUES
		  (1, 1, 'file_source', 'scripts/install.js', 3),
		  (1, 2, 'file_source', 'scripts/install.js', 3),
		  (1, 3, 'file_source', 'scripts/install.js', 4),
		  (1, 4, 'file_source', 'scripts/install.js', 1),
		-- Same URL and package but a different file: stays separate.
		  (1, 1, 'file_docs', 'README.md', 12);
	`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer db.Close()

	type row struct {
		location    string
		first, last string
		count       int
	}
	var got []row
	rows, err := db.DB().Query(`
		SELECT l.path, fv.version, lv.version, o.version_count
		FROM url_occurrences o
		JOIN locations l         ON l.id = o.location_id
		JOIN package_versions fv ON fv.id = o.first_version_id
		JOIN package_versions lv ON lv.id = o.last_version_id
		ORDER BY l.path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.location, &r.first, &r.last, &r.count); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}

	want := []row{
		{"README.md", "2.0.0", "2.0.0", 1},
		// 0.9.0 has no publication date, so it sorts first: the honest answer
		// when nothing is known about when it was live.
		{"scripts/install.js", "0.9.0", "2.0.0", 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collapsed rows = %+v, want %+v", got, want)
	}

	// occurrence_count totalled raw sightings before; it now totals references.
	var n int
	if err := db.DB().QueryRow(
		`SELECT occurrence_count FROM hosts WHERE host = 'lapsed.example.com'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("occurrence_count = %d, want 2 recomputed references", n)
	}

	// The parsed-copy columns are gone.
	cols, err := tableColumns(db.DB(), "urls")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"scheme", "port", "path"} {
		if cols[c] {
			t.Errorf("urls.%s survived the migration", c)
		}
	}
}
