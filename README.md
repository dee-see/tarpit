# tarpit

> Artifacts sink in, get preserved perfectly, and stay there. We go dig them out.

Package registries are immutable. `left-pad@0.1.3` is as installable today as it was in 2016,
and old versions stay pinned in lockfiles, vendored into container images, and resolved by CI.

If a version published in 2016 has a `postinstall` that fetches a binary from a domain whose
registration lapsed in 2019, that is not a historical curiosity — it is a live install-time code
execution vector that anyone can claim for the price of a domain.

Existing takeover tooling scans the surface a project presents *today*. `tarpit` walks the
version history instead.

## What it does

`tarpit` crawls a package ecosystem from a seed package, samples each package's version history,
and extracts every URL it can find — from both the registry metadata and the contents of every
sampled tarball — into a local SQLite corpus that maps each URL back to the exact package,
version, and file it came from.

Checking whether those URLs are claimable is a **separate command**, by design. Extraction is
slow, network-heavy, and produces immutable results; takeover fingerprints are cheap and change
constantly. Keeping them apart means improving a fingerprint re-queries a local database instead
of re-crawling the registry.

## Usage

```
tarpit crawl <package>...   crawl from seed packages and extract URLs
tarpit status               summarize the corpus
tarpit export               stream the corpus as JSONL
```

Crawl a package and its direct dependencies:

```console
$ tarpit crawl sqlite3 --depth 1 --sample major
crawling [sqlite3] (edges: [runtime optional], sample: major, db: corpus.db)
sqlite3: 5 version(s) scanned, 0 skipped, 5132 URL(s), depth 0
```

Then look at what it found, worst first:

```sql
SELECT o.source_kind, p.name || '@' || v.version, u.host, o.location
FROM url_occurrences o
JOIN urls u             ON u.id = o.url_id
JOIN package_versions v ON v.id = o.version_id
JOIN packages p         ON p.id = v.package_id
WHERE o.source_kind IN ('metadata_script', 'file_install_script', 'metadata_binary');
```

```
metadata_binary | sqlite3@2.2.7  | mapbox-node-binary.s3.amazonaws.com | binary.host
metadata_binary | sqlite3@3.1.13 | mapbox-node-binary.s3.amazonaws.com | binary.host
metadata_binary | sqlite3@4.2.0  | mapbox-node-binary.s3.amazonaws.com | binary.host
```

### Notable flags

| Flag | Meaning |
|---|---|
| `--depth N` | Hops from the seed. `0` is the seeds alone, `-1` (default) is unlimited. |
| `--sample minor\|major\|all` | Version density. Default `minor`: the highest patch of each release line. |
| `--dev` | Also follow devDependencies. Incremental - see below. |
| `--no-optional`, `--peer` | Adjust which dependency kinds are followed. |
| `--prerelease` | Include `-alpha` / `-rc` versions, which are skipped by default. |
| `--rate N` | Registry requests per second. Default 10. |
| `--concurrency N` | Packages in flight. Defaults to the machine's core count. |
| `--attempts N` | Failures before a package is parked. Default 3. Interruptions do not count. |

The crawl is a persistent queue in SQLite, so it runs until you stop it and resumes where it
left off. Interrupt it with Ctrl-C and rerun the same command; anything left in flight by a
process that died is recovered on the next start.

### Scale

Crawling the full transitive tree from `react`, sampling one version per minor line:

```
4,593 packages     61,140 versions     33.4 GB streamed
7,239 hosts        5,395 registrable domains
281,745 distinct URLs across 7.6M occurrences
max depth 32       1.1 GB corpus
```

Of those 7.6M occurrences, 1,389 are install-time (`file_install_script`, `metadata_dep_spec`,
`metadata_binary`) - 0.018%. That ratio is the whole reason `source_kind` is recorded per
occurrence rather than filtered at extraction time.

Two packages dominated the cost, in opposite ways. `aws-sdk` bumps its minor on nearly every
release, so minor sampling kept 1,712 of its 1,936 versions and it ran for twelve hours while
contributing almost no new hosts. `flow-bin` ships a compiled binary per platform per release,
averaging 29 MB a version across 329 of them. Sampling density is the lever for the first shape;
there is currently no good lever for the second.

## How it works

Every URL is stored with a `source_kind` recording where it came from, because that is what
separates a finding from noise. In the `sqlite3` crawl above, 3584 URLs came from README files
and 3 came from `binary.host`; only the latter is fetched during `npm install`.

| Kind | Origin | Runs at install? |
|---|---|---|
| `metadata_script` | `preinstall` / `install` / `postinstall` / `prepare` | yes |
| `file_install_script` | a file those hooks invoke, e.g. `scripts/install.js` | yes |
| `metadata_binary` | `binary.host` (node-pre-gyp, prebuild-install) | yes |
| `metadata_dep_spec` | an `http(s)` or `git+` dependency spec | yes |
| `file_build_config` | `binding.gyp`, `Makefile`, shell scripts, CI config | sometimes |
| `metadata_repo` | `repository`, `homepage`, `bugs`, `funding` | no |
| `file_source` / `file_docs` / `file_test` | everything else in the tarball | no |

Two things make the crawl cheap to extend:

**Tarballs never touch disk.** The response body is piped straight through gzip into tar and
scanned in 1 MiB chunks with an overlap between them, so nothing is held whole in memory and
nothing is skipped for being large.

**Every dependency kind is recorded, even the ones being ignored.** `--dev` controls what gets
*enqueued*, never what gets *stored*. So this:

```console
$ tarpit crawl react              # runtime + optional edges
$ tarpit crawl react --dev        # now also devDependencies
```

...costs no re-crawling. The second command finds newly-reachable packages with a query over
edges the first one already wrote, and skips every version already on disk.

## Status

Phase 1 (extraction) works. The `check` command does not exist yet.

## Scope and conduct

`tarpit` is built for supply-chain security research and defensive auditing.

- The extraction phase talks to the package registry and nothing else. It makes no requests to
  any host it discovers.
- The forthcoming `check` command will **detect** claimability only. It will never register a
  domain, create a bucket, or otherwise take control of a resource.
- Corpus databases are gitignored. They describe unreported third-party infrastructure; treat
  them accordingly and follow coordinated disclosure for anything you find.

## License

MIT
