// Package sample chooses which versions of a package to actually download.
//
// Scanning every published version of a popular package means thousands of
// tarballs for very little marginal signal, because a URL introduced in 1.4.0
// usually persists unchanged across every 1.4.x patch. Sampling one version per
// release line keeps coverage of every point where a URL could have been
// introduced or removed, at a fraction of the cost.
//
// The selection itself lives in Retainer, which decides incrementally so that
// the registry document never has to be held whole.
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

// Sort orders versions oldest to newest. Anything that is not valid semver
// sorts last, lexically among itself, so the order is always deterministic.
func Sort(versions []string) {
	sort.Slice(versions, func(i, j int) bool {
		a, b := canonical(versions[i]), canonical(versions[j])
		switch {
		case a == "" && b == "":
			return versions[i] < versions[j]
		case a == "":
			return false
		case b == "":
			return true
		}
		if c := semver.Compare(a, b); c != 0 {
			return c < 0
		}
		return versions[i] < versions[j]
	})
}
