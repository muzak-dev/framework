package muzak

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestHealthProbesPassTheHostAllowlistAndTheHTTPSRedirect is the regression
// test for probes refused by the edge checks. A platform probes a replica by
// its own address over plain HTTP, so with AllowedHosts every probe was
// answered 421 and with RedirectHTTPS 301, and the platform took every replica
// out of rotation. The probes are exempt; nothing else is.
func TestHealthProbesPassTheHostAllowlistAndTheHTTPSRedirect(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.AllowedHosts = []string{"example.com"}
	opts.RedirectHTTPS = &RedirectHTTPSOptions{}
	opts.Health = HealthOptions{Enabled: true}
	app := New(opts)
	app.Get("/hello", okHandler)
	mustBuild(t, app)
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.StopLifecycle(context.Background()) })

	for _, path := range []string{DefaultLivenessPath, DefaultReadinessPath} {
		res := doRequest(t, app, requestFor(http.MethodGet, path, "10.1.2.3:8080"))
		assertStatus(t, res, http.StatusOK)
		if strings.Contains(res.Body.String(), "misdirected") {
			t.Errorf("%s answered as a refusal: %s", path, res.Body.String())
		}
	}

	// Everything else is still held to both checks.
	assertStatus(t, doRequest(t, app, requestFor(http.MethodGet, "/hello", "10.1.2.3:8080")), http.StatusMisdirectedRequest)
	res := doRequest(t, app, requestFor(http.MethodGet, "/hello", "example.com"))
	assertStatus(t, res, http.StatusMovedPermanently)
	if loc := res.Header().Get("Location"); loc != "https://example.com/hello" {
		t.Errorf("Location = %q", loc)
	}
	// A path that merely starts like a probe is not one.
	assertStatus(t, doRequest(t, app, requestFor(http.MethodGet, DefaultLivenessPath+"x", "10.1.2.3")), http.StatusMisdirectedRequest)
}

// TestHealthPathsAndHandlerMounts holds the health paths to the mounts the way
// they are held to routes: a mount that would answer a probe path is a build
// error, except a mount at the root, which answers only what nothing else does,
// as a frontend at the root does, and so loses nothing to a probe answered
// ahead of routing.
func TestHealthPathsAndHandlerMounts(t *testing.T) {
	t.Parallel()
	legacy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("legacy")) })

	opts := quietOptions()
	opts.Health = HealthOptions{Enabled: true}
	app := New(opts)
	app.Mount("/", legacy)
	mustBuild(t, app)
	if err := app.StartLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.StopLifecycle(context.Background()) })
	if res := do(t, app, http.MethodGet, DefaultLivenessPath, ""); res.Code != http.StatusOK || strings.Contains(res.Body.String(), "legacy") {
		t.Errorf("the probe was answered %d %q, want the health endpoint", res.Code, res.Body.String())
	}
	if res := do(t, app, http.MethodGet, "/anything", ""); res.Body.String() != "legacy" {
		t.Errorf("the root mount lost a path to the health endpoints: %q", res.Body.String())
	}

	for _, prefix := range []string{"/livez", "/readyz"} {
		app := New(opts)
		app.Mount(prefix, legacy)
		err := app.Build()
		if err == nil || !strings.Contains(err.Error(), "handler mounted at") {
			t.Errorf("a mount at %s built beside the health endpoints: %v", prefix, err)
		}
	}
}
