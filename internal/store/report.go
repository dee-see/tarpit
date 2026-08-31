package store

import (
	"context"
	"database/sql"
)

// Stats summarizes a corpus for the `status` command.
type Stats struct {
	Packages         int
	VersionsDone     int
	VersionsFail     int
	URLs             int
	Hosts            int
	Domains          int
	Occurrences      int
	ByKind           map[string]int
	FrontierByStatus map[string]int
}

// Stats reads corpus totals.
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	out := &Stats{ByKind: map[string]int{}, FrontierByStatus: map[string]int{}}

	scalars := []struct {
		query string
		dest  *int
	}{
		{`SELECT count(*) FROM packages`, &out.Packages},
		{`SELECT count(*) FROM package_versions WHERE extract_status = 'done'`, &out.VersionsDone},
		{`SELECT count(*) FROM package_versions WHERE extract_status = 'failed'`, &out.VersionsFail},
		{`SELECT count(*) FROM urls`, &out.URLs},
		{`SELECT count(*) FROM hosts`, &out.Hosts},
		{`SELECT count(DISTINCT registrable_domain) FROM urls WHERE registrable_domain != ''`, &out.Domains},
		{`SELECT count(*) FROM url_occurrences`, &out.Occurrences},
	}
	for _, q := range scalars {
		if err := s.db.QueryRowContext(ctx, q.query).Scan(q.dest); err != nil {
			return nil, err
		}
	}

	if err := scanCounts(ctx, s.db,
		`SELECT source_kind, count(*) FROM url_occurrences GROUP BY source_kind`,
		out.ByKind); err != nil {
		return nil, err
	}
	if err := scanCounts(ctx, s.db,
		`SELECT status, count(*) FROM frontier GROUP BY status`,
		out.FrontierByStatus); err != nil {
		return nil, err
	}
	return out, nil
}

func scanCounts(ctx context.Context, db *sql.DB, query string, into map[string]int) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		into[k] = n
	}
	return rows.Err()
}

// ExportRow is one URL sighting joined back to the package version it came
// from - the shape the phase-two checker and any external triage will consume.
type ExportRow struct {
	URL               string `json:"url"`
	Host              string `json:"host"`
	RegistrableDomain string `json:"registrable_domain,omitempty"`
	HasPlaceholder    bool   `json:"has_placeholder,omitempty"`
	Ecosystem         string `json:"ecosystem"`
	Package           string `json:"package"`
	Version           string `json:"version"`
	PublishedAt       string `json:"published_at,omitempty"`
	SourceKind        string `json:"source_kind"`
	Location          string `json:"location"`
	Line              int    `json:"line,omitempty"`
}

// Export streams every occurrence in the corpus to fn, ordered so that the
// highest-severity kinds come first: whoever reads the dump sees install-time
// fetches before README badges.
func (s *Store) Export(ctx context.Context, fn func(ExportRow) error) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.url, u.host, u.registrable_domain, u.has_placeholder,
		       p.ecosystem, p.name, v.version, COALESCE(v.published_at, ''),
		       o.source_kind, o.location, o.line
		FROM url_occurrences o
		JOIN urls u             ON u.id = o.url_id
		JOIN package_versions v ON v.id = o.version_id
		JOIN packages p         ON p.id = v.package_id
		ORDER BY
			CASE o.source_kind
				WHEN 'metadata_script'     THEN 0
				WHEN 'file_install_script' THEN 1
				WHEN 'metadata_binary'     THEN 2
				WHEN 'metadata_dep_spec'   THEN 3
				WHEN 'file_build_config'   THEN 4
				WHEN 'metadata_repo'       THEN 5
				WHEN 'file_source'         THEN 6
				ELSE 7
			END,
			u.host, p.name, v.version`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r ExportRow
		if err := rows.Scan(&r.URL, &r.Host, &r.RegistrableDomain, &r.HasPlaceholder,
			&r.Ecosystem, &r.Package, &r.Version, &r.PublishedAt,
			&r.SourceKind, &r.Location, &r.Line); err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}
