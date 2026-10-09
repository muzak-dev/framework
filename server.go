package muzak

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Default listener timeouts. Every one of them is non-zero on purpose: an
// http.Server left with the standard library's zero values will hold a
// connection open indefinitely, which is all a slow-loris client needs to
// exhaust the server's connection budget.
const (
	// DefaultReadHeaderTimeout bounds how long a client may take to send the
	// request headers.
	DefaultReadHeaderTimeout = 5 * time.Second
	// DefaultReadTimeout bounds how long a client may take to send the
	// headers and the body together.
	DefaultReadTimeout = 30 * time.Second
	// DefaultWriteTimeout bounds how long a handler may take to write its
	// response.
	DefaultWriteTimeout = 30 * time.Second
	// DefaultIdleTimeout bounds how long a keep-alive connection may sit
	// unused before it is closed.
	DefaultIdleTimeout = 120 * time.Second
	// DefaultShutdownTimeout bounds how long a graceful shutdown waits for
	// in-flight requests before connections are closed. It is one budget for
	// the whole shutdown; see [App.Shutdown] for how it is spent.
	//
	// Set it below whatever grace period the platform allows, or the platform
	// kills a drain that is still running. The common ones are worth knowing:
	// Cloud Run allows about ten seconds after SIGTERM by default, Kubernetes
	// uses terminationGracePeriodSeconds and defaults to thirty, and ECS uses
	// stopTimeout and defaults to thirty. Leave room afterwards for anything
	// the application flushes on the way out, such as a message publisher.
	DefaultShutdownTimeout = 15 * time.Second
)

// ServerOptions configures the HTTP listener.
//
// Its fields may be set directly inside an [AppOptions] literal, because
// AppOptions embeds it. Every timeout defaults to a non-zero value; setting
// one to a negative number disables it, which should be reserved for a server
// behind a proxy that enforces its own limits.
type ServerOptions struct {
	// ReadHeaderTimeout bounds the time allowed to read request headers,
	// defaulting to [DefaultReadHeaderTimeout].
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds the time allowed to read the entire request,
	// defaulting to [DefaultReadTimeout].
	ReadTimeout time.Duration
	// WriteTimeout bounds the time allowed to write the response, defaulting
	// to [DefaultWriteTimeout]. Over HTTP/2 it also bounds how long the
	// connection's socket may accept no bytes at all: a client that stops
	// reading has the whole connection closed after this long, because a
	// deadline on one stream cannot interrupt a write already in progress.
	//
	// ReadHeaderTimeout is not applied to HTTP/2, whose header blocks net/http
	// reads under the connection's own timeouts; a client that never finishes
	// one is held until IdleTimeout, not for ReadHeaderTimeout.
	WriteTimeout time.Duration
	// IdleTimeout bounds how long an idle keep-alive connection is kept,
	// defaulting to [DefaultIdleTimeout].
	IdleTimeout time.Duration
	// ShutdownTimeout bounds how long [App.Shutdown] waits for in-flight
	// requests, defaulting to [DefaultShutdownTimeout]. It is one deadline
	// for the whole shutdown: WebSocket connections, event streams and
	// ordinary requests are drained together within it, and the lifecycle
	// components are then stopped with what it leaves, or with at least one
	// second when the drain used all of it. A negative value waits for
	// in-flight requests without limit.
	ShutdownTimeout time.Duration
	// MaxHeaderBytes bounds the size of the request header block, defaulting
	// to [DefaultMaxHeaderBytes].
	MaxHeaderBytes int
	// TLSConfig enables HTTPS when set. [App.Run] serves TLS whenever this or
	// a certificate pair is supplied.
	TLSConfig *tls.Config
	// CertFile and KeyFile enable HTTPS from a certificate and key on disk.
	// Setting only one of them is a build error.
	CertFile string
	KeyFile  string
	// UnencryptedHTTP2 also accepts HTTP/2 without TLS, sent with prior
	// knowledge, on the same port as HTTP/1. It is for a server behind a
	// platform that speaks HTTP/2 to the container in the clear, such as Cloud
	// Run with an h2c port, where it is what lets a request be cancelled the
	// moment its client leaves: over HTTP/1 the platform's proxy keeps the
	// connection to the container open, and a handler runs on for as long as it
	// likes after its client has gone. Leave it off anywhere the proxy speaks
	// HTTP/1, and never for a server reachable directly from an untrusted
	// network. A WebSocket handshake is not carried over HTTP/2 and is answered
	// 426, and Cloud Run does not pass one to an h2c container at all.
	UnencryptedHTTP2 bool
	// BaseContext returns the base context for incoming requests. When nil,
	// requests derive from context.Background.
	BaseContext func(net.Listener) context.Context
}

// validate reports a certificate pair that is only half there. Either file
// alone leaves servesTLS false, so the server would come up in plaintext on
// the port its operator believes is HTTPS, with nothing to say so; that is a
// misconfiguration to refuse rather than a mode to fall back to.
func (o ServerOptions) validate() error {
	switch {
	case o.CertFile != "" && o.KeyFile == "":
		return errors.New("muzak: ServerOptions.CertFile is set without ServerOptions.KeyFile; set both to serve HTTPS, or neither")
	case o.CertFile == "" && o.KeyFile != "":
		return errors.New("muzak: ServerOptions.KeyFile is set without ServerOptions.CertFile; set both to serve HTTPS, or neither")
	}
	return nil
}

// withDefaults fills in the unset timeouts and normalises the disabling
// convention, so that a negative value becomes the zero net/http uses to mean
// "no limit".
func (o ServerOptions) withDefaults() ServerOptions {
	o.ReadHeaderTimeout = orDefaultDuration(o.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	o.ReadTimeout = orDefaultDuration(o.ReadTimeout, DefaultReadTimeout)
	o.WriteTimeout = orDefaultDuration(o.WriteTimeout, DefaultWriteTimeout)
	o.IdleTimeout = orDefaultDuration(o.IdleTimeout, DefaultIdleTimeout)
	o.ShutdownTimeout = orDefaultDuration(o.ShutdownTimeout, DefaultShutdownTimeout)
	if o.MaxHeaderBytes == 0 {
		o.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	return o
}

// orDefaultDuration applies the default for an unset duration and turns an
// explicit negative into zero, which is how net/http spells "unbounded".
func orDefaultDuration(v, fallback time.Duration) time.Duration {
	switch {
	case v == 0:
		return fallback
	case v < 0:
		return 0
	default:
		return v
	}
}

// Bounds on the part of a shutdown that comes after its deadline.
const (
	// shutdownHandlerGrace is how long a shutdown whose deadline has passed
	// still waits for handlers once their connections are closed. Closing a
	// connection cancels its request's context and fails any read or write
	// on it, and a handler that honours either returns within this; one that
	// honours neither is not waited for any longer.
	shutdownHandlerGrace = 100 * time.Millisecond
	// lifecycleStopFloor is the least time lifecycle components are given to
	// stop, however little of the shutdown deadline the drain left, so that a
	// component flushing on the way out is not handed a context that has
	// already expired.
	lifecycleStopFloor = time.Second
)

// runState is what an App knows about being run: the run in progress, if
// there is one, and the most recent run, which [App.Addr] reports.
//
// One App is served by one run at a time. A second run method called while
// one is in progress used to run alongside it: it failed to bind the address
// the first was serving on and then, as any run whose socket could not be
// opened does, stopped the lifecycle components the first was still using.
// It had also replaced the runner the App kept, so cancelling the first run's
// context shut down a runner that never served, and the first run never
// returned. Claiming the run under a lock is what lets the second one be
// refused before it touches anything.
type runState struct {
	mu sync.Mutex
	// running is the run in progress, and nil between runs.
	running *serverRunner
	// last is the most recent run, kept once it has ended.
	last *serverRunner
	// stopPending records a Shutdown that found no run in progress, for the
	// next run to honour; see [App.Shutdown].
	stopPending bool
}

// errAlreadyRunning is what a run method returns while another is serving
// the same application.
var errAlreadyRunning = errors.New("muzak: the application is already running, and one App is served by one run at a time; " +
	"wait for the run method in progress to return, or build a second App to serve another address")

// claimRun records a new run as the one in progress, and refuses it while
// another is. A Shutdown that arrived with no run to stop is handed to the
// new run as already requested, and is then spent.
func (a *App) claimRun(ctx context.Context) (*serverRunner, error) {
	a.server.mu.Lock()
	defer a.server.mu.Unlock()
	if a.server.running != nil {
		return nil, errAlreadyRunning
	}
	runner := newServerRunner(ctx)
	runner.stopRequested = a.server.stopPending
	a.server.stopPending = false
	a.server.running = runner
	a.server.last = runner
	return runner, nil
}

// endRun releases what a run held and lets the application be run again.
func (a *App) endRun(runner *serverRunner) {
	runner.release()
	a.server.mu.Lock()
	defer a.server.mu.Unlock()
	a.server.running = nil
}

// currentOrPend returns the run in progress, or, when there is none, records
// a stop for the next run and returns nil. Both happen under the lock a run
// is claimed under, which is what keeps a Shutdown from falling between a
// run that has not been recorded yet and one that has.
func (a *App) currentOrPend() *serverRunner {
	a.server.mu.Lock()
	defer a.server.mu.Unlock()
	if a.server.running == nil {
		a.server.stopPending = true
	}
	return a.server.running
}

// serverRunner owns the http.Server and the state needed to shut it down
// exactly once, no matter which of the run methods started it.
//
// A runner is recorded as the run in progress as soon as a run method is
// called, before the application is built, so that a Shutdown arriving while
// the lifecycle components are still starting is recorded rather than lost.
// The fields above mu are written before it is recorded and only read
// afterwards; the lock it is recorded under supplies the happens-before edge
// another goroutine needs to read them. Those below it are filled in once the
// socket is open, and are read under mu.
type serverRunner struct {
	done     chan struct{}
	stopOnce sync.Once
	// stopped is closed once a shutdown has finished, lifecycle components
	// included, which is what a run method waits for before it returns.
	stopped chan struct{}
	// handlers counts the requests being served, hijacked ones included.
	handlers handlerTracker
	// startCtx is what the lifecycle components are started with, and
	// cancelStart is how a shutdown requested during start-up tells a
	// component still dialling to give up. endStartUp stops the run's own
	// context from cancelling it, once the components are up; see
	// newServerRunner.
	startCtx    context.Context
	cancelStart context.CancelFunc
	endStartUp  func() bool
	// provided is a listener the caller opened, served on instead of a socket
	// opened on AppOptions.Addr, and plaintext serves it HTTP whatever TLS is
	// configured. Both are for the test client; see [App.serveInProcess].
	// They are set before the run starts and read only by the run itself.
	provided  net.Listener
	plaintext bool

	mu sync.Mutex
	// stopRequested records a Shutdown, including one that arrived before
	// there was a server to shut down.
	stopRequested bool
	http          *http.Server
	listener      net.Listener
}

// newServerRunner prepares the runner for one call of a run method.
//
// The context the components are started with keeps ctx's values but is not
// its child. Cancelling ctx is how [App.RunContext] is asked to shut down, and
// how [App.RunSignals] answers SIGTERM, so a child ended the moment the drain
// began: a worker a component had kept on it was cancelled while requests that
// still used it were being served, and Stop was called on components whose
// context had already gone, against what [Lifecycle] promises. ctx still
// cancels it while the components are starting, which is when a component
// dialling should hear that the run is over; after that it ends only once the
// components have been stopped, when the lifecycle manager and the run
// method's return both release it.
func newServerRunner(ctx context.Context) *serverRunner {
	startCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &serverRunner{
		done:        make(chan struct{}),
		stopped:     make(chan struct{}),
		startCtx:    startCtx,
		cancelStart: cancel,
		endStartUp:  context.AfterFunc(ctx, cancel),
	}
}

// release ends the start context and whatever still ties it to the run's
// context, on every way out of a run method.
func (r *serverRunner) release() {
	r.endStartUp()
	r.cancelStart()
}

// handlerTracker counts the handlers a server is running, so that a shutdown
// can wait for them before the lifecycle components they use are stopped.
//
// net/http cannot answer this itself. It forgets a hijacked connection, and a
// shutdown that reaches its deadline closes the connections it does know
// about without waiting for their handlers to return. The count is kept with
// one atomic add on each side of a request; the lock is taken only once a
// shutdown is waiting.
type handlerTracker struct {
	active   atomic.Int64
	draining atomic.Bool
	mu       sync.Mutex
	// idle is closed by the last handler to return while a shutdown waits.
	idle chan struct{}
}

// track wraps the handler the server runs so that every call is counted.
func (t *handlerTracker) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.active.Add(1)
		defer t.finish()
		next.ServeHTTP(w, r)
	})
}

// finish records a handler that returned, and tells a waiting shutdown when
// it was the last one.
func (t *handlerTracker) finish() {
	if t.active.Add(-1) != 0 || !t.draining.Load() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.idle != nil {
		close(t.idle)
		t.idle = nil
	}
}

// wait blocks until no handler is running or timeout has passed, and reports
// how many handlers were still running when it gave up.
func (t *handlerTracker) wait(timeout time.Duration) int64 {
	t.mu.Lock()
	t.draining.Store(true)
	if t.active.Load() == 0 {
		t.mu.Unlock()
		return 0
	}
	if t.idle == nil {
		t.idle = make(chan struct{})
	}
	idle := t.idle
	t.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return 0
	case <-timer.C:
		return t.active.Load()
	}
}

// newServer builds the http.Server for the application, applying every
// configured limit.
func (a *App) newServer() *http.Server {
	opts := a.opts.ServerOptions
	server := &http.Server{
		Addr:              a.opts.Addr,
		Handler:           a,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
		ReadTimeout:       opts.ReadTimeout,
		WriteTimeout:      opts.WriteTimeout,
		IdleTimeout:       opts.IdleTimeout,
		MaxHeaderBytes:    opts.MaxHeaderBytes,
		TLSConfig:         opts.TLSConfig,
		BaseContext:       opts.BaseContext,
		ErrorLog:          slog.NewLogLogger(a.logger.Handler(), slog.LevelWarn),
		// A write deadline set on an HTTP/2 stream cannot interrupt the write
		// of a frame that is already in progress: the connection's one
		// writer parks in the socket, and a client that grants a huge flow
		// control window and then stops reading the socket keeps it there,
		// with every stream on the connection, past every WriteTimeout. This
		// closes a connection that a write makes no progress on for as long
		// as WriteTimeout, which is the bound HTTP/1 already has.
		HTTP2: &http.HTTP2Config{WriteByteTimeout: opts.WriteTimeout},
	}
	if opts.UnencryptedHTTP2 {
		// Naming the protocols replaces net/http's own choice, so the two it
		// makes by default are named again alongside the one asked for.
		var protocols http.Protocols
		protocols.SetHTTP1(true)
		protocols.SetHTTP2(true)
		protocols.SetUnencryptedHTTP2(true)
		server.Protocols = &protocols
	}
	return server
}

// Run starts the server and blocks until it stops.
//
// It builds the application first, so a configuration error is reported before
// any socket is opened. The server listens on [AppOptions.Addr], serving TLS
// when a certificate pair or a TLS configuration was supplied.
//
// Run returns nil after a graceful shutdown and an error if the listener could
// not be opened, the application could not be built, or serving failed, in
// which last case the lifecycle components are stopped first. A shutdown requested
// while Run is still starting, before the socket is open, is honoured too:
// Run stops the components that started and returns nil without serving. So
// is one requested before Run was called at all; see [App.Shutdown]. Use [App.RunContext]
// for a server that should stop when a context is cancelled, or
// [App.RunSignals] for one that should stop on an interrupt.
//
// An App is served by one run method at a time. One called while another is
// still running, starting or shutting down returns an error at once, and
// touches nothing the run in progress is using. Once that run has returned,
// the application may be run again.
func (a *App) Run() error {
	return a.RunContext(context.Background())
}

// RunContext starts the server and blocks until ctx is cancelled or the server
// fails.
//
// When ctx is cancelled the server shuts down as [App.Shutdown] describes,
// waiting up to [ServerOptions.ShutdownTimeout] for in-flight requests to
// finish before closing the rest. A shutdown triggered this way returns nil, because
// stopping on request is the expected outcome rather than a failure. As with
// [App.Run], it refuses to start while another run method is running the
// same application.
func (a *App) RunContext(ctx context.Context) error {
	runner, err := a.claimRun(ctx)
	if err != nil {
		return err
	}
	defer a.endRun(runner)
	listener, err := a.listen(ctx, runner)
	if err != nil || listener == nil {
		return err
	}
	return a.serve(ctx, runner, listener)
}

// RunSignals starts the server and blocks until it is interrupted.
//
// It stops on SIGINT or SIGTERM, which is what a terminal, a container
// runtime and an init system all send to ask a process to stop, and then shuts
// down gracefully. It is the method a main function usually wants.
//
// Only the first signal is graceful. Once it has been received the handlers
// are released, so a second SIGINT or SIGTERM while the server is still
// draining takes the default action and ends the process, which is what an
// operator pressing Ctrl-C twice expects.
func (a *App) RunSignals() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// NotifyContext keeps swallowing signals until stop runs, which on its
	// own would be when the drain has finished: a second interrupt during a
	// drain that may last the whole ShutdownTimeout, or without limit when it
	// is negative, would do nothing at all. stop also ends the goroutine, so
	// a return before any signal leaves nothing behind.
	go func() {
		<-ctx.Done()
		stop()
	}()
	return a.RunContext(ctx)
}

// listen builds the application, brings up its lifecycle components and opens
// the listening socket, reporting the address actually bound. Binding before
// serving is what makes ":0" usable in tests: the port is known as soon as
// this returns.
//
// It returns a nil listener and the error from stopping the components when
// a shutdown was requested before or while it ran, since nothing is to be
// served.
func (a *App) listen(ctx context.Context, runner *serverRunner) (net.Listener, error) {
	if err := a.Build(); err != nil {
		return nil, err
	}
	if runner.stopWasRequested() {
		// A Shutdown came before this run was recorded, and was kept for it.
		// Nothing has started yet, so there is nothing to stop either.
		Scoped(a.logger, ScopeServer).Info("Shutdown was requested before the server started; not serving")
		return nil, nil
	}
	// An application that was shut down and is being run again must admit
	// WebSockets and event streams again; only a run in progress may refuse
	// them for shutting down.
	a.websockets.reopen()
	a.streams.reopen()
	// Components come up before the socket opens, so the first request can
	// never reach a handler whose database pool is still dialling.
	if err := a.StartLifecycle(runner.startCtx); err != nil {
		if runner.stopWasRequested() {
			// The failure is most likely the cancellation the shutdown
			// caused, and the lifecycle manager has logged it either way.
			return nil, nil
		}
		return nil, err
	}
	// From here on the components belong to the server rather than to its
	// start-up, and cancelling the run's context means shutting the server
	// down, which must not end the context they are using while it drains.
	runner.endStartUp()
	listener, err := runner.open(a.opts.Addr)
	if err != nil {
		return nil, errors.Join(err, a.stopAfterFailedStart(ctx))
	}
	server := a.newServer()
	server.Handler = runner.handlers.track(server.Handler)

	runner.mu.Lock()
	if runner.stopRequested {
		runner.mu.Unlock()
		// A Shutdown arrived while the components were starting. It returned
		// at once, as there was nothing to drain, and left the rest here:
		// the socket is closed before a connection is accepted on it.
		Scoped(a.logger, ScopeServer).Info("Shutdown was requested during start-up; not serving")
		return nil, errors.Join(listener.Close(), a.stopAfterFailedStart(ctx))
	}
	runner.http, runner.listener = server, listener
	runner.mu.Unlock()
	return listener, nil
}

// stopAfterFailedStart releases the lifecycle components after a run that got
// no further than starting them. Nothing else bounds this, since no shutdown
// deadline is running, so it takes the one a start-up failure does.
func (a *App) stopAfterFailedStart(ctx context.Context) error {
	stop, cancel := a.lifecycle.failedStartContext(ctx)
	defer cancel()
	return a.StopLifecycle(stop)
}

// open returns the listener the run serves on: the one it was handed, or a
// socket opened on addr.
func (r *serverRunner) open(addr string) (net.Listener, error) {
	if r.provided != nil {
		return r.provided, nil
	}
	return net.Listen("tcp", addr)
}

// servesTLS reports whether this run serves HTTPS, which is what the
// configuration asks for unless the run was told to serve plain HTTP.
func (r *serverRunner) servesTLS(a *App) bool {
	return !r.plaintext && a.servesTLS()
}

// stopWasRequested reports whether Shutdown has been called on this runner.
func (r *serverRunner) stopWasRequested() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopRequested
}

// serve runs the accept loop until the context is cancelled or the server
// stops on its own.
func (a *App) serve(ctx context.Context, runner *serverRunner, listener net.Listener) error {
	scheme := "http"
	if runner.servesTLS(a) {
		scheme = "https"
	}
	Scoped(a.logger, ScopeServer).Info("Listening on "+listener.Addr().String(),
		slog.String("scheme", scheme))
	a.logDocumentation(scheme, listener.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		defer close(runner.done)
		var err error
		if runner.servesTLS(a) {
			err = runner.http.ServeTLS(listener, a.opts.CertFile, a.opts.KeyFile)
		} else {
			err = runner.http.Serve(listener)
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			// The server was closed by a Shutdown called elsewhere, which is
			// still draining and stopping components. Returning now would let
			// a main function exit underneath it.
			<-runner.stopped
			return nil
		}
		// The server failed on its own, a certificate that would not load or
		// an accept that cannot be retried, while the components that came up
		// before the socket opened are still running. They are stopped the
		// same way a requested shutdown stops them, so that a program which
		// survives Run's error does not leave a database pool or a consumer
		// behind. ServeTLS returns before it tracks the listener when the
		// certificate is bad, so nothing else would close it.
		_ = listener.Close()
		return errors.Join(err, a.shutdownRunner(context.Background(), runner))
	case <-ctx.Done():
		// This run's own runner, not whichever the App records, so that the
		// shutdown can only ever reach the server this call is serving.
		if err := a.shutdownRunner(context.Background(), runner); err != nil {
			return err
		}
		return <-errCh
	}
}

// servesTLS reports whether the configuration asks for HTTPS.
func (a *App) servesTLS() bool {
	opts := a.opts.ServerOptions
	return opts.TLSConfig != nil || (opts.CertFile != "" && opts.KeyFile != "")
}

// Shutdown stops the server gracefully.
//
// It stops accepting new connections and waits for in-flight requests to
// finish, then stops the lifecycle components. [ServerOptions.ShutdownTimeout]
// is one deadline for all of it, counted from the call, and the ctx argument
// can bring it forward; pass context.Background to use the configured timeout
// alone.
//
// Within the deadline, WebSocket connections are told the server is going
// away, event streams are ended, and ordinary requests are left to finish, all
// at once. When the deadline passes, every connection still open is closed,
// hijacked ones included, and handlers are given a further 100 milliseconds to
// notice and return. The lifecycle components are then stopped with a context
// that expires at the deadline, or one second after they are asked to stop if
// that is later. Shutdown therefore returns within ShutdownTimeout plus about
// one second, unless a component's Stop ignores its context.
//
// A component is stopped only after every handler has returned, with one
// exception: a handler that ignores both its request's context and its
// connection being closed, and is still running when the grace period ends,
// may still be running when Stop is called. Shutdown logs how many there were.
//
// Shutdown is safe to call more than once and from more than one goroutine;
// only the first call does the work. A run method that was serving returns
// once the shutdown has finished, after which the application may be run
// again.
//
// Called while a run method is still starting, before its socket is open,
// Shutdown records the request, cancels the context the lifecycle components
// are being started with, and returns nil at once. The run method then stops
// whatever did start and returns nil without serving a request.
//
// Called when no run method is running the application, Shutdown returns nil
// and is kept for the next run, which returns nil at once without starting the
// components or opening a socket, much as net/http's ListenAndServe returns
// ErrServerClosed after Shutdown. That is what makes
//
//	go app.Run()
//	// ...
//	app.Shutdown(ctx)
//
// stop the server even when the goroutine has not reached Run yet, which a
// Shutdown that returned and was forgotten would not. Unlike net/http, only
// that next run is stopped, and the one after it serves as usual, which is what
// lets an application be run again; but a Shutdown made after a run has
// already returned stops the next one too, so code that means to run the
// application again should not shut it down twice.
func (a *App) Shutdown(ctx context.Context) error {
	runner := a.currentOrPend()
	if runner == nil {
		return nil
	}
	return a.shutdownRunner(ctx, runner)
}

// shutdownRunner is [App.Shutdown] for one run, the one runner belongs to.
func (a *App) shutdownRunner(ctx context.Context, runner *serverRunner) error {
	runner.mu.Lock()
	runner.stopRequested = true
	serving := runner.http != nil
	runner.mu.Unlock()
	if !serving {
		runner.cancelStart()
		return nil
	}
	var err error
	runner.stopOnce.Do(func() {
		defer close(runner.stopped)
		err = a.drain(ctx, runner)
	})
	return err
}

// drain is the body of [App.Shutdown], run once per server.
func (a *App) drain(ctx context.Context, runner *serverRunner) error {
	log := Scoped(a.logger, ScopeServer)
	log.Info("Shutting down, waiting for in-flight requests...")

	// One deadline covers the whole drain. Each phase used to be given the
	// full timeout in turn, so a shutdown could take three times as long as
	// configured and overrun the grace period the platform allowed.
	if timeout := a.opts.ShutdownTimeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// The listeners are closed and ordinary requests drained while the
	// long-lived responses below are ended, rather than after them, so that
	// a slow goodbye from one kind does not spend the others' share.
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- runner.http.Shutdown(ctx) }()

	// A hijacked connection is no longer one net/http tracks, so a WebSocket
	// would otherwise be left open through the whole shutdown with its peer
	// none the wiser.
	closed := a.websockets.shutdown(untilDeadline(ctx), wsCloseGoingAway)
	if closed > 0 {
		log.Info(fmt.Sprintf("Closed %d websocket %s", closed, plural(closed, "connection")))
	}

	// An event stream is tracked by net/http, which is exactly why it has to
	// be ended here: waiting for a handler that is streaming means waiting
	// for the whole shutdown deadline, once per stream.
	ended := a.streams.shutdown(untilDeadline(ctx), (*sseStream).shuttingDown)
	if ended > 0 {
		log.Info(fmt.Sprintf("Ended %d event %s", ended, plural(ended, "stream")))
	}

	err := <-shutdownErr
	if err != nil {
		log.Warn("Shutdown deadline reached, closing remaining connections",
			slog.String("error", err.Error()))
		err = errors.Join(err, runner.http.Close())
	}
	<-runner.done

	// A WebSocket whose peer stopped reading can hold its handler in a write,
	// and the goodbye above behind it, for the whole write timeout. Past the
	// deadline its transport is closed outright, as net/http has just done
	// for every connection it tracks, which fails that write at once. The
	// grace period for handlers to return starts now and is shared by both
	// waits below.
	graceEnds := time.Now().Add(shutdownHandlerGrace)
	a.websockets.shutdown(shutdownHandlerGrace, wsAbandon)

	// Only once no handler is running is it safe to release the resources
	// those handlers were using.
	wait := max(untilDeadline(ctx), time.Until(graceEnds), time.Nanosecond)
	if running := runner.handlers.wait(wait); running > 0 {
		log.Warn(fmt.Sprintf("%d %s still running after the shutdown deadline and its grace period; stopping lifecycle components anyway",
			running, plural(int(running), "handler")))
	}
	stopCtx, cancel := lifecycleStopContext(ctx)
	defer cancel()
	err = errors.Join(err, a.StopLifecycle(stopCtx))
	log.Info("Stopped")
	return err
}

// wsAbandon closes a connection's transport without a goodbye, for a
// connection still open once the shutdown deadline has passed.
func wsAbandon(conn *WSConn) {
	_ = conn.fail(&WSCloseError{Status: WSStatusAbnormalClosure, Reason: "the server shut down"})
}

// untilDeadline returns how long remains before ctx's deadline, and zero when
// it has none, which a registry reads as its own default bound. A deadline
// already passed yields the smallest positive wait, so that the registry
// still stops admitting and ends what it holds, without waiting.
func untilDeadline(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return max(time.Until(deadline), time.Nanosecond)
}

// lifecycleStopContext derives the context lifecycle components are stopped
// with. It keeps ctx's values but not its cancellation, as the components
// must be released even when the caller gave up waiting, and it expires at
// ctx's deadline or [lifecycleStopFloor] from now, whichever is later. With no
// deadline at all, which is a ShutdownTimeout disabled on purpose, it has
// none either.
func lifecycleStopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	stop := context.WithoutCancel(ctx)
	deadline, ok := ctx.Deadline()
	if !ok {
		return stop, func() {}
	}
	if floor := time.Now().Add(lifecycleStopFloor); floor.After(deadline) {
		deadline = floor
	}
	return context.WithDeadline(stop, deadline)
}

// Addr returns the address the server is listening on, which is how a test
// that asked for ":0" discovers the port that was assigned. It returns an
// empty string before the server has started. Once a run has returned it
// still reports where that run listened, until the next run opens a socket
// of its own.
func (a *App) Addr() string {
	a.server.mu.Lock()
	runner := a.server.last
	a.server.mu.Unlock()
	if runner == nil {
		return ""
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.listener == nil {
		return ""
	}
	return runner.listener.Addr().String()
}
