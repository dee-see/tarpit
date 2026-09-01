package sample

import (
	"reflect"
	"slices"
	"sort"
	"testing"

	"golang.org/x/mod/semver"
)

// pick is the batch form of the selection, kept here rather than in the package
// because nothing in the crawler calls it: Retainer decides incrementally so
// that the registry document never has to be held whole. It survives as the
// oracle that Retainer is checked against, and as the readable statement of
// what the sampling rules are.
func pick(versions []string, opts Options, latest string) []string {
	keep := map[string]bool{}
	if latest != "" && slices.Contains(versions, latest) {
		keep[latest] = true
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

func TestPickOracle(t *testing.T) {
	versions := []string{
		"0.1.0", "1.0.0", "1.0.1", "1.0.2",
		"1.1.0", "1.1.9", "1.2.0",
		"2.0.0-beta.1", "2.0.0", "2.0.1",
		"not-a-version",
	}

	tests := []struct {
		name   string
		opts   Options
		latest string
		want   []string
	}{
		{
			name: "minor keeps highest patch per line",
			opts: Options{Strategy: Minor},
			want: []string{"0.1.0", "1.0.2", "1.1.9", "1.2.0", "2.0.1"},
		},
		{
			name: "major keeps highest per major line",
			opts: Options{Strategy: Major},
			want: []string{"0.1.0", "1.2.0", "2.0.1"},
		},
		{
			name: "prereleases excluded by default",
			opts: Options{Strategy: All},
			want: []string{"0.1.0", "1.0.0", "1.0.1", "1.0.2", "1.1.0", "1.1.9", "1.2.0", "2.0.0", "2.0.1"},
		},
		{
			name: "prereleases included on request",
			opts: Options{Strategy: Major, IncludePrerelease: true},
			want: []string{"0.1.0", "1.2.0", "2.0.1"},
		},
		{
			name:   "latest dist-tag always kept even off-strategy",
			opts:   Options{Strategy: Major},
			latest: "1.0.1",
			want:   []string{"0.1.0", "1.0.1", "1.2.0", "2.0.1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pick(versions, tc.opts, tc.latest); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("pick() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPickSkipsInvalidSemver(t *testing.T) {
	got := pick([]string{"garbage", "1.0.0"}, Options{Strategy: Minor}, "")
	if !reflect.DeepEqual(got, []string{"1.0.0"}) {
		t.Errorf("pick() = %v, want [1.0.0]", got)
	}
}
