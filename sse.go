package badele

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ErrSSEStreamEnded reports an event stream that is no longer being written
// to: the client disconnected, the request was cancelled, the server is
// shutting down, or the handler has already returned.
//
// Every send on an ended stream reports it, so a handler's loop ends on the
// first one it sees:
//
//	for update := range updates {
//		if err := stream.Send(update); err != nil {
//			return err
//		}
//	}
//
// A handler that returns it is treated as a stream that finished rather than
// as one that failed, so a client going away is not logged as an error. Test
// for it with [errors.Is]; the reason the stream ended is wrapped inside.
var ErrSSEStreamEnded = errors.New("badele: the event stream has ended")

// ended reports a stream that ended for a reason of its own.
func ended(reason string) error {
	return fmt.Errorf("%w: %s", ErrSSEStreamEnded, reason)
}

// endedBy reports a stream that ended because something underneath it failed,
// keeping the cause reachable through [errors.Is] and [errors.As].
func endedBy(reason string, cause error) error {
	return fmt.Errorf("%w: %s: %w", ErrSSEStreamEnded, reason, cause)
}

// errSSEFinished reports something sent after the handler returned. The stream
// is over by then: the response has gone back to net/http, and writing into it
// would write into whatever request comes next on the same connection.
var errSSEFinished = ended("the handler has returned")

// errSSEBothPayloads reports an event that was given both a value and a text
// payload, which cannot both be the data field.
var errSSEBothPayloads = errors.New("badele: an event carries either Data or Text, not both")

// sseKeepAliveComment is what a keepalive says. The content is meaningless to
// every client, which ignores comments; it is spelled out so that anyone
// watching a stream by hand can see what it is.
const sseKeepAliveComment = "keepalive"

// SSEEvent is one event, for the sends that need more than a value.
//
// Use [SSEStream.Send] when the event is only data, which it usually is, and
// this when the event needs a name to be dispatched under, an identifier to
// resume from, a reconnection delay, or a payload that is not JSON:
//
//	stream.SendEvent(badele.SSEEvent[ItemOut]{Name: "item_update", ID: "42", Data: &item})
//
// The zero value carries nothing, which is a valid event: a client sees it
// dispatched with empty data.
type SSEEvent[Out any] struct {
	// Name is the event's type, which a browser dispatches it under, as in
	// addEventListener("item_update", ...). An empty Name dispatches as the
	// default "message" event. It may not contain a line break.
	Name string

	// ID identifies the event. A browser remembers the last one it saw and
	// sends it back in the Last-Event-ID header when it reconnects, which is
	// what lets a stream resume where it left off; read it with
	// [SSEStream.LastEventID]. It may not contain a line break.
	ID string

	// Retry asks the client to wait this long before reconnecting after the
	// stream ends. It is sent as a whole number of milliseconds. Set
	// [SSEOptions.Retry] instead to say it once at the start of every stream.
	Retry time.Duration

	// Comment is text no client acts on, written as a comment line before the
	// event's fields. It exists for the same reason the keepalive does: to put
	// something on the wire that means nothing.
	Comment string

	// Data is the value the event carries, encoded as JSON into its data
	// field. It is a pointer so that an event carrying nothing can be told
	// from one carrying a zero value, and it cannot be combined with Text.
	Data *Out

	// Text is a payload written as it stands rather than encoded, for a stream
	// whose events are not JSON: a log line, or a sentinel such as the "[DONE]"
	// some protocols end with. A payload spanning several lines is written as
	// several data lines and arrives whole.
	Text string
}

// SSEStream is the open event stream a server-sent events handler writes to.
//
// The type parameter is the model the stream's events carry, which is what
// makes the contract a compiler-checked one: nothing but an Out can be sent
// with [SSEStream.Send], and the generated document describes the stream with
// that type.
//
// # Concurrency
//
// Writes are serialized, so any number of goroutines may write to one stream
// and each event goes out whole. A handler that fans out to several producers
// needs no lock of its own.
//
// # Lifetime
//
// A stream belongs to one request and ends when the handler returns, which is
// why nothing here takes a context: [SSEStream.Context] is the one context
// that governs every send, and it is cancelled when the client disconnects or
// the server begins shutting down. Neither the stream nor the [Context] may be
// used after the handler returns.
//
// # Failure
//
// A stream is single use. The first failure ends it, and every later send
// reports that same error rather than writing into a response that is no
// longer being read. Sends that report a mistake instead, such as an event
// name carrying a line break, leave the stream usable.
type SSEStream[Out any] struct {
	core *sseStream
}

// Send writes one event carrying data, encoded as JSON into its data field.
// It is the whole of what most streams do.
func (s *SSEStream[Out]) Send(data Out) error {
	return s.core.send(sseFrame{data: data, hasData: true})
}

// SendEvent writes one event described in full, which is what a name, an
// identifier, a reconnection delay or a payload that is not JSON takes. An
// event that sets both [SSEEvent.Data] and [SSEEvent.Text] is refused, because
// only one of them can be the data field.
func (s *SSEStream[Out]) SendEvent(event SSEEvent[Out]) error {
	frame := sseFrame{
		comment: event.Comment,
		name:    event.Name,
		id:      event.ID,
		retry:   event.Retry,
	}
	switch {
	case event.Data != nil && event.Text != "":
		return errSSEBothPayloads
	case event.Data != nil:
		frame.data, frame.hasData = *event.Data, true
	case event.Text != "":
		frame.text, frame.hasText = event.Text, true
	}
	return s.core.send(frame)
}

// Comment writes a comment, which no client acts on and every client accepts.
// Badele already sends one every [SSEOptions.KeepAlive] to hold an idle stream
// open, so this is for a stream that wants to say something to whoever is
// watching it by hand.
func (s *SSEStream[Out]) Comment(text string) error {
	return s.core.send(sseFrame{comment: text})
}

// LastEventID returns the identifier the client last saw, taken from the
// Last-Event-ID header a browser sends when its EventSource reconnects. It is
// empty for a stream opened for the first time, and is a value the client
// controls, so treat it as input rather than as a cursor to be trusted.
func (s *SSEStream[Out]) LastEventID() string { return s.core.lastEventID }

// Context returns the context that governs the stream. It is derived from the
// request's own and is cancelled when the client disconnects, when the request
// ends, or when the server begins shutting down, which makes it the one thing
// a handler has to watch:
//
//	select {
//	case <-stream.Context().Done():
//		return nil
//	case update := <-updates:
//		return stream.Send(update)
//	}
func (s *SSEStream[Out]) Context() context.Context { return s.core.ctx }

// Err returns the error that ended the stream, or nil while it is still
// usable. Every send reports the same error, so this is only needed by a
// handler that watches [SSEStream.Context] rather than a send's result.
func (s *SSEStream[Out]) Err() error { return s.core.failure() }

// sseStream is the machinery behind [SSEStream], holding everything that does
// not depend on the type of the events.
//
// It is separate because the application tracks its open streams in one
// register, and a generic type cannot be put in one list with its own
// instantiations. The split costs nothing: the type parameter is only needed
// where a value is encoded.
type sseStream struct {
	w  http.ResponseWriter
	rc *http.ResponseController

	lastEventID  string
	writeTimeout time.Duration
	retry        time.Duration

	// deadlines records whether the response writer carries deadlines at all.
	// net/http's does; a writer supplied by a test or by middleware that does
	// not forward to it may not, and a stream is served either way.
	deadlines bool

	// writeSem serializes the writers. It is a channel rather than a mutex so
	// that waiting for it can be abandoned when the stream ends.
	writeSem chan struct{}

	// buf assembles the event being written and payload holds its encoded data
	// field. Both are owned by whoever holds writeSem.
	buf     []byte
	payload []byte

	// lastWrite records when something was last written, in Unix nanoseconds,
	// so that a keepalive says nothing on a stream that is already busy.
	lastWrite atomic.Int64

	// ctx governs the stream and is cancelled when it ends, whichever end
	// ended it. unwatch undoes the arrangement that ends the stream with the
	// request.
	ctx     context.Context
	cancel  context.CancelFunc
	unwatch func() bool

	mu  sync.Mutex
	err error
	// finished records that the response has gone back to net/http, after
	// which nothing here may touch it again: the connection may already be
	// carrying somebody else's request.
	finished bool
}

// newSSEStream builds the stream for one request.
func newSSEStream(c *Context, opts SSEOptions) *sseStream {
	ctx, cancel := context.WithCancel(c.Context())
	s := &sseStream{
		w:            c.w,
		rc:           http.NewResponseController(c.w),
		lastEventID:  c.r.Header.Get("Last-Event-ID"),
		writeTimeout: opts.WriteTimeout,
		retry:        opts.Retry,
		writeSem:     make(chan struct{}, 1),
		ctx:          ctx,
		cancel:       cancel,
	}
	s.lastWrite.Store(time.Now().UnixNano())
	return s
}

// open writes the response header and puts it on the wire, so that a client
// sees the stream open rather than waiting for the first event.
func (s *sseStream) open(c *Context) error {
	header := s.w.Header()
	setIfAbsent(header, "Content-Type", "text/event-stream; charset=utf-8")
	// A cached event stream is a replayed one, and a transformed one is
	// usually a buffered one, which is the single thing a stream cannot
	// survive.
	setIfAbsent(header, "Cache-Control", "no-cache, no-transform")
	// Several proxies, nginx among them, buffer a response by default and
	// would hold every event until the buffer filled. This is the header they
	// agreed on for saying not to.
	setIfAbsent(header, "X-Accel-Buffering", "no")

	if !sseFlushable(s.w) {
		// A stream that cannot be pushed to the client is not a stream: every
		// event would sit in a buffer until the response ended. Saying so with
		// an ordinary error is possible only here, before the header is
		// written, which is why it is checked before anything else.
		return NewHTTPError(http.StatusInternalServerError,
			"this response cannot carry an event stream, because it cannot be flushed to the client")
	}

	// The listener's timeouts were written for a request that ends. A stream
	// does not, so they are cleared here and every write from now on carries a
	// deadline of its own. Without this the whole stream would die at
	// WriteTimeout no matter how healthy it was.
	s.deadlines = s.rc.SetWriteDeadline(time.Time{}) == nil
	// The read half is cleared for a different reason. The request has been
	// read in full, and the only read left is the one net/http makes in the
	// background to notice a client going away. If that read hits a deadline
	// it cancels the request context, which would end every stream at
	// ReadTimeout and blame the client for it.
	_ = s.rc.SetReadDeadline(time.Time{})

	s.w.WriteHeader(http.StatusOK)
	if err := s.rc.Flush(); err != nil {
		return NewHTTPError(http.StatusInternalServerError,
			"the event stream could not be started").Wrap(err)
	}

	// The request's cancellation is watched once here rather than once per
	// event, and only for what cancelling has to do beyond ending the stream:
	// a write already in progress has to be brought to a stop as well.
	s.unwatch = context.AfterFunc(c.Context(), s.interrupt)

	if s.retry > 0 {
		// The reconnection delay is said once, before anything else, so that a
		// client that loses the stream on the first event already knows it.
		if err := s.send(sseFrame{retry: s.retry}); err != nil {
			return NewHTTPError(http.StatusInternalServerError,
				"the event stream could not be started").Wrap(err)
		}
	}
	return nil
}

// sseFlushable reports whether the response can be pushed to the client.
//
// The chain is walked here rather than asked through [http.ResponseController]
// because the controller answers by trying, and trying means writing the
// header first. Knowing before then is what lets a response that cannot stream
// be refused with an ordinary error rather than an empty 200.
//
// It is the innermost writer that is asked, because that is the one holding
// the connection. A wrapper in between either forwards a flush or has already
// broken streaming for every handler in the application, and no check here
// could tell the two apart.
func sseFlushable(w http.ResponseWriter) bool {
	for range maxWriterChain {
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = unwrapper.Unwrap()
	}
	// Both spellings are accepted, for the same reason the controller accepts
	// both: the newer one reports the write failures a flush can run into, and
	// the older one is what most writers implement.
	if _, ok := w.(interface{ FlushError() error }); ok {
		return true
	}
	_, ok := w.(http.Flusher)
	return ok
}

// sseFrame is one event on its way to the wire, with everything about it
// already reduced to the fields the format defines.
type sseFrame struct {
	comment string
	name    string
	id      string
	retry   time.Duration
	// data is the value the event carries and text is a payload written as it
	// stands. hasData and hasText say which of them, if either, is meant; an
	// event with neither carries no data field at all.
	data    any
	hasData bool
	text    string
	hasText bool
}

// send encodes one event and writes it, taking the write semaphore first so
// that concurrent writers cannot interleave two events.
func (s *sseStream) send(frame sseFrame) error {
	if err := frame.check(); err != nil {
		// The event was never going to be valid. The stream is left alone,
		// because nothing has reached the wire and the mistake is the caller's
		// to fix rather than the stream's to die of.
		return err
	}
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.release()

	s.payload = s.payload[:0]
	if frame.hasData {
		if err := json.MarshalWrite(sseAppender{buf: &s.payload}, frame.data); err != nil {
			return fmt.Errorf("badele: encoding an event: %w", err)
		}
	} else if frame.hasText {
		s.payload = append(s.payload, frame.text...)
	}

	s.buf = frame.appendFields(s.buf[:0])
	if frame.hasData || frame.hasText {
		s.buf = appendSSELines(s.buf, "data: ", s.payload)
	}
	// A blank line is what tells the client the event is complete. Every event
	// is assembled in full before any of it is written, so a client never sees
	// half of one.
	s.buf = append(s.buf, '\n')

	err := s.write(s.buf)
	s.shrink()
	return err
}

// appendFields writes everything before the data field. The order is the
// conventional one; the format itself does not care.
func (f sseFrame) appendFields(dst []byte) []byte {
	if f.comment != "" {
		dst = appendSSELines(dst, ": ", []byte(f.comment))
	}
	if f.name != "" {
		dst = append(append(append(dst, "event: "...), f.name...), '\n')
	}
	if f.id != "" {
		dst = append(append(append(dst, "id: "...), f.id...), '\n')
	}
	if f.retry > 0 {
		dst = append(dst, "retry: "...)
		dst = strconv.AppendInt(dst, f.retry.Milliseconds(), 10)
		dst = append(dst, '\n')
	}
	return dst
}

// check reports an event that cannot be written as described.
//
// The line break rule is the one that matters. An event stream is a sequence
// of lines, so a name or an identifier carrying one would end its own field
// and let whatever followed be read as fields of its own. On a stream that
// carries one client's input to another, that is event forgery, so a value
// with a break in it is refused rather than quietly repaired.
func (f sseFrame) check() error {
	if err := sseCheckLine("event name", f.name); err != nil {
		return err
	}
	if err := sseCheckLine("event id", f.id); err != nil {
		return err
	}
	if f.retry < 0 {
		return errors.New("badele: the retry delay of an event cannot be negative")
	}
	if !utf8.ValidString(f.comment) {
		return errors.New("badele: the comment of an event must be valid UTF-8, which is the only encoding an event stream has")
	}
	if f.hasText && !utf8.ValidString(f.text) {
		return errors.New("badele: the text of an event must be valid UTF-8, which is the only encoding an event stream has")
	}
	return nil
}

// sseCheckLine reports a field value that cannot be written on one line.
func sseCheckLine(field, value string) error {
	for i := range len(value) {
		switch value[i] {
		case '\r', '\n':
			return fmt.Errorf("badele: the %s %q contains a line break, which would let it forge events of its own", field, sseShorten(value))
		case 0:
			return fmt.Errorf("badele: the %s %q contains a null byte, which a client discards the whole field for", field, sseShorten(value))
		}
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("badele: the %s %q is not valid UTF-8, which is the only encoding an event stream has", field, sseShorten(value))
	}
	return nil
}

// sseShorten bounds a value on its way into an error message, so that a field
// built from something the size of the header limit does not become a log line
// that size.
func sseShorten(value string) string {
	const most = 64
	if len(value) <= most {
		return value
	}
	return value[:most] + "..."
}

// appendSSELines writes a payload as one or more prefixed lines.
//
// Splitting is what makes a multi-line payload safe as well as correct: every
// line of it carries the prefix, so no part of a payload can be read as a
// field, and a client joins them back into the text that was sent. The three
// line endings the format recognises are all understood, because a payload
// assembled elsewhere may carry any of them.
func appendSSELines(dst []byte, prefix string, payload []byte) []byte {
	for {
		end := -1
		for i := range len(payload) {
			if payload[i] == '\n' || payload[i] == '\r' {
				end = i
				break
			}
		}
		if end < 0 {
			return append(append(append(dst, prefix...), payload...), '\n')
		}
		dst = append(append(append(dst, prefix...), payload[:end]...), '\n')
		if payload[end] == '\r' && end+1 < len(payload) && payload[end+1] == '\n' {
			end++
		}
		payload = payload[end+1:]
	}
}

// sseAppender lets a value be encoded straight onto the buffer the event is
// assembled in, so that framing an event allocates nothing at all and a stream
// pays only for handing its value to the encoder.
type sseAppender struct{ buf *[]byte }

// Write implements io.Writer by appending.
func (a sseAppender) Write(p []byte) (int, error) {
	*a.buf = append(*a.buf, p...)
	return len(p), nil
}

// maxSSEBuffer bounds the buffers a stream keeps between events. A stream may
// last for hours, and one unusually large event should not pin the memory it
// needed for all of them.
const maxSSEBuffer = maxPooledBodyBuffer

// shrink releases a buffer that one event grew out of all proportion.
func (s *sseStream) shrink() {
	if cap(s.buf) > maxSSEBuffer {
		s.buf = nil
	}
	if cap(s.payload) > maxSSEBuffer {
		s.payload = nil
	}
}

// write puts one assembled event on the wire under its own deadline.
func (s *sseStream) write(b []byte) error {
	if s.deadlines && s.writeTimeout > 0 {
		// This is the bound that matters: without it, a client that opens a
		// stream and never reads it holds a goroutine and a growing socket
		// buffer for as long as it cares to.
		_ = s.rc.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	}
	if _, err := s.w.Write(b); err != nil {
		return s.fail(endedBy("writing an event failed", err))
	}
	if err := s.rc.Flush(); err != nil {
		return s.fail(endedBy("flushing an event failed", err))
	}
	s.lastWrite.Store(time.Now().UnixNano())
	return nil
}

// acquire takes the write semaphore, giving up if the stream ends while it
// waits.
func (s *sseStream) acquire() error {
	select {
	case s.writeSem <- struct{}{}:
	default:
		select {
		case s.writeSem <- struct{}{}:
		case <-s.ctx.Done():
			return s.failure()
		}
	}
	// The stream may have ended before this call arrived, or while it waited
	// for its turn. Either way the semaphore has to go back, or nothing could
	// finish the stream afterwards.
	if err := s.failure(); err != nil {
		s.release()
		return err
	}
	return nil
}

// release returns the write semaphore.
func (s *sseStream) release() { <-s.writeSem }

// failure returns the error that ended the stream, or nil while it is still
// usable.
//
// A cancelled context is turned into one here rather than by whatever
// cancelled it, because the context is cancelled by everything that ends a
// stream, including the client simply going away, and the first caller to
// notice should not have to know which.
func (s *sseStream) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil && s.ctx.Err() != nil {
		s.err = endedBy("the request ended", context.Cause(s.ctx))
	}
	return s.err
}

// record stores the first failure. Later calls keep the error already
// recorded, because the first one is the one that explains what happened.
func (s *sseStream) record(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
	return s.err
}

// fail ends the stream, waking anything waiting on it and bringing a write
// already in progress to a stop.
func (s *sseStream) fail(err error) error {
	recorded := s.record(err)
	s.cancel()
	s.interrupt()
	return recorded
}

// shuttingDown ends a stream because the server is going away. It is what the
// register calls for every stream it holds, and it returns at once: a stream
// ends when its handler returns, and this is what tells the handler to.
func (s *sseStream) shuttingDown() {
	_ = s.fail(ended("the server is shutting down"))
}

// interrupt brings a write already in progress to a stop, by moving its
// deadline into the past.
//
// It is what turns "eventually" into "now" for a stream being ended while a
// client that has stopped reading holds a write open. It does nothing once the
// response has gone back to net/http, because by then the deadline it would
// move belongs to whatever request is using that connection next.
func (s *sseStream) interrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || !s.deadlines {
		return
	}
	_ = s.rc.SetWriteDeadline(time.Now())
}

// finish ends the stream once its handler has returned, and reports whether
// the response could be handed back with nothing still writing into it.
//
// Waiting for the write half is the whole point. A goroutine the handler left
// behind mid-write would otherwise write into a response net/http has already
// taken back, and on a keep-alive connection that means writing into somebody
// else's request.
func (s *sseStream) finish() bool {
	handed := s.acquireFinal()
	s.mu.Lock()
	if s.err == nil {
		s.err = errSSEFinished
	}
	s.finished = true
	s.mu.Unlock()
	if handed {
		s.release()
	}
	s.cancel()
	if s.unwatch != nil {
		s.unwatch()
	}
	return handed
}

// acquireFinal takes the write semaphore for the last time, waiting no longer
// than one write is allowed to take.
func (s *sseStream) acquireFinal() bool {
	select {
	case s.writeSem <- struct{}{}:
		return true
	default:
	}
	// Something is mid-write. Its deadline is brought forward so that it gives
	// up now rather than carrying on into a response that is no longer its
	// own.
	s.interrupt()
	timer := time.NewTimer(s.finalWait())
	defer timer.Stop()
	select {
	case s.writeSem <- struct{}{}:
		return true
	case <-timer.C:
		// coverage: reaching this needs a write that outlives its own
		// interrupted deadline, which a response writer that carries no
		// deadlines is the only way to arrange; the wait is bounded so that a
		// shutdown cannot be held up by one either way.
		return false
	}
}

// finalWait bounds that last wait. A stream whose write timeout was disabled
// still needs one, because the alternative is a request that never ends.
func (s *sseStream) finalWait() time.Duration {
	if s.writeTimeout <= 0 {
		return DefaultSSEWriteTimeout
	}
	return s.writeTimeout
}

// keepalive writes a comment to a stream that has said nothing for a while,
// which is what keeps a proxy from closing a connection it believes to be idle
// and what tells the client the stream is still there.
//
// It runs only when [SSEOptions.KeepAlive] asks for it, and it stops as soon
// as the stream does.
func (s *sseStream) keepalive(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Since(time.Unix(0, s.lastWrite.Load())) < interval {
			// The stream is busy, which is all a keepalive is there to prove.
			continue
		}
		if err := s.send(sseFrame{comment: sseKeepAliveComment}); err != nil {
			return
		}
	}
}
