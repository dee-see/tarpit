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
	FrontierByStatus map[string]int
}

// Stats reads corpus totals.
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	out := &Stats{FrontierByStatus: map[string]int{}}

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

// ExportRow is one URL sighting joined back to where it was found - the shape
// the takeover checker and any external triage will consume.
type ExportRow struct {
	URL               string `json:"url"`
	Host              string `json:"host"`
	RegistrableDomain string `json:"registrable_domain,omitempty"`
	HasPlaceholder    bool   `json:"has_placeholder,omitempty"`
	Ecosystem         string `json:"ecosystem"`
	Package           string `json:"package"`
	Location          string `json:"location"`
	FirstVersion      string `json:"first_version"`
	LastVersion       string `json:"last_version"`
	VersionCount      int    `json:"version_count"`
}

// Export streams every reference in the corpus to fn, grouped by host so that
// everything pointing at one piece of infrastructure arrives together. That is
// the order the work is actually done in: a host is claimable or it is not, and
// the packages behind it are what gets read afterwards.
func (s *Store) Export(ctx context.Context, fn func(ExportRow) error) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.url, u.host, u.registrable_domain, u.has_placeholder,
		       p.ecosystem, p.name, l.path,
		       fv.version, lv.version, o.version_count
		FROM url_occurrences o
		JOIN urls u             ON u.id = o.url_id
		JOIN packages p         ON p.id = o.package_id
		JOIN locations l        ON l.id = o.location_id
		JOIN package_versions fv ON fv.id = o.first_version_id
		JOIN package_versions lv ON lv.id = o.last_version_id
		ORDER BY u.host, p.name, l.path`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r ExportRow
		if err := rows.Scan(&r.URL, &r.Host, &r.RegistrableDomain, &r.HasPlaceholder,
			&r.Ecosystem, &r.Package, &r.Location,
			&r.FirstVersion, &r.LastVersion, &r.VersionCount); err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}
