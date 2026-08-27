package extract

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantOK     bool
		wantNorm   string
		wantHost   string
		wantETLD   string
		wantHolder bool
	}{
		{name: "plain", in: "https://example.com/a.tgz", wantOK: true,
			wantNorm: "https://example.com/a.tgz", wantHost: "example.com", wantETLD: "example.com"},
		{name: "trailing period from prose", in: "https://example.com/a.tgz.", wantOK: true,
			wantNorm: "https://example.com/a.tgz", wantHost: "example.com", wantETLD: "example.com"},
		{name: "markdown link paren", in: "https://example.com/a)", wantOK: true,
			wantNorm: "https://example.com/a", wantHost: "example.com", wantETLD: "example.com"},
		{name: "balanced paren kept", in: "https://en.wikipedia.org/wiki/Foo_(bar)", wantOK: true,
			wantNorm: "https://en.wikipedia.org/wiki/Foo_(bar)", wantHost: "en.wikipedia.org", wantETLD: "wikipedia.org"},
		{name: "uppercase host lowered", in: "HTTPS://Example.COM/Path", wantOK: true,
			wantNorm: "https://example.com/Path", wantHost: "example.com", wantETLD: "example.com"},
		{name: "default port stripped", in: "https://example.com:443/x", wantOK: true,
			wantNorm: "https://example.com/x", wantHost: "example.com", wantETLD: "example.com"},
		{name: "nonstandard port kept", in: "https://example.com:8443/x", wantOK: true,
			wantNorm: "https://example.com:8443/x", wantHost: "example.com", wantETLD: "example.com"},
		{name: "s3 bucket becomes host", in: "s3://my-node-binaries/v1/bin.tgz", wantOK: true,
			wantNorm: "s3://my-node-binaries/v1/bin.tgz", wantHost: "my-node-binaries"},
		{name: "git ssh with userinfo", in: "ssh://git@git.internal.example.org/repo.git", wantOK: true,
			wantNorm: "ssh://git@git.internal.example.org/repo.git", wantHost: "git.internal.example.org", wantETLD: "example.org"},
		{name: "templated path keeps literal host", in: "https://bin.example.com/${VERSION}/x.tgz", wantOK: true,
			wantNorm: "https://bin.example.com/${VERSION}/x.tgz", wantHost: "bin.example.com",
			wantETLD: "example.com", wantHolder: true},
		{name: "templated host yields no host", in: "https://${HOST}/x.tgz", wantOK: true,
			wantNorm: "https://${host}/x.tgz", wantHost: "", wantHolder: true},
		{name: "not a url", in: "https://", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Normalize(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Normalized != tc.wantNorm {
				t.Errorf("Normalized = %q, want %q", got.Normalized, tc.wantNorm)
			}
			if got.Host != tc.wantHost {
				t.Errorf("Host = %q, want %q", got.Host, tc.wantHost)
			}
			if got.RegistrableDomain != tc.wantETLD {
				t.Errorf("RegistrableDomain = %q, want %q", got.RegistrableDomain, tc.wantETLD)
			}
			if got.HasPlaceholder != tc.wantHolder {
				t.Errorf("HasPlaceholder = %v, want %v", got.HasPlaceholder, tc.wantHolder)
			}
		})
	}
}

func TestFind(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "install script with pipe to shell",
			in:   `"postinstall": "curl -sL https://cdn.example.com/bin.tar.gz | tar xz"`,
			want: []string{"https://cdn.example.com/bin.tar.gz"},
		},
		{
			name: "quotes terminate the match",
			in:   `{"host":"https://binaries.example.com","path":"/v1"}`,
			want: []string{"https://binaries.example.com"},
		},
		{
			name: "markdown badge and link",
			in:   "[![build](https://img.example.com/b.svg)](https://ci.example.com/job)",
			want: []string{"https://img.example.com/b.svg", "https://ci.example.com/job"},
		},
		{
			name: "protocol relative cdn is found",
			in:   `<script src="//cdn.example.com/lib.js">`,
			want: []string{"//cdn.example.com/lib.js"},
		},
		{
			name: "line comment is not a url",
			in:   "// TODO: fix.this later",
			want: nil,
		},
		{
			name: "git dependency spec",
			in:   `"dep": "git+https://git.example.com/a/b.git#v1"`,
			want: []string{"git+https://git.example.com/a/b.git#v1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, m := range Find([]byte(tc.in)) {
				if u, ok := Normalize(m.Raw); ok {
					_ = u
					got = append(got, trimTrailing(m.Raw))
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d matches %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("match %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
