package muzak

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"muzak.dev/framework/i18n"
)

// testAllowedHosts is the allowlist the matcher tests run against: a plain
// name, a wildcard, a name with a port, IPv6 with and without one, IPv4, an
// internationalized name in punycode, and names whose letters have Unicode
// look-alikes that fold to them.
var testAllowedHosts = []string{
	"example.com",
	"*.example.org",
	"api.example.net:8443",
	"[::1]",
	"[2001:db8::1]:8080",
	"127.0.0.1",
	"xn--bcher-kva.de",
	"localhost:8080",
	"kubernetes.local",
	" sso.example.io ",
	"a_b.internal",
}

// TestHostAllowlistMatching is the attacker's table: every Host header trick
// against the allowlist, each with the verdict it must get.
func TestHostAllowlistMatching(t *testing.T) {
	t.Parallel()
	list, err := newHostAllowlist(testAllowedHosts)
	if err != nil {
		t.Fatal(err)
	}
	label63 := strings.Repeat("a", 63)
	for _, tc := range []struct {
		host string
		want bool
	}{
		// A plain name: any case, any port, the fully qualified spelling.
		{"example.com", true},
		{"EXAMPLE.COM", true},
		{"Example.Com:80", true},
		{"example.com:65535", true},
		{"example.com.", true},
		{"example.com.:443", true},
		{"example.com:", true},
		{"example.com..", false},
		{".example.com", false},
		{"www.example.com", false},
		{"example.com.evil.com", false},
		{"evil.com", false},
		{"evilexample.com", false},
		{"example.co", false},
		{"example.comm", false},
		// Smuggling another host past a prefix or suffix comparison.
		{"example.com@evil.com", false},
		{"evil.com@example.com", false},
		{"evil.com#example.com", false},
		{"evil.com?.example.com", false},
		{"example.com/evil", false},
		{"evil.com/example.com", false},
		{"example.com\\evil", false},
		{"example%2Ecom", false},
		{"example.com%00", false},
		{"example.com\x00", false},
		{"example.com\r\nX-Injected: 1", false},
		{" example.com", false},
		{"example.com ", false},
		{"exa mple.com", false},
		{"exa\tmple.com", false},
		// Ports that are not one.
		{"example.com:8080:80", false},
		{"example.com:99999", false},
		{"example.com:65536", false},
		{"example.com:-1", false},
		{"example.com:+80", false},
		{"example.com:8a", false},
		{"example.com:000080", false},
		{"example.com: 80", false},
		{"", false},
		{":80", false},
		{":", false},
		// The wildcard: one label or more, never the domain itself.
		{"a.example.org", true},
		{"a.b.c.example.org", true},
		{"A.EXAMPLE.ORG.", true},
		{"a.example.org:1234", true},
		{"example.org", false},
		{".example.org", false},
		{"a..example.org", false},
		{"aexample.org", false},
		{"a-example.org", false},
		{"example.org.evil.com", false},
		{"a.example.org.evil.com", false},
		{"a.example.org@evil.com", false},
		// A port named on the entry is the only port allowed.
		{"api.example.net:8443", true},
		{"API.example.net:08443", true},
		{"api.example.net", false},
		{"api.example.net:443", false},
		{"api.example.net:8444", false},
		{"localhost:8080", true},
		{"LOCALHOST:8080", true},
		{"localhost", false},
		{"localhost:8081", false},
		// IPv6 in brackets, in any spelling of the same address.
		{"[::1]", true},
		{"[::1]:9999", true},
		{"[0:0:0:0:0:0:0:1]", true},
		{"[::0:1]", true},
		{"::1", false},
		{"[::1", false},
		{"::1]", false},
		{"[::1]x", false},
		{"[::1]:", true},
		{"[::1%25lo0]", false},
		{"[::1%lo0]", false},
		{"[2001:db8::1]:8080", true},
		{"[2001:DB8::1]:8080", true},
		{"[2001:db8:0:0::1]:8080", true},
		{"[2001:db8::1]", false},
		{"[2001:db8::1]:80", false},
		{"[::ffff:127.0.0.1]", false},
		{"[127.0.0.1]", false},
		{"[]", false},
		// IPv4 only as written.
		{"127.0.0.1", true},
		{"127.0.0.1:5000", true},
		{"127.1", false},
		{"0x7f.0.0.1", false},
		{"127.000.000.001", false},
		{"2130706433", false},
		{"127.0.0.1.", true},
		// Internationalized names are matched in their ASCII form only.
		{"xn--bcher-kva.de", true},
		{"XN--BCHER-KVA.DE", true},
		{"b\u00fccher.de", false},
		// Letters outside ASCII whose Unicode folding is an ASCII letter: the
		// Kelvin sign folds to k and the long s to s under strings.EqualFold.
		{"kubernetes.local", true},
		{"\u212aubernetes.local", false},
		{"sso.example.io", true},
		{"\u017fso.example.io", false},
		{"\uff45xample.com", false},
		// Underscores are allowed, since internal names carry them.
		{"a_b.internal", true},
		// Lengths: a label of 63 is fine, 64 is not, and so is a name over 253.
		{label63 + ".example.org", true},
		{"a" + label63 + ".example.org", false},
		{strings.Repeat("a.", 125) + "example.org", false},
		{strings.Repeat("x", 10000), false},
	} {
		if got := list.match(tc.host); got != tc.want {
			t.Errorf("match(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestAllowedHostsMergesRepeatedEntries widens what one host allows as it is
// listed again, and never narrows an entry that allows any port.
func TestAllowedHostsMergesRepeatedEntries(t *testing.T) {
	t.Parallel()
	list, err := newHostAllowlist([]string{
		"a.example:1", "A.example:2", "a.example:1",
		"b.example", "b.example:5",
		"*.c.example:7", "*.C.example:8", "*.c.example:7",
		"*.d.example:9", "*.d.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"a.example:1": true, "a.example:2": true, "a.example:3": false, "a.example": false,
		"b.example": true, "b.example:6": true,
		"x.c.example:7": true, "x.c.example:8": true, "x.c.example": false,
		"x.d.example": true, "x.d.example:1": true,
	} {
		if got := list.match(host); got != want {
			t.Errorf("match(%q) = %v, want %v", host, got, want)
		}
	}
	if len(list.wildcards) != 2 {
		t.Errorf("the wildcards were not merged: %+v", list.wildcards)
	}
}

// TestAllowedHostsEntriesThatCanNeverMatch lists every mistake an entry can
// carry, and checks each is reported, all of them in one build.
func TestAllowedHostsEntriesThatCanNeverMatch(t *testing.T) {
	t.Parallel()
	for entry, why := range map[string]string{
		"":                    "is empty",
		"   ":                 "is empty",
		"https://example.com": "carries a scheme",
		"example.com/":        "carries a path",
		"example.com/admin":   "carries a path",
		"example.com\\":       "carries a path",
		"user@example.com":    "carries credentials",
		"example.com?x=1":     "carries a query or a fragment",
		"example.com#top":     "carries a query or a fragment",
		"b\u00fccher.de":      "write its punycode (xn--) spelling",
		"exa mple.com":        "holds whitespace",
		"*":                   `holds a "*" somewhere other than a leading "*."`,
		"**.example.com":      `holds a "*" somewhere other than a leading "*."`,
		"a.*.example.com":     `holds a "*" somewhere other than a leading "*."`,
		"*example.com":        `holds a "*" somewhere other than a leading "*."`,
		"example.*":           `holds a "*" somewhere other than a leading "*."`,
		"*.*.example.com":     `holds a "*" somewhere other than a leading "*."`,
		"*.":                  "is not a valid host name",
		"example.com:":        "ends in a colon",
		"[::1]:":              "ends in a colon",
		"example.com:0":       "names port 0",
		"example.com:65536":   "is not a valid host name, IP address or port",
		"example.com:http":    "is not a valid host name, IP address or port",
		"::1":                 "is an IPv6 address without brackets",
		"2001:db8::1":         "is an IPv6 address without brackets",
		"[::1":                "is not a valid host name",
		"[fe80::1%25en0]":     "is not a valid host name",
		"example..com":        "is not a valid host name",
		".example.com":        "is not a valid host name",
		"example.com..":       "is not a valid host name",
		"*.127.0.0.1":         "puts a wildcard in front of an address",
		"*.[::1]":             "puts a wildcard in front of an address",
	} {
		options := quietOptions()
		options.AllowedHosts = []string{"fine.example.com", entry}
		err := New(options).Build()
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("AllowedHosts entry %q: Build() = %v, want it reported as one that %s", entry, err, why)
		}
	}

	options := quietOptions()
	options.AllowedHosts = []string{"https://a.example", "b.example/x", "c.example"}
	err := New(options).Build()
	if err == nil || !strings.Contains(err.Error(), "https://a.example") || !strings.Contains(err.Error(), "b.example/x") {
		t.Errorf("Build() = %v, want both bad entries reported together", err)
	}
}

// hostApp is an application with an allowlist, a route, a mount and a
// documented API, recording whether anything of it ran.
func hostApp(t *testing.T, configure func(*AppOptions)) (*App, *syncBuffer, *mountRecorder) {
	t.Helper()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	options.AllowedHosts = []string{"example.com", "*.example.com"}
	if configure != nil {
		configure(&options)
	}
	rec := &mountRecorder{}
	app := New(options)
	app.Get("/hello", func(*Context, Empty) (encodedUserOut, error) { return encodedUserOut{Route: "hello"}, nil })
	app.Mount("/legacy", rec)
	return mustBuild(t, app), logs, rec
}

// requestFor builds a request for target with the given Host.
func requestFor(method, target, host string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	return req
}

// TestAllowedHostsRefusesAnUnlistedHost covers the refusal itself: 421 through
// the error renderer, nothing of the application run, and a log line that
// records the host in a form that cannot forge another line.
func TestAllowedHostsRefusesAnUnlistedHost(t *testing.T) {
	t.Parallel()
	app, logs, rec := hostApp(t, nil)

	assertStatus(t, doRequest(t, app, requestFor("GET", "/hello", "example.com")), http.StatusOK)
	assertStatus(t, doRequest(t, app, requestFor("GET", "/hello", "api.example.com:8443")), http.StatusOK)

	for _, target := range []string{"/hello", "/legacy/x", "/openapi.json", "/missing"} {
		res := doRequest(t, app, requestFor("GET", target, "evil.com"))
		assertStatus(t, res, http.StatusMisdirectedRequest)
		body := decodeError(t, res)
		if body.Error.Code != CodeMisdirectedRequest || body.Error.Message != misdirectedMessage || body.RequestID == "" {
			t.Errorf("GET %s for evil.com was refused as %+v", target, body)
		}
		if strings.Contains(res.Body.String(), "evil") {
			t.Errorf("the refusal echoes the host: %s", res.Body.String())
		}
		if res.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("the refusal may be cached: %q", res.Header().Get("Cache-Control"))
		}
	}
	if rec.count() != 0 {
		t.Error("a request for an unlisted host reached the mount")
	}
	for _, want := range []string{`"msg":"muzak: refused a request for a host this application does not serve"`, `"host":"evil.com"`, `"status":421`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not record %s:\n%s", want, logs.String())
		}
	}

	// A host crafted to forge a log line is recorded as inert text.
	doRequest(t, app, requestFor("GET", "/hello", "evil.com\n{\"level\":\"INFO\",\"msg\":\"forged\"}"))
	if strings.Contains(logs.String(), "\n{\"level\":\"INFO\",\"msg\":\"forged\"}") {
		t.Errorf("a Host header forged a log line:\n%s", logs.String())
	}
}

// TestAllowedHostsIgnoresForwardingHeaders keeps X-Forwarded-Host and its
// relatives from standing in for the Host the request named, from any peer.
func TestAllowedHostsIgnoresForwardingHeaders(t *testing.T) {
	t.Parallel()
	app, _, _ := hostApp(t, func(o *AppOptions) {
		o.ClientIP = ClientIPOptions{TrustedProxies: []string{"192.0.2.0/24"}}
	})
	for _, header := range []string{"X-Forwarded-Host", "X-Host", "X-Original-Host", "Forwarded"} {
		req := requestFor("GET", "/hello", "evil.com")
		value := "example.com"
		if header == "Forwarded" {
			value = "host=example.com"
		}
		req.Header.Set(header, value)
		assertStatus(t, doRequest(t, app, req), http.StatusMisdirectedRequest)

		req = requestFor("GET", "/hello", "example.com")
		req.Header.Set(header, strings.Replace(value, "example.com", "evil.com", 1))
		assertStatus(t, doRequest(t, app, req), http.StatusOK)
	}
}

// TestAllowedHostsRunsBeforeEverythingElse refuses a CORS preflight and a
// request middleware would otherwise have answered, before either runs.
func TestAllowedHostsRunsBeforeEverythingElse(t *testing.T) {
	t.Parallel()
	ran := false
	options := quietOptions()
	options.AllowedHosts = []string{"example.com"}
	options.CORS = CORSOptions{AllowedOrigins: []string{"https://evil.com"}}
	app := New(options)
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ran = true
			next.ServeHTTP(w, r)
		})
	})
	app.Get("/x", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	req := requestFor("OPTIONS", "/x", "evil.com")
	req.Header.Set("Origin", "https://evil.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	res := doRequest(t, app, req)
	assertStatus(t, res, http.StatusMisdirectedRequest)
	if res.Header().Get("Access-Control-Allow-Origin") != "" || ran {
		t.Error("the CORS policy or middleware ran for a request whose host was refused")
	}
	if res.Header().Get(HeaderRequestID) == "" || res.Header().Get("X-Content-Type-Options") == "" {
		t.Error("the refusal is missing the request identifier or the security headers")
	}
}

// TestAllowedHostsExemption checks the internal hook a health probe will use:
// nil exempts nothing, and a predicate exempts exactly what it says.
func TestAllowedHostsExemption(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.AllowedHosts = []string{"example.com"}
	app := New(options)
	app.Get("/healthz", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.Get("/x", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	if app.edgeExempt != nil {
		t.Fatal("a new application exempts something by default")
	}
	app.edgeExempt = func(r *http.Request) bool { return r.URL.Path == "/healthz" }
	mustBuild(t, app)

	assertStatus(t, doRequest(t, app, requestFor("GET", "/healthz", "10.0.0.7:8080")), http.StatusOK)
	assertStatus(t, doRequest(t, app, requestFor("GET", "/x", "10.0.0.7:8080")), http.StatusMisdirectedRequest)
}

// TestAllowedHostsUnsetCostsNothing shows the check is absent, not merely
// permissive, when nothing is listed, and that a configured check does not
// allocate for an ordinary host.
func TestAllowedHostsUnsetCostsNothing(t *testing.T) {
	app := mustBuild(t, New(quietOptions()))
	if app.edge != nil {
		t.Fatal("an application with no AllowedHosts and no redirect installed the check")
	}
	assertStatus(t, doRequest(t, app, requestFor("GET", "/openapi.json", "anything.example")), http.StatusOK)

	list, err := newHostAllowlist(testAllowedHosts)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"example.com", "Deep.Sub.Example.Org.", "[2001:db8::1]:8080", "evil.com"} {
		if allocs := testing.AllocsPerRun(100, func() { list.match(host) }); allocs != 0 {
			t.Errorf("match(%q) allocates %.0f times, want none", host, allocs)
		}
	}
}

// TestAllowedHostsBoundsTheWork checks a hostile Host is refused in time
// proportional to the bound, not to its length, and a large allowlist stays a
// map lookup.
func TestAllowedHostsBoundsTheWork(t *testing.T) {
	t.Parallel()
	entries := make([]string, 0, 10_000)
	for i := range 10_000 {
		entries = append(entries, "host-"+strings.Repeat("x", i%40)+"-"+strconv.Itoa(i)+".example.com")
	}
	list, err := newHostAllowlist(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.exact) != 10_000 || len(list.wildcards) != 0 {
		t.Fatalf("the allowlist holds %d exact entries and %d wildcards", len(list.exact), len(list.wildcards))
	}
	if !list.match("HOST-X-1.example.com") || list.match("host-x-10001.example.com") {
		t.Error("a large allowlist matched wrongly")
	}
	huge := strings.Repeat("a", 1<<20)
	if list.match(huge) || list.match(huge+".example.com") {
		t.Error("a megabyte Host matched")
	}
}

// TestAllowedHostsOnTheWire sends what only a raw connection can: an absolute
// request target that disagrees with the Host header, two Host headers, none,
// and bytes a client library would refuse to send.
func TestAllowedHostsOnTheWire(t *testing.T) {
	t.Parallel()
	rec := &mountRecorder{}
	options := quietOptions()
	options.AllowedHosts = []string{"example.com", "[::1]"}
	app := New(options)
	app.Mount("/", rec)
	addr := serveOnWire(t, app)

	for _, tc := range []struct {
		name, request string
		allowed       bool
	}{
		{"plain", "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n", true},
		{"trailing dot", "GET / HTTP/1.1\r\nHost: EXAMPLE.com.\r\nConnection: close\r\n\r\n", true},
		{"IPv6", "GET / HTTP/1.1\r\nHost: [0::1]:8080\r\nConnection: close\r\n\r\n", true},
		{"absolute target names an evil host", "GET http://evil.com/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n", false},
		{"absolute target names the allowed host", "GET http://example.com/ HTTP/1.1\r\nHost: evil.com\r\nConnection: close\r\n\r\n", true},
		{"two Host headers", "GET / HTTP/1.1\r\nHost: example.com\r\nHost: evil.com\r\nConnection: close\r\n\r\n", false},
		{"two Host headers the other way", "GET / HTTP/1.1\r\nHost: evil.com\r\nHost: example.com\r\nConnection: close\r\n\r\n", false},
		{"no Host on HTTP/1.0", "GET / HTTP/1.0\r\n\r\n", false},
		{"userinfo", "GET / HTTP/1.1\r\nHost: example.com@evil.com\r\nConnection: close\r\n\r\n", false},
		{"suffix", "GET / HTTP/1.1\r\nHost: example.com.evil.com\r\nConnection: close\r\n\r\n", false},
		{"raw UTF-8", "GET / HTTP/1.1\r\nHost: ex\u00e4mple.com\r\nConnection: close\r\n\r\n", false},
		{"Kelvin sign", "GET / HTTP/1.1\r\nHost: \u212a.example.com\r\nConnection: close\r\n\r\n", false},
		{"X-Forwarded-Host", "GET / HTTP/1.1\r\nHost: evil.com\r\nX-Forwarded-Host: example.com\r\nConnection: close\r\n\r\n", false},
		{"long", "GET / HTTP/1.1\r\nHost: " + strings.Repeat("a", 4000) + ".example.com\r\nConnection: close\r\n\r\n", false},
	} {
		before := rec.count()
		res, _ := rawExchange(t, addr, tc.request)
		reached := rec.count() > before
		if reached != tc.allowed {
			t.Errorf("%s: reached the application = %v (status %d), want %v", tc.name, reached, res.StatusCode, tc.allowed)
		}
		if !tc.allowed && res.StatusCode < 400 {
			t.Errorf("%s: answered %d, want a refusal", tc.name, res.StatusCode)
		}
	}
}

// TestMisdirectedMessageIsTranslatable keeps the English the framework writes
// and the shipped locale in step, as the drift guard does for every other
// status sentence, and shows a locale can reword it.
func TestMisdirectedMessageIsTranslatable(t *testing.T) {
	t.Parallel()
	store := i18n.Builtin()
	if got := store.T("en", "muzak.http.421"); got != misdirectedMessage {
		t.Errorf("the shipped sentence for 421 is %q, want %q", got, misdirectedMessage)
	}
	app, _, _ := hostApp(t, func(o *AppOptions) { o.I18n = I18nOptions{Store: interopStore(t)} })
	req := requestFor("GET", "/hello", "evil.com")
	req.Header.Set("Accept-Language", "es")
	body := decodeError(t, doRequest(t, app, req))
	if body.Error.Message != "Este servidor no atiende el host solicitado." || body.Error.Code != CodeMisdirectedRequest {
		t.Errorf("the Spanish refusal is %+v", body.Error)
	}
}
