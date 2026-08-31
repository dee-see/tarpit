package sample

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// The streaming selection must agree with the batch one exactly, whatever order
// versions arrive in - the registry document does not promise any.
func TestRetainerMatchesPick(t *testing.T) {
	versions := []string{
		"0.1.0", "1.0.0", "1.0.1", "1.0.2", "1.1.0", "1.1.9", "1.2.0",
		"2.0.0-beta.1", "2.0.0", "2.0.1", "10.0.0", "2.10.0", "not-a-version",
	}

	for _, strategy := range []Strategy{Minor, Major, All} {
		for _, pre := range []bool{false, true} {
			opts := Options{Strategy: strategy, IncludePrerelease: pre}

			for attempt := 0; attempt < 20; attempt++ {
				shuffled := append([]string(nil), versions...)
				rng := rand.New(rand.NewSource(int64(attempt)))
				rng.Shuffle(len(shuffled), func(i, j int) {
					shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
				})

				retained := map[string]bool{}
				r := NewRetainer(opts)
				for _, v := range shuffled {
					keep, evict := r.Offer(v)
					if evict != "" {
						if !retained[evict] {
							t.Fatalf("%s/%v: evicted %q which was never retained", strategy, pre, evict)
						}
						delete(retained, evict)
					}
					if keep {
						retained[v] = true
					}
				}

				got := make([]string, 0, len(retained))
				for v := range retained {
					got = append(got, v)
				}
				want := Pick(versions, opts)
				sort.Strings(got)
				sortedWant := append([]string(nil), want...)
				sort.Strings(sortedWant)
				if !reflect.DeepEqual(got, sortedWant) {
					t.Fatalf("%s/prerelease=%v order %d:\n  retainer = %v\n  pick     = %v",
						strategy, pre, attempt, got, sortedWant)
				}
			}
		}
	}
}
