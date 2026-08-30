// Package crawl walks a registry from a seed package and fills the corpus.
package crawl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dee-see/tarpit/internal/extract"
	"github.com/dee-see/tarpit/internal/registry"
	"github.com/dee-see/tarpit/internal/sample"
	"github.com/dee-see/tarpit/internal/store"
	"github.com/dee-see/tarpit/internal/tarball"
)

// Config controls a crawl.
type Config struct {
	Ecosystem   string
	Client      *registry.Client
	Store       *store.Store
	Sample      sample.Options
	FollowKinds []string
	// MaxDepth counts hops from the seed: 0 crawls the seeds alone, 1 adds
	// their direct dependencies. Negative means unlimited.
	MaxDepth    int
	Concurrency int
	MaxDecomp   int64
	MaxAttempts int
	Logf        func(string, ...any)
}

// Result reports what a crawl accomplished.
type Result struct {
	Packages int
	Versions int
	URLs     int
	Skipped  int
}

// Run drains the frontier, seeding it first. It returns when the queue is empty
// or the context is cancelled; either way, in-flight work is returned to the
// queue so a later invocation picks up exactly where this one stopped.
func Run(ctx context.Context, cfg Config, seeds []string) (Result, error) {
	var result Result

	// Recover anything a previously killed process left claimed.
	if n, err := cfg.Store.ReleaseClaimed(ctx); err != nil {
		return result, err
	} else if n > 0 {
		cfg.Logf("recovered %d package(s) left in flight by a previous run", n)
	}

	if err := cfg.Store.Seed(ctx, cfg.Ecosystem, seeds); err != nil {
		return result, err
	}

	// Enqueue anything reachable through the currently-followed dependency
	// kinds that earlier runs stored but did not follow. This is what makes
	// turning on --dev incremental rather than a re-crawl.
	if n, err := cfg.Store.BackfillDeps(ctx, cfg.Ecosystem, cfg.FollowKinds); err != nil {
		return result, err
	} else if n > 0 {
		cfg.Logf("backfilled %d package(s) newly reachable via %v edges", n, cfg.FollowKinds)
	}

	var (
		wg     sync.WaitGroup
		active atomic.Int64
		pkgs   atomic.Int64
		vers   atomic.Int64
		urls   atomic.Int64
		skips  atomic.Int64
	)

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				item, err := cfg.Store.Claim(ctx, cfg.Ecosystem, cfg.MaxDepth)
				if err != nil {
					cfg.Logf("claim failed: %v", err)
					return
				}
				if item == nil {
					// The queue is empty, but a peer may still be about to
					// enqueue this package's dependencies. Only stop once no
					// worker is doing anything.
					if active.Load() == 0 {
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
					continue
				}

				active.Add(1)
				stats, err := processPackage(ctx, cfg, *item)
				active.Add(-1)

				pkgs.Add(1)
				vers.Add(int64(stats.versions))
				urls.Add(int64(stats.urls))
				skips.Add(int64(stats.skipped))

				if err != nil {
					if ctx.Err() != nil {
						return
					}
					cfg.Logf("%s: %v", item.Name, err)
					if ferr := cfg.Store.Fail(ctx, item.ID, err.Error(), cfg.MaxAttempts); ferr != nil {
						cfg.Logf("recording failure for %s: %v", item.Name, ferr)
					}
					continue
				}
				if err := cfg.Store.Complete(ctx, item.ID); err != nil {
					cfg.Logf("completing %s: %v", item.Name, err)
				}
			}
		}()
	}
	wg.Wait()

	// Whether we drained the queue or were interrupted, nothing stays claimed.
	if n, err := cfg.Store.ReleaseClaimed(context.WithoutCancel(ctx)); err == nil && n > 0 {
		cfg.Logf("returned %d in-flight package(s) to the queue", n)
	}

	// A depth limit can leave the queue non-empty while nothing is claimable.
	// Saying so beats reporting "0 packages" and letting it read as "all done".
	if n, err := cfg.Store.PendingBeyondDepth(
		context.WithoutCancel(ctx), cfg.Ecosystem, cfg.MaxDepth); err == nil && n > 0 {
		cfg.Logf("%d package(s) remain queued beyond --depth %d; raise it to continue", n, cfg.MaxDepth)
	}

	result = Result{
		Packages: int(pkgs.Load()), Versions: int(vers.Load()),
		URLs: int(urls.Load()), Skipped: int(skips.Load()),
	}
	return result, nil
}

type packageStats struct{ versions, urls, skipped int }

func processPackage(ctx context.Context, cfg Config, item store.FrontierItem) (packageStats, error) {
	var stats packageStats

	packument, err := cfg.Client.Packument(ctx, item.Name)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			// Unpublished packages are ordinary in old dependency trees. Record
			// the package so the crawl does not keep rediscovering it, and move
			// on rather than burning retries.
			cfg.Logf("%s: not in registry (unpublished?)", item.Name)
			if _, err := cfg.Store.UpsertPackage(ctx, cfg.Ecosystem, item.Name); err != nil {
				return stats, err
			}
			return stats, nil
		}
		return stats, err
	}

	packageID, err := cfg.Store.UpsertPackage(ctx, cfg.Ecosystem, item.Name)
	if err != nil {
		return stats, err
	}

	opts := cfg.Sample
	opts.Latest = packument.Latest()
	versions := sample.Pick(packument.VersionList(), opts)

	done, err := cfg.Store.ScannedVersions(ctx, packageID)
	if err != nil {
		return stats, err
	}

	follow := map[string]bool{}
	for _, k := range cfg.FollowKinds {
		follow[k] = true
	}
	nextHop := map[string]bool{}

	for _, version := range versions {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		if done[version] {
			stats.skipped++
			continue
		}

		result, deps := scanVersion(ctx, cfg, packument, version)
		if err := cfg.Store.SaveVersion(ctx, packageID, result); err != nil {
			return stats, fmt.Errorf("saving %s@%s: %w", item.Name, version, err)
		}
		stats.versions++
		stats.urls += len(result.Findings)

		for _, d := range deps {
			if follow[string(d.Kind)] {
				nextHop[d.Name] = true
			}
		}
	}

	cfg.Logf("%s: %d version(s) scanned, %d skipped, %d URL(s), depth %d",
		item.Name, stats.versions, stats.skipped, stats.urls, item.Depth)

	if cfg.MaxDepth >= 0 && item.Depth+1 > cfg.MaxDepth {
		return stats, nil
	}
	names := make([]string, 0, len(nextHop))
	for n := range nextHop {
		names = append(names, n)
	}
	if len(names) > 0 {
		if _, err := cfg.Store.Enqueue(ctx, cfg.Ecosystem, names, item.Depth+1, item.Name); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// scanVersion extracts everything from one version. A failure to read the
// tarball is recorded on the version row rather than returned, so that the
// metadata findings - which are often the valuable ones - are still kept and
// the version is retried on a later run.
func scanVersion(ctx context.Context, cfg Config, packument *registry.Packument, version string) (store.VersionResult, []registry.Dependency) {
	result := store.VersionResult{Version: version}

	if t, ok := packument.PublishedAt(version); ok {
		result.PublishedAt = &t
	}

	manifest, err := registry.ParseManifest(packument.Versions[version])
	if err != nil {
		result.Err = fmt.Sprintf("parse manifest: %v", err)
		return result, nil
	}

	result.TarballURL = manifest.TarballURL
	for _, d := range manifest.Deps {
		result.Deps = append(result.Deps, store.Dep{Name: d.Name, Range: d.Range, Kind: string(d.Kind)})
	}
	fromManifest := map[string]bool{}
	for _, f := range manifest.URLs {
		fromManifest[f.URL.Normalized] = true
		result.Findings = append(result.Findings, toFinding(f.URL, f.Kind, f.Location, 0, f.Snippet))
	}

	if manifest.TarballURL == "" {
		result.Err = "version has no tarball URL"
		return result, manifest.Deps
	}

	size, sum, findings, err := scanTarball(ctx, cfg, manifest)
	result.TarballBytes = size
	result.TarballSHA256 = sum
	if err != nil {
		result.Err = fmt.Sprintf("tarball: %v", err)
		return result, manifest.Deps
	}
	for _, f := range findings {
		// The archive's root package.json is the same document the registry
		// serves as the manifest, so most of its URLs are already recorded with
		// better provenance. Only exact duplicates are dropped: npm rewrites
		// repository.url on publish ("https://..." becomes "git+https://..."),
		// and a nested package.json is a different document entirely - a
		// vendored dependency or build output - so both must survive.
		if f.Path == "package.json" && fromManifest[f.URL.Normalized] {
			continue
		}
		result.Findings = append(result.Findings, toFinding(f.URL, f.Kind, f.Path, f.Line, f.Snippet))
	}
	return result, manifest.Deps
}

// scanTarball streams the artifact through the scanner without ever writing it
// to disk, hashing and counting the bytes as they pass.
func scanTarball(ctx context.Context, cfg Config, manifest *registry.Manifest) (int64, string, []tarball.Finding, error) {
	body, err := cfg.Client.Tarball(ctx, manifest.TarballURL)
	if err != nil {
		return 0, "", nil, err
	}
	defer body.Close()

	hasher := sha256.New()
	counter := &countingWriter{}
	stream := io.TeeReader(body, io.MultiWriter(hasher, counter))

	findings, scanErr := tarball.Scan(stream, tarball.Options{
		MaxDecompressed: cfg.MaxDecomp,
		InstallScripts:  manifest.InstallScriptFiles,
	})

	// Drain whatever the scanner did not consume so the hash and byte count
	// describe the whole artifact, and the connection can be reused.
	io.Copy(io.Discard, stream)

	return counter.n, hex.EncodeToString(hasher.Sum(nil)), findings, scanErr
}

func toFinding(u extract.URL, kind extract.SourceKind, location string, line int, snippet string) store.Finding {
	return store.Finding{
		URL:               u.Normalized,
		Scheme:            u.Scheme,
		Host:              u.Host,
		Port:              u.Port,
		Path:              u.Path,
		RegistrableDomain: u.RegistrableDomain,
		HasPlaceholder:    u.HasPlaceholder,
		SourceKind:        string(kind),
		Location:          location,
		Line:              line,
		Snippet:           snippet,
	}
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
