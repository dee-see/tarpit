package store

// schemaSQL is applied on every Open. Every statement is idempotent, so this
// doubles as the migration path for now; once the corpus is something people
// keep, versioned migrations go in schema_version.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS packages (
  id            INTEGER PRIMARY KEY,
  ecosystem     TEXT NOT NULL,
  name          TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  UNIQUE(ecosystem, name)
);

-- extract_status is the authoritative scan history: a package version is
-- scanned at most once ever, no matter how many crawls reach it or which
-- dependency kinds were being followed at the time.
CREATE TABLE IF NOT EXISTS package_versions (
  id             INTEGER PRIMARY KEY,
  package_id     INTEGER NOT NULL REFERENCES packages(id),
  version        TEXT NOT NULL,
  published_at   TEXT,
  tarball_url    TEXT,
  tarball_bytes  INTEGER,
  tarball_sha256 TEXT,
  extract_status TEXT NOT NULL DEFAULT 'pending',
  extract_error  TEXT,
  extracted_at   TEXT,
  UNIQUE(package_id, version)
);
CREATE INDEX IF NOT EXISTS idx_versions_status ON package_versions(extract_status);

-- Every dependency kind is stored, including dev, regardless of what the crawl
-- was configured to follow. Enabling --dev later is then a query over these
-- rows rather than a re-crawl.
CREATE TABLE IF NOT EXISTS dependencies (
  version_id INTEGER NOT NULL REFERENCES package_versions(id),
  dep_name   TEXT NOT NULL,
  dep_range  TEXT NOT NULL,
  kind       TEXT NOT NULL,
  UNIQUE(version_id, dep_name, kind)
);
CREATE INDEX IF NOT EXISTS idx_deps_name_kind ON dependencies(dep_name, kind);

-- scheme, port and path are not stored: all three are url.Parse(url), and the
-- parsed copy cost more than the string it was derived from.
CREATE TABLE IF NOT EXISTS urls (
  id                 INTEGER PRIMARY KEY,
  url                TEXT NOT NULL UNIQUE,
  host               TEXT NOT NULL,
  registrable_domain TEXT NOT NULL,
  has_placeholder    INTEGER NOT NULL,
  first_seen_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_urls_host ON urls(host);
CREATE INDEX IF NOT EXISTS idx_urls_domain ON urls(registrable_domain);

-- File paths repeat relentlessly: 55k distinct paths backed 7.6M occurrence
-- rows in the first full crawl, and every byte was paid for twice because the
-- path sat inside the UNIQUE constraint as well as the row.
CREATE TABLE IF NOT EXISTS locations (
  id   INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE
);

-- One row per (url, package, file) rather than per version. A package's README
-- cites the same URL in every version sampled, and storing that separately each
-- time cost more than the rest of the corpus put together.
--
-- The version range survives the collapse because old versions are the premise
-- of the tool: first_version_id and last_version_id bound where the reference
-- was seen, ordered by publication date, and version_count says how many
-- sampled versions in between carried it.
CREATE TABLE IF NOT EXISTS url_occurrences (
  url_id           INTEGER NOT NULL REFERENCES urls(id),
  package_id       INTEGER NOT NULL REFERENCES packages(id),
  location_id      INTEGER NOT NULL REFERENCES locations(id),
  first_version_id INTEGER NOT NULL REFERENCES package_versions(id),
  last_version_id  INTEGER NOT NULL REFERENCES package_versions(id),
  version_count    INTEGER NOT NULL DEFAULT 1,
  UNIQUE(url_id, package_id, location_id)
);
CREATE INDEX IF NOT EXISTS idx_occ_package ON url_occurrences(package_id);

-- Seam for the phase-two checker: last_checked_at lets it re-run cheaply over
-- only what it has not seen since fingerprints last changed.
CREATE TABLE IF NOT EXISTS hosts (
  host               TEXT PRIMARY KEY,
  registrable_domain TEXT NOT NULL,
  first_seen_at      TEXT NOT NULL,
  occurrence_count   INTEGER NOT NULL DEFAULT 0,
  last_checked_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_hosts_unchecked ON hosts(last_checked_at);

CREATE TABLE IF NOT EXISTS frontier (
  id            INTEGER PRIMARY KEY,
  ecosystem     TEXT NOT NULL,
  name          TEXT NOT NULL,
  depth         INTEGER NOT NULL,
  status        TEXT NOT NULL DEFAULT 'pending',
  claimed_at    TEXT,
  attempts      INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT,
  enqueued_from TEXT,
  UNIQUE(ecosystem, name)
);
CREATE INDEX IF NOT EXISTS idx_frontier_status ON frontier(status, depth, id);

CREATE TABLE IF NOT EXISTS crawl_runs (
  id                 INTEGER PRIMARY KEY,
  started_at         TEXT NOT NULL,
  ended_at           TEXT,
  seed               TEXT NOT NULL,
  flags_json         TEXT NOT NULL,
  packages_processed INTEGER NOT NULL DEFAULT 0,
  versions_processed INTEGER NOT NULL DEFAULT 0,
  urls_found         INTEGER NOT NULL DEFAULT 0
);
`
