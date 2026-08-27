package sample

import (
	"reflect"
	"testing"
)

func TestPick(t *testing.T) {
	versions := []string{
		"0.1.0", "1.0.0", "1.0.1", "1.0.2",
		"1.1.0", "1.1.9", "1.2.0",
		"2.0.0-beta.1", "2.0.0", "2.0.1",
		"not-a-version",
	}

	tests := []struct {
		name string
		opts Options
		want []string
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
			name: "latest dist-tag always kept even off-strategy",
			opts: Options{Strategy: Major, Latest: "1.0.1"},
			want: []string{"0.1.0", "1.0.1", "1.2.0", "2.0.1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Pick(versions, tc.opts); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Pick() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPickSkipsInvalidSemver(t *testing.T) {
	got := Pick([]string{"garbage", "1.0.0"}, Options{Strategy: Minor})
	if !reflect.DeepEqual(got, []string{"1.0.0"}) {
		t.Errorf("Pick() = %v, want [1.0.0]", got)
	}
}
