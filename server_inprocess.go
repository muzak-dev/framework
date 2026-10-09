package muzak

import (
	"context"
	"errors"
	"net"

	"muzak.dev/framework/internal/inprocess"
)

func init() {
	inprocess.Serve = func(app any, addr string) (string, func(context.Context) error, error) {
		return app.(*App).serveInProcessAt(addr)
	}
}

// serveInProcessAt is [App.serveInProcess] on a socket it opens on addr, and
// reports the URL the application is served at.
func (a *App) serveInProcessAt(addr string) (url string, stop func(context.Context) error, err error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, err
	}
	stop, err = a.serveInProcess(listener)
	if err != nil {
		return "", nil, err
	}
	return "http://" + listener.Addr().String(), stop, nil
}

// errShutDownBeforeServing is what serveInProcess reports for a run that a
// Shutdown stopped before it served, which [App.RunContext] answers with nil:
// a test client told nothing would send its requests to a port nobody
// answers.
var errShutDownBeforeServing = errors.New("muzak: the application was shut down before it could be served, " +
	"by a Shutdown called before or while it started; serve it before shutting it down")

// serveInProcess is the run method of the test client: it serves the
// application on a listener the caller opened, through the same start-up,
// server construction, handler accounting and shutdown as [App.RunContext],
// and returns once the application is serving rather than when it stops.
//
// It serves plain HTTP whatever TLS is configured, because the client is the
// only peer and a certificate configured for production, perhaps a path that
// does not exist on a developer's machine, would otherwise fail every test.
// Everything else in [ServerOptions] applies as it would in production.
//
// It is one run like any other, so a second one on an application already
// running is refused, and a Shutdown called on the application ends it. stop
// ends this run alone, never a later one, which matters to a test that shut
// the application down and served it again with a second client.
func (a *App) serveInProcess(listener net.Listener) (stop func(context.Context) error, err error) {
	ctx := context.Background()
	runner, err := a.claimRun(ctx)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	runner.provided, runner.plaintext = listener, true
	opened, err := a.listen(ctx, runner)
	if opened == nil {
		// listen closes the listener only on the one way out it takes after
		// starting to serve on it, a Shutdown that arrived meanwhile, so it
		// is closed here for every other; the second Close in that one case
		// reports an error nothing needs.
		_ = listener.Close()
		a.endRun(runner)
		if err == nil {
			err = errShutDownBeforeServing
		}
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := a.serve(ctx, runner, opened)
		// Ended before the result is handed over, so that a test waiting on
		// stop can serve the application again as soon as stop returns.
		a.endRun(runner)
		done <- err
	}()
	return func(ctx context.Context) error {
		return errors.Join(a.shutdownRunner(ctx, runner), <-done)
	}, nil
}
