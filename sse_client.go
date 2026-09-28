package muzak

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Defaults applied by [SSEDial] when [SSEDialOptions] leaves them unset.
const (
	// DefaultSSEReadLimit is the largest single event a reader accepts, at one
	// mebibyte. It bounds what a server can make a client hold in memory,
	// which matters because a client cannot choose what it is sent.
	DefaultSSEReadLimit int64 = 1 << 20
	// DefaultSSEReadTimeout bounds how long one event may take to arrive once
	// it has begun, at thirty seconds. It does not bound how long a stream may
	// sit idle between events, because waiting is what a stream is for.
	DefaultSSEReadTimeout = 30 * time.Second
)

// SSEDialOptions configures [SSEDial].
//
// The zero value issues a GET with the default HTTP client and applies the
// bounds above.
type SSEDialOptions struct {
	// HTTPClient issues the request, which is how a reader reaches a server on
	// a network of its own, such as the in-process one a test client serves
	// over. It defaults to a fresh [net/http.Client].
	//
	// A client with a Timeout is used with that timeout removed, because it
	// would otherwise apply to the whole life of the stream rather than to the
	// request and cut it short.
	HTTPClient *http.Client

	// Method is the request method, defaulting to GET. A stream reached by
	// POST is not unusual: it is how a protocol that streams its answer to a
	// posted document works.
	Method string

	// Body is the request body, for a stream opened with a method that carries
	// one. Set the Content-Type through Header alongside it.
	Body io.Reader

	// Header carries extra request headers, which is where an Authorization
	// header belongs. Accept and Last-Event-ID are set afterwards from the
	// fields below.
	Header http.Header

	// LastEventID resumes a stream from the identifier a previous reader last
	// saw, sent as the Last-Event-ID header exactly as a browser sends it.
	LastEventID string

	// ReadLimit is the largest single event accepted, in bytes, defaulting to
	// [DefaultSSEReadLimit]. An event larger than it ends the stream rather
	// than being buffered, so a server cannot decide how much memory this
	// process spends.
	ReadLimit int64

	// ReadTimeout bounds how long one event may take to arrive once its first
	// line has, defaulting to [DefaultSSEReadTimeout]. A negative value
	// removes the bound.
	ReadTimeout time.Duration

	// KeepComments delivers comment lines as messages of their own rather than
	// skipping them, which is how a caller sees the keepalives a server sends.
	KeepComments bool
}

// SSEMessage is one event read from a stream.
type SSEMessage struct {
	// Name is the event's type, empty for the default one a browser dispatches
	// as "message".
	Name string
	// ID is the last identifier the stream carried, which is what a reconnect
	// resumes from. It is not necessarily set by this event: the identifier
	// persists until another one arrives, exactly as it does in a browser.
	ID string
	// Data is the event's payload, with the data lines joined by newlines.
	Data string
	// Comment is the text of a comment line, set only on the messages
	// [SSEDialOptions.KeepComments] asks for, which carry nothing else.
	Comment string
}

// Decode decodes the event's data as JSON into a value of type T.
//
// The type argument is written at the call site, which keeps the expected
// shape visible and checked by the compiler:
//
//	item, err := message.Decode[ItemOut]()
func (m SSEMessage) Decode[T any]() (T, error) {
	var out T
	if err := json.Unmarshal([]byte(m.Data), &out); err != nil {
		return out, fmt.Errorf("muzak: the event's data is not JSON that fits %T: %w", out, err)
	}
	return out, nil
}

// SSEDial opens an event stream and returns a reader for it.
//
// It is the client side of the same engine that serves streams, which is what
// makes an SSE route testable end to end without a second implementation to
// disagree with the first:
//
//	reader, _, err := muzak.SSEDial(ctx, "http://"+app.Addr()+"/items/stream", muzak.SSEDialOptions{})
//	if err != nil {
//		return err
//	}
//	defer reader.Close()
//
//	for {
//		message, err := reader.Next(ctx)
//		if errors.Is(err, muzak.ErrSSEStreamEnded) {
//			return nil
//		}
//		if err != nil {
//			return err
//		}
//		// ...
//	}
//
// The response is returned alongside the reader so that a caller can read the
// headers, and on failure so that it can read the status and body the server
// refused with; a refused response has its body read into memory already and
// may be read again. The reader is nil unless the stream opened.
//
// A response that is not 200 with a text/event-stream body is refused rather
// than parsed, because a stream reader that quietly accepts an HTML error page
// reports "no events" for what is actually a failure.
//
// Cancelling ctx ends the stream, whether the handshake or a later read is
// waiting on it.
//
// Redirects are not followed, whatever the supplied client would do, and a
// redirect is refused like any other response that is not a stream, with the
// Location it named left on the returned response. Following one would send
// the request's headers to wherever the answer pointed: net/http drops an
// Authorization header on the way to another host, but not one it does not
// recognise as a credential, such as an API key header, and not an
// Authorization header sent to the same host over plain HTTP after an https
// origin redirected there. A caller that means to follow a redirect reads
// the Location and dials it, deciding for itself which headers go along.
func SSEDial(ctx context.Context, rawURL string, opts SSEDialOptions) (*SSEReader, *http.Response, error) {
	method := opts.Method
	if method == "" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), rawURL, opts.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("muzak: building the event stream request: %w", err)
	}
	for name, values := range opts.Header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Accept", "text/event-stream")
	// A cached stream is a replayed one, and an intermediary told nothing is
	// entitled to serve one.
	request.Header.Set("Cache-Control", "no-cache")
	if opts.LastEventID != "" {
		request.Header.Set("Last-Event-ID", opts.LastEventID)
	}

	response, err := sseDialClient(opts.HTTPClient).Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("muzak: the event stream request failed: %w", err)
	}
	if err := sseCheckResponse(response); err != nil {
		return nil, sseBufferBody(response), err
	}
	return newSSEReader(response.Body, opts), response, nil
}

// sseCheckResponse reports a response that is not an event stream.
func sseCheckResponse(response *http.Response) error {
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return fmt.Errorf("muzak: the event stream answered with a redirect (status %d) to %q, which is not followed; dial that address directly if it is trusted",
			response.StatusCode, sseShorten(response.Header.Get("Location")))
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("muzak: the event stream was refused with status %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return fmt.Errorf("muzak: the response is %q, not an event stream", sseShorten(response.Header.Get("Content-Type")))
	}
	return nil
}

// sseDialClient returns the client to open the stream with.
//
// Two things are changed about whatever the caller supplied, on a copy so that
// the caller's own client is left as it was. The timeout goes, because it
// would otherwise bound the whole life of the stream rather than the request
// that opens it, and a stream that is meant to last for hours would end at
// whatever the client was configured with. Redirects are refused, for the
// reason [SSEDial] gives and exactly as [WSDial] refuses them: following one
// would carry the request's credentials to whatever the answer named.
func sseDialClient(client *http.Client) *http.Client {
	dialer := &http.Client{}
	if client != nil {
		copied := *client
		dialer = &copied
	}
	dialer.Timeout = 0
	dialer.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return dialer
}

// sseBufferBody reads a refused response's body into memory so that the caller
// can read it after the connection has been released.
func sseBufferBody(response *http.Response) *http.Response {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	return response
}

// sseReadBufferSize is the buffer a stream is read through. Events are small
// and arrive one at a time, so this is sized for a handful of them rather than
// for throughput.
const sseReadBufferSize = 4 << 10

// SSEReader reads the events of one stream.
//
// A reader is not safe for concurrent use: the events of a stream arrive in
// order and one loop should consume them. It is safe to [SSEReader.Close] from
// another goroutine, which is what ends a read that is waiting.
type SSEReader struct {
	body io.ReadCloser
	br   *bufio.Reader

	readLimit    int64
	readTimeout  time.Duration
	keepComments bool

	// line is where one line is assembled, and skipLF records a carriage
	// return whose line feed had not arrived yet, so that a CRLF split across
	// two reads is still one line ending. started marks the first read, which
	// is the only one that may find a byte order mark.
	line    []byte
	skipLF  bool
	started bool

	// data, name and dataSeen are the event being assembled. They live here
	// rather than in Next so that a comment delivered in the middle of an
	// event does not lose the fields already read.
	data     []byte
	name     string
	dataSeen bool

	// begun is called by readLine as soon as the first byte of a line has
	// arrived, which is what starts the clock on an event. It is set by the
	// call reading at the time, and there is only ever one.
	begun func()

	// lastEventID persists across events, exactly as it does in a browser: an
	// event without an id of its own carries the last one that arrived.
	lastEventID string
	retry       time.Duration

	closeOnce sync.Once
	mu        sync.Mutex
	err       error
}

// newSSEReader wraps a stream body.
func newSSEReader(body io.ReadCloser, opts SSEDialOptions) *SSEReader {
	limit := opts.ReadLimit
	if limit <= 0 {
		limit = DefaultSSEReadLimit
	}
	return &SSEReader{
		body:         body,
		br:           bufio.NewReaderSize(body, sseReadBufferSize),
		readLimit:    limit,
		readTimeout:  orDefaultDuration(opts.ReadTimeout, DefaultSSEReadTimeout),
		keepComments: opts.KeepComments,
		lastEventID:  opts.LastEventID,
	}
}

// LastEventID returns the identifier of the last event that carried one, which
// is what a reconnecting reader passes to [SSEDialOptions.LastEventID] to
// resume where this one left off.
func (r *SSEReader) LastEventID() string { return r.lastEventID }

// Retry returns the reconnection delay the server asked for, or zero when it
// asked for none. A reader does not reconnect on its own; this is the value to
// wait before opening the stream again. It is never negative and never more
// than an hour, however much the server asked for, because the value is the
// server's to choose and a client sleeps for it.
func (r *SSEReader) Retry() time.Duration { return r.retry }

// Close ends the stream and releases the connection. It is safe to call more
// than once and from more than one goroutine, and it releases a [SSEReader.Next]
// that is waiting.
func (r *SSEReader) Close() error {
	var err error
	r.closeOnce.Do(func() { err = r.body.Close() })
	return err
}

// Next returns the next event, blocking until one arrives.
//
// A stream that has ended reports [ErrSSEStreamEnded], which is what a read
// loop stops on, whether the server finished, the connection dropped or ctx
// was cancelled. Comments are skipped unless
// [SSEDialOptions.KeepComments] asked for them.
//
// An event with no data at all is not delivered, because a browser does not
// dispatch one either: such an event exists to carry an identifier or a
// reconnection delay, both of which are applied to the reader instead.
func (r *SSEReader) Next(ctx context.Context) (SSEMessage, error) {
	// Cancelling ends the stream rather than the read alone, because a
	// half-read event cannot be resumed and the connection would be left
	// holding one.
	stop := context.AfterFunc(ctx, func() { r.abort(endedBy("the read was cancelled", context.Cause(ctx))) })
	defer stop()

	// The clock starts with the event rather than with the call, so a stream
	// may idle between events for as long as it likes and an event may not
	// take as long as it likes to finish arriving.
	var clock *time.Timer
	idle := func() {
		if clock != nil {
			clock.Stop()
			clock = nil
		}
	}
	defer idle()
	if r.readTimeout > 0 {
		r.begun = func() {
			if clock == nil {
				clock = time.AfterFunc(r.readTimeout, func() {
					r.abort(ended("the event did not arrive within the time allowed"))
				})
			}
		}
		defer func() { r.begun = nil }()
	}

	for {
		line, err := r.readLine()
		if err != nil {
			return SSEMessage{}, r.failed(err)
		}
		switch {
		case len(line) == 0:
			if message, dispatched := r.dispatch(); dispatched {
				return message, nil
			}
		case line[0] == ':':
			if r.keepComments {
				return SSEMessage{Comment: sseFieldValue(string(line[1:]))}, nil
			}
		default:
			r.field(line)
		}
		if !r.dataSeen && r.name == "" {
			// Nothing of an event is pending, so the wait for the next one is
			// a wait rather than an event taking its time.
			idle()
		}
	}
}

// dispatch completes the event being assembled, reporting whether there was
// one to deliver.
//
// An event with no data is not delivered, which is what the HTML specification
// says a browser does with one: the buffers are cleared and the stream carries
// on, with whatever identifier or reconnection delay it carried already
// applied.
func (r *SSEReader) dispatch() (SSEMessage, bool) {
	name, data, seen := r.name, r.data, r.dataSeen
	r.name, r.data, r.dataSeen = "", r.data[:0], false
	if !seen {
		return SSEMessage{}, false
	}
	// Every data line was appended with a newline, and the last of them is not
	// part of the payload.
	return SSEMessage{Name: name, ID: r.lastEventID, Data: string(data[:len(data)-1])}, true
}

// field applies one field line to the event being assembled. A field this
// reader does not know is ignored, which is what the format asks for and what
// keeps a server free to add one.
func (r *SSEReader) field(line []byte) {
	name, rest, _ := strings.Cut(string(line), ":")
	value := sseFieldValue(rest)
	switch name {
	case "event":
		r.name = value
	case "data":
		r.data = append(append(r.data, value...), '\n')
		r.dataSeen = true
	case "id":
		if !strings.ContainsRune(value, 0) {
			// An identifier with a null byte in it is dropped rather than
			// stored, which is the one rule the format states about it.
			r.lastEventID = value
		}
	case "retry":
		if delay, ok := sseParseRetry(value); ok {
			r.retry = delay
		}
	}
}

// sseMaxRetry is the longest reconnection delay a reader will report, however
// much longer a server asks for. The delay is whatever the server chooses, and
// a caller sleeps for what Retry returns, so an unbounded value is a server
// deciding that a client never comes back, and one large enough to overflow a
// duration would wrap to a negative sleep and a reconnection loop with no
// pause at all.
const sseMaxRetry = time.Hour

// sseParseRetry reads a retry field's value, which the format defines as digits
// and nothing else: a sign, a space or an exponent makes it not a delay, and the
// field is ignored. A value past sseMaxRetry is reported as sseMaxRetry rather
// than ignored, since a server that asked for a long wait is better obeyed as
// far as is safe than taken to have asked for none.
func sseParseRetry(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	const most = uint64(sseMaxRetry / time.Millisecond)
	var milliseconds uint64
	for i := range len(value) {
		digit := value[i]
		if digit < '0' || digit > '9' {
			return 0, false
		}
		if milliseconds > most {
			// Already past the cap, and every digit that follows only makes it
			// larger, so there is no need to keep multiplying into an overflow.
			continue
		}
		milliseconds = milliseconds*10 + uint64(digit-'0')
	}
	return time.Duration(min(milliseconds, most)) * time.Millisecond, true
}

// sseFieldValue removes the single optional space that follows a field's
// colon, which is written by convention and is not part of the value.
func sseFieldValue(rest string) string {
	return strings.TrimPrefix(rest, " ")
}

// failed turns a read failure into the error the caller sees, preferring the
// reason this reader recorded over whatever the closed connection reported.
func (r *SSEReader) failed(err error) error {
	r.mu.Lock()
	recorded := r.err
	r.mu.Unlock()
	if recorded != nil {
		return recorded
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return endedBy("the server closed the stream", err)
	}
	return endedBy("reading the stream failed", err)
}

// abort records why the stream is ending and closes it, which is what wakes a
// read that is waiting.
func (r *SSEReader) abort(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	_ = r.Close()
}

// errSSEEventTooLarge reports an event larger than the reader accepts.
var errSSEEventTooLarge = errors.New("muzak: the event exceeds the reader's limit")

// readLine returns the next line of the stream without its terminator.
//
// All three of the line endings the format defines are understood, including a
// carriage return whose line feed lands in the next read, which is what the
// skipLF flag is for. Lines are taken from what the buffer already holds
// rather than a byte at a time, and the length of one is counted against the
// reader's limit as it is read, so a server that never ends a line cannot make
// this process grow.
func (r *SSEReader) readLine() ([]byte, error) {
	r.line = r.line[:0]
	length := int64(0)
	for {
		next, err := r.br.Peek(1)
		if err != nil {
			return nil, err
		}
		if r.skipLF {
			r.skipLF = false
			if next[0] == '\n' {
				_, _ = r.br.Discard(1)
				continue
			}
		}
		if r.begun != nil {
			// Something of a line has arrived, so whatever it belongs to has
			// begun and is now on the clock.
			r.begun()
		}
		if !r.started {
			r.started = true
			if next[0] == 0xEF {
				// A stream may open with a byte order mark, which is not part
				// of the first field's name and would otherwise make that
				// field one this reader does not know.
				if mark, err := r.br.Peek(3); err == nil && string(mark) == "\ufeff" {
					_, _ = r.br.Discard(3)
					continue
				}
			}
		}
		chunk, _ := r.br.Peek(r.br.Buffered())
		end := sseLineEnd(chunk)
		if end < 0 {
			length += int64(len(chunk))
			if length > r.readLimit {
				return nil, errSSEEventTooLarge
			}
			r.line = append(r.line, chunk...)
			_, _ = r.br.Discard(len(chunk))
			continue
		}
		length += int64(end)
		if length > r.readLimit {
			return nil, errSSEEventTooLarge
		}
		r.line = append(r.line, chunk[:end]...)
		_, _ = r.br.Discard(end + 1)
		if chunk[end] == '\r' {
			if end+1 < len(chunk) {
				if chunk[end+1] == '\n' {
					_, _ = r.br.Discard(1)
				}
			} else {
				// The line feed of a CRLF may not have arrived yet, and
				// waiting for it here would block on a stream whose last line
				// ended with a bare carriage return.
				r.skipLF = true
			}
		}
		if int64(len(r.data))+length > r.readLimit {
			// The event as a whole is bounded too, so that many short lines
			// cannot do what one long line is refused for.
			return nil, errSSEEventTooLarge
		}
		return r.line, nil
	}
}

// sseLineEnd returns the index of the first line ending in b, or -1.
func sseLineEnd(b []byte) int {
	for i := range len(b) {
		if b[i] == '\n' || b[i] == '\r' {
			return i
		}
	}
	return -1
}
