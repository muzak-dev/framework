package muzak

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults of [JWKSOptions].
const (
	// DefaultJWKSMaxBytes is the largest key set document read, at one
	// mebibyte. A key set of a dozen RSA keys is a few kilobytes.
	DefaultJWKSMaxBytes int64 = 1 << 20
	// DefaultJWKSMaxKeys is the most keys a key set may hold. A document with
	// more is refused whole, since dropping some would drop them silently.
	DefaultJWKSMaxKeys = 32
	// DefaultJWKSTimeout bounds one fetch of the key set.
	DefaultJWKSTimeout = 10 * time.Second
	// DefaultJWKSMinRefreshInterval is the least time between two fetches
	// made because a token named a key that is not in the set.
	DefaultJWKSMinRefreshInterval = 30 * time.Second
	// DefaultJWKSMinCacheTTL and DefaultJWKSMaxCacheTTL bound how long a key
	// set is kept, whatever its Cache-Control says.
	DefaultJWKSMinCacheTTL = 5 * time.Minute
	DefaultJWKSMaxCacheTTL = 24 * time.Hour
	// defaultJWKSCacheTTL is how long a key set whose response carries no
	// max-age is kept, within the bounds.
	defaultJWKSCacheTTL = time.Hour
	// maxJWKSKeysCeiling bounds [JWKSOptions.MaxKeys], since every key a token
	// without a kid could match is one more signature checked.
	maxJWKSKeysCeiling = 1024
	// maxJWKSRetryDelay is the longest the background refresh waits between
	// attempts while fetches keep failing.
	maxJWKSRetryDelay = 5 * time.Minute
)

// HTTPDoer sends an HTTP request. Both *[Client] and *http.Client satisfy it,
// which is how [JWKSOptions] accepts either.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// JWKSOptions fetches a [JWTBearer] scheme's keys from the issuer's JSON Web
// Key Set and keeps them fresh. Every bound has a default; set a field only to
// change it.
//
// The set is fetched when the first token needs it, and in the background
// once the application's lifecycle has started, before the cached copy
// expires. A token naming a kid the set does not hold makes it fetched again,
// in case the issuer rotated its keys, but at most once per
// MinRefreshInterval however many such tokens arrive, and every request
// waiting for the same fetch shares it. A fetch that fails keeps the last set
// that was fetched; until one has succeeded, a token is answered with 503.
type JWKSOptions struct {
	// URL is where the key set is published, often found as jwks_uri in the
	// issuer's OpenID Connect discovery document. It must be https, except
	// for a loopback address in a test, and so must any URL the client is
	// redirected to: a key set that arrives over plain http is refused.
	URL string

	// Client fetches the key set. Nil builds one with [NewClient] and
	// [ClientOptions] left at their defaults, which refuses private, loopback
	// and link-local addresses. An identity provider on a private network,
	// such as a Keycloak beside the service, needs a client that may reach
	// it: muzak.NewClient(muzak.ClientOptions{AllowPrivateNetworks: true}),
	// or AllowedNetworks naming its subnet. An *http.Client is accepted too,
	// and then decides on its own where it may connect. The size limit and
	// the timeout below apply whichever client is used.
	Client HTTPDoer

	// MaxBytes is the largest key set document read, defaulting to
	// [DefaultJWKSMaxBytes]. A larger one is refused, and the last set kept.
	MaxBytes int64

	// MaxKeys is the most keys a document may hold, defaulting to
	// [DefaultJWKSMaxKeys] and at most 1024. A document with more is refused.
	MaxKeys int

	// Timeout bounds one fetch, defaulting to [DefaultJWKSTimeout]. A request
	// waiting for a fetch also stops waiting when its own context ends.
	Timeout time.Duration

	// MinRefreshInterval is the least time between two fetches, defaulting to
	// [DefaultJWKSMinRefreshInterval]. It is what keeps tokens naming random
	// kids from becoming a flood of fetches sent to the issuer.
	MinRefreshInterval time.Duration

	// MinCacheTTL and MaxCacheTTL bound how long a fetched set is used before
	// it is fetched again, defaulting to [DefaultJWKSMinCacheTTL] and
	// [DefaultJWKSMaxCacheTTL]. Within them the response's Cache-Control
	// max-age decides, and a response without one is kept for an hour.
	MinCacheTTL time.Duration
	MaxCacheTTL time.Duration
}

// configured reports whether any field is set. It compares field by field,
// since a Client of a type that cannot be compared would make comparing the
// struct panic.
func (o JWKSOptions) configured() bool {
	return o.URL != "" || o.Client != nil || o.MaxBytes != 0 || o.MaxKeys != 0 || o.Timeout != 0 ||
		o.MinRefreshInterval != 0 || o.MinCacheTTL != 0 || o.MaxCacheTTL != 0
}

// normalize fills in the defaults and reports every value that cannot be used.
func (o JWKSOptions) normalize() (JWKSOptions, []error) {
	var errs []error
	if err := checkJWKSURL(o.URL); err != nil {
		errs = append(errs, err)
	}
	fill := func(field string, value *time.Duration, fallback time.Duration) {
		switch {
		case *value == 0:
			*value = fallback
		case *value < 0:
			errs = append(errs, fmt.Errorf("JWTOptions.JWKS.%s is %s; it must not be negative", field, *value))
		}
	}
	fill("Timeout", &o.Timeout, DefaultJWKSTimeout)
	fill("MinRefreshInterval", &o.MinRefreshInterval, DefaultJWKSMinRefreshInterval)
	fill("MinCacheTTL", &o.MinCacheTTL, DefaultJWKSMinCacheTTL)
	fill("MaxCacheTTL", &o.MaxCacheTTL, DefaultJWKSMaxCacheTTL)
	if o.MinCacheTTL > o.MaxCacheTTL {
		errs = append(errs, fmt.Errorf("JWTOptions.JWKS.MinCacheTTL (%s) is longer than MaxCacheTTL (%s)", o.MinCacheTTL, o.MaxCacheTTL))
	}
	switch {
	case o.MaxBytes == 0:
		o.MaxBytes = DefaultJWKSMaxBytes
	case o.MaxBytes < 0:
		errs = append(errs, fmt.Errorf("JWTOptions.JWKS.MaxBytes is %d; it must not be negative", o.MaxBytes))
	}
	switch {
	case o.MaxKeys == 0:
		o.MaxKeys = DefaultJWKSMaxKeys
	case o.MaxKeys < 0 || o.MaxKeys > maxJWKSKeysCeiling:
		errs = append(errs, fmt.Errorf("JWTOptions.JWKS.MaxKeys is %d; it must be between 1 and %d", o.MaxKeys, maxJWKSKeysCeiling))
	}
	return o, errs
}

// checkJWKSURL reports a key set URL that is not https, or that is plain
// http to anything but a loopback address. Keys fetched in the clear are keys
// anyone on the path could have replaced.
func checkJWKSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("JWTOptions.JWKS.URL must be an absolute https URL with a host, no credentials and no fragment")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
	}
	return errors.New("JWTOptions.JWKS.URL must be https; plain http is accepted only for a loopback address, in a test")
}

// isLoopbackHost reports whether a host names this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// jwksSnapshot is one key set fetched, and when it should be fetched again.
// It is immutable once published.
type jwksSnapshot struct {
	keys    *keySet
	fetched time.Time
	expires time.Time
}

// jwksFetch is one fetch in flight, which every request that needs it waits
// on rather than starting one of its own.
type jwksFetch struct {
	done chan struct{}
}

// jwksCache is one application's copy of a scheme's key set: fetched, cached,
// refreshed in the background while the application runs, and fetched again
// when a token names a key it does not hold. It is the [Lifecycle] component
// the application starts and stops.
type jwksCache struct {
	name   string
	opts   JWKSOptions
	client HTTPDoer
	// owned is the client built here when none was configured, whose idle
	// connections are closed when the application stops.
	owned   *Client
	allowed map[string]*jwsAlgorithm
	logger  *slog.Logger
	now     func() time.Time

	// current is the last key set fetched, nil until one has been. Reading it
	// takes no lock, so verifying a token never waits on a fetch it does not
	// need.
	current atomic.Pointer[jwksSnapshot]
	// fetches counts the fetches started, for the tests that bound it.
	fetches atomic.Int64

	mu          sync.Mutex
	inflight    *jwksFetch
	lastAttempt time.Time
	failures    int
	// base is what a fetch's context is derived from: the background while
	// the application is not running, and the lifecycle's context while it
	// is, so that stopping the application cancels a fetch in flight.
	base     context.Context
	cancel   context.CancelFunc
	loopDone chan struct{}
}

// newJWKSCache builds an application's cache for a scheme's key set.
func newJWKSCache(name string, spec *jwtSpec, logger *slog.Logger) *jwksCache {
	j := &jwksCache{
		name:    name,
		opts:    *spec.jwks,
		client:  spec.jwks.Client,
		allowed: spec.policy.algorithms,
		logger:  logger,
		now:     spec.now,
		base:    context.Background(),
	}
	if j.client == nil {
		j.owned = NewClient(ClientOptions{Timeout: j.opts.Timeout, MaxResponseBytes: j.opts.MaxBytes})
		j.client = j.owned
	}
	return j
}

// Name implements [Lifecycle].
func (j *jwksCache) Name() string { return "jwks " + j.name }

// Start implements [Lifecycle]. It starts the background refresh, which
// fetches the key set at once and then again before each copy expires. It does
// not wait for the first fetch, and never fails: an issuer that is down when
// the application starts is not a reason for the application not to, and the
// tokens that arrive meanwhile are answered with 503 until a fetch succeeds.
func (j *jwksCache) Start(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancel != nil {
		return nil
	}
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	j.base, j.cancel = base, cancel
	done := make(chan struct{})
	j.loopDone = done
	go j.loop(base, done)
	return nil
}

// Stop implements [Lifecycle]. It stops the background refresh, cancels a
// fetch in flight, and waits for the refresh to end or for ctx to.
func (j *jwksCache) Stop(ctx context.Context) error {
	j.mu.Lock()
	cancel, done := j.cancel, j.loopDone
	j.cancel, j.loopDone, j.base = nil, nil, context.Background()
	j.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("muzak: the key set refresh of scheme %q did not stop in time: %w", j.name, ctx.Err())
	}
	if j.owned != nil {
		_ = j.owned.Close()
	}
	return nil
}

// loop is the background refresh: it fetches whenever the cached set is due
// to be refreshed, and sleeps until then.
func (j *jwksCache) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		if j.due() {
			j.backgroundRefresh(ctx)
		}
		timer := time.NewTimer(j.nextRefresh())
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// backgroundRefresh fetches the set on the background goroutine. A panic
// there, from a client the application supplied, would end the process, since
// no request is around to recover it; it is logged instead and counted as a
// failed fetch, and the refresh goes on.
func (j *jwksCache) backgroundRefresh(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			j.logger.Error("muzak: the key set refresh panicked; the last set fetched is kept",
				slog.String("scheme", j.name), slog.String("panic", fmt.Sprint(recovered)),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	j.refresh(ctx, false)
}

// due reports whether the background refresh should fetch now: when no set
// has been fetched, or when the cached one is in the last tenth of its life.
func (j *jwksCache) due() bool {
	snap := j.current.Load()
	if snap == nil {
		return true
	}
	return !j.now().Before(j.refreshAt(snap))
}

// refreshAt is when the background refresh replaces a set: a tenth of its
// life before it expires, so that a token never waits for one.
func (j *jwksCache) refreshAt(snap *jwksSnapshot) time.Time {
	return snap.expires.Add(-snap.expires.Sub(snap.fetched) / 10)
}

// nextRefresh is how long the background refresh sleeps: until the set is
// due, or, while fetches fail, a delay that doubles from MinRefreshInterval up
// to five minutes, never less than MinRefreshInterval.
func (j *jwksCache) nextRefresh() time.Duration {
	j.mu.Lock()
	failures := j.failures
	j.mu.Unlock()
	snap := j.current.Load()
	if snap == nil || failures > 0 {
		delay := j.opts.MinRefreshInterval
		for i := 1; i < failures && delay < maxJWKSRetryDelay; i++ {
			delay *= 2
		}
		return max(min(delay, maxJWKSRetryDelay), j.opts.MinRefreshInterval)
	}
	return max(j.refreshAt(snap).Sub(j.now()), j.opts.MinRefreshInterval)
}

// errKeysUnavailable is what a token is refused with while no key set has been
// fetched: the server cannot judge it, which is a 503 rather than the client's
// fault.
var errKeysUnavailable = errors.New("muzak: no key set has been fetched yet, so the token cannot be verified")

// candidates returns the keys a token is checked against from the key set,
// fetching it first when there is none, when it has expired, or when the
// token names a kid it does not hold. Every such fetch is subject to
// MinRefreshInterval except the very first, and a request whose context ends
// stops waiting for one.
func (j *jwksCache) candidates(ctx context.Context, kid string, hasKid bool) ([]*verifyKey, error) {
	snap := j.current.Load()
	if snap == nil {
		// Nothing to verify with at all: wait for a fetch, whoever starts it.
		j.refresh(ctx, true)
		if snap = j.current.Load(); snap == nil {
			return nil, errKeysUnavailable
		}
	} else if !j.now().Before(snap.expires) {
		// Expired, which only happens while no background refresh runs. The
		// stale set keeps serving if the fetch fails, or another request is
		// already fetching.
		j.refresh(ctx, false)
		snap = j.current.Load()
	}
	keys := snap.keys.candidates(kid, hasKid)
	if len(keys) == 0 && hasKid {
		// The issuer may have rotated its keys since the set was fetched.
		j.refresh(ctx, true)
		keys = j.current.Load().keys.candidates(kid, hasKid)
	}
	return keys, nil
}

// refresh fetches the key set unless one fetched less than MinRefreshInterval
// ago forbids it. A fetch already in flight is joined rather than repeated:
// with wait set, the caller waits for it to finish or for its own context to
// end; without, the caller goes on with the set it has.
//
// The fetch runs on the calling goroutine, with a context that keeps nothing
// of the caller's but is cancelled when the application stops and bounded by
// Timeout, so a client that disconnects does not fail it for the requests
// waiting on it.
func (j *jwksCache) refresh(ctx context.Context, wait bool) {
	j.mu.Lock()
	if call := j.inflight; call != nil {
		j.mu.Unlock()
		if wait {
			select {
			case <-call.done:
			case <-ctx.Done():
			}
		}
		return
	}
	now := j.now()
	if !j.lastAttempt.IsZero() && now.Sub(j.lastAttempt) < j.opts.MinRefreshInterval {
		j.mu.Unlock()
		return
	}
	call := &jwksFetch{done: make(chan struct{})}
	j.inflight, j.lastAttempt = call, now
	base := j.base
	j.mu.Unlock()

	failed := true
	defer func() {
		j.mu.Lock()
		j.inflight = nil
		if failed {
			j.failures++
		} else {
			j.failures = 0
		}
		j.mu.Unlock()
		close(call.done)
	}()
	failed = !j.fetch(base)
}

// fetch fetches and parses the key set once, publishing it on success. A
// failure is logged, with the reason but never the document, and the last
// set is kept.
func (j *jwksCache) fetch(base context.Context) bool {
	j.fetches.Add(1)
	ctx, cancel := context.WithTimeout(base, j.opts.Timeout)
	defer cancel()
	keys, ttl, err := j.download(ctx)
	if err != nil {
		j.logger.Warn("muzak: the key set could not be refreshed; the last one fetched is kept",
			slog.String("scheme", j.name), slog.String("error", err.Error()))
		return false
	}
	now := j.now()
	j.current.Store(&jwksSnapshot{keys: keys, fetched: now, expires: now.Add(ttl)})
	return true
}

// download makes one request for the key set and reads it, within the size
// limit, into a set of keys.
func (j *jwksCache) download(ctx context.Context) (*keySet, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.opts.URL, http.NoBody)
	if err != nil {
		// coverage: the URL was parsed when the application was built, and a
		// GET with no body cannot be refused for any other reason.
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, 0, withoutURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if last := resp.Request; last != nil && last.URL != nil && !secureOrLoopback(last.URL) {
		// The URL was checked when the application was built, but a client
		// that follows redirects, as an *http.Client does, may have been sent
		// from it down to plain http, where anyone on the path could have
		// replaced the keys. The URL is left out, as above.
		return nil, 0, errors.New("muzak: the key set was redirected to a plain http URL, where anyone on the path could replace it")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("muzak: the key set was answered with %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, j.opts.MaxBytes+1))
	if err != nil {
		return nil, 0, withoutURL(err)
	}
	if int64(len(body)) > j.opts.MaxBytes {
		return nil, 0, fmt.Errorf("muzak: the key set is larger than the limit of %d bytes", j.opts.MaxBytes)
	}
	keys, skipped, err := parseJWKS(body, j.opts.MaxKeys, j.allowed)
	if err != nil {
		return nil, 0, err
	}
	if keys.count == 0 {
		return nil, 0, fmt.Errorf("muzak: the key set holds no key this scheme can verify with (%d skipped)", skipped)
	}
	return keys, cacheTTL(resp.Header, j.opts.MinCacheTTL, j.opts.MaxCacheTTL), nil
}

// withoutURL drops the URL net/http puts in a client error, which may carry a
// query an issuer considers private, keeping the reason.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// cacheTTL is how long a key set is kept: its Cache-Control max-age, or an
// hour when it has none, held within [lo, hi]. A response that may not be
// stored, by no-store or no-cache, is kept for lo, since a key set fetched on
// every token is no cache at all. The header is read in one pass, and a
// max-age given twice counts at its smaller value.
func cacheTTL(h http.Header, lo, hi time.Duration) time.Duration {
	ttl := defaultJWKSCacheTTL
	found := false
	for _, value := range h.Values("Cache-Control") {
		for directive := range strings.SplitSeq(value, ",") {
			name, arg, _ := strings.Cut(strings.TrimSpace(directive), "=")
			switch strings.ToLower(name) {
			case "no-store", "no-cache":
				return lo
			case "max-age":
				seconds, err := strconv.ParseUint(strings.Trim(arg, `"`), 10, 32)
				if err != nil {
					continue
				}
				if age := time.Duration(seconds) * time.Second; !found || age < ttl {
					ttl, found = age, true
				}
			}
		}
	}
	return min(max(ttl, lo), hi)
}
