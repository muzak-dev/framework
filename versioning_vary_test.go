package muzak

import (
	"net/http"
	"strings"
	"testing"
)

// varyNamesField reports whether a response's Vary covers field.
func varyNamesField(h http.Header, field string) bool {
	for _, value := range h.Values("Vary") {
		for part := range strings.SplitSeq(value, ",") {
			part = strings.TrimSpace(part)
			if part == "*" || strings.EqualFold(part, field) {
				return true
			}
		}
	}
	return false
}

// Header and media type versioning answer one URL with a different handler
// per request header, and used to say nothing about it in Vary, so a shared
// cache would hand the answer stored for one version to a client asking for
// another. The header consulted is now named, on the 404 for a version
// nothing answers as well.
func TestVersioningByHeaderVaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, header, field, v1, v2, none string
		opts                              VersioningOptions
	}{
		{
			name: "header", header: "X-API-Version", field: "X-Api-Version", v1: "1", v2: "2", none: "9",
			opts: VersioningOptions{Type: VersioningHeader, Header: "x-api-version"},
		},
		{
			name: "mediatype", header: "Accept", field: "Accept",
			v1: "application/json;v=1", v2: "application/json;v=2", none: "application/json;v=9",
			opts: VersioningOptions{Type: VersioningMediaType, Key: "v="},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := quietOptions()
			options.Versioning = tc.opts
			app := New(options)
			app.Get("/users/me", versionHandler("1"), WithVersion("1"))
			app.Get("/users/me", versionHandler("2"), WithVersion("2"))
			app.Get("/health", versionHandler("any"), WithVersion(VersionNeutral))
			mustBuild(t, app)

			for _, value := range []string{tc.v1, tc.v2} {
				rec := doHeader(t, app, http.MethodGet, "/users/me", tc.header, value)
				assertStatus(t, rec, http.StatusOK)
				if !varyNamesField(rec.Header(), tc.field) {
					t.Errorf("%s: %s: Vary = %q, want it to name %s", tc.header, value, rec.Header().Values("Vary"), tc.field)
				}
				if got := rec.Header().Values("Vary"); len(got) != 1 || got[0] != tc.field {
					t.Errorf("%s: %s: Vary = %q, want exactly [%s]", tc.header, value, got, tc.field)
				}
			}
			rec := doHeader(t, app, http.MethodGet, "/users/me", tc.header, tc.none)
			assertStatus(t, rec, http.StatusNotFound)
			if !varyNamesField(rec.Header(), tc.field) {
				t.Errorf("no matching version: Vary = %q, want it to name %s", rec.Header().Values("Vary"), tc.field)
			}
			rec = doHeader(t, app, http.MethodHead, "/users/me", tc.header, tc.v1)
			assertStatus(t, rec, http.StatusOK)
			if !varyNamesField(rec.Header(), tc.field) {
				t.Errorf("HEAD: Vary = %q, want it to name %s", rec.Header().Values("Vary"), tc.field)
			}

			// A path only a version-neutral route answers is the same for
			// every request, so it does not vary.
			rec = doHeader(t, app, http.MethodGet, "/health", tc.header, tc.v1)
			assertStatus(t, rec, http.StatusOK)
			if varyNamesField(rec.Header(), tc.field) {
				t.Errorf("neutral route: Vary = %q, want nothing about %s", rec.Header().Values("Vary"), tc.field)
			}
		})
	}
}

// Path versioning puts the version in the URL a cache already keys on, and a
// custom extractor may read anything, so neither adds a Vary of its own.
func TestVersioningByPathOrExtractorAddsNoVary(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]VersioningOptions{
		"uri": {Type: VersioningURI},
		"custom": {Type: VersioningCustom, Extractor: func(r *http.Request) []string {
			return []string{r.URL.Query().Get("v")}
		}},
	} {
		options := quietOptions()
		options.Versioning = opts
		app := New(options)
		app.Get("/users/me", versionHandler("1"), WithVersion("1"))
		mustBuild(t, app)

		target := "/users/me?v=1"
		if name == "uri" {
			target = "/v1/users/me"
		}
		rec := do(t, app, http.MethodGet, target)
		assertStatus(t, rec, http.StatusOK)
		if got := rec.Header().Values("Vary"); len(got) != 0 {
			t.Errorf("%s: Vary = %q, want none", name, got)
		}
	}
}
