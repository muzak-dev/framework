package muzak_test

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	muzak "muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// The quota resolver exists for limits an application cannot know when it is
// built: a customer's plan, a negotiated ceiling, a tier read from a database.

type resolverOut struct {
	OK bool `json:"ok"`
}

func resolverApp(t *testing.T, opts muzak.RateLimitOptions) *muzak.App {
	t.Helper()

	app := muzak.New(muzak.AppOptions{
		Title: "Resolver", Version: "1.0.0", Addr: ":0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	},
		muzak.WithRateLimit(opts),
	)
	app.Get("/thing", func(ctx *muzak.Context, _ muzak.Empty) (resolverOut, error) {
		return resolverOut{OK: true}, nil
	})
	return app
}

func fixedTracker(key string) muzak.RateLimitTracker {
	return func(*muzak.Context) (string, error) { return key, nil }
}

func TestQuotaResolverEnforcesResolvedQuotas(t *testing.T) {
	app := resolverApp(t, muzak.RateLimitOptions{
		Tracker: fixedTracker("client"),
		Resolver: func(*muzak.Context) ([]muzak.Quota, error) {
			return []muzak.Quota{{Name: "plan", Window: time.Minute, Limit: 2}}, nil
		},
	})
	client := testclient.New(t, app)

	client.Get("/thing").AssertStatus(http.StatusOK)
	client.Get("/thing").AssertStatus(http.StatusOK)
	client.Get("/thing").AssertStatus(http.StatusTooManyRequests)
}

// A resolver alone turns limiting on, even with no static quota declared: what
// is enforced is then a run time question.
func TestQuotaResolverAloneEnablesLimiting(t *testing.T) {
	var calls atomic.Int64

	app := resolverApp(t, muzak.RateLimitOptions{
		Tracker: fixedTracker("client"),
		Resolver: func(*muzak.Context) ([]muzak.Quota, error) {
			calls.Add(1)
			return []muzak.Quota{{Name: "only", Window: time.Minute, Limit: 1}}, nil
		},
	})
	client := testclient.New(t, app)

	client.Get("/thing").AssertStatus(http.StatusOK)
	client.Get("/thing").AssertStatus(http.StatusTooManyRequests)

	if calls.Load() != 2 {
		t.Errorf("the resolver ran %d times, want once per request", calls.Load())
	}
}

// Returning nothing leaves the request unlimited, which is what an
// unauthenticated request reaching a plan-based route should do.
func TestQuotaResolverReturningNothingEnforcesNothing(t *testing.T) {
	app := resolverApp(t, muzak.RateLimitOptions{
		Tracker:  fixedTracker("client"),
		Resolver: func(*muzak.Context) ([]muzak.Quota, error) { return nil, nil },
	})
	client := testclient.New(t, app)

	for range 20 {
		client.Get("/thing").AssertStatus(http.StatusOK)
	}
}

// A static quota is the floor everyone shares; the resolver adds the ceiling
// that varies. Both are counted.
func TestQuotaResolverAddsToStaticQuotas(t *testing.T) {
	app := resolverApp(t, muzak.RateLimitOptions{
		Tracker: fixedTracker("client"),
		Quotas:  []muzak.Quota{{Name: "floor", Window: time.Minute, Limit: 100}},
		Resolver: func(*muzak.Context) ([]muzak.Quota, error) {
			return []muzak.Quota{{Name: "ceiling", Window: time.Minute, Limit: 1}}, nil
		},
	})
	client := testclient.New(t, app)

	first := client.Get("/thing").AssertStatus(http.StatusOK)
	if policy := first.Header.Get("RateLimit-Policy"); policy != "100;w=60, 1;w=60" {
		t.Errorf("RateLimit-Policy = %q, want both quotas described", policy)
	}

	// The tighter of the two rejects, and the headers describe it.
	limited := client.Get("/thing").AssertStatus(http.StatusTooManyRequests)
	if limit := limited.Header.Get("RateLimit-Limit"); limit != "1" {
		t.Errorf("RateLimit-Limit = %q, want the quota that rejected", limit)
	}
	if limited.Header.Get("Retry-After") == "" {
		t.Error("Retry-After was not set")
	}
}

func TestQuotaResolverErrorBecomesTheResponse(t *testing.T) {
	app := resolverApp(t, muzak.RateLimitOptions{
		Tracker: fixedTracker("client"),
		Resolver: func(*muzak.Context) ([]muzak.Quota, error) {
			return nil, muzak.PaymentRequired("this account has no plan")
		},
	})

	testclient.New(t, app).Get("/thing").
		AssertStatus(http.StatusPaymentRequired).
		AssertErrorCode("payment_required")
}

// A quota built at run time cannot be validated at build time, so it is checked
// where it arrives rather than counted under an empty name or a zero window.
func TestQuotaResolverRejectsAnUnusableQuota(t *testing.T) {
	tests := []struct {
		name  string
		quota muzak.Quota
	}{
		{name: "no name", quota: muzak.Quota{Window: time.Minute, Limit: 1}},
		{name: "no window", quota: muzak.Quota{Name: "q", Limit: 1}},
		{name: "no limit", quota: muzak.Quota{Name: "q", Window: time.Minute}},
		{name: "negative limit", quota: muzak.Quota{Name: "q", Window: time.Minute, Limit: -1}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := resolverApp(t, muzak.RateLimitOptions{
				Tracker: fixedTracker("client"),
				Resolver: func(*muzak.Context) ([]muzak.Quota, error) {
					return []muzak.Quota{test.quota}, nil
				},
			})
			testclient.New(t, app).Get("/thing").AssertStatus(http.StatusInternalServerError)
		})
	}
}

// The resolver is per route like every other rate limit option, and layers the
// same way.
func TestQuotaResolverLayersPerRoute(t *testing.T) {
	app := muzak.New(muzak.AppOptions{
		Title: "Layered", Version: "1.0.0", Addr: ":0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	},
		muzak.WithRateLimit(muzak.RateLimitOptions{Tracker: fixedTracker("client")}),
	)

	app.Get("/loose", func(ctx *muzak.Context, _ muzak.Empty) (resolverOut, error) {
		return resolverOut{OK: true}, nil
	})
	app.Get("/tight", func(ctx *muzak.Context, _ muzak.Empty) (resolverOut, error) {
		return resolverOut{OK: true}, nil
	}, muzak.WithRateLimit(muzak.RateLimitOptions{
		Resolver: func(*muzak.Context) ([]muzak.Quota, error) {
			return []muzak.Quota{{Name: "tight", Window: time.Minute, Limit: 1}}, nil
		},
	}))

	client := testclient.New(t, app)

	// The route with no policy is unlimited.
	for range 5 {
		client.Get("/loose").AssertStatus(http.StatusOK)
	}

	client.Get("/tight").AssertStatus(http.StatusOK)
	client.Get("/tight").AssertStatus(http.StatusTooManyRequests)
}

// A resolver reading an identity a dependency produced needs the dependency to
// have run, which is what AfterDependencies is for.
func TestQuotaResolverAfterDependencies(t *testing.T) {
	type plan struct{ limit int }

	app := muzak.New(muzak.AppOptions{
		Title: "Deps", Version: "1.0.0", Addr: ":0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	})

	app.Get("/thing", func(ctx *muzak.Context, _ muzak.Empty) (resolverOut, error) {
		return resolverOut{OK: true}, nil
	},
		muzak.Needs(func(*muzak.Context) (plan, error) { return plan{limit: 1}, nil }),
		muzak.WithRateLimit(muzak.RateLimitOptions{
			Tracker:           fixedTracker("client"),
			AfterDependencies: true,
			Resolver: func(ctx *muzak.Context) ([]muzak.Quota, error) {
				resolved, ok := muzak.TryFrom[plan](ctx)
				if !ok {
					return nil, errors.New("the plan dependency had not run")
				}
				return []muzak.Quota{{Name: "byplan", Window: time.Minute, Limit: resolved.limit}}, nil
			},
		}),
	)

	client := testclient.New(t, app)
	client.Get("/thing").AssertStatus(http.StatusOK)
	client.Get("/thing").AssertStatus(http.StatusTooManyRequests)
}

// A resolver's quotas are held to the same rules as static ones, because where
// a quota came from decides nothing about how its name is stored: a colon or a
// NUL in it collides with another quota's storage key, and a name used twice in
// one policy counts every request once per copy against one counter.
func TestQuotaResolverRejectsAnUnstorableOrRepeatedName(t *testing.T) {
	q := func(name string) muzak.Quota { return muzak.Quota{Name: name, Window: time.Minute, Limit: 2} }
	tests := []struct {
		name   string
		static []muzak.Quota
		quotas []muzak.Quota
	}{
		{name: "a colon", quotas: []muzak.Quota{q("plan:pro")}},
		{name: "a NUL", quotas: []muzak.Quota{q("plan\x00pro")}},
		{name: "a space", quotas: []muzak.Quota{q("plan pro")}},
		{name: "a repeated name", quotas: []muzak.Quota{q("plan"), q("plan")}},
		{name: "a name a static quota has", static: []muzak.Quota{q("plan")}, quotas: []muzak.Quota{q("plan")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := resolverApp(t, muzak.RateLimitOptions{
				Tracker: fixedTracker("client"),
				Quotas:  test.static,
				Resolver: func(*muzak.Context) ([]muzak.Quota, error) {
					return test.quotas, nil
				},
			})
			testclient.New(t, app).Get("/thing").AssertStatus(http.StatusInternalServerError)
		})
	}
}

// Names that are valid, distinct and alongside a static quota count normally,
// each request once per quota.
func TestQuotaResolverAcceptsDistinctTokenNames(t *testing.T) {
	app := resolverApp(t, muzak.RateLimitOptions{
		Tracker: fixedTracker("client"),
		Quotas:  []muzak.Quota{{Name: "floor", Window: time.Minute, Limit: 10}},
		Resolver: func(*muzak.Context) ([]muzak.Quota, error) {
			return []muzak.Quota{
				{Name: "plan-pro", Window: time.Minute, Limit: 2},
				{Name: "burst.1", Window: time.Minute, Limit: 5},
			}, nil
		},
	})
	client := testclient.New(t, app)
	client.Get("/thing").AssertStatus(http.StatusOK)
	client.Get("/thing").AssertStatus(http.StatusOK)
	client.Get("/thing").AssertStatus(http.StatusTooManyRequests)
}
