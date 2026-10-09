package muzak

import (
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// referenceHostSplit is a second, deliberately plain reading of a host and
// port, written from the rules AppOptions.AllowedHosts documents rather than
// from the matcher's code, for the fuzzer to compare the matcher against.
func referenceHostSplit(s string) (name string, port int, ok bool) {
	if s == "" || len(s) > maxHostHeader {
		return "", 0, false
	}
	portText := ""
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", 0, false
		}
		if rest := s[end+1:]; rest != "" {
			if rest[0] != ':' {
				return "", 0, false
			}
			portText = rest[1:]
		}
		addr, err := netip.ParseAddr(s[1:end])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", 0, false
		}
		name = "[" + addr.String() + "]"
	} else {
		if strings.Count(s, ":") > 1 {
			return "", 0, false
		}
		host, p, _ := strings.Cut(s, ":")
		portText = p
		host = strings.TrimSuffix(host, ".")
		if host == "" || len(host) > maxHostName {
			return "", 0, false
		}
		for label := range strings.SplitSeq(host, ".") {
			if label == "" || len(label) > maxHostLabel {
				return "", 0, false
			}
			if strings.Trim(label, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
				return "", 0, false
			}
		}
		name = asciiLowerForTest(host)
	}
	port = -1
	if portText != "" {
		if len(portText) > 5 || strings.Trim(portText, "0123456789") != "" {
			return "", 0, false
		}
		n, err := strconv.Atoi(portText)
		if err != nil || n > 65535 {
			return "", 0, false
		}
		port = n
	}
	return name, port, true
}

// referenceHostMatch decides whether one allowlist entry admits a host, by the
// documented rules and nothing else.
func referenceHostMatch(entry, host string) bool {
	e := strings.TrimSpace(entry)
	wildcard := strings.HasPrefix(e, "*.")
	if wildcard {
		e = e[2:]
	}
	entryName, entryPort, ok := referenceHostSplit(e)
	if !ok {
		return false
	}
	name, port, ok := referenceHostSplit(host)
	if !ok || (entryPort >= 0 && port != entryPort) {
		return false
	}
	if wildcard {
		return len(name) > len(entryName)+1 && strings.HasSuffix(name, "."+entryName)
	}
	return name == entryName
}

// asciiLowerForTest lowercases ASCII letters only, as the matcher must.
func asciiLowerForTest(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// asciiUpperForTest uppercases ASCII letters only.
func asciiUpperForTest(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}

// FuzzHostAllowlist compares the allowlist with the reference reading for any
// entry and any Host, and checks the properties an attacker would probe: no
// panic, case of ASCII letters never matters, a host carrying a byte that can
// smuggle another host is never admitted, and an entry admits itself.
func FuzzHostAllowlist(f *testing.F) {
	for _, entry := range testAllowedHosts {
		f.Add(entry, strings.TrimSpace(entry))
	}
	for _, seed := range [][2]string{
		{"example.com", "example.com@evil.com"},
		{"example.com", "EXAMPLE.COM."},
		{"*.example.org", "a.example.org"},
		{"*.example.org", "example.org"},
		{"[::1]:8080", "[0:0::1]:8080"},
		{"kubernetes.local", "\u212aubernetes.local"},
		{"example.com", "example.com:"},
		{"example.com:443", "example.com:00443"},
		{"a.b", "a.b.."},
		{"x", strings.Repeat("x.", 130)},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, entry, host string) {
		list, err := newHostAllowlist([]string{entry})
		if err != nil {
			return
		}
		got := list.match(host)
		if want := referenceHostMatch(entry, host); got != want {
			t.Fatalf("entry %q, host %q: match = %v, reference = %v", entry, host, got, want)
		}
		if upper := list.match(asciiUpperForTest(host)); upper != got {
			t.Fatalf("entry %q: %q and its upper case disagree", entry, host)
		}
		if got && (!isPrintableASCII(host) || strings.ContainsAny(host, "@/\\%?# ")) {
			t.Fatalf("entry %q admitted %q, which carries a byte no host does", entry, host)
		}
		trimmed := strings.TrimSpace(entry)
		if rest, wildcard := strings.CutPrefix(trimmed, "*."); wildcard {
			if !list.match("sub." + rest) {
				t.Fatalf("wildcard entry %q does not admit a subdomain", entry)
			}
			if list.match(rest) {
				t.Fatalf("wildcard entry %q admits the bare domain", entry)
			}
		} else if !list.match(trimmed) {
			t.Fatalf("entry %q does not admit itself", entry)
		}
	})
}

// FuzzRedirectLocation feeds any host, path and query to the redirect and
// checks the Location could only ever take a client to the vouched host over
// https, with every part well formed and its length bounded.
func FuzzRedirectLocation(f *testing.F) {
	for _, seed := range [][3]string{
		{"example.com", "/a/b", "x=1"},
		{"Example.com:8080", "//evil.com/x", ""},
		{"example.com", "/\\evil.com", "a=\r\nb"},
		{"[::1]", "*", "%zz"},
		{"example.com", "", "#frag"},
		{"evil.com@example.com", "/x", ""},
		{"example.com", "/caf\u00e9", "q=\u00e9&r=%"},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	keeping := &httpsRedirect{port: ":8443"}
	canonical := &httpsRedirect{host: "www.example.com"}
	f.Fuzz(func(t *testing.T, host, path, query string) {
		r := &http.Request{Host: host, URL: &url.URL{Path: path, RawQuery: query}}
		var buf hostBuffer
		name, _, valid := splitHostHeader(&buf, host)
		for _, redirect := range []*httpsRedirect{keeping, canonical} {
			wantHost := "www.example.com"
			if redirect == keeping {
				// A redirect that keeps the host only ever sees one the
				// allowlist admitted, which reads as valid.
				if !valid {
					continue
				}
				wantHost = string(name) + ":8443"
			}
			location := redirect.location(r)
			if !isPrintableASCII(location) || strings.ContainsAny(location, " \"<>\\^`{|}#") {
				t.Fatalf("Location %q carries a byte that must be encoded", location)
			}
			if limit := len("https://") + maxHostHeader + 3*(len(path)+len(query)) + 8; len(location) > limit {
				t.Fatalf("Location is %d bytes, over the bound %d", len(location), limit)
			}
			parsed, err := url.Parse(location)
			if err != nil {
				t.Fatalf("Location %q does not parse: %v", location, err)
			}
			if parsed.Scheme != "https" || parsed.Host != wantHost || parsed.User != nil || !strings.HasPrefix(parsed.EscapedPath(), "/") {
				t.Fatalf("Location %q names scheme %q host %q (want %q) path %q", location, parsed.Scheme, parsed.Host, wantHost, parsed.EscapedPath())
			}
		}
	})
}

// FuzzForwardedParam checks the reader of a Forwarded parameter agrees with
// the reader of its for parameter that client address resolution already
// relies on, and that reading proto never panics.
func FuzzForwardedParam(f *testing.F) {
	for _, seed := range []string{
		"for=192.0.2.1;proto=https",
		`for="[2001:db8::1]:443";proto="https"`,
		"proto=https;proto=http",
		`for="unterminated`,
		`for="a\"b";by=x`,
		"PROTO=HTTPS",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, element string) {
		gotFor, okFor := forwardedParam(element, "for")
		wantFor, wantOK := forwardedFor(element)
		if gotFor != wantFor || okFor != wantOK {
			t.Fatalf("Forwarded element %q: for = %q %v, forwardedFor = %q %v", element, gotFor, okFor, wantFor, wantOK)
		}
		_, _ = forwardedParam(element, "proto")
		_ = forwardedHTTPS([]string{element})
	})
}
