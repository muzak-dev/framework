package badele

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/textproto"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// The response headers a rate-limited route sets, following the shape the
// HTTP working group's RateLimit header fields draft settled on and that most
// clients already understand.
const (
	// HeaderRateLimitLimit reports the quota that is closest to being spent.
	HeaderRateLimitLimit = "RateLimit-Limit"
	// HeaderRateLimitRemaining reports how many requests are left in that
	// quota's current window.
	HeaderRateLimitRemaining = "RateLimit-Remaining"
	// HeaderRateLimitReset reports how many seconds remain before that window
	// starts again.
	HeaderRateLimitReset = "RateLimit-Reset"
	// HeaderRateLimitPolicy describes every quota the route enforces, as a
	// list of "limit;w=seconds" entries. It is fixed for a route, so a client
	// can learn the whole policy from one response.
	HeaderRateLimitPolicy = "RateLimit-Policy"
	// HeaderRetryAfter tells a refused client how long to wait, in seconds.
	HeaderRetryAfter = "Retry-After"
)

// The canonical spellings of the headers above, worked out once.
//
// net/http canonicalises every header name it is given, and a name that is not
// already canonical is rewritten, and so allocated, on every call. Doing it
// here instead means the per-request writes hand it a name it can use as it
// stands. The exported constants keep the specification's own spelling,
// because that is what a reader looking for them will search for.
var (
	canonicalRateLimitLimit     = textproto.CanonicalMIMEHeaderKey(HeaderRateLimitLimit)
	canonicalRateLimitRemaining = textproto.CanonicalMIMEHeaderKey(HeaderRateLimitRemaining)
	canonicalRateLimitReset     = textproto.CanonicalMIMEHeaderKey(HeaderRateLimitReset)
	canonicalRateLimitPolicy    = textproto.CanonicalMIMEHeaderKey(HeaderRateLimitPolicy)
	canonicalRetryAfter         = textproto.CanonicalMIMEHeaderKey(HeaderRetryAfter)
)

// maxRateLimitKey bounds the length of the key a tracker may return before it
// is replaced by a digest of itself.
//
// A tracker usually reads something the client sent, an API key or a tenant
// header, and a client that can choose the key can choose how much of it the
// storage has to hold. Hashing past this length keeps every key small without
// merging two clients into one budget the way truncation would.
const maxRateLimitKey = 256

// Quota is one rate limit: how many requests a client may make in a window.
//
// Several quotas describe a policy together, which is what tells a burst from
// sustained abuse. Three requests a second is generous for a person clicking
// and impossible for a script, while a hundred a minute is the reverse, so a
// policy that means "quick but not tireless" needs both:
//
//	Quotas: []badele.Quota{
//		{Name: "short", Window: time.Second, Limit: 3},
//		{Name: "medium", Window: 10 * time.Second, Limit: 20},
//		{Name: "long", Window: time.Minute, Limit: 100},
//	}
//
// Every quota in a policy is counted for every request, so a client that
// overruns the short window still accrues against the long one and cannot
// escape a sustained limit by pausing between bursts.
type Quota struct {
	// Name identifies the quota. It is the namespace its counters are stored
	// under, so two quotas that share a name share a budget and must agree on
	// their window and limit; the application refuses to build when they do
	// not. It must be a valid HTTP token, because it is reported to clients.
	Name string
	// Window is how long one counting period lasts. It must be positive.
	Window time.Duration
	// Limit is how many requests are allowed within one window. It must be
	// positive; a quota that allows nothing is a route that should not be
	// registered.
	Limit int
}

// RateLimitStorage counts requests.
//
// It is the whole of what the limiter needs from the outside world, and it is
// an interface because where the counters live is an operational decision:
// a single process is well served by the storage [NewMemoryRateLimitStorage]
// returns, while several processes behind a load balancer need something they
// share, which is usually whatever they already run.
//
// An implementation that talks to a shared store should do the increment and
// the expiry in one round trip, so that two requests arriving together cannot
// both create the window:
//
//	func (s *RedisRateLimitStorage) Increment(ctx context.Context, quota, key string, window time.Duration) (int, time.Duration, error) {
//		res, err := s.script.Run(ctx, s.client, []string{"ratelimit:" + quota + ":" + key}, window.Milliseconds()).Result()
//		// INCR, then PEXPIRE when the counter is new, then PTTL.
//	}
//
// If the implementation also satisfies [Lifecycle], the application starts it
// before serving and stops it after draining, so a pool or a sweeper needs no
// separate registration.
type RateLimitStorage interface {
	// Increment counts one request against a quota for one client and returns
	// the new count and how long the current window has left to run.
	//
	// The count includes the request being counted, so the first request in a
	// window returns one. The key is opaque and may contain any bytes; it is
	// derived from client-supplied data and must never be logged, because it
	// routinely carries an API key or a user identifier.
	//
	// The window is how long a newly created counter should live. An
	// implementation must not extend the life of a counter that already
	// exists, because a limit whose window restarts on every request is a
	// limit that never resets.
	Increment(ctx context.Context, quota, key string, window time.Duration) (count int, reset time.Duration, err error)
}

// RateLimitTracker decides whose budget a request is spent from.
//
// Returning an error abandons the request, and the error becomes the response
// exactly as one returned from a handler would, so a tracker that requires a
// credential can insist on one:
//
//	func APIKeyTracker(ctx *badele.Context) (string, error) {
//		key := ctx.Header("X-API-Key")
//		if key == "" {
//			return "", badele.NewHTTPError(http.StatusUnauthorized, "an API key is required")
//		}
//		return "apikey:" + key, nil
//	}
//
// The key must not be empty. Prefix keys that come from different sources
// differently, as the example above does, so that a user identifier and an
// address can never collide into one budget.
type RateLimitTracker func(ctx *Context) (string, error)

// IPTracker keys a rate limit on the client's address, and is what a policy
// that does not name a tracker uses.
//
// The address is the one [Context.ClientIP] resolves, so it is the peer's
// unless the application names a trusted proxy. A request whose address cannot
// be parsed is refused rather than counted anonymously, because counting every
// such request under one key would give them all a single shared budget.
func IPTracker(ctx *Context) (string, error) {
	ip := ctx.ClientIP()
	if ip == "" {
		return "", errRateLimitNoAddress
	}
	return "ip:" + ip, nil
}

// errRateLimitNoAddress reports a request that cannot be attributed to an
// address, which is a deployment [IPTracker] does not fit rather than
// something the client did.
var errRateLimitNoAddress = errors.New("badele: the rate limiter could not determine the client address; " +
	"a listener that is not addressed by IP needs a tracker of its own")

// RateLimitOptions configures rate limiting.
//
// Rate limiting is off until a policy declares a quota. It can then be set
// application-wide through [AppOptions.RateLimit] and narrowed for a router or
// a single route with [WithRateLimit] or [RateLimit], and turned off again for
// one route with [SkipRateLimit]. Layering works field by field, so a route
// that replaces the quotas keeps the application's storage and tracker.
type RateLimitOptions struct {
	// Quotas are the limits enforced, all of them, for every request. An empty
	// list turns rate limiting off, which is the default. A narrower scope
	// that declares quotas replaces the inherited list rather than adding to
	// it, so a route states the whole policy it wants.
	Quotas []Quota

	// Storage counts the requests, defaulting to a process-local storage
	// equivalent to [NewMemoryRateLimitStorage] with its own defaults. One
	// storage is created for the whole application, so routes that do not name
	// their own share its counters.
	//
	// A process-local storage means a per-process limit, which is a different
	// limit from the one intended as soon as there are two processes. Name a
	// shared storage for anything running more than once.
	Storage RateLimitStorage

	// Tracker decides whose budget a request is spent from, defaulting to
	// [IPTracker].
	Tracker RateLimitTracker

	// FailOpen serves a request that the storage could not count.
	//
	// By default a storage that cannot answer refuses the request with 503,
	// because a limiter that cannot count is a limiter that is not enforcing
	// anything, and an attacker who can reach the storage can choose the
	// moment it stops answering. Setting FailOpen trades that for
	// availability: a storage outage lets traffic through unmetered instead of
	// turning into an outage of its own. The failure is logged either way.
	FailOpen bool

	// DisableHeaders stops the RateLimit response headers from being set.
	// They are set by default, because a client that can see its own budget is
	// a client that can stay inside it.
	DisableHeaders bool

	// AfterDependencies counts the request after the route's guards and value
	// dependencies have run, rather than before, so that a tracker can key on
	// an identity a dependency produced:
	//
	//	func UserOrIPTracker(ctx *badele.Context) (string, error) {
	//		if user, ok := badele.TryFrom[CurrentUser](ctx); ok {
	//			return "user:" + user.ID, nil
	//		}
	//		return "ip:" + ctx.ClientIP(), nil
	//	}
	//
	// It is off by default, and the reason is worth understanding before
	// turning it on: a request rejected by a guard never reaches the limiter,
	// so a route that defers the count does not limit failed authentication at
	// all. That is exactly the traffic a login route needs to limit, so leave
	// it off there and let the tracker fall back to the address.
	AfterDependencies bool
}

// overlay layers a narrower scope's options on top of a wider one's, leaving
// whatever the narrower scope did not set alone.
func (o RateLimitOptions) overlay(over RateLimitOptions) RateLimitOptions {
	if over.Quotas != nil {
		o.Quotas = over.Quotas
	}
	if over.Storage != nil {
		o.Storage = over.Storage
	}
	if over.Tracker != nil {
		o.Tracker = over.Tracker
	}
	if over.FailOpen {
		o.FailOpen = true
	}
	if over.DisableHeaders {
		o.DisableHeaders = true
	}
	if over.AfterDependencies {
		o.AfterDependencies = true
	}
	return o
}

// WithRateLimit configures rate limiting for an application, a router or a
// single route.
//
//	app := badele.New(badele.AppOptions{Title: "Shop"},
//		badele.WithRateLimit(badele.RateLimitOptions{
//			Storage: NewRedisRateLimitStorage(settings.RedisAddr),
//			Tracker: UserOrIPTracker,
//			Quotas: []badele.Quota{
//				{Name: "short", Window: time.Second, Limit: 3},
//				{Name: "medium", Window: 10 * time.Second, Limit: 20},
//				{Name: "long", Window: time.Minute, Limit: 100},
//			},
//		}),
//	)
//
// Options layer field by field on top of [AppOptions.RateLimit] and on top of
// whatever an enclosing router declared, so a route can change the quotas
// without restating the storage. Use [RateLimit] for the common case of
// changing only the quotas, and [SkipRateLimit] to exempt a route.
func WithRateLimit(opts RateLimitOptions) SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.rateLimit = &opts },
		router: func(c *routerConfig) { c.rateLimit = &opts },
	}
}

// RateLimit replaces the quotas a route, or every route beneath a router,
// is limited by, keeping the storage and tracker it inherits.
//
// It is how one route is held to a stricter policy than the rest, which a
// login route needs whether or not the rest of the application is limited at
// all:
//
//	r.Post("/login", login,
//		badele.RateLimit(badele.Quota{Name: "login", Window: time.Minute, Limit: 5}))
//
// The quotas given replace those inherited rather than adding to them, so a
// route that wants both restates the ones it is keeping.
func RateLimit(quotas ...Quota) SharedOption {
	// A nil slice means "inherited" to overlay, so an explicit request for no
	// quotas is carried as an empty non-nil one and turns limiting off.
	if quotas == nil {
		quotas = []Quota{}
	}
	return WithRateLimit(RateLimitOptions{Quotas: quotas})
}

// SkipRateLimit exempts a route, or every route beneath a router, from rate
// limiting, including the message limits of a WebSocket route.
//
// It is what a health check wants, since a monitor polling every second is the
// one client that should never be told to slow down:
//
//	r.Get("/health", health, badele.SkipRateLimit())
//
// An exemption cannot be undone by a narrower scope: once a router is exempt,
// every route beneath it is.
func SkipRateLimit() SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.skipRateLimit = true },
		router: func(c *routerConfig) { c.skipRateLimit = true },
	}
}

// rateLimitConfig is a route's resolved rate limiting, with everything that
// can be worked out once already worked out.
type rateLimitConfig struct {
	quotas  []Quota
	storage RateLimitStorage
	tracker RateLimitTracker
	// policy is the fixed RateLimit-Policy header value for these quotas.
	policy            string
	failOpen          bool
	headers           bool
	afterDependencies bool
}

// resolveRateLimit works out a route's rate limiting from what it inherits and
// what it declares, and reports a policy that cannot be enforced.
//
// The resolved options are kept whether or not any quota was declared, because
// a WebSocket route's message limits are counted with the same storage and
// tracker as its requests would be.
func (rt *Route) resolveRateLimit(in inherited) error {
	opts := in.rateLimit
	if rt.cfg.rateLimit != nil {
		opts = opts.overlay(*rt.cfg.rateLimit)
	}
	if opts.Tracker == nil {
		opts.Tracker = IPTracker
	}
	rt.rateLimitOpts = opts
	rt.skipRateLimit = in.skipRateLimit || rt.cfg.skipRateLimit
	if rt.skipRateLimit || len(opts.Quotas) == 0 {
		return nil
	}
	cfg, err := newRateLimitConfig(opts, opts.Quotas)
	if err != nil {
		return fmt.Errorf("badele: %s %s: %w", rt.Method, rt.Path, err)
	}
	rt.rateLimit = cfg
	rt.responses = append(rt.responses, responseDoc{
		code:        http.StatusTooManyRequests,
		description: "The client has made too many requests and must wait before making another.",
	})
	return nil
}

// newRateLimitConfig validates a set of quotas and precomputes what every
// request would otherwise have to work out again.
func newRateLimitConfig(opts RateLimitOptions, quotas []Quota) (*rateLimitConfig, error) {
	seen := make(map[string]struct{}, len(quotas))
	var policy strings.Builder
	for _, quota := range quotas {
		if quota.Name == "" {
			return nil, errors.New("a quota needs a name, because the name is the namespace its counters are stored under")
		}
		if !isHTTPToken(quota.Name) {
			// The name is reported to clients and stored as part of a key, so
			// one carrying a separator or a control character is refused here
			// rather than escaped later.
			return nil, fmt.Errorf("quota name %q is not a valid token", quota.Name)
		}
		if _, taken := seen[quota.Name]; taken {
			return nil, fmt.Errorf("quota %q is declared twice in one policy", quota.Name)
		}
		seen[quota.Name] = struct{}{}
		if quota.Window <= 0 {
			return nil, fmt.Errorf("quota %q needs a positive window, not %s", quota.Name, quota.Window)
		}
		if quota.Limit <= 0 {
			return nil, fmt.Errorf("quota %q needs a positive limit, not %d; a route that allows nothing should not be registered",
				quota.Name, quota.Limit)
		}
		if policy.Len() > 0 {
			policy.WriteString(", ")
		}
		policy.WriteString(strconv.Itoa(quota.Limit))
		policy.WriteString(";w=")
		policy.WriteString(strconv.Itoa(windowSeconds(quota.Window)))
	}
	return &rateLimitConfig{
		quotas:            quotas,
		storage:           opts.Storage,
		tracker:           opts.Tracker,
		policy:            policy.String(),
		failOpen:          opts.FailOpen,
		headers:           !opts.DisableHeaders,
		afterDependencies: opts.AfterDependencies,
	}, nil
}

// windowSeconds renders a window for the policy header, where a window shorter
// than a second still has to be described as one.
func windowSeconds(window time.Duration) int {
	if seconds := int(window / time.Second); seconds > 0 {
		return seconds
	}
	return 1
}

// outcome is what one quota said about one request.
type outcome struct {
	quota Quota
	count int
	reset time.Duration
}

// remaining reports how much of the quota is left, never going below zero
// however far past the limit the count has run.
func (o outcome) remaining() int {
	if left := o.quota.Limit - o.count; left > 0 {
		return left
	}
	return 0
}

// exceeded reports whether this request was over the quota.
func (o outcome) exceeded() bool { return o.count > o.quota.Limit }

// key resolves whose budget the request is spent from, bounding the result so
// that a client cannot choose how much the storage has to hold.
func (cfg *rateLimitConfig) key(c *Context) (string, error) {
	key, err := cfg.tracker(c)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", errRateLimitEmptyKey
	}
	return boundRateLimitKey(key), nil
}

// errRateLimitEmptyKey reports a tracker that returned nothing, which would
// put every client that reached it into a single shared budget.
var errRateLimitEmptyKey = errors.New("badele: the rate limit tracker returned an empty key; " +
	"return an error instead of an empty key for a request the tracker cannot attribute")

// boundRateLimitKey replaces an over-long key with a digest of itself.
//
// Hashing rather than truncating is what keeps two clients with a long shared
// prefix, which is what an API key format usually produces, from being counted
// as one.
func boundRateLimitKey(key string) string {
	if len(key) <= maxRateLimitKey {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// check counts a request against every quota and reports whether it may
// proceed, setting the RateLimit headers on the way.
func (cfg *rateLimitConfig) check(c *Context) error {
	key, err := cfg.key(c)
	if err != nil {
		return err
	}
	ctx := c.Context()

	var worst outcome
	var tightest outcome
	for i, quota := range cfg.quotas {
		count, reset, storageErr := cfg.storage.Increment(ctx, quota.Name, key, quota.Window)
		if storageErr != nil {
			return cfg.storageFailed(c, quota, storageErr)
		}
		current := outcome{quota: quota, count: count, reset: reset}
		if i == 0 || current.remaining() < tightest.remaining() ||
			(current.remaining() == tightest.remaining() && current.reset > tightest.reset) {
			tightest = current
		}
		if current.exceeded() && (!worst.exceeded() || current.reset > worst.reset) {
			worst = current
		}
	}

	if worst.exceeded() {
		cfg.setHeaders(c, worst)
		// Retry-After is set even where the RateLimit headers are turned off.
		// Refusing a client without telling it when to come back is what
		// produces a client that comes back immediately, forever.
		retry := max(resetSeconds(worst.reset), 1)
		c.w.Header().Set(canonicalRetryAfter, strconv.Itoa(retry))
		return NewHTTPErrorf(http.StatusTooManyRequests,
			"the %q rate limit of %d requests per %d seconds has been exceeded; retry in %d seconds",
			worst.quota.Name, worst.quota.Limit, windowSeconds(worst.quota.Window), retry)
	}
	cfg.setHeaders(c, tightest)
	return nil
}

// storageFailed decides what to do about a storage that could not count, and
// records why without recording the key, which carries client-supplied data.
func (cfg *rateLimitConfig) storageFailed(c *Context, quota Quota, err error) error {
	if cfg.failOpen {
		c.logger.WarnContext(c.Context(), "badele: the rate limit storage failed; serving the request unmetered",
			slog.String("quota", quota.Name),
			slog.String(RequestIDKey, c.RequestID()),
			slog.String("error", err.Error()))
		return nil
	}
	// The cause is attached rather than described, so that it reaches the log
	// through the error pipeline and never reaches the client.
	return NewHTTPError(http.StatusServiceUnavailable,
		"the request could not be rate limited and was refused; try again shortly").Wrap(err)
}

// setHeaders reports one quota's state to the client.
func (cfg *rateLimitConfig) setHeaders(c *Context, state outcome) {
	if !cfg.headers || c.w.written {
		return
	}
	header := c.w.Header()
	header.Set(canonicalRateLimitPolicy, cfg.policy)
	header.Set(canonicalRateLimitLimit, strconv.Itoa(state.quota.Limit))
	header.Set(canonicalRateLimitRemaining, strconv.Itoa(state.remaining()))
	header.Set(canonicalRateLimitReset, strconv.Itoa(resetSeconds(state.reset)))
}

// resetSeconds rounds a window's remaining time up to whole seconds, which is
// the only unit the headers have. A window with any time left rounds up to one
// rather than down to zero, because zero invites a client to retry at once.
func resetSeconds(reset time.Duration) int {
	if reset <= 0 {
		return 0
	}
	return int((reset + time.Second - 1) / time.Second)
}

// resolveRateLimiting completes the rate limiting of every route once the
// whole routing tree is known.
//
// Two things can only be settled here. A quota name is the namespace its
// counters live in, so the same name used with two different windows anywhere
// in the application is a counter whose meaning depends on which route reached
// it first. And a storage that no policy named has to be created once and
// shared, rather than once per route.
func (a *App) resolveRateLimiting(state *buildState) {
	declared := make(map[string]Quota)
	var storages []RateLimitStorage
	var shared RateLimitStorage

	for _, rt := range a.routes {
		for _, cfg := range rt.rateLimiters() {
			for _, quota := range cfg.quotas {
				previous, seen := declared[quota.Name]
				if seen && (previous.Window != quota.Window || previous.Limit != quota.Limit) {
					state.errs = append(state.errs, fmt.Errorf(
						"badele: %s %s: quota %q is declared as %d requests per %s here and as %d per %s elsewhere; "+
							"counters are stored under the quota name, so one name cannot mean two policies",
						rt.Method, rt.Path, quota.Name, quota.Limit, quota.Window, previous.Limit, previous.Window))
					continue
				}
				declared[quota.Name] = quota
			}
			if cfg.storage == nil {
				if shared == nil {
					shared = NewMemoryRateLimitStorage(MemoryRateLimitOptions{})
				}
				cfg.storage = shared
			}
			if !containsStorage(storages, cfg.storage) {
				storages = append(storages, cfg.storage)
			}
		}
	}

	// A storage that manages something, a sweeper or a connection pool, is
	// brought up with the application rather than left to the first request
	// that needs it.
	for _, storage := range storages {
		if managed, ok := storage.(Lifecycle); ok {
			state.lifecycles = append(state.lifecycles, managed)
		}
	}
}

// rateLimiters returns every resolved policy attached to a route.
func (rt *Route) rateLimiters() []*rateLimitConfig {
	var configs []*rateLimitConfig
	if rt.rateLimit != nil {
		configs = append(configs, rt.rateLimit)
	}
	return configs
}

// containsStorage reports whether a storage is already in the list.
//
// Comparing interface values directly would panic for a storage whose concrete
// type is not comparable, so the comparison is only made when it is safe;
// every storage worth sharing is a pointer, and one that is not is simply
// registered again.
func containsStorage(storages []RateLimitStorage, candidate RateLimitStorage) bool {
	candidateType := reflect.TypeOf(candidate)
	if candidateType == nil || !candidateType.Comparable() {
		// An uncomparable storage cannot be recognised as one already seen, so
		// it is reported as new and registered again. That is the cost of
		// never letting the comparison below panic.
		return false
	}
	for _, storage := range storages {
		if reflect.TypeOf(storage) == candidateType && storage == candidate {
			return true
		}
	}
	return false
}
