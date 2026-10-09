package muzak

import (
	"errors"
	"io/fs"
	"mime"
	"net/url"
	"path"
	"strings"
	"testing"
	"unicode/utf8"
)

// Every parser of a value a client can shape is fuzzed against the property
// that makes it safe, not only against panics.

// FuzzIfNoneMatch holds the If-None-Match reader to two properties: a match is
// never claimed for a tag the field does not quote, and a tag a well-formed
// list does quote is always found, commas inside tags included.
func FuzzIfNoneMatch(f *testing.F) {
	f.Add(`"a", W/"b,c", "d"`, `"b,c"`, "x", "y,z")
	f.Add("*", `"a"`, "", "")
	f.Add(`W/"`, `W/""`, `"`, "W/")
	f.Add(" ,, \"a\" ,", `"a"`, ",", ",,")
	f.Fuzz(func(t *testing.T, field, tag, first, second string) {
		matched := ifNoneMatch([]string{field}, tag)
		if opaque, ok := opaqueTag(tag); matched {
			if !ok {
				t.Fatalf("a malformed tag %q was matched by %q", tag, field)
			}
			if strings.Trim(field, " \t") != "*" && !strings.Contains(field, `"`+opaque+`"`) {
				t.Fatalf("%q matched %q, which it does not quote", field, tag)
			}
		}

		// A list built from two arbitrary tags finds each of them.
		a, b := etagText(first), etagText(second)
		list := `"` + a + `" , W/"` + b + `",`
		for _, want := range []string{a, b} {
			if !ifNoneMatch([]string{list}, `"`+want+`"`) || !ifNoneMatch([]string{list}, `W/"`+want+`"`) {
				t.Fatalf("%q does not find %q", list, want)
			}
		}
		if other := a + b + "!"; other != a && other != b && ifNoneMatch([]string{list}, `"`+other+`"`) {
			t.Fatalf("%q found %q, which it does not hold", list, other)
		}
	})
}

// etagText keeps only the characters an opaque tag may hold.
func etagText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if isETagChar(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// FuzzRedirectTarget holds the redirect check to what it promises: a relative
// target it accepts stays on this origin however it is resolved, and an
// absolute one names a listed host, with nothing in either a header or a
// URL parser could read differently.
func FuzzRedirectTarget(f *testing.F) {
	for _, seed := range []string{
		"/", "/a/b?c#d", "//evil.com", "/%2F/evil.com", "/.//evil.com", `/\evil`, "/%252F%252F",
		"https://accounts.example.com/", "https://accounts.example.com@evil.com/", "https://ACCOUNTS.example.com:443/x",
		"javascript:alert(1)", "https:evil.com", "/\t/evil.com", "/%2e%2e//x",
	} {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	hosts := []redirectHost{{host: "accounts.example.com"}}
	base, _ := url.Parse("https://app.example.com/dir/page")
	f.Fuzz(func(t *testing.T, to string, external bool) {
		err := checkRedirectTarget(to, external, hosts)
		if err != nil {
			return
		}
		if len(to) > maxRedirectLength || strings.ContainsAny(to, " \\\t\r\n\x00\x7f") {
			t.Fatalf("accepted %q, which a header or a browser would read differently", to)
		}
		for i := 0; i < len(to); i++ {
			if to[i] >= utf8.RuneSelf || to[i] < ' ' {
				t.Fatalf("accepted %q, which holds byte %#x", to, to[i])
			}
		}
		u, parseErr := url.Parse(to)
		if parseErr != nil {
			t.Fatalf("accepted %q, which url.Parse refuses: %v", to, parseErr)
		}
		resolved := base.ResolveReference(u)
		if to[0] == '/' {
			if resolved.Host != base.Host || u.Host != "" || u.User != nil {
				t.Fatalf("accepted the relative %q, which resolves to host %q", to, resolved.Host)
			}
			// However often the path is decoded, it never begins with a
			// second separator, which is what another host looks like.
			path := u.EscapedPath()
			for range maxRedirectDecodes {
				path = unescapeLenient(path)
			}
			if strings.HasPrefix(path, "//") || strings.HasPrefix(path, `/\`) {
				t.Fatalf("accepted %q, whose path decodes to %q", to, path)
			}
			return
		}
		if u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			t.Fatalf("accepted the absolute %q with scheme %q, host %q and user %v", to, u.Scheme, u.Host, u.User)
		}
		if !external && strings.ToLower(u.Hostname()) != "accounts.example.com" {
			t.Fatalf("accepted %q, whose host %q is not listed", to, u.Hostname())
		}
	})
}

// FuzzContentDisposition holds the header built from a filename to being safe
// to send whatever the name, and to reading back, by mime.ParseMediaType as a
// client would, as exactly the cleaned name.
func FuzzContentDisposition(f *testing.F) {
	for _, seed := range []string{
		"report.pdf", "a\r\nb", `x"; filename*=UTF-8''evil.exe`, "invoice\xe2\x80\xaefdp.exe",
		"r\xc3\xa9sum\xc3\xa9.pdf", "100%.pdf", "\xff", strings.Repeat("\xe2\x82\xac", 200),
	} {
		f.Add(seed, true)
		f.Add(seed, false)
	}
	f.Fuzz(func(t *testing.T, filename string, download bool) {
		value := contentDisposition(download, filename)
		if len(value) > 4*maxDispositionName+64 {
			t.Fatalf("a header of %d bytes, want it bounded", len(value))
		}
		for i := 0; i < len(value); i++ {
			if c := value[i]; c < ' ' || c > '~' {
				t.Fatalf("%q holds byte %#x", value, c)
			}
		}
		if value == "" || !strings.Contains(value, "filename") {
			return
		}
		disposition, params, err := mime.ParseMediaType(value)
		if err != nil {
			t.Fatalf("%q does not parse: %v", value, err)
		}
		if want := map[bool]string{true: "attachment", false: "inline"}[download]; disposition != want {
			t.Fatalf("%q reads as %q, want %q", value, disposition, want)
		}
		if got, want := params["filename"], cleanFilename(filename); got != want {
			t.Fatalf("%q reads back as %q, want %q", value, got, want)
		}
	})
}

// FuzzValidMediaType holds the media type check to admitting only values
// that are safe to send as they are and that a standard parser reads.
func FuzzValidMediaType(f *testing.F) {
	for _, seed := range []string{
		"text/plain", "text/csv; charset=utf-8", `multipart/form-data; boundary="a;b"`, "text/html\r\nX: y",
		`a/b; c="d\"e"`, "a/b;", "*/*", "a/b; c=d; C=e",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if !validMediaType(value) {
			return
		}
		if len(value) > maxMediaTypeLength {
			t.Fatalf("accepted %d bytes", len(value))
		}
		for i := 0; i < len(value); i++ {
			if c := value[i]; (c < ' ' && c != '\t') || c > '~' {
				t.Fatalf("accepted %q, which holds byte %#x", value, c)
			}
		}
		_, _, err := mime.ParseMediaType(value)
		if err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter) && !strings.Contains(err.Error(), "duplicate parameter") {
			t.Fatalf("accepted %q, which mime.ParseMediaType refuses: %v", value, err)
		}
	})
}

// FuzzFileName holds the name check to admitting only a clean relative path
// that names no dotfile and holds nothing a filesystem could read as
// something else.
func FuzzFileName(f *testing.F) {
	for _, seed := range []string{
		"docs/guide.pdf", "../x", ".env", "a/.git/config", `a\b`, "a\x00b", "CON", "a/b/", "x:y", "GIT~1",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		_ = windowsUnsafeName(name)
		if !validFileName(name) {
			return
		}
		if !fs.ValidPath(name) || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == "." {
			t.Fatalf("accepted %q, which is not a clean relative path", name)
		}
		if strings.ContainsAny(name, "\\\x00\r\n") {
			t.Fatalf("accepted %q", name)
		}
		for segment := range strings.SplitSeq(name, "/") {
			if strings.HasPrefix(segment, ".") {
				t.Fatalf("accepted %q, which names a dotfile", name)
			}
		}
	})
}
