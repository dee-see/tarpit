// Package store persists the URL corpus in SQLite.
//
// Writes are serialized behind a mutex because SQLite admits a single writer.
// Reads are not: the database runs in WAL mode, so the crawl's readers proceed
// concurrently with the writer.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a handle on a corpus database.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// Open opens or creates a corpus at path and applies the schema.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// The writer is serialized by s.mu; extra pooled connections would only
	// contend for the write lock.
	db.SetMaxOpenConns(8)

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for ad-hoc reads (status, export).
func (s *Store) DB() *sql.DB { return s.db }

// write runs fn inside a write transaction, serialized against other writers.
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// text makes a value safe for a SQLite TEXT column. URLs and snippets are
// lifted verbatim out of tarballs, and binaries are scanned rather than
// skipped, so these bytes are not guaranteed to be valid UTF-8. Storing raw
// bytes in a TEXT column produces a database that standard clients cannot read
// - a Python sqlite3 cursor raises rather than returning the row.
func text(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// ---------------------------------------------------------------- packages

// UpsertPackage returns the id of a package row, creating it if needed.
func (s *Store) UpsertPackage(ctx context.Context, ecosystem, name string) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO packages (ecosystem, name, first_seen_at) VALUES (?, ?, ?)`,
			ecosystem, name, now()); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx,
			`SELECT id FROM packages WHERE ecosystem = ? AND name = ?`,
			ecosystem, name).Scan(&id)
	})
	return id, err
}

// ScannedVersions returns the versions of a package that are already finished,
// so the crawler can skip them. This is what makes a re-run with different
// dependency flags download only genuinely new work.
func (s *Store) ScannedVersions(ctx context.Context, packageID int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version FROM package_versions WHERE package_id = ? AND extract_status = 'done'`,
		packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- versions

// Dep is one dependency edge, kept free of any registry types so that the
// store stays independent of the ecosystem being crawled.
type Dep struct {
	Name  string
	Range string
	Kind  string
}

// Finding is one URL sighting to record.
type Finding struct {
	URL               string
	Scheme            string
	Host              string
	Port              string
	Path              string
	RegistrableDomain string
	HasPlaceholder    bool
	SourceKind        string
	Location          string
	Line              int
	Snippet           string
}

// VersionResult is everything learned about one package version.
type VersionResult struct {
	Version       string
	PublishedAt   *time.Time
	TarballURL    string
	TarballBytes  int64
	TarballSHA256 string
	Deps          []Dep
	Findings      []Finding
	// Err, when non-empty, marks the version failed so a later run retries it.
	Err string
}

// SaveVersion writes one version and everything found in it as a single
// transaction. Atomicity matters here: a crash midway must not leave a version
// marked done with only half its URLs recorded, because nothing would ever
// revisit it.
func (s *Store) SaveVersion(ctx context.Context, packageID int64, r VersionResult) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		status := "done"
		var errText any
		if r.Err != "" {
			status, errText = "failed", r.Err
		}

		var published any
		if r.PublishedAt != nil {
			published = r.PublishedAt.UTC().Format(time.RFC3339)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO package_versions
				(package_id, version, published_at, tarball_url, tarball_bytes,
				 tarball_sha256, extract_status, extract_error, extracted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(package_id, version) DO UPDATE SET
				published_at   = excluded.published_at,
				tarball_url    = excluded.tarball_url,
				tarball_bytes  = excluded.tarball_bytes,
				tarball_sha256 = excluded.tarball_sha256,
				extract_status = excluded.extract_status,
				extract_error  = excluded.extract_error,
				extracted_at   = excluded.extracted_at`,
			packageID, r.Version, published, r.TarballURL, r.TarballBytes,
			r.TarballSHA256, status, errText, now()); err != nil {
			return err
		}

		var versionID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM package_versions WHERE package_id = ? AND version = ?`,
			packageID, r.Version).Scan(&versionID); err != nil {
			return err
		}

		for _, d := range r.Deps {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO dependencies (version_id, dep_name, dep_range, kind)
				 VALUES (?, ?, ?, ?)`,
				versionID, d.Name, d.Range, d.Kind); err != nil {
				return err
			}
		}

		for _, f := range r.Findings {
			urlID, err := upsertURL(ctx, tx, f)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO url_occurrences
					(url_id, version_id, source_kind, location, line, snippet)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				urlID, versionID, f.SourceKind, text(f.Location), f.Line, text(f.Snippet))
			if err != nil {
				return err
			}
			// Only count a host sighting when the occurrence was actually new,
			// so re-running a version does not inflate the tally.
			if n, _ := res.RowsAffected(); n > 0 && f.Host != "" {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO hosts (host, registrable_domain, first_seen_at, occurrence_count)
					VALUES (?, ?, ?, 1)
					ON CONFLICT(host) DO UPDATE SET occurrence_count = occurrence_count + 1`,
					text(f.Host), text(f.RegistrableDomain), now()); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func upsertURL(ctx context.Context, tx *sql.Tx, f Finding) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO urls
			(url, scheme, host, port, path, registrable_domain, has_placeholder, first_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		text(f.URL), f.Scheme, text(f.Host), f.Port, text(f.Path),
		text(f.RegistrableDomain), f.HasPlaceholder, now()); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM urls WHERE url = ?`, text(f.URL)).Scan(&id)
	return id, err
}

// ---------------------------------------------------------------- frontier

// FrontierItem is a claimed unit of crawl work.
type FrontierItem struct {
	ID    int64
	Name  string
	Depth int
}

// Enqueue adds packages to the frontier, ignoring any already queued or already
// scanned. It returns how many were genuinely new.
func (s *Store) Enqueue(ctx context.Context, ecosystem string, names []string, depth int, from string) (int, error) {
	var added int
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, name := range names {
			res, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO frontier (ecosystem, name, depth, status, enqueued_from)
				VALUES (?, ?, ?, 'pending', ?)`,
				ecosystem, name, depth, from)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				added++
			}
		}
		return nil
	})
	return added, err
}

// Seed places explicitly requested packages at the root of the crawl.
//
// Unlike Enqueue this resets an existing row: depth returns to 0 and a finished
// or parked package becomes pending again. Without that, seeding a package that
// an earlier crawl already reached as a dependency is a silent no-op - its
// depth stays measured from that older root, so "crawl b --depth 1" claims
// nothing and reports no work rather than crawling b's direct dependencies.
//
// No version is rescanned as a result. Version-level scan state lives in
// package_versions.extract_status, which this does not touch, so reprocessing a
// finished package costs one packument request and no tarballs.
func (s *Store) Seed(ctx context.Context, ecosystem string, names []string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, name := range names {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO frontier (ecosystem, name, depth, status, enqueued_from)
				VALUES (?, ?, 0, 'pending', 'seed')
				ON CONFLICT(ecosystem, name) DO UPDATE SET
					depth = 0, status = 'pending', claimed_at = NULL, attempts = 0`,
				ecosystem, name); err != nil {
				return err
			}
		}
		return nil
	})
}

// PendingBeyondDepth counts queued packages the current depth limit excludes,
// so a crawl that stops early can say so instead of reporting an empty queue.
func (s *Store) PendingBeyondDepth(ctx context.Context, ecosystem string, maxDepth int) (int, error) {
	if maxDepth < 0 {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM frontier WHERE ecosystem = ? AND status = 'pending' AND depth > ?`,
		ecosystem, maxDepth).Scan(&n)
	return n, err
}

// Claim takes the shallowest pending package off the frontier. It returns nil
// when the queue is drained. A negative maxDepth means unlimited.
func (s *Store) Claim(ctx context.Context, ecosystem string, maxDepth int) (*FrontierItem, error) {
	depthClause := ""
	args := []any{now(), ecosystem}
	if maxDepth >= 0 {
		depthClause = " AND depth <= ?"
		args = append(args, maxDepth)
	}

	var item FrontierItem
	err := s.write(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			UPDATE frontier SET status = 'claimed', claimed_at = ?, attempts = attempts + 1
			WHERE id = (
				SELECT id FROM frontier
				WHERE status = 'pending' AND ecosystem = ?`+depthClause+`
				ORDER BY depth, id LIMIT 1
			)
			RETURNING id, name, depth`, args...).Scan(&item.ID, &item.Name, &item.Depth)
	})
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// Complete marks a claimed package finished.
func (s *Store) Complete(ctx context.Context, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE frontier SET status = 'done', claimed_at = NULL WHERE id = ?`, id)
		return err
	})
}

// Fail records an error against a package. Below maxAttempts it goes back to
// pending for a later run to retry; past that it is parked as failed.
func (s *Store) Fail(ctx context.Context, id int64, reason string, maxAttempts int) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE frontier
			SET status = CASE WHEN attempts >= ? THEN 'failed' ELSE 'pending' END,
			    claimed_at = NULL, last_error = ?
			WHERE id = ?`, maxAttempts, reason, id)
		return err
	})
}

// ReleaseClaimed returns in-flight work to the queue. Called on shutdown, and
// on startup to recover from a process that died without unwinding.
func (s *Store) ReleaseClaimed(ctx context.Context) (int, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE frontier SET status = 'pending', claimed_at = NULL WHERE status = 'claimed'`)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return int(n), err
}

// BackfillDeps enqueues packages reachable through dependency edges of the
// given kinds that are not yet known.
//
// This is what makes `crawl <pkg> --dev` incremental: because every dependency
// kind was stored on the first pass, turning on --dev is a query over rows we
// already have. Nothing already scanned is fetched again.
func (s *Store) BackfillDeps(ctx context.Context, ecosystem string, kinds []string) (int, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := []any{ecosystem}
	for _, k := range kinds {
		args = append(args, k)
	}

	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO frontier (ecosystem, name, depth, status, enqueued_from)
			SELECT ?1, d.dep_name, MIN(COALESCE(pf.depth, 0)) + 1, 'pending', 'backfill'
			FROM dependencies d
			JOIN package_versions v ON v.id = d.version_id
			JOIN packages p ON p.id = v.package_id AND p.ecosystem = ?1
			LEFT JOIN frontier pf ON pf.ecosystem = p.ecosystem AND pf.name = p.name
			WHERE d.kind IN (`+placeholders+`)
			  AND NOT EXISTS (
				SELECT 1 FROM packages ex WHERE ex.ecosystem = ?1 AND ex.name = d.dep_name
			  )
			GROUP BY d.dep_name`, args...)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return int(n), err
}

// ---------------------------------------------------------------- runs

// StartRun opens a crawl_runs row and returns its id.
func (s *Store) StartRun(ctx context.Context, seed, flagsJSON string) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO crawl_runs (started_at, seed, flags_json) VALUES (?, ?, ?)`,
			now(), seed, flagsJSON)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// FinishRun closes out a crawl_runs row with its totals.
func (s *Store) FinishRun(ctx context.Context, id int64, packages, versions, urls int) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE crawl_runs
			SET ended_at = ?, packages_processed = ?, versions_processed = ?, urls_found = ?
			WHERE id = ?`, now(), packages, versions, urls, id)
		return err
	})
}
