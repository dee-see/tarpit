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
sampled tarball — into a local SQLite corpus that records which package cited which URL, in which
file, across which span of versions.

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
crawling [sqlite3] (edges: [runtime optional], dev: seeds, sample: major, db: corpus.db)
sqlite3: 5 version(s) scanned, 0 skipped, 5132 URL(s), depth 0
```

Then ask what a given host is doing in the corpus:

```sql
SELECT p.name, l.path, fv.version || '..' || lv.version AS versions, o.version_count
FROM url_occurrences o
JOIN urls u              ON u.id  = o.url_id
JOIN packages p          ON p.id  = o.package_id
JOIN locations l         ON l.id  = o.location_id
JOIN package_versions fv ON fv.id = o.first_version_id
JOIN package_versions lv ON lv.id = o.last_version_id
WHERE u.host = 'd332vdhbectycy.cloudfront.net';
```

```
aws-crt | README.md        | 1.9.8..1.33.1 | 26
aws-crt | scripts/build.js | 1.9.8..1.33.1 | 26
```

One row says the interesting thing: across 26 sampled versions spanning `1.9.8` to `1.33.1`,
`aws-crt` reaches that CloudFront distribution from `scripts/build.js`. Whether that is reachable
from an install hook is a question for the package, answered by reading it.

### Notable flags

| Flag | Meaning |
|---|---|
| `--depth N` | Hops from the seed. `0` is the seeds alone, `-1` (default) is unlimited. |
| `--sample minor\|major\|all` | Version density. Default `minor`: the highest patch of each release line. |
| `--dev seeds\|all\|none` | Which devDependency edges to follow. Default `seeds`. Incremental - see below. |
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
281,745 distinct URLs across 1.16M references
max depth 32       223 MB corpus
```

Two packages dominated the cost, in opposite ways. `aws-sdk` bumps its minor on nearly every
release, so minor sampling kept 1,712 of its 1,936 versions and it ran for twelve hours while
contributing almost no new hosts. `flow-bin` ships a compiled binary per platform per release,
averaging 29 MB a version across 329 of them. Sampling density is the lever for the first shape;
there is currently no good lever for the second.

## How it works

**Every URL is recorded with where it was found** — an archive-relative file path, or a dotted
manifest field like `scripts.postinstall` or `binary.host`. That is provenance, not a severity
rating, and the distinction is deliberate. An earlier version of this tool classified each URL
into an `install-time` / `docs` / `source` taxonomy, and the classification was wrong often
enough to be worse than useless.

The reason is that install-time reach is transitive and the classifier was not. A hook reads
`"install": "node scripts/install.js"`, so `scripts/install.js` was marked install-time — but if
that file does `require('./build.js')`, the URL actually being fetched sits in `build.js`, which
was marked ordinary source. In the react corpus `aws-crt` cites the same CloudFront distribution
in the same file 26 times; the old scheme called it install-time in 6 of them and inert source in
the other 20, purely according to which filename that version's hook happened to name.

So `tarpit` does not guess. It records the breadcrumb, and exploitability is established by hand
once a host is known to be claimable — which is the rarer property, and therefore the one worth
filtering on first. Automating the transitive walk is worth doing only if that manual step ever
becomes the bottleneck.

**Tarballs never touch disk.** The response body is piped straight through gzip into tar and
scanned in 1 MiB chunks with an overlap between them, so nothing is held whole in memory and
nothing is skipped for being large.

**One row per (URL, package, file), not per version.** A package's README cites the same URL in
every version sampled, and storing that separately each time cost more than the rest of the
corpus put together. The version span survives the collapse — `first_version_id`,
`last_version_id` and `version_count` — because old versions are the entire premise.

**devDependencies are followed from the seeds and nowhere else.** npm does not install the
devDependencies *of your dependencies* - they install only when you are developing that package
itself. So a lapsed domain on a transitive dev dep is a risk to that package's maintainers and
its CI, not an install-time vector for anyone downstream. A seed's own dev deps are the opposite:
they install on your machine and in your CI, with whatever credentials those have, which is the
strongest install-time story in the corpus. `--dev seeds` is the default for that reason.

`--dev all` follows them at every depth. On the react corpus that is 3,460 package names the
crawl has never seen from the first hop alone - a 75% increase - each arriving with its own dev
tree, against 17.9 hours already spent reaching 4,593 packages. `--dev none` skips them entirely.

**Every dependency kind is recorded, even the ones being ignored.** `--dev` controls what gets
*enqueued*, never what gets *stored*. So this:

```console
$ tarpit crawl react              # runtime + optional, plus the seed's dev deps
$ tarpit crawl react --dev all    # now devDependencies at every depth
```

...costs no re-crawling. The second command finds newly-reachable packages with a query over
edges the first one already wrote, and skips every version already on disk.

"Seed" means any package at depth 0 in the frontier, which is every package ever named on a
`crawl` command line, not just this run's. Naming one pins it there permanently.

## Status

Phase 1 (extraction) works. The `check` command does not exist yet; it will consume the corpus
through `export` rather than reimplementing takeover fingerprints here.

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
