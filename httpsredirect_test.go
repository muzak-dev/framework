package muzak

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// httpsRedirectApp is an application that redirects plain HTTP, with a route at
// the root wildcard so a request that is not redirected is answered 200.
func httpsRedirectApp(t *testing.T, configure func(*AppOptions)) *App {
	t.Helper()
	options := quietOptions()
	options.AllowedHosts = []string{"example.com", "*.example.com"}
	options.RedirectHTTPS = &RedirectHTTPSOptions{}
	if configure != nil {
		configure(&options)
	}
	app := New(options)
	app.Get("/{rest...}", func(_ *Context, in struct {
		Rest string `path:"rest"`
	}) (encodedUserOut, error) {
		return encodedUserOut{Route: "served", ID: in.Rest}, nil
	})
	return mustBuild(t, app)
}

// TestRedirectHTTPSBuildErrors covers every way to configure a redirect that
// could not be trusted or could not be followed.
func TestRedirectHTTPSBuildErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		hosts    []string
		redirect RedirectHTTPSOptions
		want     string
	}{
		"no vouched host":   {nil, RedirectHTTPSOptions{}, "neither AppOptions.AllowedHosts nor RedirectHTTPS.Host"},
		"port only":         {nil, RedirectHTTPSOptions{Port: 8443}, "neither AppOptions.AllowedHosts nor RedirectHTTPS.Host"},
		"wildcard host":     {nil, RedirectHTTPSOptions{Host: "*.example.com"}, "is a wildcard"},
		"host with scheme":  {nil, RedirectHTTPSOptions{Host: "https://example.com"}, "RedirectHTTPS.Host \"https://example.com\" carries a scheme"},
		"host with path":    {nil, RedirectHTTPSOptions{Host: "example.com/x"}, "carries a path"},
		"negative port":     {[]string{"example.com"}, RedirectHTTPSOptions{Port: -1}, "RedirectHTTPS.Port is -1"},
		"port out of range": {[]string{"example.com"}, RedirectHTTPSOptions{Port: 65536}, "RedirectHTTPS.Port is 65536"},
		"two ports":         {nil, RedirectHTTPSOptions{Host: "example.com:8443", Port: 9443}, "set the port in one place"},
	} {
		options := quietOptions()
		options.AllowedHosts = tc.hosts
		redirect := tc.redirect
		options.RedirectHTTPS = &redirect
		if err := New(options).Build(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Build() = %v, want %q", name, err, tc.want)
		}
	}
}

// TestRedirectHTTPS covers the redirect itself: the status for each method,
// the host and port it names, and what it leaves alone.
func TestRedirectHTTPS(t *testing.T) {
	t.Parallel()
	app := httpsRedirectApp(t, nil)
	for _, tc := range []struct {
		method, target, host string
		status               int
		location             string
	}{
		{"GET", "/a/b?x=1&y=%20z", "Example.com:8080", http.StatusMovedPermanently, "https://example.com/a/b?x=1&y=%20z"},
		{"HEAD", "/", "example.com", http.StatusMovedPermanently, "https://example.com/"},
		{"POST", "/orders", "api.example.com.", http.StatusPermanentRedirect, "https://api.example.com/orders"},
		{"PUT", "/orders/1", "example.com", http.StatusPermanentRedirect, "https://example.com/orders/1"},
		{"DELETE", "/orders/1?", "example.com", http.StatusPermanentRedirect, "https://example.com/orders/1?"},
		{"OPTIONS", "/x", "example.com", http.StatusPermanentRedirect, "https://example.com/x"},
	} {
		res := doRequest(t, app, requestFor(tc.method, tc.target, tc.host))
		if res.Code != tc.status || res.Header().Get("Location") != tc.location {
			t.Errorf("%s %s (Host %s) = %d %q, want %d %q", tc.method, tc.target, tc.host,
				res.Code, res.Header().Get("Location"), tc.status, tc.location)
		}
		if res.Body.Len() != 0 {
			t.Errorf("%s %s: the redirect carries a body: %q", tc.method, tc.target, res.Body.String())
		}
		if res.Header().Get("Strict-Transport-Security") != "" {
			t.Errorf("%s %s: HSTS was sent over plain HTTP", tc.method, tc.target)
		}
	}

	secure := requestFor("GET", "/a", "example.com")
	secure.TLS = &tls.ConnectionState{}
	res := doRequest(t, app, secure)
	assertStatus(t, res, http.StatusOK)
	if res.Header().Get("Strict-Transport-Security") == "" {
		t.Error("a response over TLS lost the HSTS header SecurityHeaders sends")
	}

	// A host the allowlist refuses is refused, never redirected.
	res = doRequest(t, app, requestFor("GET", "/a", "evil.com"))
	assertStatus(t, res, http.StatusMisdirectedRequest)
	if res.Header().Get("Location") != "" {
		t.Error("a refused host was given a Location")
	}

	// The documentation is behind the redirect too.
	assertStatus(t, doRequest(t, app, requestFor("GET", "/openapi.json", "example.com")), http.StatusMovedPermanently)
}

// TestRedirectHTTPSPortAndCanonicalHost names the https port, and moves every
// client onto one host without trusting the one it named.
func TestRedirectHTTPSPortAndCanonicalHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		hosts    []string
		redirect RedirectHTTPSOptions
		host     string
		want     string
	}{
		{[]string{"example.com"}, RedirectHTTPSOptions{Port: 8443}, "example.com:8080", "https://example.com:8443/p"},
		{[]string{"example.com"}, RedirectHTTPSOptions{Port: 443}, "example.com:80", "https://example.com/p"},
		{[]string{"[::1]"}, RedirectHTTPSOptions{Port: 8443}, "[0:0::1]:8080", "https://[::1]:8443/p"},
		{nil, RedirectHTTPSOptions{Host: "WWW.Example.com"}, "evil.com", "https://www.example.com/p"},
		{nil, RedirectHTTPSOptions{Host: "www.example.com:8443"}, "anything", "https://www.example.com:8443/p"},
		{nil, RedirectHTTPSOptions{Host: "www.example.com", Port: 8443}, "", "https://www.example.com:8443/p"},
		{nil, RedirectHTTPSOptions{Host: "www.example.com:443"}, "x", "https://www.example.com/p"},
		{[]string{"example.com"}, RedirectHTTPSOptions{Host: "www.example.com"}, "example.com", "https://www.example.com/p"},
	} {
		app := httpsRedirectApp(t, func(o *AppOptions) {
			o.AllowedHosts = tc.hosts
			redirect := tc.redirect
			o.RedirectHTTPS = &redirect
		})
		res := doRequest(t, app, requestFor("GET", "/p", tc.host))
		if got := res.Header().Get("Location"); got != tc.want {
			t.Errorf("%+v for Host %q: Location = %q, want %q", tc.redirect, tc.host, got, tc.want)
		}
	}
}

// TestRedirectHTTPSExemptsACMEChallenges lets a certificate authority reach
// its token over plain HTTP, and nothing that only begins like one.
func TestRedirectHTTPSExemptsACMEChallenges(t *testing.T) {
	t.Parallel()
	app := httpsRedirectApp(t, nil)
	for target, exempt := range map[string]bool{
		"/.well-known/acme-challenge/abc_DEF-123":                            true,
		"/.well-known/acme-challenge/":                                       false,
		"/.well-known/acme-challenge":                                        false,
		"/.well-known/acme-challengeX/abc":                                   false,
		"/.well-known/acme-challenge/a/b":                                    false,
		"/.well-known/acme-challenge/../../admin":                            false,
		"/.well-known/acme-challenge/a%2F..%2Fadmin":                         false,
		"/.well-known/acme-challenge/a.b":                                    false,
		"/.well-known/acme-challenge/" + strings.Repeat("a", maxACMEToken+1): false,
		"/.WELL-KNOWN/acme-challenge/abc":                                    false,
	} {
		res := doRequest(t, app, requestFor("GET", target, "example.com"))
		if got := res.Code != http.StatusMovedPermanently; got != exempt {
			t.Errorf("GET %s = %d, exempt = %v, want %v", target, res.Code, got, exempt)
		}
	}
}

// TestRedirectHTTPSCannotBeTurnedIntoAnOpenRedirect tries every way a path or
// query could carry the client somewhere else, and checks each Location names
// the vouched host and nothing a client wrote can break out of its part.
func TestRedirectHTTPSCannotBeTurnedIntoAnOpenRedirect(t *testing.T) {
	t.Parallel()
	app := httpsRedirectApp(t, nil)
	build := func(path, rawPath, rawQuery string) *http.Request {
		req := requestFor("GET", "/", "example.com")
		req.URL.Path, req.URL.RawPath, req.URL.RawQuery = path, rawPath, rawQuery
		return req
	}
	for name, req := range map[string]*http.Request{
		"double slash":         requestFor("GET", "//evil.com/x", "example.com"),
		"triple slash":         requestFor("GET", "///evil.com", "example.com"),
		"backslash":            build("/\\evil.com", "", ""),
		"backslash escaped":    requestFor("GET", "/%5Cevil.com", "example.com"),
		"at sign":              build("/@evil.com", "", ""),
		"encoded CRLF":         requestFor("GET", "/%0d%0aSet-Cookie:%20a=b", "example.com"),
		"raw CRLF in path":     build("/x\r\nSet-Cookie: a=b", "", ""),
		"raw CRLF in query":    build("/x", "", "a=\r\nSet-Cookie: b=c"),
		"NUL":                  build("/x\x00y", "", "q=\x00"),
		"fragment in query":    build("/x", "", "a=#@evil.com"),
		"non-ASCII":            build("/caf\u00e9", "", "q=\u00e9"),
		"stray percent":        build("/x", "", "a=%zz&b=%"),
		"asterisk":             build("*", "", ""),
		"empty path":           build("", "", ""),
		"no leading slash":     build("evil.com/x", "", ""),
		"scheme in path":       build("https://evil.com", "", ""),
		"quote and angle":      build("/\"><script>", "", "x=<y>"),
		"invalid raw path":     build("/a b", "/a b", ""),
		"query with question":  build("/x", "", "next=https://evil.com/?a"),
		"space in query":       build("/x", "", "a b"),
		"tab in path":          build("/a\tb", "", ""),
		"DEL":                  build("/a\x7fb", "", ""),
		"high byte":            build("/a\xffb", "", ""),
		"percent-encoded host": requestFor("GET", "/%2F%2Fevil.com", "example.com"),
	} {
		res := doRequest(t, app, req)
		location := res.Header().Get("Location")
		if res.Code != http.StatusMovedPermanently {
			t.Errorf("%s: status %d, want the redirect", name, res.Code)
			continue
		}
		if strings.ContainsAny(location, "\r\n\x00 \t\\\"<>#\x7f") || !isPrintableASCII(location) {
			t.Errorf("%s: Location %q carries a byte that should have been encoded", name, location)
		}
		parsed, err := url.Parse(location)
		if err != nil {
			t.Errorf("%s: Location %q does not parse: %v", name, location, err)
			continue
		}
		if parsed.Scheme != "https" || parsed.Host != "example.com" || parsed.User != nil || !strings.HasPrefix(parsed.Path, "/") {
			t.Errorf("%s: Location %q resolves to scheme %q host %q path %q", name, location, parsed.Scheme, parsed.Host, parsed.Path)
		}
	}
}

// TestRedirectHTTPSTrustsTheProtocolOnlyFromAProxy is the spoofing table: a
// claim of https is believed from a trusted proxy and ignored from anyone
// else, and only the value the proxy itself wrote counts.
func TestRedirectHTTPSTrustsTheProtocolOnlyFromAProxy(t *testing.T) {
	t.Parallel()
	trusted := ClientIPOptions{TrustedProxies: []string{"192.0.2.0/24"}}
	plain := httpsRedirectApp(t, func(o *AppOptions) { o.ClientIP = trusted })
	forwarded := httpsRedirectApp(t, func(o *AppOptions) {
		o.ClientIP = ClientIPOptions{TrustedProxies: []string{"192.0.2.0/24"}, Header: "Forwarded"}
	})
	send := func(app *App, peer string, header string, values ...string) *httptest.ResponseRecorder {
		req := requestFor("GET", "/x", "example.com")
		req.RemoteAddr = peer + ":4000"
		for _, v := range values {
			req.Header.Add(header, v)
		}
		return doRequest(t, app, req)
	}
	const proxy, stranger = "192.0.2.10", "203.0.113.9"

	// A peer with no address, as on a Unix socket, can be trusted by nobody.
	unaddressed := requestFor("GET", "/x", "example.com")
	unaddressed.RemoteAddr = "@"
	unaddressed.Header.Set("X-Forwarded-Proto", "https")
	assertStatus(t, doRequest(t, plain, unaddressed), http.StatusMovedPermanently)

	for _, tc := range []struct {
		name     string
		app      *App
		peer     string
		header   string
		values   []string
		redirect bool
		vary     string
	}{
		{"stranger claims https", plain, stranger, "X-Forwarded-Proto", []string{"https"}, true, ""},
		{"stranger claims https in Forwarded", forwarded, stranger, "Forwarded", []string{"for=1.2.3.4;proto=https"}, true, ""},
		{"proxy says https", plain, proxy, "X-Forwarded-Proto", []string{"https"}, false, "X-Forwarded-Proto"},
		{"proxy says HTTPS", plain, proxy, "X-Forwarded-Proto", []string{" HTTPS "}, false, "X-Forwarded-Proto"},
		{"proxy says http", plain, proxy, "X-Forwarded-Proto", []string{"http"}, true, "X-Forwarded-Proto"},
		{"proxy says nothing", plain, proxy, "X-Other", nil, true, "X-Forwarded-Proto"},
		{"client prepended https", plain, proxy, "X-Forwarded-Proto", []string{"https, http"}, true, "X-Forwarded-Proto"},
		{"client line before the proxy's", plain, proxy, "X-Forwarded-Proto", []string{"https", "http"}, true, "X-Forwarded-Proto"},
		{"proxy appended https", plain, proxy, "X-Forwarded-Proto", []string{"http, https"}, false, "X-Forwarded-Proto"},
		{"wss is not https", plain, proxy, "X-Forwarded-Proto", []string{"wss"}, true, "X-Forwarded-Proto"},
		{"Forwarded ignored for X-Forwarded-For", plain, proxy, "Forwarded", []string{"proto=https"}, true, "X-Forwarded-Proto"},
		{"proxy Forwarded https", forwarded, proxy, "Forwarded", []string{"for=203.0.113.9;proto=https"}, false, "Forwarded"},
		{"proxy Forwarded quoted", forwarded, proxy, "Forwarded", []string{`for="[2001:db8::1]";proto="https"`}, false, "Forwarded"},
		{"proxy Forwarded last element", forwarded, proxy, "Forwarded", []string{"for=1.1.1.1;proto=http, for=203.0.113.9;proto=https"}, false, "Forwarded"},
		{"client Forwarded element first", forwarded, proxy, "Forwarded", []string{"proto=https, for=203.0.113.9;proto=http"}, true, "Forwarded"},
		{"proxy Forwarded without proto", forwarded, proxy, "Forwarded", []string{"for=203.0.113.9"}, true, "Forwarded"},
		{"proxy Forwarded proto twice", forwarded, proxy, "Forwarded", []string{"proto=https;proto=https"}, true, "Forwarded"},
		{"proxy Forwarded unclosed quote", forwarded, proxy, "Forwarded", []string{`proto="https`}, true, "Forwarded"},
		{"proxy Forwarded stray backslash", forwarded, proxy, "Forwarded", []string{`proto=https\`}, true, "Forwarded"},
		{"X-Forwarded-Proto ignored for Forwarded", forwarded, proxy, "X-Forwarded-Proto", []string{"https"}, true, "Forwarded"},
	} {
		res := send(tc.app, tc.peer, tc.header, tc.values...)
		if redirected := res.Code == http.StatusMovedPermanently; redirected != tc.redirect {
			t.Errorf("%s: status %d, redirected = %v, want %v", tc.name, res.Code, redirected, tc.redirect)
		}
		vary := strings.Join(res.Header().Values("Vary"), ", ")
		if tc.vary != "" && !strings.Contains(vary, tc.vary) {
			t.Errorf("%s: Vary %q does not name %s, which the answer depended on", tc.name, vary, tc.vary)
		}
		if tc.vary == "" && (strings.Contains(vary, "Forwarded")) {
			t.Errorf("%s: Vary %q names a header that was never read", tc.name, vary)
		}
	}
}

// TestRedirectHTTPSExemption checks the redirect honours the same internal
// hook the allowlist does.
func TestRedirectHTTPSExemption(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.RedirectHTTPS = &RedirectHTTPSOptions{Host: "example.com"}
	app := New(options)
	app.Get("/healthz", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	app.edgeExempt = func(r *http.Request) bool { return r.URL.Path == "/healthz" }
	mustBuild(t, app)
	assertStatus(t, doRequest(t, app, requestFor("GET", "/healthz", "10.0.0.1")), http.StatusOK)
	assertStatus(t, doRequest(t, app, requestFor("GET", "/other", "10.0.0.1")), http.StatusMovedPermanently)
}

// TestRedirectHTTPSOnTheWire sends raw requests through a real server: the
// Location is one well-formed header, and an absolute request target cannot
// choose the host it names.
func TestRedirectHTTPSOnTheWire(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.AllowedHosts = []string{"example.com"}
	options.RedirectHTTPS = &RedirectHTTPSOptions{}
	addr := serveOnWire(t, New(options))

	res, _ := rawGet(t, addr, "/a%0d%0aSet-Cookie:%20pwned=1/b?q=%0a", "example.com", "")
	if res.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status %d, want the redirect", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "https://example.com/a%0d%0aSet-Cookie:%20pwned=1/b?q=%0a" {
		t.Errorf("Location = %q", got)
	}
	if res.Header.Get("Set-Cookie") != "" {
		t.Error("the request line injected a header")
	}

	res, _ = rawExchange(t, addr, "GET http://evil.com/x HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	if res.StatusCode != http.StatusMisdirectedRequest || res.Header.Get("Location") != "" {
		t.Errorf("an absolute target naming evil.com = %d %q, want 421 without a redirect", res.StatusCode, res.Header.Get("Location"))
	}
	res, _ = rawExchange(t, addr, "GET http://example.com//evil.com/x HTTP/1.1\r\nHost: evil.com\r\nConnection: close\r\n\r\n")
	if got := res.Header.Get("Location"); got != "https://example.com//evil.com/x" {
		t.Errorf("Location = %q, want the path kept on the vouched host", got)
	}
}
