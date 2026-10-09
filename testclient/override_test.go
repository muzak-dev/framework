package testclient_test

import (
	"net/http"
	"sync"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// caller is the identity the application below resolves from a token, and
// the tests replace with a fake.
type caller struct{ Name string }

type whoIn struct {
	Caller muzak.Dep[caller]
}

type whoOut struct {
	Name string `json:"name"`
}

// buildIdentityApp resolves the caller from a token no test has, so that only
// an override lets a request through.
func buildIdentityApp() *muzak.App {
	app := muzak.New(muzak.AppOptions{
		Title:         "Override Example",
		Version:       "1.0.0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	}, muzak.Needs(func(*muzak.Context) (caller, error) {
		return caller{}, muzak.Unauthorized("no token")
	}))
	app.Get("/me", func(_ *muzak.Context, in whoIn) (whoOut, error) {
		return whoOut{Name: in.Caller.Get().Name}, nil
	})
	return app
}

func TestOverrideReplacesAProviderForTheTest(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildIdentityApp(),
		testclient.Override(func(*muzak.Context) (caller, error) { return caller{Name: "alice"}, nil }))
	res := client.Get("/me")
	res.AssertStatus(http.StatusOK)
	res.AssertJSON(`{"name":"alice"}`)

	// The same application built without the option still refuses.
	testclient.New(t, buildIdentityApp()).Get("/me").AssertStatus(http.StatusUnauthorized)
}

func TestOverrideAcquireReleasesAfterEachRequest(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var failures []error
	client := testclient.New(t, buildIdentityApp(),
		testclient.OverrideAcquire(func(*muzak.Context) (caller, muzak.Release, error) {
			return caller{Name: "bob"}, func(failure error) error {
				mu.Lock()
				defer mu.Unlock()
				failures = append(failures, failure)
				return nil
			}, nil
		}))
	client.Get("/me").AssertJSON(`{"name":"bob"}`)
	client.Get("/me").AssertJSON(`{"name":"bob"}`)

	mu.Lock()
	defer mu.Unlock()
	if len(failures) != 2 || failures[0] != nil || failures[1] != nil {
		t.Errorf("releases saw %v, want two successes", failures)
	}
}

func TestOverrideOfABuiltApplicationPanics(t *testing.T) {
	t.Parallel()
	app := buildIdentityApp()
	if err := app.Build(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("overriding a built application did not panic")
		}
	}()
	testclient.New(t, app, testclient.Override(func(*muzak.Context) (caller, error) { return caller{}, nil }))
}
