package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// versionOut is the small response body the tests in this file use to prove
// which version answered a request.
type versionOut struct {
	V string `json:"v"`
}

// versionHandler returns a handler that answers with its own version tag, so
// a test can tell which registration was actually reached.
func versionHandler(tag string) Handler[Empty, versionOut] {
	return func(_ *Context, _ Empty) (versionOut, error) {
		return versionOut{V: tag}, nil
	}
}

// --- unit tests: the matching primitives, independent of HTTP -------------

func TestVersionsOverlap(t *testing.T) {
	t.Parallel()
	neutral := []Version{VersionNeutral}
	for _, tc := range []struct {
		name string
		a, b []Version
		want bool
	}{
		{"both empty", nil, nil, false},
		{"one empty", []Version{"1"}, nil, false},
		{"disjoint", []Version{"1"}, []Version{"2"}, false},
		{"shared", []Version{"1", "2"}, []Version{"2", "3"}, true},
		{"identical", []Version{"1"}, []Version{"1"}, true},
		{"neutral vs specific", neutral, []Version{"1"}, true},
		{"neutral vs neutral", neutral, neutral, true},
		{"neutral vs empty", neutral, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := versionsOverlap(tc.a, tc.b); got != tc.want {
				t.Errorf("versionsOverlap(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := versionsOverlap(tc.b, tc.a); got != tc.want {
				t.Errorf("versionsOverlap is not symmetric for (%v, %v)", tc.b, tc.a)
			}
		})
	}
}

func TestSelectVersion(t *testing.T) {
	t.Parallel()
	v1 := &Route{OperationID: "v1", Versions: []Version{"1"}}
	v2 := &Route{OperationID: "v2", Versions: []Version{"2"}}
	neutral := &Route{OperationID: "neutral", Versions: []Version{VersionNeutral}}

	for _, tc := range []struct {
		name       string
		candidates []*Route
		requested  []Version
		want       *Route
	}{
		{"exact match", []*Route{v1, v2}, []Version{"1"}, v1},
		{"highest preferred first", []*Route{v1, v2}, []Version{"2", "1"}, v2},
		{"falls through to a lower preference", []*Route{v1}, []Version{"3", "1"}, v1},
		{"no match, no neutral", []*Route{v1, v2}, []Version{"3"}, nil},
		{"no requested version matches neutral", []*Route{neutral}, nil, neutral},
		{"no requested version does not match a specific one", []*Route{v1}, nil, nil},
		{"neutral answers any requested version too", []*Route{neutral}, []Version{"9"}, neutral},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := selectVersion(tc.candidates, tc.requested)
			if got != tc.want {
				t.Errorf("selectVersion(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateVersionList(t *testing.T) {
	t.Parallel()
	if err := validateVersionList(nil, "x"); err != nil {
		t.Errorf("empty list: %v, want nil", err)
	}
	if err := validateVersionList([]Version{"1"}, "x"); err != nil {
		t.Errorf("single version: %v, want nil", err)
	}
	if err := validateVersionList([]Version{"1", "2"}, "x"); err != nil {
		t.Errorf("disjoint versions: %v, want nil", err)
	}
	if err := validateVersionList([]Version{VersionNeutral}, "x"); err != nil {
		t.Errorf("neutral alone: %v, want nil", err)
	}
	err := validateVersionList([]Version{VersionNeutral, "1"}, "x")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("neutral combined with another version: %v, want a combination error", err)
	}
}

func TestVersioningOptionsValidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		opts    VersioningOptions
		wantErr string
	}{
		{"none, no default", VersioningOptions{}, ""},
		{"none with a default is an error", VersioningOptions{DefaultVersion: []Version{"1"}}, "Type is not"},
		{"uri needs nothing extra", VersioningOptions{Type: VersioningURI}, ""},
		{"header needs a header name", VersioningOptions{Type: VersioningHeader}, "must name a header"},
		{"header with a name is fine", VersioningOptions{Type: VersioningHeader, Header: "X-Version"}, ""},
		{"media type needs a key", VersioningOptions{Type: VersioningMediaType}, "Key must be set"},
		{"custom needs an extractor", VersioningOptions{Type: VersioningCustom}, "Extractor must be set"},
		{"bad default version list", VersioningOptions{Type: VersioningURI, DefaultVersion: []Version{VersionNeutral, "1"}}, "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.opts.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validate() = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestRequestedVersions(t *testing.T) {
	t.Parallel()

	header := VersioningOptions{Type: VersioningHeader, Header: "X-API-Version"}
	req := httptestRequest(t, "GET", "/x", "X-API-Version", "2")
	if got := header.requestedVersions(req); len(got) != 1 || got[0] != "2" {
		t.Errorf("header extraction = %v, want [2]", got)
	}
	if got := header.requestedVersions(httptestRequest(t, "GET", "/x")); got != nil {
		t.Errorf("missing header = %v, want nil", got)
	}

	media := VersioningOptions{Type: VersioningMediaType, Key: "v="}
	req = httptestRequest(t, "GET", "/x", "Accept", "application/json;v=3")
	if got := media.requestedVersions(req); len(got) != 1 || got[0] != "3" {
		t.Errorf("media type extraction = %v, want [3]", got)
	}
	req = httptestRequest(t, "GET", "/x", "Accept", "text/html, application/json;charset=utf-8;v=5")
	if got := media.requestedVersions(req); len(got) != 1 || got[0] != "5" {
		t.Errorf("media type extraction among several ranges = %v, want [5]", got)
	}
	if got := media.requestedVersions(httptestRequest(t, "GET", "/x", "Accept", "application/json")); got != nil {
		t.Errorf("accept without the key = %v, want nil", got)
	}

	custom := VersioningOptions{Type: VersioningCustom, Extractor: func(r *http.Request) []string {
		return strings.Split(r.Header.Get("X-Versions"), ",")
	}}
	req = httptestRequest(t, "GET", "/x", "X-Versions", "3,2,1")
	got := custom.requestedVersions(req)
	want := []Version{"3", "2", "1"}
	if len(got) != len(want) {
		t.Fatalf("custom extraction = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("custom extraction[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := custom.requestedVersions(httptestRequest(t, "GET", "/x", "X-Versions", "")); got != nil {
		t.Errorf("empty extraction = %v, want nil", got)
	}
}

// httptestRequest builds a request carrying the given header name/value
// pairs, for the extraction tests above which never reach a served app.
func httptestRequest(t *testing.T, method, target string, headers ...string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "http://example.test"+target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return req
}

// --- integration tests: a served application end to end --------------------

func TestURIVersioning(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion("1"))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/v1/cats"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/cats"), http.StatusNotFound)
	assertStatus(t, do(t, app, "GET", "/v2/cats"), http.StatusNotFound)
}

func TestURIVersioningCustomPrefix(t *testing.T) {
	t.Parallel()
	custom := "ver-"
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI, Prefix: &custom}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion("1"))
	mustBuild(t, app)
	assertStatus(t, do(t, app, "GET", "/ver-1/cats"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/v1/cats"), http.StatusNotFound)

	none := ""
	opts2 := quietOptions()
	opts2.Versioning = VersioningOptions{Type: VersioningURI, Prefix: &none}
	app2 := New(opts2)
	app2.Get("/cats", versionHandler("1"), WithVersion("1"))
	mustBuild(t, app2)
	assertStatus(t, do(t, app2, "GET", "/1/cats"), http.StatusOK)
}

func TestURIVersioningNeutralHasNoPrefix(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)
	app.Get("/health", versionHandler("any"), WithVersion(VersionNeutral))
	mustBuild(t, app)
	assertStatus(t, do(t, app, "GET", "/health"), http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/v1/health"), http.StatusNotFound)
}

func TestURIVersioningMultipleVersions(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)
	app.Get("/cats", versionHandler("both"), WithVersion("1", "2"))
	mustBuild(t, app)
	assertJSON(t, do(t, app, "GET", "/v1/cats"), `{"v":"both"}`)
	assertJSON(t, do(t, app, "GET", "/v2/cats"), `{"v":"both"}`)
	assertStatus(t, do(t, app, "GET", "/v3/cats"), http.StatusNotFound)
}

func TestURIVersioningRouterLevelIsOverriddenByRoute(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)
	admin := NewRouter(WithVersion("1"))
	admin.Get("/cats", versionHandler("router-default"))
	admin.Get("/dogs", versionHandler("route-override"), WithVersion("2"))
	app.Include(admin)
	mustBuild(t, app)

	assertJSON(t, do(t, app, "GET", "/v1/cats"), `{"v":"router-default"}`)
	assertJSON(t, do(t, app, "GET", "/v2/dogs"), `{"v":"route-override"}`)
	assertStatus(t, do(t, app, "GET", "/v1/dogs"), http.StatusNotFound)
}

func TestHeaderVersioning(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningHeader, Header: "X-API-Version"}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion("1"))
	app.Get("/cats", versionHandler("2"), WithVersion("2"))
	mustBuild(t, app)

	assertJSON(t, doHeader(t, app, "GET", "/cats", "X-API-Version", "1"), `{"v":"1"}`)
	assertJSON(t, doHeader(t, app, "GET", "/cats", "X-API-Version", "2"), `{"v":"2"}`)
	assertStatus(t, doHeader(t, app, "GET", "/cats", "X-API-Version", "3"), http.StatusNotFound)
	// No header at all: neither specific version answers, and there is no
	// version-neutral registration here to fall back to.
	assertStatus(t, do(t, app, "GET", "/cats"), http.StatusNotFound)
}

func TestHeaderVersioningFallsBackToNeutral(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningHeader, Header: "X-API-Version"}
	app := New(opts)
	app.Get("/health", versionHandler("any"), WithVersion(VersionNeutral))
	mustBuild(t, app)

	assertStatus(t, do(t, app, "GET", "/health"), http.StatusOK)
	assertStatus(t, doHeader(t, app, "GET", "/health", "X-API-Version", "1"), http.StatusOK)
}

func TestMediaTypeVersioning(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningMediaType, Key: "v="}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion("1"))
	app.Get("/cats", versionHandler("2"), WithVersion("2"))
	mustBuild(t, app)

	assertJSON(t, doHeader(t, app, "GET", "/cats", "Accept", "application/json;v=1"), `{"v":"1"}`)
	assertJSON(t, doHeader(t, app, "GET", "/cats", "Accept", "application/json;v=2"), `{"v":"2"}`)
	assertStatus(t, doHeader(t, app, "GET", "/cats", "Accept", "application/json"), http.StatusNotFound)
}

func TestCustomVersioning(t *testing.T) {
	t.Parallel()
	extractor := func(r *http.Request) []string {
		raw := r.Header.Get("X-Versions")
		if raw == "" {
			return nil
		}
		return strings.Split(raw, ",")
	}
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningCustom, Extractor: extractor}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion("1"))
	app.Get("/cats", versionHandler("2"), WithVersion("2"))
	mustBuild(t, app)

	// The highest version the client offers that some route actually
	// answers wins, tried in the order the extractor returned.
	assertJSON(t, doHeader(t, app, "GET", "/cats", "X-Versions", "3,2,1"), `{"v":"2"}`)
	assertJSON(t, doHeader(t, app, "GET", "/cats", "X-Versions", "1"), `{"v":"1"}`)
	assertStatus(t, doHeader(t, app, "GET", "/cats", "X-Versions", "9"), http.StatusNotFound)
}

func TestVersioningDefaultVersion(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI, DefaultVersion: []Version{"1"}}
	app := New(opts)
	app.Get("/cats", versionHandler("1")) // no WithVersion at all
	mustBuild(t, app)
	assertStatus(t, do(t, app, "GET", "/v1/cats"), http.StatusOK)
}

func TestVersioningUnreachableWithoutDefault(t *testing.T) {
	t.Parallel()
	// A route declaring no version, under an application with versioning
	// enabled and no default, builds successfully but answers nothing: the
	// documented "unspecified version" behaviour, not a build error.
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningHeader, Header: "X-API-Version"}
	app := New(opts)
	app.Get("/cats", versionHandler("1"))
	mustBuild(t, app)
	assertStatus(t, do(t, app, "GET", "/cats"), http.StatusNotFound)
	assertStatus(t, doHeader(t, app, "GET", "/cats", "X-API-Version", "1"), http.StatusNotFound)
}

func TestWithVersionRequiresVersioningEnabled(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/cats", versionHandler("1"), WithVersion("1"))
	if got := buildError(t, app); !strings.Contains(got, "versioning is not enabled") {
		t.Errorf("build error = %q, want it to say versioning is not enabled", got)
	}
}

func TestWithVersionEmptyIsBuildError(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion())
	if got := buildError(t, app); !strings.Contains(got, "no versions") {
		t.Errorf("build error = %q, want it to mention no versions", got)
	}
}

func TestVersionNeutralCannotCombineAtBuildTime(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion(VersionNeutral, "1"))
	if got := buildError(t, app); !strings.Contains(got, "cannot be combined") {
		t.Errorf("build error = %q, want it to mention the combination", got)
	}
}

func TestOverlappingVersionsAreRegisteredTwice(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningHeader, Header: "X-API-Version"}
	app := New(opts)
	app.Get("/cats", versionHandler("a"), WithVersion("1"))
	app.Get("/cats", versionHandler("b"), WithVersion("1"))
	if got := buildError(t, app); !strings.Contains(got, "registered twice") {
		t.Errorf("build error = %q, want it to say registered twice", got)
	}
}

func TestDisjointVersionsCoexistAtOnePath(t *testing.T) {
	t.Parallel()
	// Not a build error: distinct, non-overlapping versions of one route are
	// exactly what a header/media-type/custom scheme registers at one path.
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningHeader, Header: "X-API-Version"}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion("1"))
	app.Get("/cats", versionHandler("2"), WithVersion("2"))
	mustBuild(t, app)
}

func TestURIVersioningOperationIDs(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)
	app.Get("/cats", versionHandler("both"), WithVersion("1", "2"), OperationID("listCats"))
	mustBuild(t, app)

	ids := map[string]bool{}
	for _, rt := range app.routes {
		ids[rt.OperationID] = true
	}
	if !ids["listCats_1"] || !ids["listCats_2"] {
		t.Errorf("operation ids = %v, want listCats_1 and listCats_2", ids)
	}
}

// doHeader sends a request carrying one header through an application, for
// the versioning tests that dispatch on a header or the Accept header rather
// than on the path.
func doHeader(t *testing.T, app *App, method, target, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set(header, value)
	return doRequest(t, app, req)
}

// TestMediaTypeVersionParsesAcceptAsAMediaRange is the regression test for
// the version being read out of Accept by text matching, which took a bare
// "v=1" for a media range, ignored case, quoting and q, and read only the
// first line of a header sent as several. A proxy or firewall that parses the
// header properly would then disagree with the application about which
// version a request asked for.
func TestMediaTypeVersionParsesAcceptAsAMediaRange(t *testing.T) {
	t.Parallel()
	opts := VersioningOptions{Type: VersioningMediaType, Key: "v="}
	cases := []struct {
		name   string
		accept []string
		want   Version
	}{
		{"plain parameter", []string{"application/json;v=2"}, "2"},
		{"parameter name is case insensitive", []string{"application/json;V=2"}, "2"},
		{"quoted value", []string{`application/json;v="2"`}, "2"},
		{"spaces around the parameter", []string{"application/json ; v=2"}, "2"},
		{"a bare token is not a media range", []string{"v=1"}, ""},
		{"a bare token beside a real range", []string{"v=1, application/json;v=2"}, "2"},
		{"the highest quality wins", []string{"application/json;v=1;q=0.1, application/json;v=2"}, "2"},
		{"quality wins whatever the order", []string{"application/json;v=2;q=0.1, application/json;v=1"}, "1"},
		{"an equal quality keeps the order written", []string{"application/json;v=3, application/json;v=2"}, "3"},
		{"a range refused with q=0 is not chosen", []string{"application/json;v=1;q=0, application/json;v=2;q=0.5"}, "2"},
		{"only a refused range", []string{"application/json;v=1;q=0"}, ""},
		{"an unreadable quality is not trusted", []string{"application/json;v=1;q=abc"}, ""},
		{"every line is read, by quality", []string{"application/json;v=1;q=0.2", "application/json;v=2"}, "2"},
		{"every line is read, in order", []string{"application/json;v=1", "application/json;v=2"}, "1"},
		{"no parameter", []string{"application/json"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptestRequest(t, "GET", "/x")
			for _, line := range tc.accept {
				req.Header.Add("Accept", line)
			}
			got := opts.requestedVersions(req)
			if tc.want == "" {
				if got != nil {
					t.Errorf("requestedVersions(%q) = %v, want none", tc.accept, got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("requestedVersions(%q) = %v, want [%s]", tc.accept, got, tc.want)
			}
		})
	}

	// A key configured without its "=" names the same parameter.
	bare := VersioningOptions{Type: VersioningMediaType, Key: "v"}
	if got := bare.requestedVersions(httptestRequest(t, "GET", "/x", "Accept", "application/json;v=4")); len(got) != 1 || got[0] != "4" {
		t.Errorf("Key %q read %v, want [4]", bare.Key, got)
	}
}

// TestHeaderVersionWithSeveralLinesMatchesNothing is the regression test for
// a version header sent on two lines being read from the first only, which a
// proxy that reads the last line would disagree with. HTTP defines the two
// lines as one comma-separated list, and that list names no version.
func TestHeaderVersionWithSeveralLinesMatchesNothing(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningHeader, Header: "X-API-Version"}
	app := New(opts)
	app.Get("/cats", versionHandler("1"), WithVersion("1"))
	app.Get("/cats", versionHandler("2"), WithVersion("2"))
	mustBuild(t, app)

	req := httptest.NewRequest("GET", "/cats", nil)
	req.Header.Add("X-Api-Version", "2")
	req.Header.Add("X-Api-Version", "1")
	assertStatus(t, doRequest(t, app, req), http.StatusNotFound)

	assertJSON(t, doHeader(t, app, "GET", "/cats", "X-Api-Version", "2"), `{"v":"2"}`)
}
