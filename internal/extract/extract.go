// Package extract finds URLs in arbitrary bytes and normalizes them into a form
// the takeover checker can key on later.
//
// The bias here is recall over precision: this phase builds a corpus, and
// deciding which URLs are interesting is deliberately a later concern. What we
// do insist on is provenance — every URL is recorded with where it came from,
// because SourceKind is what makes the corpus rankable in phase two.
package extract

import (
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// SourceKind records where a URL was found. The metadata_* kinds come from the
// registry document; the file_* kinds come from inside the tarball.
type SourceKind string

const (
	MetadataScript    SourceKind = "metadata_script"
	MetadataBinary    SourceKind = "metadata_binary"
	MetadataDepSpec   SourceKind = "metadata_dep_spec"
	MetadataRepo      SourceKind = "metadata_repo"
	FileInstallScript SourceKind = "file_install_script"
	FileBuildConfig   SourceKind = "file_build_config"
	FileSource        SourceKind = "file_source"
	FileDocs          SourceKind = "file_docs"
	FileTest          SourceKind = "file_test"
	// MetadataOther covers URLs found by sweeping the raw version manifest
	// that no typed field claimed - custom fields, publisher tooling residue.
	MetadataOther SourceKind = "metadata_other"
)

// urlChar is the set of bytes allowed to continue a URL. Braces and '$' are
// deliberately allowed so that templated paths like
// https://host/${VERSION}/bin.tar.gz survive intact rather than truncating at
// the '$' — the host is usually still literal, which is the part that matters.
const urlChar = "[^\\s\"'<>`\\\\|^\\[\\]]"

var (
	// No leading \b: the literal scheme:// is specific enough on its own, and a
	// word boundary would drop URLs concatenated onto preceding text, which is
	// common in minified sources and in strings baked into binaries.
	//
	// A {...} span is matched as a unit so that a template expression may
	// contain spaces. Real code does: `https://${publishableKey == null ? void 0
	// : publishableKey.frontendApi}/...` would otherwise truncate at the first
	// space, losing the closing brace and with it any chance of recognising the
	// result as templated rather than as a literal host.
	urlRe = regexp.MustCompile(`(?i)(?:git\+)?(?:https?|ftps?|ssh|git|s3|gs)://(?:\{[^}]*\}|` + urlChar + `)+`)

	// Protocol-relative URLs (//cdn.example.com/x) are worth catching but look
	// exactly like line comments, so this pattern is strict: a plausible dotted
	// hostname is required, and Normalize additionally rejects anything whose
	// suffix is not ICANN-managed. That gate is what keeps "// TODO: fix.this"
	// out of the corpus.
	protoRelRe = regexp.MustCompile(`(?i)(?:^|[\s"'(=,])//([a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+)(/` + urlChar + `*)?`)

	placeholderRe = regexp.MustCompile(`\$\{[^}]*\}|\{\{[^}]*\}\}|\{[A-Za-z0-9_.-]+\}|%[sdvq]|__[A-Z0-9_]+__|\$[A-Z][A-Z0-9_]*`)
)

var defaultPorts = map[string]string{
	"http": "80", "https": "443", "ftp": "21", "ftps": "990", "ssh": "22", "git": "9418",
}

// Match is a raw hit with its byte offset in the scanned buffer, so callers can
// resolve it to a line number.
type Match struct {
	Raw    string
	Offset int
}

// URL is a normalized URL ready to be stored.
type URL struct {
	Raw               string
	Normalized        string
	Scheme            string
	Host              string
	Port              string
	Path              string
	RegistrableDomain string
	HasPlaceholder    bool
}

// Find returns every URL-shaped run of bytes in b, in order of appearance.
// Results are raw and unvalidated; run them through Normalize.
func Find(b []byte) []Match {
	var out []Match
	for _, loc := range urlRe.FindAllIndex(b, -1) {
		out = append(out, Match{Raw: string(b[loc[0]:loc[1]]), Offset: loc[0]})
	}
	for _, loc := range protoRelRe.FindAllSubmatchIndex(b, -1) {
		// loc[0] may include the leading delimiter; anchor on the "//" instead.
		start := loc[2] - 2
		if start < 0 {
			continue
		}
		end := loc[1]
		out = append(out, Match{Raw: string(b[start:end]), Offset: start})
	}
	return out
}

// Normalize parses a raw match into a storable URL. It reports false for
// anything that does not survive as a plausible network location.
//
// Parsing is done by hand rather than with net/url because placeholders like
// ${VERSION} must be preserved verbatim in the stored string, and net/url's
// round-trip escaping mangles them.
func Normalize(raw string) (URL, bool) {
	raw = trimTrailing(strings.TrimSpace(raw))
	if raw == "" {
		return URL{}, false
	}

	u := URL{Raw: raw, HasPlaceholder: placeholderRe.MatchString(raw)}

	var scheme, rest string
	if strings.HasPrefix(raw, "//") {
		// Protocol-relative. Record it as https, which is what any modern
		// client resolves it to, but keep Raw as written.
		scheme, rest = "https", raw[2:]
	} else {
		i := strings.Index(raw, "://")
		if i <= 0 {
			return u, false
		}
		scheme, rest = strings.ToLower(raw[:i]), raw[i+3:]
	}

	authority := rest
	if authEnd := authorityEnd(rest); authEnd >= 0 {
		authority, u.Path = rest[:authEnd], rest[authEnd:]
	}

	var userinfo string
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo, authority = authority[:at], authority[at+1:]
	}
	if authority == "" {
		return u, false
	}

	host, port := splitHostPort(authority)
	if host == "" {
		return u, false
	}

	u.Scheme = scheme
	// A placeholder in the authority means we do not actually know the host.
	// Record the URL so the templated fetch is still visible in the corpus, but
	// leave Host empty so the phase-two checker skips it rather than resolving
	// a literal "${HOST}".
	if !placeholderRe.MatchString(authority) {
		u.Host = strings.ToLower(host)
		if etld, err := publicsuffix.EffectiveTLDPlusOne(u.Host); err == nil {
			u.RegistrableDomain = etld
		}
	}
	if port != "" && port != defaultPorts[scheme] {
		u.Port = port
	}

	// Protocol-relative matches are only trusted when they land on a real
	// ICANN suffix, which is what separates //cdn.jsdelivr.net/x from a comment.
	if strings.HasPrefix(raw, "//") {
		if u.Host == "" {
			return u, false
		}
		if suffix, icann := publicsuffix.PublicSuffix(u.Host); !icann || suffix == u.Host {
			return u, false
		}
	}

	var b strings.Builder
	b.WriteString(scheme)
	b.WriteString("://")
	if userinfo != "" {
		b.WriteString(userinfo)
		b.WriteByte('@')
	}
	// Case-fold the host only when it really is one. Lowercasing a template
	// expression corrupts it: ${publishableKey} is a JavaScript identifier, not
	// a hostname, and u.Host is left empty precisely in that case.
	if u.Host != "" {
		b.WriteString(u.Host)
	} else {
		b.WriteString(host)
	}
	if u.Port != "" {
		b.WriteByte(':')
		b.WriteString(u.Port)
	}
	b.WriteString(u.Path)
	u.Normalized = b.String()

	return u, true
}

// authorityEnd returns the offset of the first "/", "?" or "#" that ends the
// authority, ignoring any that falls inside a {...} span.
//
// Template expressions routinely contain those characters:
// https://${publishableKey?.frontendApi}/.well-known/x would otherwise be cut
// at the "?" inside the braces, yielding "${publishableKey" as the authority
// and defeating the placeholder check that exists to blank the host.
func authorityEnd(rest string) int {
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			if j := strings.IndexByte(rest[i:], '}'); j >= 0 {
				i += j
				continue
			}
		case '/', '?', '#':
			return i
		}
	}
	return -1
}

// splitHostPort separates a trailing :port only when it is genuinely numeric,
// so that a scp-style "host:path/to/repo" is not mangled.
func splitHostPort(authority string) (host, port string) {
	i := strings.LastIndex(authority, ":")
	if i < 0 {
		return authority, ""
	}
	cand := authority[i+1:]
	if cand == "" || strings.IndexFunc(cand, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return authority, ""
	}
	return authority[:i], cand
}

// trimTrailing removes punctuation that the URL pattern reliably over-captures
// from prose, Markdown and source. Parens and braces are only stripped when
// unbalanced, so that URLs which legitimately contain them survive.
func trimTrailing(s string) string {
	for s != "" {
		switch c := s[len(s)-1]; c {
		case '.', ',', ';', ':', '!', '?', '"', '\'', '*', '\\':
			s = s[:len(s)-1]
		case ')':
			if strings.Count(s, ")") <= strings.Count(s, "(") {
				return s
			}
			s = s[:len(s)-1]
		case '}':
			if strings.Count(s, "}") <= strings.Count(s, "{") {
				return s
			}
			s = s[:len(s)-1]
		default:
			return s
		}
	}
	return s
}
