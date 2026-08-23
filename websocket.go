package muzak

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"muzak.dev/framework/internal/wsframe"
)

// WSMessageType says what a WebSocket message carries.
type WSMessageType uint8

const (
	// WSText is a message whose payload is UTF-8 text. The protocol requires
	// the encoding, so a text message is validated on the way in and on the
	// way out.
	WSText WSMessageType = WSMessageType(wsframe.Text)
	// WSBinary is a message whose payload is arbitrary bytes.
	WSBinary WSMessageType = WSMessageType(wsframe.Binary)
)

// String names the message type, for logs and error messages.
func (t WSMessageType) String() string {
	switch t {
	case WSText:
		return "text"
	case WSBinary:
		return "binary"
	default:
		return "unknown message type " + strconv.Itoa(int(t))
	}
}

// WSStatus is a WebSocket close status code, the two byte number that says why
// a connection ended.
//
// The codes below 3000 are the ones RFC 6455 defines. Everything from 3000 to
// 3999 is registered by libraries and everything from 4000 to 4999 is free for
// an application to define, which is where a status meaningful to one protocol
// rather than to WebSocket itself belongs.
type WSStatus uint16

// The close status codes defined by RFC 6455.
//
// Three of them describe a local observation rather than something a peer
// said: [WSStatusNoStatusReceived], [WSStatusAbnormalClosure] and
// [WSStatusTLSHandshake] are reported by Muzak but are never written to the
// wire, and passing one to [WSConn.Close] closes without a status code.
const (
	// WSStatusNormalClosure reports a connection closed because whatever it
	// was opened for is finished. It is what Muzak sends when a handler
	// returns without an error.
	WSStatusNormalClosure WSStatus = 1000
	// WSStatusGoingAway reports an endpoint that is disappearing, such as a
	// server shutting down or a browser navigating away.
	WSStatusGoingAway WSStatus = 1001
	// WSStatusProtocolError reports a frame that breaks the protocol.
	WSStatusProtocolError WSStatus = 1002
	// WSStatusUnsupportedData reports a message of a type the endpoint cannot
	// accept, such as binary where only text is understood.
	WSStatusUnsupportedData WSStatus = 1003
	// WSStatusNoStatusReceived reports a close frame that carried no code.
	WSStatusNoStatusReceived WSStatus = 1005
	// WSStatusAbnormalClosure reports a connection lost without a close frame,
	// which is what a dropped network or a killed process looks like.
	WSStatusAbnormalClosure WSStatus = 1006
	// WSStatusInvalidFramePayload reports a payload that is not what its type
	// promised, such as a text message that is not valid UTF-8.
	WSStatusInvalidFramePayload WSStatus = 1007
	// WSStatusPolicyViolation reports a message refused on policy grounds,
	// which is the general code for a rejection with no more specific one.
	WSStatusPolicyViolation WSStatus = 1008
	// WSStatusMessageTooBig reports a message larger than the receiver accepts.
	WSStatusMessageTooBig WSStatus = 1009
	// WSStatusMandatoryExtension reports a client giving up because the server
	// declined an extension it required.
	WSStatusMandatoryExtension WSStatus = 1010
	// WSStatusInternalError reports a condition that stopped the endpoint from
	// fulfilling the request. It is what Muzak sends when a handler returns an
	// error or panics.
	WSStatusInternalError WSStatus = 1011
	// WSStatusServiceRestart reports a server restarting.
	WSStatusServiceRestart WSStatus = 1012
	// WSStatusTryAgainLater reports a server refusing because it is overloaded.
	WSStatusTryAgainLater WSStatus = 1013
	// WSStatusBadGateway reports a gateway that received an invalid response
	// from the server it forwards to.
	WSStatusBadGateway WSStatus = 1014
	// WSStatusTLSHandshake reports a TLS handshake that failed.
	WSStatusTLSHandshake WSStatus = 1015
)

// String renders the status as its number and, for the codes the protocol
// defines, its meaning.
func (s WSStatus) String() string {
	name := ""
	switch s {
	case WSStatusNormalClosure:
		name = "normal closure"
	case WSStatusGoingAway:
		name = "going away"
	case WSStatusProtocolError:
		name = "protocol error"
	case WSStatusUnsupportedData:
		name = "unsupported data"
	case WSStatusNoStatusReceived:
		name = "no status received"
	case WSStatusAbnormalClosure:
		name = "abnormal closure"
	case WSStatusInvalidFramePayload:
		name = "invalid frame payload"
	case WSStatusPolicyViolation:
		name = "policy violation"
	case WSStatusMessageTooBig:
		name = "message too big"
	case WSStatusMandatoryExtension:
		name = "mandatory extension"
	case WSStatusInternalError:
		name = "internal error"
	case WSStatusServiceRestart:
		name = "service restart"
	case WSStatusTryAgainLater:
		name = "try again later"
	case WSStatusBadGateway:
		name = "bad gateway"
	case WSStatusTLSHandshake:
		name = "TLS handshake"
	}
	if name == "" {
		return strconv.FormatUint(uint64(s), 10)
	}
	return strconv.FormatUint(uint64(s), 10) + " " + name
}

// WSCloseError reports a WebSocket connection that has ended.
//
// Every read and every write returns one once the connection is finished, so a
// handler's loop ends on the first error it sees whether the peer closed
// politely, broke the protocol or vanished:
//
//	for {
//		msg, err := conn.ReadText(ctx.Context())
//		if err != nil {
//			return nil
//		}
//		// ...
//	}
//
// Use [WSCloseStatus] to find out which of those happened when it matters.
type WSCloseError struct {
	// Status is the close status code. It is [WSStatusNoStatusReceived] for a
	// close frame that carried no code, and [WSStatusAbnormalClosure] for a
	// connection lost without one.
	Status WSStatus
	// Reason is the human-readable explanation that accompanied the status,
	// which is often empty.
	Reason string

	// cause is the underlying transport failure, when there was one. It is
	// unexported because it names local conditions a peer never sent.
	cause error
}

// Error implements the error interface.
func (e *WSCloseError) Error() string {
	message := "muzak: the websocket connection was closed with status " + e.Status.String()
	if e.Reason != "" {
		message += ": " + e.Reason
	}
	if e.cause != nil {
		message += ": " + e.cause.Error()
	}
	return message
}

// Unwrap exposes the transport failure behind an abnormal closure, so that a
// caller can test for [io.EOF] or a network error with [errors.Is].
func (e *WSCloseError) Unwrap() error { return e.cause }

// WSCloseStatus returns the status a WebSocket connection was closed with, and
// reports whether err describes a closure at all.
//
// It is how a handler tells a polite goodbye from a protocol violation:
//
//	if status, ok := muzak.WSCloseStatus(err); ok && status == muzak.WSStatusNormalClosure {
//		return nil
//	}
func WSCloseStatus(err error) (WSStatus, bool) {
	var closed *WSCloseError
	if errors.As(err, &closed) {
		return closed.Status, true
	}
	return 0, false
}

// wsScratchSize is how much of a payload is copied into the frame buffer so
// that a small message goes out in one write. It is also the chunk size a
// client masks with, which is what keeps the buffer for a large message from
// growing to the size of the message.
const wsScratchSize = 4 << 10

// WSConn is one WebSocket connection.
//
// A handler receives a connection that is already open: the handshake
// succeeded before the handler was called, and the connection is closed for it
// when the handler returns. Reading and writing are message oriented, so a
// message that arrived in several frames is delivered once, whole.
//
// # Concurrency
//
// Writes are serialized, so any number of goroutines may write to the same
// connection and each message goes out intact. Reads are serialized too, but a
// second reader is rarely what is wanted: the messages of one connection
// arrive in order and one loop should consume them.
//
// # Failure
//
// A connection is single use. The first failure, whether a protocol violation,
// a lost transport or a cancelled operation, ends it, and every later call
// returns that same error rather than trying to carry on with a stream whose
// position is no longer known.
type WSConn struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader
	// nc is the underlying network connection when there is one, which is what
	// makes deadlines available. It is nil for a connection that reached the
	// peer through an [net/http.Client].
	nc net.Conn

	client       bool
	subprotocol  string
	readLimit    int64
	readTimeout  time.Duration
	writeTimeout time.Duration
	closeGrace   time.Duration

	// readSem and writeSem serialize the two halves of the connection. They
	// are channels rather than mutexes so that waiting for one can be
	// abandoned when the caller's context is cancelled.
	readSem  chan struct{}
	writeSem chan struct{}

	// control is where a control frame's payload is read, and scratch is where
	// an outgoing frame is assembled. Each is owned by the half of the
	// connection that holds the matching semaphore.
	control [wsframe.MaxControlPayload]byte
	scratch []byte

	// lastPong records when the peer last answered a ping, in Unix
	// nanoseconds, for the keepalive to compare against.
	lastPong atomic.Int64

	// messages bounds how fast the peer may send, and is nil unless
	// [WSOptions.MessageLimits] asked for a bound. It is written once, before
	// the handler can reach the connection, and only read afterwards.
	messages *wsMessageLimiter

	// done is closed when the connection fails, which is what lets a goroutine
	// waiting for a semaphore give up.
	done chan struct{}

	// watched is the request's context, and unwatch cancels the arrangement
	// that ends the connection when it is. Both are written before any other
	// goroutine can reach the connection and only read afterwards.
	watched context.Context
	unwatch func() bool

	mu        sync.Mutex
	err       error
	sentClose bool
}

// newWSConn builds a connection over an already upgraded transport.
func newWSConn(rwc io.ReadWriteCloser, br *bufio.Reader, client bool, subprotocol string, opts WSOptions) *WSConn {
	c := &WSConn{
		rwc:          rwc,
		br:           br,
		client:       client,
		subprotocol:  subprotocol,
		readLimit:    opts.ReadLimit,
		readTimeout:  opts.ReadTimeout,
		writeTimeout: opts.WriteTimeout,
		closeGrace:   opts.CloseGracePeriod,
		readSem:      make(chan struct{}, 1),
		writeSem:     make(chan struct{}, 1),
		scratch:      make([]byte, 0, wsframe.MaxHeaderSize+wsScratchSize),
		done:         make(chan struct{}),
	}
	if conn, ok := rwc.(net.Conn); ok {
		c.nc = conn
	}
	c.lastPong.Store(time.Now().UnixNano())
	return c
}

// Subprotocol returns the subprotocol negotiated during the handshake, or the
// empty string when none was.
func (c *WSConn) Subprotocol() string { return c.subprotocol }

// Read reads the next complete message from the connection.
//
// Fragmentation is invisible: a message split across several frames is
// reassembled and returned once, and control frames that arrive in between are
// handled without interrupting the message. A ping is answered automatically,
// and a close is answered and then reported as a *[WSCloseError].
//
// The returned slice belongs to the caller and is not reused, so it may be
// kept for as long as it is useful. A message larger than the connection's
// read limit is refused with [WSStatusMessageTooBig] before any of it is
// buffered, which is what keeps a peer from choosing how much memory the
// server spends. A peer sending faster than [WSOptions.MessageLimits] allows
// is closed rather than read from again.
func (c *WSConn) Read(ctx context.Context) (WSMessageType, []byte, error) {
	if err := c.acquire(ctx, c.readSem); err != nil {
		return 0, nil, err
	}
	defer c.release(c.readSem)
	typ, payload, err := c.readMessage(ctx)
	if err != nil || c.messages == nil {
		return typ, payload, err
	}
	// The message is counted once it is whole, so the count measures the rate
	// a peer sustains rather than refusing the one message that crossed the
	// line. The message itself is dropped along with the connection.
	if status, reason := c.messages.allow(ctx); status != 0 {
		return 0, nil, c.abort(status, reason)
	}
	return typ, payload, nil
}

// ReadText reads the next message and returns it as text.
//
// A binary message is refused with [WSStatusUnsupportedData], because a
// handler that asked for text has no way to make sense of one.
func (c *WSConn) ReadText(ctx context.Context) (string, error) {
	typ, payload, err := c.Read(ctx)
	if err != nil {
		return "", err
	}
	if typ != WSText {
		return "", c.abort(WSStatusUnsupportedData, "this connection accepts text messages only")
	}
	return string(payload), nil
}

// ReadBinary reads the next message and returns its bytes.
//
// A text message is refused with [WSStatusUnsupportedData], for the same
// reason [WSConn.ReadText] refuses a binary one.
func (c *WSConn) ReadBinary(ctx context.Context) ([]byte, error) {
	typ, payload, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != WSBinary {
		return nil, c.abort(WSStatusUnsupportedData, "this connection accepts binary messages only")
	}
	return payload, nil
}

// ReadJSON reads the next message and decodes it into target.
//
// The message must be text, and it is decoded with the same rules a request
// body is: unknown members, duplicate members and invalid UTF-8 are all
// rejected. A message that does not decode closes the connection with
// [WSStatusInvalidFramePayload], because a peer sending malformed JSON on a
// JSON connection is not going to be understood by carrying on.
func (c *WSConn) ReadJSON(ctx context.Context, target any) error {
	typ, payload, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != WSText {
		return c.abort(WSStatusUnsupportedData, "this connection accepts JSON text messages only")
	}
	if err := json.Unmarshal(payload, target, json.RejectUnknownMembers(true)); err != nil {
		return c.abort(WSStatusInvalidFramePayload, "the message is not the JSON this connection expects")
	}
	return nil
}

// Write sends one message.
//
// The whole message goes out as a single frame, and the payload is not
// retained, so the caller may reuse the slice as soon as Write returns. A text
// message must be valid UTF-8; one that is not is refused before anything
// reaches the wire, since sending it would oblige the peer to close the
// connection.
func (c *WSConn) Write(ctx context.Context, typ WSMessageType, payload []byte) error {
	switch typ {
	case WSText:
		if !utf8.Valid(payload) {
			return errWSNotText
		}
		return wsSend(c, ctx, wsframe.Text, payload)
	case WSBinary:
		return wsSend(c, ctx, wsframe.Binary, payload)
	default:
		return fmt.Errorf("muzak: %s cannot be sent; use muzak.WSText or muzak.WSBinary", typ)
	}
}

// WriteText sends a text message.
func (c *WSConn) WriteText(ctx context.Context, message string) error {
	if !utf8.ValidString(message) {
		return errWSNotText
	}
	// The string is written as it stands. A Go string is immutable, so nothing
	// has to be copied to keep the frame stable while it goes out.
	return wsSend(c, ctx, wsframe.Text, message)
}

// WriteBinary sends a binary message.
func (c *WSConn) WriteBinary(ctx context.Context, payload []byte) error {
	return wsSend(c, ctx, wsframe.Binary, payload)
}

// WriteJSON encodes value as JSON and sends it as a text message.
func (c *WSConn) WriteJSON(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("muzak: encoding a websocket message: %w", err)
	}
	return wsSend(c, ctx, wsframe.Text, encoded)
}

// errWSNotText reports a text message that is not valid UTF-8. The protocol
// requires the encoding, so sending one would only oblige the peer to close
// the connection with a protocol error.
var errWSNotText = errors.New("muzak: a text websocket message must be valid UTF-8")

// errWSClosing reports something sent after this end had already said goodbye.
// It is compared by identity, so it must stay a single value.
var errWSClosing = &WSCloseError{Status: WSStatusNormalClosure, Reason: "the connection is closing"}

// Ping sends a ping and returns once it is on the wire.
//
// It does not wait for the answer, because the answer arrives through
// [WSConn.Read] like everything else the peer sends. Set
// [WSOptions.PingInterval] for a connection that should be checked
// periodically rather than by hand.
func (c *WSConn) Ping(ctx context.Context) error {
	return wsSend(c, ctx, wsframe.Ping, []byte(nil))
}

// Close closes the connection with a status and a reason.
//
// Muzak closes the connection when the handler returns, so calling Close is
// only necessary to choose a status other than [WSStatusNormalClosure] or to
// end the connection from another goroutine. It is safe to call more than once
// and from more than one goroutine; only the first call sends anything, and a
// connection that has already ended reports success.
//
// The reason is truncated to the 123 bytes a close frame can carry, on a rune
// boundary. A status that describes a local observation rather than something
// worth telling the peer, such as [WSStatusAbnormalClosure], closes without a
// status code.
func (c *WSConn) Close(status WSStatus, reason string) error {
	if c.failure() != nil {
		// The connection has already ended, so there is nothing left to do and
		// nothing to report: closing what is closed is what "again" means.
		return nil //nolint:nilerr // see above
	}
	err := c.sendClose(status, reason)
	c.drain()
	_ = c.fail(&WSCloseError{Status: status, Reason: reason})
	return err
}

// acquire takes one of the connection's semaphores, giving up if the caller's
// context is cancelled or the connection fails while it waits.
func (c *WSConn) acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
	default:
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return fmt.Errorf("muzak: waiting for the websocket connection: %w", ctx.Err())
		case <-c.done:
			return c.failure()
		}
	}
	// The connection may have failed before this call arrived, or while it was
	// waiting for its turn. Either way the semaphore has to go back, or
	// nothing could close the connection afterwards.
	if err := c.failure(); err != nil {
		c.release(sem)
		return err
	}
	return nil
}

// release returns a semaphore.
func (c *WSConn) release(sem chan struct{}) { <-sem }

// failure returns the error that ended the connection, or nil while it is
// still usable.
func (c *WSConn) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// record stores the first failure and wakes anything waiting on the
// connection, leaving the transport alone. Later calls keep the error already
// recorded, because the first one is the one that explains what happened.
func (c *WSConn) record(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
	return c.err
}

// fail records a failure and closes the transport, which is what every path
// that ends a connection does.
func (c *WSConn) fail(err error) error {
	recorded := c.record(err)
	_ = c.rwc.Close()
	return recorded
}

// abort sends a close frame for a violation this end detected and then fails
// the connection, returning the error every later call will report.
func (c *WSConn) abort(status WSStatus, reason string) error {
	_ = c.sendClose(status, reason)
	return c.fail(&WSCloseError{Status: status, Reason: reason})
}

// wsMaxFramesPerMessage bounds how many frames may arrive before one message is
// complete.
//
// Without it a peer can hold a read open forever without ever reaching the read
// limit, by fragmenting a message into empty continuation frames or by
// interleaving an endless run of pings, neither of which grows the message it
// is supposedly sending. The bound is generous enough that no honest peer meets
// it and low enough that a dishonest one is cut off.
const wsMaxFramesPerMessage = 1 << 16

// wsReadChunk bounds how much of a frame is committed to memory before the
// bytes for it have actually arrived.
//
// Reading a declared length in one go would let six bytes of header buy an
// allocation the size of the whole read limit, which is the cheapest denial of
// service a WebSocket offers. Growing in chunks means a peer pays for the
// memory it asks for in bytes it has to send.
const wsReadChunk = 32 << 10

// readMessage reads frames until one message is complete.
func (c *WSConn) readMessage(ctx context.Context) (WSMessageType, []byte, error) {
	stop := c.armRead(ctx)
	defer stop()

	message := []byte{}
	var typ WSMessageType
	started := false
	for frames := 0; ; frames++ {
		if frames == wsMaxFramesPerMessage {
			return 0, nil, c.abort(WSStatusPolicyViolation,
				"too many frames arrived before a message was complete")
		}
		header, err := wsframe.ReadHeader(c.br)
		if err != nil {
			return 0, nil, c.readFailed(ctx, err)
		}
		if header.Masked == c.client {
			return 0, nil, c.abort(WSStatusProtocolError, c.maskingRule())
		}
		if header.Opcode.IsControl() {
			if err := c.handleControl(ctx, header); err != nil {
				return 0, nil, err
			}
			continue
		}
		if header.Opcode == wsframe.Continuation {
			if !started {
				return 0, nil, c.abort(WSStatusProtocolError, "a continuation frame arrived with no message to continue")
			}
		} else {
			if started {
				return 0, nil, c.abort(WSStatusProtocolError, "a new message began before the previous one finished")
			}
			started = true
			typ = WSMessageType(header.Opcode)
			// The clock starts with the message rather than with the call, so
			// that a connection may wait for as long as it likes and a message
			// may not take as long as it likes to arrive.
			c.startMessageClock(ctx)
		}

		if int64(len(message))+header.Length > c.readLimit {
			return 0, nil, c.abort(WSStatusMessageTooBig,
				"the message exceeds the "+strconv.FormatInt(c.readLimit, 10)+" byte limit for this connection")
		}
		if message, err = c.readPayload(ctx, header, message); err != nil {
			return 0, nil, err
		}
		if header.Fin {
			break
		}
	}
	if typ == WSText && !utf8.Valid(message) {
		return 0, nil, c.abort(WSStatusInvalidFramePayload, "the text message is not valid UTF-8")
	}
	return typ, message, nil
}

// readPayload appends one frame's payload to the message being assembled,
// unmasking it as it goes.
//
// The payload is taken a chunk at a time rather than in one allocation the size
// of the declared length, so a header that promises more than the peer intends
// to send costs no more than the chunk it has reached.
func (c *WSConn) readPayload(ctx context.Context, header wsframe.Header, message []byte) ([]byte, error) {
	position := 0
	for remaining := header.Length; remaining > 0; {
		chunk := int(min(remaining, wsReadChunk))
		start := len(message)
		message = slices.Grow(message, chunk)[:start+chunk]
		if _, err := io.ReadFull(c.br, message[start:]); err != nil {
			return nil, c.readFailed(ctx, err)
		}
		if header.Masked {
			position = wsframe.Mask(header.Mask, position, message[start:])
		}
		remaining -= int64(chunk)
	}
	return message, nil
}

// startMessageClock tightens the read deadline once a message has begun, which
// is what stops a peer dribbling one out a byte at a time while a goroutine
// waits on it.
//
// A caller's own deadline is left alone when it is already the tighter of the
// two, because the caller asked for it.
func (c *WSConn) startMessageClock(ctx context.Context) {
	if c.nc == nil || c.readTimeout <= 0 {
		return
	}
	deadline := time.Now().Add(c.readTimeout)
	if fromCtx, ok := ctx.Deadline(); ok && !fromCtx.After(deadline) {
		return
	}
	_ = c.nc.SetReadDeadline(deadline)
}

// maskingRule states which end is at fault for an incorrectly masked frame.
func (c *WSConn) maskingRule() string {
	if c.client {
		return "a frame from the server must not be masked"
	}
	return "a frame from the client must be masked"
}

// handleControl reads and acts on one control frame, which may arrive in the
// middle of a fragmented message.
func (c *WSConn) handleControl(ctx context.Context, header wsframe.Header) error {
	payload := c.control[:header.Length]
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return c.readFailed(ctx, err)
	}
	if header.Masked {
		wsframe.Mask(header.Mask, 0, payload)
	}
	switch header.Opcode {
	case wsframe.Ping:
		err := wsSend(c, ctx, wsframe.Pong, payload)
		if errors.Is(err, errWSClosing) {
			// Nothing more is owed to a peer this end has already said
			// goodbye to, so the ping is simply left unanswered.
			return nil
		}
		return err
	case wsframe.Pong:
		c.lastPong.Store(time.Now().UnixNano())
		return nil
	default:
		status, reason, err := wsframe.ParseClose(payload)
		if err != nil {
			return c.readFailed(ctx, err)
		}
		closure := &WSCloseError{Status: WSStatus(status), Reason: reason}
		// What the peer said is the outcome, recorded before anything is
		// written back. A server is entitled to close the transport the moment
		// its own close frame is sent, so the echo below can fail through no
		// fault of either end, and that must not overwrite the status the peer
		// went to the trouble of sending.
		_ = c.record(closure)
		_ = c.sendClose(WSStatus(status), "")
		return c.fail(closure)
	}
}

// readFailed turns a failure found while reading, whether the peer broke the
// protocol or the transport simply went, into the error the connection will
// report from then on.
func (c *WSConn) readFailed(ctx context.Context, err error) error {
	var protocol *wsframe.Error
	if errors.As(err, &protocol) {
		return c.abort(WSStatus(protocol.Status), protocol.Reason)
	}
	if ctxErr := wsContextFailure(ctx, err); ctxErr != nil {
		return c.fail(fmt.Errorf("muzak: reading a websocket message: %w", ctxErr))
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// The caller's deadline was ruled out above, so the only one left is
		// the time a message is given once it has begun. A peer that has not
		// finished sending one by now is not going to.
		return c.abort(WSStatusPolicyViolation, "the message did not arrive within the time allowed")
	}
	// The connection ended without a close frame, which is what a dropped
	// network or a peer that simply stopped looks like.
	return c.fail(&WSCloseError{Status: WSStatusAbnormalClosure, Reason: "the connection was lost", cause: err})
}

// wsContextFailure reports the caller's context as the reason an operation
// ended, when it is one.
//
// A context deadline is enforced by the socket rather than by a timer of our
// own, and the socket can notice it a moment before the context does, so a
// timeout that lands on or after the context's deadline is attributed to the
// context. The connection's own write timeout cannot be mistaken for it,
// because the deadline applied is always the earlier of the two.
func wsContextFailure(ctx context.Context, err error) error {
	deadline, ok := ctx.Deadline()
	if ok && errors.Is(err, os.ErrDeadlineExceeded) && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}

// wsPayload is what a frame's payload may be given as. A string is accepted so
// that a text message is written straight from the caller's string rather than
// copied into a fresh slice on the way.
type wsPayload interface{ ~[]byte | ~string }

// wsSend sends one whole message or control frame, taking the write semaphore
// first so that concurrent writers cannot interleave their frames.
//
// It is a function rather than a method because it is generic over the payload,
// and every exported writer is a one-line call to it.
func wsSend[T wsPayload](c *WSConn, ctx context.Context, opcode wsframe.Opcode, payload T) error {
	if err := c.acquire(ctx, c.writeSem); err != nil {
		return err
	}
	defer c.release(c.writeSem)
	if err := c.writable(); err != nil {
		return err
	}
	return wsWrite(c, ctx, c.deadline(ctx), opcode, payload)
}

// writable reports whether anything more may be sent, which is what stops a
// message being written after the close frame that ended the conversation.
//
// A connection that has failed outright is caught by [WSConn.acquire] instead,
// which every writer goes through first.
func (c *WSConn) writable() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sentClose {
		return errWSClosing
	}
	return nil
}

// sendClose writes the close frame, at most once for the life of the
// connection.
func (c *WSConn) sendClose(status WSStatus, reason string) error {
	c.mu.Lock()
	if c.sentClose {
		c.mu.Unlock()
		return nil
	}
	c.sentClose = true
	c.mu.Unlock()

	if !wsframe.ValidStatus(uint16(status)) {
		// Three of the defined codes describe a local observation and must
		// never be sent, and an unassigned one would only be refused. Closing
		// with no code at all says the same thing without breaking the rules.
		status = WSStatusNoStatusReceived
		reason = ""
	}
	var payload [wsframe.MaxControlPayload]byte
	frame := wsframe.AppendClose(payload[:0], uint16(status), reason)

	if err := c.acquireClose(); err != nil {
		return err
	}
	defer c.release(c.writeSem)
	// The close frame goes out under its own deadline rather than a caller's
	// context, because a connection being torn down should not be left open by
	// a context that was cancelled a moment ago.
	return wsWrite(c, context.Background(), time.Now().Add(c.closeTimeout()), wsframe.Close, frame)
}

// acquireClose takes the write semaphore for a close frame, waiting no longer
// than the write timeout for a peer that has stopped reading.
func (c *WSConn) acquireClose() error {
	select {
	case c.writeSem <- struct{}{}:
		return nil
	default:
	}
	timeout := c.closeTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case c.writeSem <- struct{}{}:
		return nil
	case <-c.done:
		return c.failure()
	case <-timer.C:
		return fmt.Errorf("muzak: the websocket close frame could not be sent within %s", timeout)
	}
}

// wsWrite assembles and sends one frame. The write semaphore must be held.
func wsWrite[T wsPayload](c *WSConn, ctx context.Context, deadline time.Time, opcode wsframe.Opcode, payload T) error {
	stop := c.armWrite(ctx, deadline)
	defer stop()

	header := wsframe.Header{
		Fin:    true,
		Opcode: opcode,
		Masked: c.client,
		Length: int64(len(payload)),
	}
	if c.client {
		// The masking key has to be unpredictable, or masking stops serving
		// the purpose it exists for.
		if _, err := crand.Read(header.Mask[:]); err != nil {
			// coverage: crypto/rand does not fail on any supported platform;
			// the branch keeps a failure from producing a predictable key.
			return c.fail(fmt.Errorf("muzak: generating a websocket masking key: %w", err))
		}
	}
	buf := wsframe.AppendHeader(c.scratch[:0], header)

	if !c.client {
		if len(payload) <= wsScratchSize {
			// Small enough to go out in one write, which is what most messages
			// and every control frame are.
			return c.flush(ctx, append(buf, payload...))
		}
		if err := c.flush(ctx, buf); err != nil {
			return err
		}
		// A server does not mask, so a large payload goes straight from the
		// caller's own memory with no copy at all.
		return c.flush(ctx, []byte(payload))
	}

	// A client masks what it sends, and the payload belongs to the caller, so
	// it is copied into the scratch buffer a chunk at a time. Chunking is what
	// keeps a large message from needing a buffer as large as itself.
	pos := 0
	for {
		chunk := min(len(payload), wsScratchSize)
		start := len(buf)
		buf = append(buf, payload[:chunk]...)
		pos = wsframe.Mask(header.Mask, pos, buf[start:])
		if err := c.flush(ctx, buf); err != nil {
			return err
		}
		payload = payload[chunk:]
		if len(payload) == 0 {
			return nil
		}
		buf = buf[:0]
	}
}

// flush writes one buffer, recording the failure if the transport refuses it.
// A frame that went out in part leaves the stream at an unknown position, so
// there is nothing to do but end the connection.
func (c *WSConn) flush(ctx context.Context, b []byte) error {
	if _, err := c.rwc.Write(b); err != nil {
		if ctxErr := wsContextFailure(ctx, err); ctxErr != nil {
			return c.fail(fmt.Errorf("muzak: writing a websocket message: %w", ctxErr))
		}
		return c.fail(&WSCloseError{Status: WSStatusAbnormalClosure, Reason: "the connection was lost", cause: err})
	}
	return nil
}

// deadline returns when a write must have finished by, which is the sooner of
// the connection's write timeout and the caller's own deadline. A connection
// whose write timeout was disabled is bounded only by the caller.
func (c *WSConn) deadline(ctx context.Context) time.Time {
	fromCtx, fromCaller := ctx.Deadline()
	if c.writeTimeout <= 0 {
		if fromCaller {
			return fromCtx
		}
		return time.Time{}
	}
	deadline := time.Now().Add(c.writeTimeout)
	if fromCaller && fromCtx.Before(deadline) {
		return fromCtx
	}
	return deadline
}

// closeTimeout bounds the wait for the write half when a close frame has to go
// out. A connection with no write timeout still needs one here, because the
// alternative is a teardown that never finishes.
func (c *WSConn) closeTimeout() time.Duration {
	if c.writeTimeout <= 0 {
		return DefaultWSWriteTimeout
	}
	return c.writeTimeout
}

// armRead applies the caller's deadline to the read and arranges for
// cancellation to interrupt one already in progress, returning the function
// that undoes both.
func (c *WSConn) armRead(ctx context.Context) func() bool {
	if c.nc != nil {
		var deadline time.Time
		if fromCtx, ok := ctx.Deadline(); ok {
			deadline = fromCtx
		}
		_ = c.nc.SetReadDeadline(deadline)
	}
	if c.arranged(ctx) {
		return wsNothingToStop
	}
	if c.nc == nil {
		// Without a network connection there are no deadlines to set, so the
		// only way to interrupt a blocked read is to close the transport.
		return context.AfterFunc(ctx, func() { _ = c.rwc.Close() })
	}
	return context.AfterFunc(ctx, func() { _ = c.nc.SetReadDeadline(time.Now()) })
}

// armWrite is the writing counterpart of armRead.
func (c *WSConn) armWrite(ctx context.Context, deadline time.Time) func() bool {
	if c.nc != nil {
		_ = c.nc.SetWriteDeadline(deadline)
	}
	if c.arranged(ctx) {
		return wsNothingToStop
	}
	if c.nc == nil {
		return context.AfterFunc(ctx, func() { _ = c.rwc.Close() })
	}
	return context.AfterFunc(ctx, func() { _ = c.nc.SetWriteDeadline(time.Now()) })
}

// arranged reports whether a context needs no arrangement of its own.
//
// A context that can never be cancelled needs none, which is what keeps the
// close frames and the keepalive pings off the allocator. Neither does the
// request's own context, because the connection is already watching that one
// for its whole life, and that is the context a handler passes to nearly every
// read and write it makes.
func (c *WSConn) arranged(ctx context.Context) bool {
	return ctx.Done() == nil || ctx == c.watched
}

// wsNothingToStop undoes an arrangement that was never made.
var wsNothingToStop = func() bool { return false }

// watch ends the connection if ctx is cancelled, once, for the whole life of
// the connection.
//
// It must be called before any other goroutine can reach the connection, which
// is what makes the fields it writes safe to read from the handler's own
// goroutines afterwards.
func (c *WSConn) watch(ctx context.Context) {
	if ctx.Done() == nil {
		return
	}
	c.watched = ctx
	c.unwatch = context.AfterFunc(ctx, func() {
		_ = c.fail(&WSCloseError{Status: WSStatusAbnormalClosure, Reason: "the request ended"})
	})
}

// stopWatching undoes what watch arranged, which the handler's own goroutine
// does once it has finished with the connection.
func (c *WSConn) stopWatching() {
	if c.unwatch != nil {
		c.unwatch()
	}
}

// drain reads what the peer has left to say after a close frame was sent, so
// that the transport is closed on an empty receive buffer and the peer sees
// the close rather than a reset connection.
//
// It gives up immediately when another goroutine is reading, because closing
// the transport will wake that goroutine anyway, and it never waits longer
// than the close grace period.
func (c *WSConn) drain() {
	if c.closeGrace <= 0 || c.nc == nil {
		return
	}
	select {
	case c.readSem <- struct{}{}:
		defer c.release(c.readSem)
	default:
		return
	}
	_ = c.nc.SetReadDeadline(time.Now().Add(c.closeGrace))
	for {
		header, err := wsframe.ReadHeader(c.br)
		if err != nil || header.Opcode == wsframe.Close {
			return
		}
		if _, err := io.CopyN(io.Discard, c.br, header.Length); err != nil {
			return
		}
	}
}

// keepalive pings the peer periodically and ends the connection when it stops
// answering, which is how a connection lost without a close frame is noticed
// before the operating system gets round to noticing it.
//
// It runs only when [WSOptions.PingInterval] asks for it, and it stops as soon
// as the connection does.
func (c *WSConn) keepalive(ctx context.Context, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sent := time.Now()
		if err := c.Ping(ctx); err != nil {
			return
		}
		wait := time.NewTimer(timeout)
		select {
		case <-c.done:
			wait.Stop()
			return
		case <-ctx.Done():
			wait.Stop()
			return
		case <-wait.C:
		}
		if c.lastPong.Load() < sent.UnixNano() {
			_ = c.Close(WSStatusPolicyViolation, "the peer did not answer a ping")
			return
		}
	}
}
