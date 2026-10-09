package muzak

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"muzak.dev/framework/internal/radix"
)

// Defaults applied when [HealthOptions] is enabled and leaves a field unset.
const (
	// DefaultLivenessPath is where the liveness endpoint answers.
	DefaultLivenessPath = "/livez"
	// DefaultReadinessPath is where the readiness endpoint answers.
	DefaultReadinessPath = "/readyz"
	// DefaultHealthCacheInterval is how long a readiness result is reused
	// before the checks are run again.
	DefaultHealthCacheInterval = time.Second
	// DefaultHealthCheckTimeout bounds one readiness check. It matches the
	// one-second timeoutSeconds a Kubernetes probe defaults to, so a check that
	// takes longer has failed by the time it would have answered anyway.
	DefaultHealthCheckTimeout = time.Second
)

// HealthOptions configures the liveness and readiness endpoints a platform
// probes to decide whether to restart the process and whether to send it
// traffic.
//
// The zero value serves neither and costs nothing. With Enabled set, the
// liveness endpoint answers 200 for as long as the process serves at all,
// which is the only question a restart should hang on, and the readiness
// endpoint answers 503 until the lifecycle components have started, 200 once
// they have and every check passes, and 503 again from the moment a shutdown
// begins, before any connection is closed, so that a load balancer stops
// sending traffic while in-flight requests still finish. Pair it with
// [ServerOptions.DrainDelay] to give the load balancer time to notice.
//
//	muzak.AppOptions{
//		Health: muzak.HealthOptions{
//			Enabled: true,
//			Checks: []muzak.HealthCheck{
//				{Name: "database", Check: db.PingContext},
//			},
//		},
//	}
//
// Both endpoints are answered ahead of routing, before the CORS policy and the
// middleware installed with [App.Use], and run no guard, provider or rate
// limit: a probe carries no credentials, and an endpoint a probe cannot reach
// takes the whole service out of rotation. They take GET and HEAD and answer
// 405 to anything else, are sent with "Cache-Control: no-store", and are not
// part of the OpenAPI document. A body says {"status":"ok"} or
// {"status":"unavailable"} and nothing more unless ReportChecks asks for the
// name of each check; the error a check returned is logged, never sent.
//
// Readiness checks run concurrently, each bounded by its own timeout, and
// their combined result is reused for CacheInterval. Probes that arrive while
// the checks are running wait for that run rather than starting another, so
// a flood of probes costs one run of the checks per interval, not one per
// probe.
type HealthOptions struct {
	// Enabled serves the two endpoints. Setting any other field without it is
	// a build error, rather than configuration that silently does nothing.
	Enabled bool

	// LivenessPath is where liveness is answered, defaulting to
	// [DefaultLivenessPath]. It is matched exactly, must be an absolute path
	// of letters, digits and "-._~/" in its clean form, and may not be a path
	// a route, a mount or the documentation answers, which is a build error.
	LivenessPath string

	// ReadinessPath is where readiness is answered, defaulting to
	// [DefaultReadinessPath], under the same rules as LivenessPath.
	ReadinessPath string

	// Checks are what readiness depends on beyond the lifecycle components
	// having started, such as a database answering a ping. With none,
	// readiness is the lifecycle alone.
	Checks []HealthCheck

	// CacheInterval is how long a readiness result is reused before the
	// checks are run again, defaulting to [DefaultHealthCacheInterval]. It is
	// what bounds the load probes put on a dependency: one run per interval,
	// however many probes arrive. A negative value runs the checks for every
	// probe that does not find a run already in progress, which still
	// collapses concurrent probes into one run but no longer bounds the rate.
	CacheInterval time.Duration

	// ReportChecks adds each check's name and whether it passed to the
	// readiness body, as {"status":"unavailable","checks":{"database":"fail"}}.
	// It is off by default because the endpoint is unauthenticated, and the
	// names of a service's dependencies, and which of them is down, are worth
	// something to an attacker. Turn it on when the endpoint is reachable
	// only from inside the platform.
	ReportChecks bool

	// AccessLogLevel is the level a probe is recorded at in the access log,
	// whatever its status, defaulting to slog.LevelDebug: a platform probing
	// every few seconds would otherwise outnumber real traffic, and a 503
	// during start-up or shutdown is expected rather than an error. A probe is
	// not recorded at all when [AppOptions.DisableAccessLog] is set or its path
	// is in [AccessLogOptions.SkipPaths].
	AccessLogLevel slog.Leveler
}

// HealthCheck is one dependency readiness waits for.
type HealthCheck struct {
	// Name identifies the check in logs and, with
	// [HealthOptions.ReportChecks], in the readiness body. It is required,
	// unique, at most 64 characters, and made of letters, digits, '-', '.'
	// and '_', so that it needs no escaping anywhere it is written.
	Name string

	// Check reports whether the dependency is usable, returning nil when it
	// is. It is given a context that expires after Timeout, and should honour
	// it: a check still running from a previous probe is not started again,
	// and reports failure until it returns. A panic is recovered and counts
	// as a failure. The error is logged when the check starts failing and is
	// never sent to the prober.
	Check func(ctx context.Context) error

	// Timeout bounds one run of the check, defaulting to
	// [DefaultHealthCheckTimeout]. Keep it below the probe's own timeout, so
	// that a slow dependency is reported as a 503 rather than as a probe
	// that never got an answer. A negative value is a build error.
	Timeout time.Duration
}

// maxHealthCheckName bounds a check's name; see [HealthCheck.Name].
const maxHealthCheckName = 64

// withDefaults fills in the unset fields of an enabled configuration, and
// leaves a disabled one exactly as written so that validate can tell whether
// anything was set without Enabled.
func (o HealthOptions) withDefaults() HealthOptions {
	if !o.Enabled {
		return o
	}
	if o.LivenessPath == "" {
		o.LivenessPath = DefaultLivenessPath
	}
	if o.ReadinessPath == "" {
		o.ReadinessPath = DefaultReadinessPath
	}
	if o.CacheInterval == 0 {
		o.CacheInterval = DefaultHealthCacheInterval
	}
	if o.AccessLogLevel == nil {
		o.AccessLogLevel = slog.LevelDebug
	}
	return o
}

// configured reports whether anything besides Enabled was set.
func (o HealthOptions) configured() bool {
	return o.LivenessPath != "" || o.ReadinessPath != "" || len(o.Checks) > 0 ||
		o.CacheInterval != 0 || o.ReportChecks || o.AccessLogLevel != nil
}

// isHealthPath reports whether path is one of the health endpoints, which is
// what a check that must not stand between a platform and its probes, such as
// a host allowlist or a redirect to HTTPS, asks before applying itself. A
// probe is sent to the pod's own address over plain HTTP, so either check
// would refuse it. It is false for every path when health is not enabled.
func (a *App) isHealthPath(path string) bool {
	health := &a.opts.Health
	return health.Enabled && (path == health.LivenessPath || path == health.ReadinessPath)
}

// validateHealth reports every reason the health endpoints cannot be served
// as configured. It runs once the routes and mounts are known, because one of
// the things that can be wrong with a path is that something else answers it.
func (a *App) validateHealth(state *buildState) {
	health := a.opts.Health
	if !health.Enabled {
		if health.configured() {
			state.errs = append(state.errs, errors.New("muzak: HealthOptions is configured but HealthOptions.Enabled is false, "+
				"so nothing would be served; set Enabled to serve the health endpoints, or remove the configuration"))
		}
		return
	}
	paths := [...]struct{ field, path string }{
		{"HealthOptions.LivenessPath", health.LivenessPath},
		{"HealthOptions.ReadinessPath", health.ReadinessPath},
	}
	if health.LivenessPath == health.ReadinessPath {
		state.errs = append(state.errs, fmt.Errorf("muzak: HealthOptions.LivenessPath and HealthOptions.ReadinessPath are both %q, "+
			"but liveness and readiness answer different questions and need an address each", health.LivenessPath))
	}
	for _, p := range paths {
		if err := checkHealthPath(p.field, p.path); err != nil {
			state.errs = append(state.errs, err)
			continue
		}
		if err := a.healthPathTaken(p.field, p.path); err != nil {
			state.errs = append(state.errs, err)
		}
	}
	state.errs = append(state.errs, validateHealthChecks(health.Checks)...)
}

// checkHealthPath reports a path a probe could not be pointed at exactly.
//
// The path is compared with the decoded request path, so it is held to the
// characters that never need encoding, and to its clean form: "/livez/" or
// "//livez" would answer a spelling the platform's configuration is unlikely
// to use, and whoever wrote it most likely meant the clean one.
func checkHealthPath(field, p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("muzak: %s is %q, which is not an absolute path and so would answer no probe; write it as %q",
			field, truncateForMessage(p), "/"+truncateForMessage(p))
	}
	for i := 0; i < len(p); i++ {
		if !isHealthPathByte(p[i]) {
			return fmt.Errorf("muzak: %s is %q, which holds %q; use only letters, digits, '-', '.', '_', '~' and '/', which a probe never has to encode",
				field, truncateForMessage(p), p[i:i+1])
		}
	}
	if clean := path.Clean(p); clean != p {
		return fmt.Errorf("muzak: %s is %q, which is not in its clean form; write it as %q", field, truncateForMessage(p), clean)
	}
	return nil
}

// isHealthPathByte reports whether c may appear in a health path; see
// [checkHealthPath].
func isHealthPathByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-._~/", c) >= 0
}

// healthPathTaken reports a health path that a route, a mount or the
// documentation would otherwise answer. Health is answered first, so any of
// them would lose that path without a word; that is a mistake to report
// rather than a precedence to apply.
//
// A frontend mounted at the root is the one exception. It answers whatever
// nothing else does, exactly as it does for every route, so a health path
// beneath it shadows nothing it was configured to serve, except the root
// itself: "/" is the frontend's own page, and a probe there is refused.
func (a *App) healthPathTaken(field, p string) error {
	var params radix.Params
	if entry, found := a.tree.Lookup(p, &params); found {
		switch {
		case len(entry.methods) == 0 && entry.mount != nil && entry.mount.prefix == "" && p != "/":
			// A handler mounted at the root answers only what nothing else
			// does, as a frontend at the root does, so a probe answered ahead
			// of routing takes nothing from it that was meant for it. Its
			// root is its own, as a frontend's is.
		case len(entry.methods) == 0 && entry.mount != nil:
			return fmt.Errorf("muzak: %s is %q, which lies under the handler mounted at %q; move one of the two",
				field, p, entry.mount.template)
		default:
			return fmt.Errorf("muzak: %s is %q, which the route %s also answers; move one of the two",
				field, p, describeEntry(a.entries, entry))
		}
	}
	for _, mount := range a.frontends {
		if _, under := mount.matches(p); under && (mount.path != "" || p == "/") {
			return fmt.Errorf("muzak: %s is %q, which lies under the %s mounted at %q; move one of the two",
				field, p, mount.kind, mount.mountPath())
		}
	}
	if a.opts.DisableDocs {
		return nil
	}
	if p == a.opts.OpenAPIPath {
		return fmt.Errorf("muzak: %s is %q, which is where the OpenAPI document is served; move one of the two", field, p)
	}
	if docs := a.opts.DocsPath; a.opts.DocsUI != nil && (p == docs || strings.HasPrefix(p, strings.TrimSuffix(docs, "/")+"/")) {
		return fmt.Errorf("muzak: %s is %q, which the documentation UI at %q answers; move one of the two", field, p, docs)
	}
	return nil
}

// describeEntry names the routes behind a path entry for an error message,
// as "GET, POST /users/{id}".
func describeEntry(entries map[string]*pathEntry, entry *pathEntry) string {
	methods := make([]string, 0, len(entry.methods))
	for method := range entry.methods {
		methods = append(methods, method)
	}
	slices.Sort(methods)
	for template, candidate := range entries {
		if candidate == entry {
			return strings.Join(methods, ", ") + " " + template
		}
	}
	// coverage: every entry in the tree is recorded in entries when it is
	// inserted, so the loop always finds it. The fallback keeps the message
	// readable if that ever stops being true.
	return strings.Join(methods, ", ")
}

// validateHealthChecks reports every check that cannot be run as declared.
func validateHealthChecks(checks []HealthCheck) []error {
	var errs []error
	seen := make(map[string]bool, len(checks))
	for i, check := range checks {
		name := check.Name
		switch {
		case name == "":
			errs = append(errs, fmt.Errorf("muzak: HealthOptions.Checks[%d] has no Name; give each check one, for the logs", i))
		case len(name) > maxHealthCheckName || !isHealthCheckName(name):
			errs = append(errs, fmt.Errorf("muzak: HealthOptions.Checks[%d] is named %q; a name is at most %d letters, digits, '-', '.' and '_'",
				i, truncateForMessage(name), maxHealthCheckName))
		case seen[name]:
			errs = append(errs, fmt.Errorf("muzak: HealthOptions.Checks has two checks named %q; each needs a name of its own", name))
		}
		seen[name] = true
		if check.Check == nil {
			errs = append(errs, fmt.Errorf("muzak: HealthOptions.Checks[%d] (%q) has no Check function", i, truncateForMessage(name)))
		}
		if check.Timeout < 0 {
			errs = append(errs, fmt.Errorf("muzak: HealthOptions.Checks[%d] (%q) has a negative Timeout; leave it zero for the default of %s",
				i, truncateForMessage(name), DefaultHealthCheckTimeout))
		}
	}
	return errs
}

// isHealthCheckName reports whether name holds only the characters a check's
// name may; see [HealthCheck.Name].
func isHealthCheckName(name string) bool {
	for i := 0; i < len(name); i++ {
		if c := name[i]; c == '/' || c == '~' || !isHealthPathByte(c) {
			return false
		}
	}
	return true
}

// readinessState is what readiness reports before any check runs: whether
// the lifecycle components are up, and whether a shutdown has begun. Both
// are atomic because probes read them while a run starts or a shutdown
// begins on another goroutine.
type readinessState struct {
	started  atomic.Bool
	draining atomic.Bool
}

// ready reports whether the application should be sent traffic, as far as
// its lifecycle goes.
func (s *readinessState) ready() bool {
	return s.started.Load() && !s.draining.Load()
}

// The bodies of the health responses. They are fixed so that a probe costs no
// encoding, and so that nothing a check returned can reach one.
var (
	healthBodyOK          = []byte(`{"status":"ok"}`)
	healthBodyUnavailable = []byte(`{"status":"unavailable"}`)
)

// healthAllow is the Allow header of a refused probe.
const healthAllow = "GET, HEAD"

// healthEndpoints answers the two probes; see [HealthOptions].
type healthEndpoints struct {
	app       *App
	livePath  string
	readyPath string
	// checker is nil when readiness has no checks to run.
	checker *healthChecker
	// log is the access log a probe is recorded in, and is nil when probes
	// are not recorded.
	log      *slog.Logger
	level    slog.Leveler
	skipLive bool
	skipRdy  bool
}

// healthMiddleware answers the health endpoints and passes every other request
// on. It is installed inside the request identifier, the security headers and
// panic recovery, and outside everything else, for the reasons
// [HealthOptions] gives.
func (a *App) healthMiddleware(next http.Handler) http.Handler {
	h := a.newHealthEndpoints()
	a.health = h
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case h.livePath:
			h.serve(w, r, h.skipLive, h.liveness)
		case h.readyPath:
			h.serve(w, r, h.skipRdy, h.readiness)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// newHealthEndpoints prepares the endpoints from the validated options.
func (a *App) newHealthEndpoints() *healthEndpoints {
	opts := a.opts.Health
	h := &healthEndpoints{
		app:       a,
		livePath:  opts.LivenessPath,
		readyPath: opts.ReadinessPath,
		level:     opts.AccessLogLevel,
		skipLive:  slices.Contains(a.opts.AccessLogOptions.SkipPaths, opts.LivenessPath),
		skipRdy:   slices.Contains(a.opts.AccessLogOptions.SkipPaths, opts.ReadinessPath),
	}
	if !a.opts.DisableAccessLog {
		h.log = Scoped(a.logger, ScopeRequest)
	}
	if len(opts.Checks) > 0 {
		h.checker = newHealthChecker(opts, Scoped(a.logger, ScopeServer))
	}
	return h
}

// serve answers one probe with what answer decides, and records it.
func (h *healthEndpoints) serve(w http.ResponseWriter, r *http.Request, skipLog bool, answer func(context.Context) (int, []byte)) {
	start := time.Now()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		rw := asResponseWriter(w)
		h.refuseMethod(rw, r)
		h.record(r, skipLog, rw.statusOrDefault(), rw.bytes, start)
		return
	}
	status, body := answer(r.Context())
	header := w.Header()
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	var written int64
	if r.Method != http.MethodHead {
		n, _ := w.Write(body)
		written = int64(n)
	}
	h.record(r, skipLog, status, written, start)
}

// refuseMethod answers a probe sent with a method other than GET or HEAD
// through the application's error renderer, as a 405 from a route would be.
func (h *healthEndpoints) refuseMethod(rw *responseWriter, r *http.Request) {
	a := h.app
	c := a.acquire(rw, r)
	defer a.release(c)
	rw.Header().Set("Allow", healthAllow)
	a.fail(c, NewHTTPErrorf(http.StatusMethodNotAllowed, "%s is not allowed here; allowed methods are %s",
		quotableMethod(r.Method), healthAllow))
}

// record writes a probe's access log line at the configured level, whatever
// its status; see [HealthOptions.AccessLogLevel].
func (h *healthEndpoints) record(r *http.Request, skip bool, status int, bytes int64, start time.Time) {
	if h.log == nil || skip {
		return
	}
	id, _ := RequestIDFromContext(r.Context())
	// The path is one of the two configured ones, so only the method, which
	// the client chose, needs cutting to length.
	h.log.LogAttrs(r.Context(), h.level.Level(), truncateForMessage(r.Method)+" "+r.URL.Path,
		slog.Int("status", status),
		slog.Duration("duration", time.Since(start)),
		slog.Int64("bytes", bytes),
		slog.String(RequestIDKey, id))
}

// liveness answers that the process is serving, which is all it is asked.
func (h *healthEndpoints) liveness(context.Context) (int, []byte) {
	return http.StatusOK, healthBodyOK
}

// readiness answers whether the application should be sent traffic.
func (h *healthEndpoints) readiness(ctx context.Context) (int, []byte) {
	if !h.app.readiness.ready() {
		return http.StatusServiceUnavailable, healthBodyUnavailable
	}
	if h.checker == nil {
		return http.StatusOK, healthBodyOK
	}
	report := h.checker.result(ctx)
	// A shutdown that began while the checks were running wins over what they
	// found, and so does a prober that gave up waiting for them.
	if report == nil || !h.app.readiness.ready() {
		return http.StatusServiceUnavailable, healthBodyUnavailable
	}
	return report.status, report.body
}

// Why a check that did not answer counts as failed.
var (
	errHealthCheckTimeout = errors.New("muzak: the readiness check did not answer within its timeout")
	errHealthCheckBusy    = errors.New("muzak: the readiness check has not returned from a previous run, and was not started again")
)

// healthReport is the combined result of one run of the checks, ready to
// send. It is never modified once published.
type healthReport struct {
	status int
	body   []byte
}

// healthFlight is a run of the checks in progress, which every probe arriving
// meanwhile waits for.
type healthFlight struct {
	done   chan struct{}
	report *healthReport
}

// healthOutcome is what one check returned.
type healthOutcome struct {
	index int
	err   error
}

// healthChecker runs the readiness checks on behalf of every probe at once.
//
// Its costs are bounded whatever probes arrive: at most one run is in
// progress, at most one starts per CacheInterval, a run lasts no longer than
// the longest check's timeout, and at most one goroutine per check exists,
// because a check that has not returned from a previous run is reported as
// failing rather than started a second time.
type healthChecker struct {
	checks   []HealthCheck
	interval time.Duration
	longest  time.Duration
	detail   bool
	logger   *slog.Logger
	// busy marks a check whose goroutine has not returned yet.
	busy []atomic.Bool
	// failing is each check's state at the last run, so that a failure is
	// logged when it begins rather than on every probe. Only the probe
	// running the checks touches it, and runs are ordered by mu.
	failing []bool

	mu       sync.Mutex
	cached   *healthReport
	cachedAt time.Time
	flight   *healthFlight
}

// newHealthChecker prepares the checks with their timeouts resolved.
func newHealthChecker(opts HealthOptions, logger *slog.Logger) *healthChecker {
	c := &healthChecker{
		checks:   slices.Clone(opts.Checks),
		interval: opts.CacheInterval,
		detail:   opts.ReportChecks,
		logger:   logger,
		busy:     make([]atomic.Bool, len(opts.Checks)),
		failing:  make([]bool, len(opts.Checks)),
	}
	for i := range c.checks {
		if c.checks[i].Timeout == 0 {
			c.checks[i].Timeout = DefaultHealthCheckTimeout
		}
		c.longest = max(c.longest, c.checks[i].Timeout)
	}
	return c
}

// result returns the current readiness report: the cached one while it is
// fresh, the one a run already in progress produces, or that of a new run
// this call performs. It returns nil when ctx ends while waiting on another
// probe's run.
func (c *healthChecker) result(ctx context.Context) *healthReport {
	c.mu.Lock()
	if c.cached != nil && time.Since(c.cachedAt) < c.interval {
		report := c.cached
		c.mu.Unlock()
		return report
	}
	if flight := c.flight; flight != nil {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.report
		case <-ctx.Done():
			return nil
		}
	}
	flight := &healthFlight{done: make(chan struct{})}
	c.flight = flight
	c.mu.Unlock()
	// Published on every way out, so a waiting probe is never left behind by
	// a run that did not finish normally.
	defer func() {
		c.mu.Lock()
		c.cached, c.cachedAt, c.flight = flight.report, time.Now(), nil
		c.mu.Unlock()
		close(flight.done)
	}()
	// The run continues if this probe's client leaves: other probes are
	// waiting on it, and it is bounded by the longest timeout either way.
	flight.report = c.run()
	return flight.report
}

// forget drops the cached report, so that a new run of the application does
// not answer from what the previous one found.
func (c *healthChecker) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = nil
}

// run runs every check concurrently and combines their results, waiting no
// longer than the longest timeout. A check that answers after its own timeout
// counts as failed even if it returned nil.
func (c *healthChecker) run() *healthReport {
	errs := make([]error, len(c.checks))
	outcomes := make(chan healthOutcome, len(c.checks))
	began := time.Now()
	pending := 0
	for i := range c.checks {
		if !c.busy[i].CompareAndSwap(false, true) {
			errs[i] = errHealthCheckBusy
			continue
		}
		errs[i] = errHealthCheckTimeout
		pending++
		go c.check(i, outcomes)
	}
	timer := time.NewTimer(c.longest)
	defer timer.Stop()
	for pending > 0 {
		select {
		case outcome := <-outcomes:
			pending--
			if time.Since(began) <= c.checks[outcome.index].Timeout {
				errs[outcome.index] = outcome.err
			}
		case <-timer.C:
			pending = 0
		}
	}
	return c.report(errs)
}

// check runs one check on its own goroutine and reports what it returned. The
// outcomes channel has room for every check, so a check that answers after
// the run stopped waiting still returns rather than blocking.
func (c *healthChecker) check(i int, outcomes chan<- healthOutcome) {
	defer c.busy[i].Store(false)
	check := c.checks[i]
	ctx, cancel := context.WithTimeoutCause(context.Background(), check.Timeout, errHealthCheckTimeout)
	defer cancel()
	outcomes <- healthOutcome{index: i, err: c.call(ctx, check)}
}

// call runs a check's function, turning a panic into a failure.
func (c *healthChecker) call(ctx context.Context, check HealthCheck) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("muzak: the readiness check panicked: %s", panicValue(recovered))
			c.logger.Error("muzak: recovered from a panic in a readiness check",
				slog.String("check", check.Name),
				slog.String("panic", panicValue(recovered)))
		}
	}()
	return check.Check(ctx)
}

// report logs what changed since the last run and builds the response.
func (c *healthChecker) report(errs []error) *healthReport {
	healthy := true
	for i, err := range errs {
		c.observe(i, err)
		if err != nil {
			healthy = false
		}
	}
	status, body := http.StatusOK, healthBodyOK
	if !healthy {
		status, body = http.StatusServiceUnavailable, healthBodyUnavailable
	}
	if c.detail {
		body = c.detailedBody(healthy, errs)
	}
	return &healthReport{status: status, body: body}
}

// observe logs a check that starts failing at warn level, with its error,
// and one that recovers at info level. A check that keeps failing is logged
// only at debug level, so the log grows with changes of state rather than
// with the rate of probes.
func (c *healthChecker) observe(i int, err error) {
	name := c.checks[i].Name
	switch {
	case err != nil && !c.failing[i]:
		c.failing[i] = true
		c.logger.Warn("muzak: a readiness check failed; readiness reports unavailable",
			slog.String("check", name), slog.String("error", truncateTo(err.Error(), maxLoggedPanicLength)))
	case err != nil:
		c.logger.Debug("muzak: a readiness check is still failing",
			slog.String("check", name), slog.String("error", truncateTo(err.Error(), maxLoggedPanicLength)))
	case c.failing[i]:
		c.failing[i] = false
		c.logger.Info("muzak: a readiness check passes again", slog.String("check", name))
	}
}

// detailedBody writes the readiness body with each check's name and outcome.
// A name is validated to need no escaping, and nothing of an error is used.
func (c *healthChecker) detailedBody(healthy bool, errs []error) []byte {
	body := make([]byte, 0, 48+len(c.checks)*(maxHealthCheckName+12))
	if healthy {
		body = append(body, `{"status":"ok","checks":{`...)
	} else {
		body = append(body, `{"status":"unavailable","checks":{`...)
	}
	for i, err := range errs {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, '"')
		body = append(body, c.checks[i].Name...)
		if err == nil {
			body = append(body, `":"ok"`...)
		} else {
			body = append(body, `":"fail"`...)
		}
	}
	return append(body, "}}"...)
}
