// Command tarpit builds a corpus of URLs referenced by old package versions,
// so they can later be checked for takeover.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/dee-see/tarpit/internal/crawl"
	"github.com/dee-see/tarpit/internal/registry"
	"github.com/dee-see/tarpit/internal/sample"
	"github.com/dee-see/tarpit/internal/store"
)

const usage = `tarpit - find takeover-able infrastructure referenced by old package versions

Usage:
  tarpit crawl <package>...   crawl from seed packages and extract URLs
  tarpit status               summarize the corpus
  tarpit export               stream the corpus as JSONL

Run "tarpit <command> -h" for the flags of a command.
`

func main() {
	log.SetFlags(0)
	log.SetPrefix("")

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "crawl":
		err = runCrawl(ctx, os.Args[2:])
	case "status":
		err = runStatus(ctx, os.Args[2:])
	case "export":
		err = runExport(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}

	if err != nil {
		log.Fatalf("tarpit: %v", err)
	}
}

func runCrawl(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crawl", flag.ExitOnError)
	dbPath := fs.String("db", "corpus.db", "corpus database path")
	dev := fs.Bool("dev", false, "follow devDependencies (incremental: already-scanned versions are not refetched)")
	noOptional := fs.Bool("no-optional", false, "do not follow optionalDependencies")
	peer := fs.Bool("peer", false, "follow peerDependencies")
	depth := fs.Int("depth", -1, "hops from the seed: 0 crawls the seeds alone, 1 adds their direct dependencies (-1 = unlimited)")
	strategy := fs.String("sample", "minor", "version sampling: minor, major or all")
	prerelease := fs.Bool("prerelease", false, "include prerelease versions")
	concurrency := fs.Int("concurrency", 8, "packages processed in parallel")
	rps := fs.Float64("rate", 10, "registry requests per second")
	maxDecomp := fs.Int64("max-decompressed", 2<<30, "per-version decompression ceiling in bytes")
	attempts := fs.Int("attempts", 3, "attempts before a package is parked as failed")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: tarpit crawl [flags] <package>...\n\nFlags:\n")
		fs.PrintDefaults()
	}
	seeds, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(seeds) == 0 {
		fs.Usage()
		return fmt.Errorf("at least one seed package is required")
	}
	if !sample.Valid(sample.Strategy(*strategy)) {
		return fmt.Errorf("invalid -sample %q: want minor, major or all", *strategy)
	}

	// Runtime edges are always followed: they are what actually lands on a
	// consumer's machine, which is where a takeover has real blast radius.
	kinds := []string{string(registry.DepRuntime)}
	if !*noOptional {
		kinds = append(kinds, string(registry.DepOptional))
	}
	if *dev {
		kinds = append(kinds, string(registry.DepDev))
	}
	if *peer {
		kinds = append(kinds, string(registry.DepPeer))
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	flagsJSON, _ := json.Marshal(map[string]any{
		"sample": *strategy, "depth": *depth, "kinds": kinds,
		"prerelease": *prerelease, "concurrency": *concurrency, "rate": *rps,
	})
	runID, err := db.StartRun(ctx, fmt.Sprint(seeds), string(flagsJSON))
	if err != nil {
		return err
	}

	cfg := crawl.Config{
		Ecosystem:   "npm",
		Client:      registry.NewClient(*rps),
		Store:       db,
		Sample:      sample.Options{Strategy: sample.Strategy(*strategy), IncludePrerelease: *prerelease},
		FollowKinds: kinds,
		MaxDepth:    *depth,
		Concurrency: *concurrency,
		MaxDecomp:   *maxDecomp,
		MaxAttempts: *attempts,
		Logf:        log.Printf,
	}

	log.Printf("crawling %v (edges: %v, sample: %s, db: %s)", seeds, kinds, *strategy, *dbPath)
	result, err := crawl.Run(ctx, cfg, seeds)

	// Record totals even on interruption; the run is still part of the history.
	if ferr := db.FinishRun(context.WithoutCancel(ctx), runID,
		result.Packages, result.Versions, result.URLs); ferr != nil {
		log.Printf("recording run totals: %v", ferr)
	}
	if err != nil {
		return err
	}

	log.Printf("done: %d package(s), %d version(s) scanned, %d skipped as already done, %d URL(s) recorded",
		result.Packages, result.Versions, result.Skipped, result.URLs)
	if ctx.Err() != nil {
		log.Printf("interrupted; rerun the same command to resume where this left off")
	}
	return nil
}

func runStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbPath := fs.String("db", "corpus.db", "corpus database path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	s, err := db.Stats(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("corpus:    %s\n", *dbPath)
	fmt.Printf("packages:  %d\n", s.Packages)
	fmt.Printf("versions:  %d scanned, %d failed\n", s.VersionsDone, s.VersionsFail)
	fmt.Printf("urls:      %d distinct across %d occurrence(s)\n", s.URLs, s.Occurrences)
	fmt.Printf("hosts:     %d distinct, %d registrable domain(s)\n", s.Hosts, s.Domains)

	if len(s.FrontierByStatus) > 0 {
		fmt.Printf("\nfrontier:\n")
		for _, k := range sortedKeys(s.FrontierByStatus) {
			fmt.Printf("  %-10s %d\n", k, s.FrontierByStatus[k])
		}
	}
	if len(s.ByKind) > 0 {
		fmt.Printf("\noccurrences by source:\n")
		for _, k := range sortedKeys(s.ByKind) {
			fmt.Printf("  %-22s %d\n", k, s.ByKind[k])
		}
	}
	return nil
}

func runExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dbPath := fs.String("db", "corpus.db", "corpus database path")
	out := fs.String("o", "-", "output file, or - for stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	w := os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}

	enc := json.NewEncoder(w)
	return db.Export(ctx, func(r store.ExportRow) error { return enc.Encode(r) })
}

// parseArgs parses flags that may appear before, after or between positional
// arguments. Go's flag package stops at the first non-flag argument, which
// would silently turn "crawl left-pad --db corpus.db" into a request to crawl
// packages named "--db" and "corpus.db" - and npm really does have packages
// named things like "1" and "5", so this fails quietly rather than loudly.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
