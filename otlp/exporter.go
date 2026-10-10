package otlp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	muzak "muzak.dev/framework"
)

// The exporter is a Tracer and a lifecycle component; these fail to compile if
// it stops being either.
var (
	_ muzak.Tracer    = (*Exporter)(nil)
	_ muzak.Lifecycle = (*Exporter)(nil)
)

// ErrNotRunning is returned by [Exporter.Flush] when the exporter has not been
// started, or has been stopped, so there is nothing to send the spans with.
var ErrNotRunning = errors.New("otlp: the exporter is not running")

// Exporter records spans and sends them to an OpenTelemetry collector. It is
// a [muzak.Tracer] and a [muzak.Lifecycle], and an application that names it
// as its Tracer starts and stops it with everything else:
//
//	exporter, err := otlp.New(otlp.Options{
//		Endpoint:    "https://otlp.example.com",
//		ServiceName: "shop",
//		Headers:     otlp.Headers{"Authorization": "Bearer " + token},
//		Gzip:        true,
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	app := muzak.New(muzak.AppOptions{Tracing: muzak.TracingOptions{Tracer: exporter}})
//
// A span that ends goes into a queue of [Options.QueueSize] and is sent in
// batches of [Options.BatchSize], or every [Options.BatchInterval] when a
// batch is slow to fill, by one goroutine started with the exporter. Ending a
// span never waits: when the queue is full the span is dropped and counted,
// because a request held up by a collector is worse than a missing span. Stop
// sends what is queued within its context's deadline and returns once the
// goroutine has exited.
//
// An exporter that is never started still accepts spans until its queue is
// full, and sends them once it is.
type Exporter struct {
	cfg    config
	client *http.Client
	// transport is the one the exporter made for itself when Options.Client
	// gave none, and the only one whose idle connections Stop closes. It is
	// nil when the client came from the options: its transport, or
	// http.DefaultTransport when it names none, is shared with whatever else
	// uses it.
	transport *http.Transport
	queue     chan *span
	// resource is the converted resource attributes, the same for every
	// batch, and scopeVersion the framework's version, reported with the
	// instrumentation scope.
	resource     []keyValue
	scopeVersion string

	// mu guards the lifecycle. A span is queued under the read lock, so once
	// Stop has taken the write lock and marked the exporter stopped, nothing
	// can be queued that its final flush would miss.
	mu      sync.RWMutex
	stopped bool
	current *run

	exported atomic.Uint64
	dropped  atomic.Uint64
	failed   atomic.Uint64

	// reportedDrops is the drop count last logged. Only workers touch it,
	// but the worker of a run that is finishing can still be running when
	// Start begins the next, so it is atomic.
	reportedDrops atomic.Uint64
}

// run is one stretch of the exporter running, from Start to Stop.
type run struct {
	// ctx is what every export is sent under. Stop cancels it once its own
	// context ends, which is what bounds an export in flight, a retry being
	// waited for and the final flush by Stop's deadline.
	ctx    context.Context
	cancel context.CancelFunc

	// stopping is closed by Stop, and stopDeadline written before it is,
	// so the worker reads it only after seeing the channel closed.
	stopping     chan struct{}
	stopDeadline time.Time

	flush chan chan struct{}
	done  chan struct{}

	// lost counts the spans the final flush could not send before the
	// deadline. Written by the worker, read by Stop after done is closed.
	lost int
}

// Stats is a snapshot of what an exporter has done with the spans it was
// given.
type Stats struct {
	// Exported is the number of spans the collector accepted.
	Exported uint64
	// Dropped is the number of spans never sent: those that ended while the
	// queue was full or after the exporter stopped, and those still queued
	// when Stop's deadline passed.
	Dropped uint64
	// Failed is the number of spans sent and not accepted: refused by the
	// collector, given up on after retrying, or rejected one by one in a
	// partial success.
	Failed uint64
	// Queued is the number of spans waiting to be sent.
	Queued int
}

// New builds an exporter, reporting every problem with the options at once.
// It starts nothing: [Exporter.Start] does, which an application that names
// the exporter as its Tracer calls for it.
func New(opts Options) (*Exporter, error) {
	cfg, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var client http.Client
	var transport *http.Transport
	if opts.Client != nil {
		client = *opts.Client
	} else {
		transport = newTransport(http.DefaultTransport)
		client.Transport = transport
	}
	// A redirect would carry the headers, credentials included, to whatever
	// the answer named, so the answer is taken as it is: a 3xx is a refusal.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Exporter{
		cfg:          cfg,
		client:       &client,
		transport:    transport,
		queue:        make(chan *span, cfg.queueSize),
		resource:     buildResource(cfg),
		scopeVersion: frameworkVersion(),
	}, nil
}

// newTransport returns a transport of the exporter's own, cloned from the
// default one so that its proxy and TLS settings apply, so that closing its
// idle connections on Stop closes nobody else's. A program that replaced the
// default with a transport of another type gets a plain one.
func newTransport(base http.RoundTripper) *http.Transport {
	if t, ok := base.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

// StartSpan records the start of a span, implementing [muzak.Tracer]. The
// span is queued when it ends.
func (e *Exporter) StartSpan(_ context.Context, start muzak.SpanStart) muzak.Span {
	return newSpan(e, start)
}

// enqueue hands an ended span to the worker, or drops and counts it when the
// queue is full or the exporter has stopped. It never blocks.
func (e *Exporter) enqueue(s *span) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.stopped {
		e.dropped.Add(1)
		return
	}
	select {
	case e.queue <- s:
	default:
		e.dropped.Add(1)
	}
}

// Name identifies the exporter in the application's start-up and shutdown
// logs.
func (e *Exporter) Name() string { return "otlp" }

// Start begins sending spans, implementing [muzak.Lifecycle]. It starts one
// goroutine, which Stop ends. Starting an exporter that is running does
// nothing, and one that was stopped may be started again.
func (e *Exporter) Start(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != nil {
		return nil
	}
	r := &run{
		stopping: make(chan struct{}),
		flush:    make(chan chan struct{}),
		done:     make(chan struct{}),
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	e.current = r
	e.stopped = false
	go e.work(r)
	return nil
}

// Stop sends the spans still queued and ends the exporter's goroutine,
// implementing [muzak.Lifecycle].
//
// The flush is bounded by ctx: once it ends, the request in flight is
// abandoned, nothing more is sent, and what was not sent is counted as
// dropped. Stop returns only when the goroutine has exited, which takes no
// longer than ctx allows and a moment for the abandoned request to unwind,
// and it closes the idle connections of the transport the exporter made for
// itself, so nothing the exporter started outlives it. Those of a client
// given in [Options.Client] are left to whoever owns its transport, which is
// http.DefaultTransport for a client that names none. The error reports
// spans lost to the deadline; spans the collector refused are logged and
// counted, but are not an error of Stop's. Stopping an exporter that is not
// running drops whatever it had queued.
func (e *Exporter) Stop(ctx context.Context) error {
	e.mu.Lock()
	r := e.current
	e.current = nil
	e.stopped = true
	if r != nil {
		r.stopDeadline, _ = ctx.Deadline()
		if ctx.Err() != nil {
			// Already over: cancelled before the worker hears of the stop,
			// so that it sends nothing rather than racing the cancellation
			// to start one more request.
			r.cancel()
		}
		close(r.stopping)
	}
	e.mu.Unlock()
	if e.transport != nil {
		defer e.transport.CloseIdleConnections()
	}
	if r == nil {
		e.dropped.Add(e.discardQueued())
		return nil
	}
	cancelOnExpiry := context.AfterFunc(ctx, r.cancel)
	<-r.done
	cancelOnExpiry()
	r.cancel()
	if r.lost > 0 {
		return fmt.Errorf("otlp: %d spans could not be sent before the exporter had to stop: %w", r.lost, context.Cause(ctx))
	}
	return nil
}

// Flush sends every span queued when it is called and waits until they have
// been sent or given up on, or until ctx ends. It returns [ErrNotRunning] for
// an exporter that is not running.
func (e *Exporter) Flush(ctx context.Context) error {
	e.mu.RLock()
	r := e.current
	e.mu.RUnlock()
	if r == nil {
		return ErrNotRunning
	}
	return r.requestFlush(ctx)
}

// requestFlush asks the run's worker to flush and waits for it, for no longer
// than ctx allows. A run that ends first answers [ErrNotRunning], which is
// what keeps a Flush racing a Stop from waiting on a worker that has gone.
func (r *run) requestFlush(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case r.flush <- reply:
	case <-r.done:
		return ErrNotRunning
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats reports what the exporter has done so far.
func (e *Exporter) Stats() Stats {
	return Stats{
		Exported: e.exported.Load(),
		Dropped:  e.dropped.Load(),
		Failed:   e.failed.Load(),
		Queued:   len(e.queue),
	}
}

// String names the exporter by where it sends, and never shows a header.
func (e *Exporter) String() string { return "otlp exporter to " + e.cfg.url }

// LogValue renders the exporter for a log line: where it sends, and the names
// of the headers it sends, never their values.
func (e *Exporter) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("endpoint", e.cfg.url),
		slog.Any("headers", e.cfg.headerNames))
}

// work is the exporter's goroutine: it batches what is queued, sends a batch
// when it fills or when the interval passes, and on Stop sends what is left
// and returns.
func (e *Exporter) work(r *run) {
	defer close(r.done)
	ticker := time.NewTicker(e.cfg.batchInterval)
	defer ticker.Stop()
	batch := make([]*span, 0, e.cfg.batchSize)
	send := func() {
		if len(batch) > 0 {
			e.export(r, batch)
			clear(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case s := <-e.queue:
			batch = append(batch, s)
			if len(batch) == e.cfg.batchSize {
				send()
			}
		case <-ticker.C:
			send()
			e.reportDrops()
		case reply := <-r.flush:
			batch = e.drain(r, batch)
			send()
			close(reply)
		case <-r.stopping:
			batch = e.drain(r, batch)
			send()
			e.reportDrops()
			return
		}
	}
}

// drain moves what is queued into batches, sending each one that fills, and
// returns the last, partial one. It takes only as many spans as were queued
// when it began, so that spans arriving as fast as they are sent cannot keep a
// flush from finishing.
//
// It never waits for a span: one counted at the start may have been taken
// since by a second Stop, which discards what an exporter already stopped
// still holds, or by the worker of a run started while this one finishes,
// and waiting for it would hold Stop until a span that cannot come.
func (e *Exporter) drain(r *run, batch []*span) []*span {
	for range len(e.queue) {
		var s *span
		select {
		case s = <-e.queue:
		default:
			return batch
		}
		batch = append(batch, s)
		if len(batch) == e.cfg.batchSize {
			e.export(r, batch)
			clear(batch)
			batch = batch[:0]
		}
	}
	return batch
}

// discardQueued empties the queue of an exporter that will not send it,
// returning how many spans it held.
func (e *Exporter) discardQueued() uint64 {
	var n uint64
	for {
		select {
		case <-e.queue:
			n++
		default:
			return n
		}
	}
}

// reportDrops logs how many spans were dropped since it last did, which is at
// most once per batch interval however many are.
func (e *Exporter) reportDrops() {
	dropped := e.dropped.Load()
	reported := e.reportedDrops.Load()
	if dropped <= reported || !e.reportedDrops.CompareAndSwap(reported, dropped) {
		// Nothing new, or the other worker reported it first.
		return
	}
	e.cfg.logger.Warn("otlp: spans were dropped because the export queue was full or the exporter had stopped",
		slog.Uint64("dropped", dropped-reported),
		slog.Uint64("dropped_total", dropped),
		slog.Int("queue_size", e.cfg.queueSize))
}
