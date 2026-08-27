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

## Status

Under construction. Phase 1 (extraction) is in progress; the `check` command does not exist yet.

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
