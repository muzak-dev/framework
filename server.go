package badele

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
	// in-flight requests before connections are closed.
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
	// to [DefaultWriteTimeout].
	WriteTimeout time.Duration
	// IdleTimeout bounds how long an idle keep-alive connection is kept,
	// defaulting to [DefaultIdleTimeout].
	IdleTimeout time.Duration
	// ShutdownTimeout bounds how long [App.Shutdown] waits for in-flight
	// requests, defaulting to [DefaultShutdownTimeout].
	ShutdownTimeout time.Duration
	// MaxHeaderBytes bounds the size of the request header block, defaulting
	// to [DefaultMaxHeaderBytes].
	MaxHeaderBytes int
	// TLSConfig enables HTTPS when set. [App.Run] serves TLS whenever this or
	// a certificate pair is supplied.
	TLSConfig *tls.Config
	// CertFile and KeyFile enable HTTPS from a certificate and key on disk.
	CertFile string
	KeyFile  string
	// BaseContext returns the base context for incoming requests. When nil,
	// requests derive from context.Background.
	BaseContext func(net.Listener) context.Context
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

// serverRunner owns the http.Server and the state needed to shut it down
// exactly once, no matter which of the run methods started it.
//
// Every field is written once, before the runner is published through
// App.server, and only read afterwards. The atomic store that publishes it
// supplies the happens-before edge another goroutine needs to read them.
type serverRunner struct {
	http     *http.Server
	listener net.Listener
	done     chan struct{}
	stopOnce sync.Once
}

// newServer builds the http.Server for the application, applying every
// configured limit.
func (a *App) newServer() *http.Server {
	opts := a.opts.ServerOptions
	return &http.Server{
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
	}
}

// Run starts the server and blocks until it stops.
//
// It builds the application first, so a configuration error is reported before
// any socket is opened. The server listens on [AppOptions.Addr], serving TLS
// when a certificate pair or a TLS configuration was supplied.
//
// Run returns nil after a graceful shutdown and an error if the listener could
// not be opened or the application could not be built. Use [App.RunContext]
// for a server that should stop when a context is cancelled, or
// [App.RunSignals] for one that should stop on an interrupt.
func (a *App) Run() error {
	return a.RunContext(context.Background())
}

// RunContext starts the server and blocks until ctx is cancelled or the server
// fails.
//
// When ctx is cancelled the server stops accepting new connections and waits
// up to [ServerOptions.ShutdownTimeout] for in-flight requests to finish
// before closing the rest. A shutdown triggered this way returns nil, because
// stopping on request is the expected outcome rather than a failure.
func (a *App) RunContext(ctx context.Context) error {
	listener, err := a.listen(ctx)
	if err != nil {
		return err
	}
	return a.serve(ctx, listener)
}

// RunSignals starts the server and blocks until it is interrupted.
//
// It stops on SIGINT or SIGTERM, which is what a terminal, a container
// runtime and an init system all send to ask a process to stop, and then shuts
// down gracefully. It is the method a main function usually wants.
func (a *App) RunSignals() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.RunContext(ctx)
}

// listen builds the application, brings up its lifecycle components and opens
// the listening socket, reporting the address actually bound. Binding before
// serving is what makes ":0" usable in tests: the port is known as soon as
// this returns.
func (a *App) listen(ctx context.Context) (net.Listener, error) {
	if err := a.Build(); err != nil {
		return nil, err
	}
	// Components come up before the socket opens, so the first request can
	// never reach a handler whose database pool is still dialling.
	if err := a.StartLifecycle(ctx); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", a.opts.Addr)
	if err != nil {
		return nil, errors.Join(err, a.StopLifecycle(context.WithoutCancel(ctx)))
	}
	a.server.Store(&serverRunner{
		http:     a.newServer(),
		listener: listener,
		done:     make(chan struct{}),
	})
	return listener, nil
}

// serve runs the accept loop until the context is cancelled or the server
// stops on its own.
func (a *App) serve(ctx context.Context, listener net.Listener) error {
	runner := a.server.Load()
	scheme := "http"
	if a.servesTLS() {
		scheme = "https"
	}
	Scoped(a.logger, ScopeServer).Info("Listening on "+listener.Addr().String(),
		slog.String("scheme", scheme))

	errCh := make(chan error, 1)
	go func() {
		defer close(runner.done)
		var err error
		if a.servesTLS() {
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
		return err
	case <-ctx.Done():
		if err := a.Shutdown(context.Background()); err != nil {
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
// finish, giving up after [ServerOptions.ShutdownTimeout] and closing whatever
// remains. The ctx argument can cut the wait short; pass context.Background to
// use the configured timeout alone.
//
// Shutdown is safe to call more than once and from more than one goroutine;
// only the first call does the work. Calling it on a server that was never
// started returns nil.
func (a *App) Shutdown(ctx context.Context) error {
	runner := a.server.Load()
	if runner == nil {
		return nil
	}
	var err error
	runner.stopOnce.Do(func() {
		log := Scoped(a.logger, ScopeServer)
		log.Info("Shutting down, waiting for in-flight requests...")

		// A hijacked connection is no longer one net/http tracks, so a
		// WebSocket would otherwise be left open through the whole shutdown
		// with its peer none the wiser.
		closed := a.websockets.shutdown(a.opts.ShutdownTimeout, wsCloseGoingAway)
		if closed > 0 {
			log.Info(fmt.Sprintf("Closed %d websocket %s", closed, plural(closed, "connection")))
		}

		// An event stream is tracked by net/http, which is exactly why it has
		// to be ended here: waiting for a handler that is streaming means
		// waiting for the whole shutdown deadline, once per stream.
		ended := a.streams.shutdown(a.opts.ShutdownTimeout, (*sseStream).shuttingDown)
		if ended > 0 {
			log.Info(fmt.Sprintf("Ended %d event %s", ended, plural(ended, "stream")))
		}

		timeout := a.opts.ShutdownTimeout
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		err = runner.http.Shutdown(ctx)
		if err != nil {
			log.Warn("Shutdown deadline reached, closing remaining connections",
				slog.String("error", err.Error()))
			err = errors.Join(err, runner.http.Close())
		}
		<-runner.done

		// Only now that no request is in flight is it safe to release the
		// resources those requests were using.
		err = errors.Join(err, a.StopLifecycle(context.WithoutCancel(ctx)))
		log.Info("Stopped")
	})
	return err
}

// Addr returns the address the server is listening on, which is how a test
// that asked for ":0" discovers the port that was assigned. It returns an
// empty string before the server has started.
func (a *App) Addr() string {
	runner := a.server.Load()
	if runner == nil || runner.listener == nil {
		return ""
	}
	return runner.listener.Addr().String()
}
