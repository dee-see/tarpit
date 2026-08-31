package sample

import "golang.org/x/mod/semver"

// Retainer is the streaming form of Pick.
//
// Pick needs the whole version list up front, which means the caller has to
// have parsed the entire registry document before it can decide what to keep.
// For a package like aws-sdk that document is hundreds of megabytes, and
// holding it - once per in-flight worker - is enough to get the process
// OOM-killed. A Retainer instead answers "keep this one?" from the version
// string alone, so a manifest that will not be sampled is never materialised.
//
// Offer must be called for every published version, in any order. The retained
// set afterwards is identical to what Pick would have returned, minus the
// Latest dist-tag, which the caller reconciles separately because it may not be
// known until after the versions have streamed past.
type Retainer struct {
	strategy   Strategy
	prerelease bool
	best       map[string]string // release line -> best version seen so far
}

// NewRetainer starts a selection under opts. Opts.Latest is ignored; see the
// type comment.
func NewRetainer(opts Options) *Retainer {
	return &Retainer{
		strategy:   opts.Strategy,
		prerelease: opts.IncludePrerelease,
		best:       map[string]string{},
	}
}

// Offer reports whether version's manifest should be retained, and names a
// previously retained version that it supersedes and which may now be dropped
// ("" if none).
func (r *Retainer) Offer(version string) (keep bool, evict string) {
	sv := canonical(version)
	if sv == "" {
		return false, ""
	}
	if !r.prerelease && semver.Prerelease(sv) != "" {
		return false, ""
	}
	if r.strategy == All {
		return true, ""
	}

	line := semver.MajorMinor(sv)
	if r.strategy == Major {
		line = semver.Major(sv)
	}

	current, seen := r.best[line]
	if seen && semver.Compare(canonical(current), sv) >= 0 {
		return false, ""
	}
	r.best[line] = version
	if seen {
		return true, current
	}
	return true, ""
}
