// Package sample chooses which versions of a package to actually download.
//
// Scanning every published version of a popular package means thousands of
// tarballs for very little marginal signal, because a URL introduced in 1.4.0
// usually persists unchanged across every 1.4.x patch. Sampling one version per
// release line keeps coverage of every point where a URL could have been
// introduced or removed, at a fraction of the cost.
package sample

import (
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

// Strategy selects the sampling density.
type Strategy string

const (
	// Minor keeps the highest patch of every major.minor line. The default:
	// dependency and script changes almost always land in a minor bump.
	Minor Strategy = "minor"
	// Major keeps only the highest version of each major line.
	Major Strategy = "major"
	// All keeps every published version.
	All Strategy = "all"
)

// Valid reports whether s names a known strategy.
func Valid(s Strategy) bool {
	return s == Minor || s == Major || s == All
}

// Options configures a selection.
type Options struct {
	Strategy Strategy
	// IncludePrerelease keeps -alpha/-beta/-rc versions, which are excluded by
	// default: they are rarely depended on, and they inflate the sample.
	IncludePrerelease bool
	// Latest is the version behind the "latest" dist-tag. It is always kept,
	// whatever the strategy, because it is what a fresh install resolves to.
	Latest string
}

// Pick returns the subset of versions to scan, sorted oldest to newest.
// Versions that are not valid semver are skipped, since they cannot be grouped
// into a release line; npm has rejected them at publish time for many years.
func Pick(versions []string, opts Options) []string {
	keep := map[string]bool{}
	if opts.Latest != "" && contains(versions, opts.Latest) {
		keep[opts.Latest] = true
	}

	best := map[string]string{} // release line -> highest version in it
	for _, v := range versions {
		sv := canonical(v)
		if sv == "" {
			continue
		}
		if !opts.IncludePrerelease && semver.Prerelease(sv) != "" {
			continue
		}
		if opts.Strategy == All {
			keep[v] = true
			continue
		}

		line := semver.MajorMinor(sv)
		if opts.Strategy == Major {
			line = semver.Major(sv)
		}
		if cur, ok := best[line]; !ok || semver.Compare(canonical(cur), sv) < 0 {
			best[line] = v
		}
	}
	for _, v := range best {
		keep[v] = true
	}

	out := make([]string, 0, len(keep))
	for v := range keep {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := canonical(out[i]), canonical(out[j])
		if c := semver.Compare(a, b); c != 0 {
			return c < 0
		}
		return out[i] < out[j]
	})
	return out
}

// canonical converts an npm version string into the "v"-prefixed form
// golang.org/x/mod/semver expects, returning "" if it is not valid semver.
func canonical(v string) string {
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
