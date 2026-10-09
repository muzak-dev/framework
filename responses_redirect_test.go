package muzak

import (
	"net/http"
	"strings"
	"testing"
)

// A redirect to a place a client named is how a phishing link borrows an
// application's domain. These tests are the attacks: every spelling of
// "another host" a browser, a proxy or a careless decoder might read out of a
// target, against the check that has to refuse it.

// redirectApp serves one GET route that redirects wherever the request's
// "to" query parameter says, the shape of every open redirect, with the
// given options on the route.
func redirectApp(t *testing.T, external bool, opts ...RouteOption) (*App, *syncBuffer) {
	t.Helper()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	app := New(options)
	app.Get("/go", func(ctx *Context, in struct {
		To string `query:"to"`
	}) (Redirect, error) {
		return Redirect{To: in.To, External: external}, nil
	}, opts...)
	return app, logs
}

// redirectTo asks the redirecting route to send the client to target.
func redirectTo(t *testing.T, app *App, target string) (status int, location string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/go", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	query := req.URL.Query()
	query.Set("to", target)
	req.URL.RawQuery = query.Encode()
	rec := doRequest(t, app, req)
	return rec.Code, rec.Header().Get("Location")
}

func TestRedirectAcceptsAPathOnThisOrigin(t *testing.T) {
	t.Parallel()
	app, _ := redirectApp(t, false)
	for _, target := range []string{
		"/",
		"/dashboard",
		"/items/42?tab=history#top",
		"/a/./b",
		"/a/../b",
		"/a//b",
		"/a%2Fb",
		"/%E2%82%AC",
		"/login?next=//evil.com",
		"/login?next=%2F%2Fevil.com",
		"/login#//evil.com",
		"/search?q=a+b&x=%0A",
		"/.well-known/openid-configuration",
		"/:colon",
		"/@user",
	} {
		status, location := redirectTo(t, app, target)
		if status != http.StatusFound || location != target {
			t.Errorf("%q: got %d to %q, want 302 to the target as given", target, status, location)
		}
	}
}

func TestRedirectRefusesEveryOtherHost(t *testing.T) {
	t.Parallel()
	app, logs := redirectApp(t, false)
	for _, target := range []string{
		// Scheme-relative, which a browser reads as another host.
		"//evil.com",
		"///evil.com",
		"////evil.com/x",
		// A backslash is a slash to a browser, raw or after a slash.
		`/\evil.com`,
		`\\evil.com`,
		`\/evil.com`,
		"/x\\..\\..\\evil",
		// A browser strips tabs and line breaks before it reads a URL.
		"/\t/evil.com",
		"/\n/evil.com",
		"/\r\n/evil.com",
		"\t//evil.com",
		" //evil.com",
		"/ /evil.com",
		// Every other control character, NUL and DEL included.
		"/\x00/evil.com",
		"/x\x01",
		"/x\x7f",
		"/x\x1b[31m",
		// Encoded separators that one decoding turns into "//" or "/\".
		"/%2F/evil.com",
		"/%2f%2fevil.com",
		"/%2F%2Fevil.com/%2E%2E",
		"/%5Cevil.com",
		"/%5c%5cevil.com",
		"/%5C/evil.com",
		// The same behind further layers of escaping.
		"/%252F%252Fevil.com",
		"/%25252F%25252Fevil.com",
		"/%255Cevil.com",
		"/%25%32%46%25%32%46evil.com",
		// Dot segments that resolve to "//".
		"/.//evil.com",
		"/..//evil.com",
		"/./..//evil.com",
		"/%2e//evil.com",
		"/%2E%2E//evil.com",
		"/%2e%2e%2f%2fevil.com",
		"/.%2F/evil.com",
		"/..%5C/evil.com",
		// Encoded control characters in the path.
		"/%09/evil.com",
		"/%0d%0aSet-Cookie:%20a=b",
		"/%00",
		// Unicode that looks like a separator or a dot, which must be
		// percent-encoded rather than sent raw.
		"/\xef\xbc\x8fevil.com",
		"/\xe2\x88\x95evil.com",
		"/\xe2\x81\x84evil.com",
		"/\xef\xb9\xa8evil.com",
		"/caf\xc3\xa9",
		"/\xe2\x80\xaeevil",
		"/\xc0\xaf",
		// Not a path at all.
		"evil.com",
		"evil.com/x",
		"./x",
		"../x",
		"?x",
		"#x",
		"%2F%2Fevil.com",
		"./a:b",
		"1http://evil.com",
		"a/b:c",
		"ht_tp://evil.com",
		":evil.com",
		"",
		// Characters a URI cannot hold.
		"/<script>",
		`/"onmouseover`,
		"/{x}",
		"/a|b",
		"/a^b",
		"/a`b",
		// Escapes that do not decode.
		"/%zz",
		"/%2",
		"/%",
		// Schemes that are not http or https, and absolute URLs whose host
		// nothing lists.
		"javascript:alert(1)",
		"JaVaScRiPt:alert(1)",
		"vbscript:msgbox(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"file:///etc/passwd",
		"ftp://evil.com/",
		"https://evil.com/",
		"http://evil.com",
		"https:evil.com",
		"https:/evil.com",
		"https:///evil.com",
		"https://",
		"http://:80/",
		// Long enough to cost something to check.
		"/" + strings.Repeat("a", maxRedirectLength),
	} {
		status, location := redirectTo(t, app, target)
		if status != http.StatusInternalServerError || location != "" {
			t.Errorf("%q: got %d to %q, want 500 and no Location", target, status, location)
		}
	}
	if !strings.Contains(logs.String(), "returned a muzak.Redirect that was refused") {
		t.Errorf("the refusals were not logged: %s", logs.String())
	}
}

// A refused target is logged by its reason alone, since the target may carry
// a token in its query.
func TestRedirectRefusalDoesNotLogTheTarget(t *testing.T) {
	t.Parallel()
	app, logs := redirectApp(t, false)
	status, _ := redirectTo(t, app, "https://evil.com/cb?token=s3cret-value")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Errorf("the log holds the target's query: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "RedirectHosts") {
		t.Errorf("the log does not say how to allow the host: %s", logs.String())
	}
}

func TestRedirectHostsAdmitsExactlyTheHostsListed(t *testing.T) {
	t.Parallel()
	app, _ := redirectApp(t, false, RedirectHosts("accounts.example.com", "localhost:5173", "[::1]:8443", "10.0.0.1"))
	for target, allowed := range map[string]bool{
		"https://accounts.example.com/":                              true,
		"https://accounts.example.com":                               true,
		"https://accounts.example.com/cb?x=1":                        true,
		"http://accounts.example.com/":                               true,
		"https://ACCOUNTS.Example.COM/x":                             true,
		"HTTPS://accounts.example.com/x":                             true,
		"https://accounts.example.com:443/":                          true,
		"http://accounts.example.com:80/":                            true,
		"http://localhost:5173/callback":                             true,
		"https://[::1]:8443/":                                        true,
		"http://10.0.0.1/":                                           true,
		"https://accounts.example.com.evil.com":                      false,
		"https://accounts.example.com.evil.com/accounts.example.com": false,
		"https://evilaccounts.example.com/":                          false,
		"https://sub.accounts.example.com/":                          false,
		"https://example.com/":                                       false,
		"https://evil.com/accounts.example.com":                      false,
		"https://evil.com/?accounts.example.com":                     false,
		"https://accounts.example.com@evil.com/":                     false,
		"https://accounts.example.com:443@evil.com/":                 false,
		"https://user:pw@accounts.example.com/":                      false,
		"https://evil.com#@accounts.example.com":                     false,
		"https://evil.com?@accounts.example.com":                     false,
		`https://evil.com\@accounts.example.com`:                     false,
		"https://accounts.example.com./":                             false,
		"https://accounts.example.com:8443/":                         false,
		"http://accounts.example.com:443/":                           false,
		"https://accounts.example.com:/":                             true,
		"https://accounts.example.com:0443/":                         false,
		"https://localhost/":                                         false,
		"http://localhost:5174/":                                     false,
		"https://[::1]/":                                             false,
		"https://[::1]:8444/":                                        false,
		"http://10.0.0.1:8080/":                                      false,
		"http://0x0a.0.0.1/":                                         false,
		"http://167772161/":                                          false,
		"https://accounts%2Eexample.com/":                            false,
		"https://accounts.example.co%6D/":                            false,
		"https://accounts.example.com\xe3\x80\x82evil.com/":          false,
		"https://accounts\xef\xbc\x8eexample.com/":                   false,
		"ftp://accounts.example.com/":                                false,
		"//accounts.example.com/":                                    false,
		"https:accounts.example.com":                                 false,
		"https:/accounts.example.com":                                false,
	} {
		status, location := redirectTo(t, app, target)
		switch {
		case allowed && (status != http.StatusFound || location != target):
			t.Errorf("%q: got %d to %q, want 302 to the target as given", target, status, location)
		case !allowed && (status != http.StatusInternalServerError || location != ""):
			t.Errorf("%q: got %d to %q, want it refused", target, status, location)
		}
	}
}

// Lists declared on the application, a router and a route add up, and a
// route may send a client to any host one of them names.
func TestRedirectHostsAddUpDownTheTree(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), RedirectHosts("app.example.com"))
	api := NewRouter(RedirectHosts("router.example.com"))
	api.Get("/go", func(ctx *Context, in struct {
		To string `query:"to"`
	}) (Redirect, error) {
		return Redirect{To: in.To}, nil
	}, RedirectHosts("route.example.com"))
	app.Include(api)
	other := NewRouter()
	other.Get("/elsewhere", func(ctx *Context, in struct {
		To string `query:"to"`
	}) (Redirect, error) {
		return Redirect{To: in.To}, nil
	})
	app.Include(other)

	for _, host := range []string{"app.example.com", "router.example.com", "route.example.com"} {
		if rec := do(t, app, http.MethodGet, "/go?to=https://"+host+"/"); rec.Code != http.StatusFound {
			t.Errorf("%s: status %d, want 302", host, rec.Code)
		}
	}
	if rec := do(t, app, http.MethodGet, "/elsewhere?to=https://app.example.com/"); rec.Code != http.StatusFound {
		t.Errorf("the application's list did not reach a sibling router: %d", rec.Code)
	}
	if rec := do(t, app, http.MethodGet, "/elsewhere?to=https://router.example.com/"); rec.Code != http.StatusInternalServerError {
		t.Errorf("one router's list reached its sibling: %d", rec.Code)
	}
}

// External vouches for a host, but not for anything else a target could do.
func TestRedirectExternalAcceptsAnyHostAndNothingElse(t *testing.T) {
	t.Parallel()
	app, _ := redirectApp(t, true)
	for target, allowed := range map[string]bool{
		"https://idp.example.org/authorize?client_id=x": true,
		"http://anything.test":                          true,
		"/still/local":                                  true,
		"https://user@idp.example.org/":                 false,
		"javascript:alert(1)":                           false,
		"//idp.example.org/":                            false,
		"https:idp.example.org":                         false,
		"https://idp.example.org/\r\nX-Injected: 1":     false,
		"https://idp%2Eexample.org/":                    false,
		"https://[::1/":                                 false,
	} {
		status, _ := redirectTo(t, app, target)
		if want := map[bool]int{true: http.StatusFound, false: http.StatusInternalServerError}[allowed]; status != want {
			t.Errorf("%q: status %d, want %d", target, status, want)
		}
	}
}

func TestRedirectStatusDefaultsByMethod(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	to := func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{To: "/next"}, nil }
	app.Get("/r", to)
	app.Post("/r", to)
	app.Put("/r", to)
	app.Delete("/r", to)
	for method, want := range map[string]int{
		http.MethodGet:    http.StatusFound,
		http.MethodHead:   http.StatusFound,
		http.MethodPost:   http.StatusSeeOther,
		http.MethodPut:    http.StatusSeeOther,
		http.MethodDelete: http.StatusSeeOther,
	} {
		rec := do(t, app, method, "/r")
		if rec.Code != want || rec.Header().Get("Location") != "/next" || rec.Body.Len() != 0 {
			t.Errorf("%s: got %d to %q with %d bytes, want %d to /next and no body",
				method, rec.Code, rec.Header().Get("Location"), rec.Body.Len(), want)
		}
	}
}

func TestRedirectStatusIsTheValuesOrTheRoutes(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/own", func(ctx *Context, _ Empty) (Redirect, error) {
		return Redirect{To: "/x", Status: http.StatusPermanentRedirect}, nil
	})
	app.Get("/declared", func(ctx *Context, _ Empty) (Redirect, error) {
		return Redirect{To: "/x"}, nil
	}, Status(http.StatusMovedPermanently))
	app.Get("/declared-overridden", func(ctx *Context, _ Empty) (Redirect, error) {
		return Redirect{To: "/x", Status: http.StatusTemporaryRedirect}, nil
	}, Status(http.StatusMovedPermanently))
	app.Get("/imperative", func(ctx *Context, _ Empty) (Redirect, error) {
		ctx.SetStatus(http.StatusTemporaryRedirect)
		return Redirect{To: "/x"}, nil
	})
	app.Get("/dynamic", func(ctx *Context, _ Empty) (any, error) {
		return Redirect{To: "/x"}, nil
	})
	for path, want := range map[string]int{
		"/own":                 http.StatusPermanentRedirect,
		"/declared":            http.StatusMovedPermanently,
		"/declared-overridden": http.StatusTemporaryRedirect,
		"/imperative":          http.StatusTemporaryRedirect,
		"/dynamic":             http.StatusFound,
	} {
		if rec := do(t, app, http.MethodGet, path); rec.Code != want {
			t.Errorf("%s: status %d, want %d", path, rec.Code, want)
		}
	}
}

func TestRedirectWithAStatusThatIsNotARedirectFails(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusNotModified, http.StatusMultipleChoices, 305, 306, 309, http.StatusNotFound, -1, 1000} {
		logger, logs := captureLogger(t)
		opts := quietOptions()
		opts.Logger = logger
		app := New(opts)
		app.Get("/x", func(ctx *Context, _ Empty) (Redirect, error) {
			return Redirect{To: "/next", Status: status}, nil
		})
		rec := do(t, app, http.MethodGet, "/x")
		if rec.Code != http.StatusInternalServerError || rec.Header().Get("Location") != "" {
			t.Errorf("status %d: got %d to %q, want 500 and no Location", status, rec.Code, rec.Header().Get("Location"))
		}
		if !strings.Contains(logs.String(), "use 301, 302, 303, 307 or 308") {
			t.Errorf("status %d: the cause was not logged: %s", status, logs.String())
		}
	}
}

func TestRedirectRouteMustDeclareARedirectStatus(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{To: "/"}, nil }, Status(http.StatusOK))
	if msg := buildError(t, app); !strings.Contains(msg, "returns muzak.Redirect but declares status 200") {
		t.Errorf("build error = %q, want it to name the status", msg)
	}
}

// An error renderer may answer with a redirect, which is how a failure such
// as a missing session sends a browser to a login page.
func TestErrorRendererMayAnswerWithARedirect(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.ErrorRenderer = func(ctx *Context, err error) (int, any) {
		return http.StatusSeeOther, Redirect{To: "/login"}
	}
	app := New(opts)
	app.Get("/private", func(ctx *Context, _ Empty) (rtOut, error) { return rtOut{}, Unauthorized("") })
	rec := do(t, app, http.MethodGet, "/private")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("got %d to %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
}

func TestRedirectHostsRefusesEntriesThatAreNotHosts(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{
		"",
		"https://accounts.example.com",
		"accounts.example.com/",
		"accounts.example.com/path",
		"*.example.com",
		"user@accounts.example.com",
		"accounts.example.com.",
		".example.com",
		"a..b",
		"-a.example.com",
		"a-.example.com",
		"accounts.example.com:",
		"accounts.example.com:0",
		"accounts.example.com:65536",
		"accounts.example.com:08",
		"accounts.example.com:http",
		"[::1",
		"[::1]x",
		"[::1]:",
		"[10.0.0.1]",
		"[fe80::1%25en0]",
		"::1",
		"accounts_example.com",
		"accounts.example.com?x",
		"caf\xc3\xa9.example.com",
		strings.Repeat("a", 64) + ".example.com",
		strings.Repeat("a.", 127) + "com",
		strings.Repeat("a.", 140) + "com",
	} {
		app := New(quietOptions())
		app.Get("/x", func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{To: "/"}, nil }, RedirectHosts(entry))
		if msg := buildError(t, app); !strings.Contains(msg, "RedirectHosts was given") {
			t.Errorf("%q: build error = %q, want it refused", entry, msg)
		}
	}
}

func TestRedirectHostsAcceptsHostsPortsAndAddresses(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"accounts.example.com", "ACCOUNTS.example.com", "localhost", "localhost:8080", "10.0.0.1", "10.0.0.1:65535", "[::1]", "[2001:db8::1]:443", "xn--caf-dma.example"} {
		app := New(quietOptions())
		app.Get("/x", func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{To: "/"}, nil }, RedirectHosts(entry))
		if err := app.Build(); err != nil {
			t.Errorf("%q: Build = %v", entry, err)
		}
	}
}

// The checks are linear in the target, which they bound.
func TestRedirectCheckIsBounded(t *testing.T) {
	t.Parallel()
	long := "/" + strings.Repeat("%2525", (maxRedirectLength-1)/5)
	if err := checkRedirectTarget(long, false, nil); err != nil {
		t.Errorf("a long target of escapes at the bound = %v, want it accepted", err)
	}
	if err := checkRedirectTarget(long+"xxxxx", false, nil); err == nil {
		t.Error("a target past the bound was accepted")
	}
	dots := "/" + strings.Repeat("./", (maxRedirectLength-2)/2) + "x"
	if err := checkRedirectTarget(dots, false, nil); err != nil {
		t.Errorf("a target of dot segments at the bound = %v, want it accepted", err)
	}
}

// A list declared for the whole application is read by the routes that can
// redirect and by no other, so a mistake in it is reported once per such
// route, and not once for every route in the application.
func TestRedirectHostsAreReadOnlyWhereARedirectCanBeAnswered(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), RedirectHosts("https://not-a-host"))
	for _, path := range []string{"/a", "/b", "/c"} {
		app.Get(path, func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{}, nil })
	}
	if err := app.Build(); err != nil {
		t.Fatalf("an application with no redirecting route = %v, want it built", err)
	}

	app = New(quietOptions(), RedirectHosts("https://not-a-host"))
	app.Get("/a", func(ctx *Context, _ Empty) (etagOut, error) { return etagOut{}, nil })
	app.Get("/login", func(ctx *Context, _ Empty) (Redirect, error) { return Redirect{To: "/"}, nil })
	app.Get("/any", func(ctx *Context, _ Empty) (any, error) { return Redirect{To: "/"}, nil })
	msg := buildError(t, app)
	if strings.Count(msg, "RedirectHosts was given") != 2 || !strings.Contains(msg, "GET /login") || !strings.Contains(msg, "GET /any") {
		t.Errorf("build error = %q, want it once for each route that can redirect", msg)
	}
}

// A handler that wrote its own response keeps it, whatever it returns.
func TestRedirectAfterTheHandlerWroteIsIgnored(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (Redirect, error) {
		_, _ = ctx.ResponseWriter().Write([]byte("mine"))
		return Redirect{To: "//evil.com"}, nil
	})
	rec := do(t, app, http.MethodGet, "/x")
	if rec.Code != http.StatusOK || rec.Body.String() != "mine" || rec.Header().Get("Location") != "" {
		t.Errorf("got %d %q to %q, want the handler's own answer", rec.Code, rec.Body.String(), rec.Header().Get("Location"))
	}
}
