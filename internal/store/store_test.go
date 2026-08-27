package store

import (
	"context"
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
