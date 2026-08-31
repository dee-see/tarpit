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

CREATE TABLE IF NOT EXISTS urls (
  id                 INTEGER PRIMARY KEY,
  url                TEXT NOT NULL UNIQUE,
  scheme             TEXT NOT NULL,
  host               TEXT NOT NULL,
  port               TEXT NOT NULL,
  path               TEXT NOT NULL,
  registrable_domain TEXT NOT NULL,
  has_placeholder    INTEGER NOT NULL,
  first_seen_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_urls_host ON urls(host);
CREATE INDEX IF NOT EXISTS idx_urls_domain ON urls(registrable_domain);

CREATE TABLE IF NOT EXISTS url_occurrences (
  url_id      INTEGER NOT NULL REFERENCES urls(id),
  version_id  INTEGER NOT NULL REFERENCES package_versions(id),
  source_kind TEXT NOT NULL,
  location    TEXT NOT NULL,
  line        INTEGER NOT NULL,
  UNIQUE(url_id, version_id, source_kind, location)
);
CREATE INDEX IF NOT EXISTS idx_occ_version ON url_occurrences(version_id);
CREATE INDEX IF NOT EXISTS idx_occ_kind ON url_occurrences(source_kind);

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
