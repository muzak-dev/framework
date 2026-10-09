package muzak

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
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
//
// An operation that its context ends is no exception, and the peer is told
// with a close frame wherever the stream allows one: after any read, and
// after a write that had not begun. A write the context interrupts part way
// through a frame ends the connection without one, since a close frame would
// be read as more of the frame. Only a wait for another reader or writer to
// finish gives up without ending anything, because it never touched the
// stream.
type WSConn struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader
	// nc is the underlying network connection when there is one, which is what
	// makes deadlines available. It is nil for a connection that reached the
	// peer through an [net/http.Client], whose switched transport is the body
	// of a response and has no deadlines to set.
	nc net.Conn

	// readExpiry and writeExpiry stand in for the deadlines nc would provide
	// when there is no nc. Each is owned by the half of the connection that
	// holds the matching semaphore, and neither is used when nc is set.
	readExpiry  wsExpiry
	writeExpiry wsExpiry

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

	// heard records when anything last arrived from the peer, a pong or a
	// frame or any part of one, for the keepalive to compare against.
	heard monotonicStamp

	// reading counts the reads in progress, and notReading is the last moment
	// none was. A pong is consumed by a read like any other frame, so the
	// keepalive can only conclude that a peer is gone if a read was waiting for
	// its answer the whole time; see [WSConn.keepalive].
	reading    atomic.Int32
	notReading monotonicStamp

	// spent counts the frames that carried no message, and spentSince is when
	// the count began; see [WSConn.chargeFrame]. Both are owned by the half of
	// the connection that holds readSem.
	spent      int
	spentSince time.Time

	// unread is how much of the frame being read has yet to be read, or -1 when
	// a failure left the position in the stream unknown. It is what lets the
	// closing wait after a refusal skip to the next frame and recognise the
	// peer's answer, and is owned by the half of the connection that holds
	// readSem.
	unread int64

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
	// closing is closed once the goroutine that started sending the close
	// frame has finished with it, so that a second closer can wait for the
	// frame to be out before tearing the transport down under it. It is nil
	// until a close frame is started, and is guarded by mu.
	closing chan struct{}

	// ended cancels the handler's context when the connection ends; see
	// [WSConn.cancelOnEnd]. It is guarded by mu and called at most once.
	ended context.CancelCauseFunc
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
	c.heard.start()
	c.notReading.epoch = c.heard.epoch
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
//
// A read that ctx ends, by its cancellation or its deadline, ends the
// connection, and the peer is told why: a deadline that passed is closed with
// [WSStatusPolicyViolation], which is what a per-read timeout used to bound how
// long a peer may stay silent looks like from its side, and a cancellation
// with [WSStatusGoingAway]. The read returns the context's error once the close
// frame is out and the peer has answered it, or after
// [WSOptions.CloseGracePeriod] at the most. A connection opened with [WSDial]
// has no deadline to interrupt a read with, so a cancellation closes its
// transport there, and no close frame can follow.
func (c *WSConn) Read(ctx context.Context) (WSMessageType, []byte, error) {
	if err := c.acquire(ctx, c.readSem); err != nil {
		return 0, nil, err
	}
	defer c.release(c.readSem)
	typ, payload, err := c.readMessage(ctx)
	if err != nil {
		// The target is declared only once there is an error, because taking
		// its address for errors.As moves it to the heap, and a successful
		// read should cost nothing for a failure it did not have.
		var ended *wsContextError
		if errors.As(err, &ended) {
			// This is done here rather than where the read failed, because by
			// now the arrangement that interrupted it has been undone, and
			// cannot move the deadline of the wait for the peer's answer into
			// the past.
			return 0, nil, c.abandonReading(ended)
		}
		return 0, nil, err
	}
	if c.messages == nil {
		return typ, payload, nil
	}
	// The message is counted once it is whole, so the count measures the rate
	// a peer sustains rather than refusing the one message that crossed the
	// line. The message itself is dropped along with the connection.
	if status, reason := c.messages.allow(ctx); status != 0 {
		return 0, nil, c.abortReading(status, reason)
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
//
// A write whose context has already ended by the time it gets the connection
// sends nothing of the message and ends the connection with
// [WSStatusGoingAway], so the peer is told rather than left to find it gone.
// One that its context interrupts part way through ends the connection
// without a close frame; see [WSConn].
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
	if err == nil {
		c.drain()
	}
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
			// Nothing was sent or read, so the connection is left as it was.
			return &wsContextError{err: fmt.Errorf("muzak: waiting for the websocket connection: %w", ctx.Err())}
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
		if c.ended != nil {
			c.ended(err)
		}
	}
	return c.err
}

// cancelOnEnd arranges for cancel to be called with the connection's error
// when the connection ends, or calls it at once if it already has.
//
// A served connection is hijacked, so net/http no longer cancels the request's
// context when the client goes away: it does that only once the handler
// returns. A handler that waits on ctx.Context() rather than on a read, for
// events to forward from elsewhere for instance, would otherwise go on waiting
// after its peer had left, after the server had closed the connection for
// shutdown, and while the application's components were being stopped under
// it.
func (c *WSConn) cancelOnEnd(cancel context.CancelCauseFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		cancel(c.err)
		return
	}
	c.ended = cancel
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
//
// The transport is not closed the moment the frame is out. A peer that was
// still sending when it was refused has data on its way, and closing a socket
// with unread data in its receive buffer makes the kernel answer with a reset
// instead of an orderly close, which can destroy the close frame in flight
// before the peer has read the status it was sent. So the frame is followed by
// the same wait a close by the handler gets; see [WSConn.linger].
//
// It is for a caller that does not hold the read half, and waits only if
// nothing else is reading. A reader that finds the violation itself uses
// [WSConn.abortReading].
func (c *WSConn) abort(status WSStatus, reason string) error {
	if c.sendClose(status, reason) == nil {
		c.drain()
	}
	return c.fail(&WSCloseError{Status: status, Reason: reason})
}

// abortReading is abort for the goroutine that holds the read half, which is
// where nearly every violation is found, and which therefore may read on.
func (c *WSConn) abortReading(status WSStatus, reason string) error {
	if c.sendClose(status, reason) == nil {
		c.linger()
	}
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

// wsMaxIdleFrames bounds how many pings, pongs and empty fragments a
// connection may receive in [wsIdleFrameWindow], however many messages
// complete in between.
//
// wsMaxFramesPerMessage starts over with every message, so on its own it lets a
// peer that completes one empty message per 65535 frames keep the server
// answering pings for as long as it likes while any message quota sees a
// single message. These are the frames that cost a read, and for a ping a
// write, without carrying anything, so they are counted for the life of the
// connection instead. The limit is the same size as the per message one, which
// no honest peer comes near: a keepalive sends one ping every few seconds.
const wsMaxIdleFrames = wsMaxFramesPerMessage

// wsIdleFrameWindow is how long the count of [wsMaxIdleFrames] runs before it
// starts over, which bounds a peer to about a thousand such frames a second
// for as long as it cares to keep the connection.
const wsIdleFrameWindow = time.Minute

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
	c.reading.Add(1)
	defer func() {
		c.reading.Add(-1)
		c.notReading.mark()
	}()

	message := []byte{}
	var typ WSMessageType
	started := false
	for frames := 0; ; frames++ {
		if frames == wsMaxFramesPerMessage {
			return 0, nil, c.abortReading(WSStatusPolicyViolation,
				"too many frames arrived before a message was complete")
		}
		// The wait for the next frame is kept apart from reading it, so that a
		// read ended while nothing of that frame has arrived leaves the stream
		// at a frame boundary. That is the common way for a read to end early,
		// at a caller's deadline between messages, and the closing wait that
		// follows can then recognise the peer's answer.
		c.unread = 0
		if _, err := c.br.Peek(1); err != nil {
			return 0, nil, c.readFailed(ctx, err)
		}
		// A header cut short or refused leaves the stream somewhere inside it.
		c.unread = -1
		header, err := wsframe.ReadHeader(c.br)
		if err != nil {
			return 0, nil, c.readFailed(ctx, err)
		}
		// Whatever the frame is, a pong among them, its arrival is what the
		// keepalive is asking about.
		c.heard.mark()
		c.unread = header.Length
		if header.Masked == c.client {
			return 0, nil, c.abortReading(WSStatusProtocolError, c.maskingRule())
		}
		if header.Opcode.IsControl() {
			if header.Opcode != wsframe.Close {
				if err := c.chargeFrame(); err != nil {
					return 0, nil, err
				}
			}
			if err := c.handleControl(ctx, header); err != nil {
				return 0, nil, err
			}
			continue
		}
		if header.Length == 0 && (header.Opcode == wsframe.Continuation || !header.Fin) {
			// An empty fragment adds nothing to the message it belongs to. An
			// empty message of a frame's own is a message, which is what
			// MessageLimits counts, and is not charged here.
			if err := c.chargeFrame(); err != nil {
				return 0, nil, err
			}
		}
		if header.Opcode == wsframe.Continuation {
			if !started {
				return 0, nil, c.abortReading(WSStatusProtocolError, "a continuation frame arrived with no message to continue")
			}
		} else {
			if started {
				return 0, nil, c.abortReading(WSStatusProtocolError, "a new message began before the previous one finished")
			}
			started = true
			typ = WSMessageType(header.Opcode)
			// The clock starts with the message rather than with the call, so
			// that a connection may wait for as long as it likes and a message
			// may not take as long as it likes to arrive.
			c.startMessageClock(ctx)
		}

		// The comparison is arranged so that nothing in it can overflow. The
		// sum of what has arrived and what the header declares is the obvious
		// way to write it, but a peer chooses the declared length, up to the
		// largest int64 there is, and adding even one byte already received
		// to that wraps the sum negative and waves the frame through. What
		// has arrived never exceeds the limit, so the room left is never
		// negative and subtracting it is always exact.
		if header.Length > c.readLimit-int64(len(message)) {
			return 0, nil, c.abortReading(WSStatusMessageTooBig,
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
		return 0, nil, c.abortReading(WSStatusInvalidFramePayload, "the text message is not valid UTF-8")
	}
	return typ, message, nil
}

// chargeFrame counts a frame that carried no message against the connection's
// budget, and ends the connection when the budget is spent. The read half must
// be held.
func (c *WSConn) chargeFrame() error {
	now := time.Now()
	if c.spentSince.IsZero() || now.Sub(c.spentSince) >= wsIdleFrameWindow {
		c.spentSince, c.spent = now, 0
	}
	c.spent++
	if c.spent > wsMaxIdleFrames {
		return c.abortReading(WSStatusPolicyViolation, "too many frames arrived that carried no message")
	}
	return nil
}

// readPayload appends one frame's payload to the message being assembled,
// unmasking it as it goes.
//
// The payload is taken a chunk at a time rather than in one allocation the size
// of the declared length, so a header that promises more than the peer intends
// to send costs no more than the chunk it has reached.
//
// Every read that brings part of the payload counts as hearing from the peer.
// A peer part way through a large frame cannot answer a ping until the frame
// is finished, because its pong would have to go out in the middle of it, so
// a keepalive that waited for the pong alone would close a healthy peer whose
// message merely took longer than the pong timeout to arrive. One that stops
// sending part way through is still caught, by the keepalive once nothing has
// arrived for a pong timeout, and by the read timeout in any case.
func (c *WSConn) readPayload(ctx context.Context, header wsframe.Header, message []byte) ([]byte, error) {
	position := 0
	for remaining := header.Length; remaining > 0; {
		chunk := int(min(remaining, wsReadChunk))
		start := len(message)
		message = slices.Grow(message, chunk)[:start+chunk]
		if _, err := io.ReadFull(wsArrivals{c}, message[start:]); err != nil {
			c.unread = -1
			return nil, c.readFailed(ctx, err)
		}
		if header.Masked {
			position = wsframe.Mask(header.Mask, position, message[start:])
		}
		remaining -= int64(chunk)
		c.unread = remaining
	}
	return message, nil
}

// wsArrivals reads a payload through the connection's buffered reader and
// marks the peer as heard from whenever a read brings something.
//
// A chunk is tens of kilobytes, which a slow peer can take longer than a pong
// timeout to fill, so it is each read rather than each chunk that counts. A
// struct holding only a pointer fits in an interface as it stands, so passing
// one to [io.ReadFull] costs no allocation.
type wsArrivals struct{ c *WSConn }

// Read implements [io.Reader].
func (a wsArrivals) Read(p []byte) (int, error) {
	n, err := a.c.br.Read(p)
	if n > 0 {
		a.c.heard.mark()
	}
	return n, err
}

// startMessageClock tightens the read deadline once a message has begun, which
// is what stops a peer dribbling one out a byte at a time while a goroutine
// waits on it.
//
// A caller's own deadline is left alone when it is already the tighter of the
// two, because the caller asked for it.
func (c *WSConn) startMessageClock(ctx context.Context) {
	if c.readTimeout <= 0 {
		return
	}
	deadline := time.Now().Add(c.readTimeout)
	if fromCtx, ok := ctx.Deadline(); ok && !fromCtx.After(deadline) {
		return
	}
	if c.nc == nil {
		// The expiry is disarmed by the function armRead returned, however
		// the read ends, so it cannot outlive the message it was set for.
		c.readExpiry.arm(deadline, c.readExpired)
		return
	}
	_ = c.nc.SetReadDeadline(deadline)
}

// wsReadTimeoutReason is the close reason for a message that began and then
// stopped arriving, whichever way the connection enforces the read timeout.
const wsReadTimeoutReason = "the message did not arrive within the time allowed"

// readExpired ends a connection whose message clock ran out on a transport
// with no deadlines of its own.
//
// It runs on a timer while the reader is still blocked, so it does what
// [WSConn.readFailed] would have done for a deadline in the reader's place:
// the peer is told why with a close frame, and only then is the transport
// closed, which is the one way to wake a read that has no deadline. The
// outcome is recorded first, because the reader wakes to a closed transport
// and would otherwise report the closure as a lost connection rather than as
// the timeout it was.
func (c *WSConn) readExpired() {
	closure := &WSCloseError{Status: WSStatusPolicyViolation, Reason: wsReadTimeoutReason}
	_ = c.record(closure)
	_ = c.sendClose(closure.Status, closure.Reason)
	_ = c.fail(closure)
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
		c.unread = -1
		return c.readFailed(ctx, err)
	}
	c.unread = 0
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
		// Its arrival was marked with its header, which is all the keepalive
		// needs to know about it.
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
		if c.sendCloseAfterFailure(WSStatus(status), "") == nil && c.client {
			// A server closes the TCP connection first; see [WSConn.linger].
			c.awaitHangUp()
		}
		return c.fail(closure)
	}
}

// readFailed turns a failure found while reading, whether the peer broke the
// protocol or the transport simply went, into the error the connection will
// report from then on.
func (c *WSConn) readFailed(ctx context.Context, err error) error {
	var protocol *wsframe.Error
	if errors.As(err, &protocol) {
		return c.abortReading(WSStatus(protocol.Status), protocol.Reason)
	}
	if ctxErr := wsContextFailure(ctx, err); ctxErr != nil {
		// Not recorded yet: [WSConn.Read] says goodbye first, once the
		// arrangement that interrupted the read has been undone.
		return &wsContextError{err: fmt.Errorf("muzak: reading a websocket message: %w", ctxErr)}
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// The caller's deadline was ruled out above, so the only one left is
		// the time a message is given once it has begun. A peer that has not
		// finished sending one by now is not going to.
		return c.abortReading(WSStatusPolicyViolation, wsReadTimeoutReason)
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

// wsContextError reports an operation on a connection that the caller's own
// context ended, by its cancellation or its deadline.
//
// It is a type of its own so that a handler returning it, which is what a
// handler loop does with any error, is told apart from one that failed: the
// handler's own context ended the conversation, which is not something to log
// as a failure. A deadline that ran out on a query the handler made is not one
// of these, and is still reported as the failure it is.
type wsContextError struct{ err error }

// Error implements the error interface.
func (e *wsContextError) Error() string { return e.err.Error() }

// Unwrap exposes the context's error, so that a caller can test for
// [context.Canceled] or [context.DeadlineExceeded] with [errors.Is].
func (e *wsContextError) Unwrap() error { return e.err }

// wsContextClosure chooses what a peer is told when the caller's own context
// ended a read or a write.
//
// A deadline that passed while reading says the peer stayed silent for longer
// than the handler allows, which is the same complaint the connection's own
// read timeout and keepalive make, so it is answered with the same status,
// [WSStatusPolicyViolation]. Anything else is this end giving up for reasons
// of its own, the peer having done nothing wrong, which is what
// [WSStatusGoingAway] says.
func wsContextClosure(ctxErr error, reading bool) (WSStatus, string) {
	switch {
	case !errors.Is(ctxErr, context.DeadlineExceeded):
		return WSStatusGoingAway, "the conversation was cancelled"
	case reading:
		return WSStatusPolicyViolation, "no message arrived within the time allowed"
	default:
		return WSStatusGoingAway, "the time allowed for the conversation ran out"
	}
}

// abandonReading ends a connection whose read the caller's context ended,
// telling the peer why. The read half must be held.
//
// The connection's outbound stream is at a frame boundary whatever the inbound
// one was doing, because a close frame waits for the write half, so the peer
// can always be told. What it costs is the wait for the peer's answer that
// follows every close frame; see [WSConn.linger].
func (c *WSConn) abandonReading(ended *wsContextError) error {
	if c.nc == nil || c.failure() != nil {
		// Without deadlines, the only way the context could interrupt the
		// read was to close the transport, so there is nobody left to tell.
		// And a connection that had already ended is what cancelled the
		// context, through [WSConn.cancelOnEnd]: whatever ended it has said
		// all there is to say.
		return c.fail(ended)
	}
	status, reason := wsContextClosure(ended.err, true)
	if c.sendClose(status, reason) == nil {
		c.linger()
	}
	return c.fail(ended)
}

// abandonWriting ends a connection whose write the caller's context ended
// before any of the frame had gone out, telling the peer why. The write half
// must be held, and the close frame goes out under it.
func (c *WSConn) abandonWriting(ctxErr error) error {
	ended := &wsContextError{err: fmt.Errorf("muzak: writing a websocket message: %w", ctxErr)}
	status, reason := wsContextClosure(ctxErr, false)
	if c.sendCloseHolding(status, reason) == nil {
		c.drain()
	}
	return c.fail(ended)
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The context ended before any of the frame went out, so the stream
		// is at a frame boundary and the peer can be told why the connection
		// is ending. Starting the write instead would race the cancellation
		// to the wire, and leave the stream wherever the race ended.
		//
		// A write the context ends part way through cannot be followed by a
		// close frame, which would be read as more of the frame it had
		// interrupted, so that one ends the connection without a word, and so
		// does one stuck behind a peer that has stopped reading, which a close
		// frame could not get past any more than the message could.
		return c.abandonWriting(ctxErr)
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
//
// A caller that finds the frame already being sent by another goroutine waits
// for that send to end before it returns, because what it does next is close
// the transport, and closing it under a frame half written would show the peer
// a lost connection instead of the status it was being told.
func (c *WSConn) sendClose(status WSStatus, reason string) error {
	return c.sendCloseFrame(status, reason, false, false)
}

// sendCloseAfterFailure is sendClose for the echo of a close frame the peer
// sent, which is written after the connection's outcome has been recorded and
// so has to go out although the connection already reads as ended. Without
// that, an echo would be skipped whenever another goroutine held the write half
// at that moment.
func (c *WSConn) sendCloseAfterFailure(status WSStatus, reason string) error {
	return c.sendCloseFrame(status, reason, true, false)
}

// sendCloseHolding is sendClose for a caller that already holds the write
// half.
//
// It does not wait for another goroutine that has begun sending a close frame
// of its own: that goroutine is waiting for the write half this caller holds,
// so neither would ever finish, and the frame it means to send has not been
// started, so there is nothing on the wire to protect.
func (c *WSConn) sendCloseHolding(status WSStatus, reason string) error {
	return c.sendCloseFrame(status, reason, false, true)
}

func (c *WSConn) sendCloseFrame(status WSStatus, reason string, afterFailure, holding bool) error {
	c.mu.Lock()
	if c.sentClose {
		closing := c.closing
		c.mu.Unlock()
		if holding {
			return errWSClosing
		}
		if closing != nil {
			// The sender is bounded by the write timeout, which is what keeps
			// this from waiting on a peer that has stopped reading for longer
			// than a close is ever allowed to take.
			<-closing
		}
		return nil
	}
	c.sentClose = true
	c.closing = make(chan struct{})
	closing := c.closing
	c.mu.Unlock()
	defer close(closing)

	if !wsframe.ValidStatus(uint16(status)) {
		// Three of the defined codes describe a local observation and must
		// never be sent, and an unassigned one would only be refused. Closing
		// with no code at all says the same thing without breaking the rules.
		status = WSStatusNoStatusReceived
		reason = ""
	}
	var payload [wsframe.MaxControlPayload]byte
	frame := wsframe.AppendClose(payload[:0], uint16(status), reason)

	if !holding {
		if err := c.acquireClose(afterFailure); err != nil {
			return err
		}
		defer c.release(c.writeSem)
	}
	// The close frame goes out under its own deadline rather than a caller's
	// context, because a connection being torn down should not be left open by
	// a context that was cancelled a moment ago.
	return wsWrite(c, context.Background(), time.Now().Add(c.closeTimeout()), wsframe.Close, frame)
}

// acquireClose takes the write semaphore for a close frame, waiting no longer
// than the write timeout for a peer that has stopped reading. It gives up early
// when the connection fails while it waits, unless the frame is being sent
// after a failure it was always going to follow.
func (c *WSConn) acquireClose(afterFailure bool) error {
	select {
	case c.writeSem <- struct{}{}:
		return nil
	default:
	}
	done := c.done
	if afterFailure {
		done = nil
	}
	timeout := c.closeTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case c.writeSem <- struct{}{}:
		return nil
	case <-done:
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
		return c.fail(wsWriteFailure(ctx, err))
	}
	return nil
}

// wsWriteFailure turns a failed write into the error the connection will
// report from then on: the caller's context when that is what ended it, and a
// lost connection carrying the transport's own failure otherwise.
func wsWriteFailure(ctx context.Context, err error) error {
	if ctxErr := wsContextFailure(ctx, err); ctxErr != nil {
		return &wsContextError{err: fmt.Errorf("muzak: writing a websocket message: %w", ctxErr)}
	}
	return &WSCloseError{Status: WSStatusAbnormalClosure, Reason: "the connection was lost", cause: err}
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
	if c.nc == nil {
		// Nothing is armed here: the only clock a read has of its own is the
		// message clock, which starts later, once a message begins. What is
		// returned disarms it however the read ends, because an expiry left
		// running would fire between messages and close a connection that is
		// merely idle.
		return c.interruptible(ctx, &c.readExpiry)
	}
	var deadline time.Time
	if fromCtx, ok := ctx.Deadline(); ok {
		deadline = fromCtx
	}
	_ = c.nc.SetReadDeadline(deadline)
	if c.arranged(ctx) {
		return wsNothingToStop
	}
	return wsAfterFunc(ctx, func() { _ = c.nc.SetReadDeadline(time.Now()) })
}

// armWrite is the writing counterpart of armRead.
func (c *WSConn) armWrite(ctx context.Context, deadline time.Time) func() bool {
	if c.nc == nil {
		if !deadline.IsZero() {
			// A write that outlives its deadline ends the connection, as it
			// would on a network connection, and reports the same thing: the
			// caller's context when it was the caller's deadline that passed,
			// and a lost connection whose cause is a timeout otherwise.
			c.writeExpiry.arm(deadline, func() {
				_ = c.fail(wsWriteFailure(ctx, os.ErrDeadlineExceeded))
			})
		}
		return c.interruptible(ctx, &c.writeExpiry)
	}
	_ = c.nc.SetWriteDeadline(deadline)
	if c.arranged(ctx) {
		return wsNothingToStop
	}
	return wsAfterFunc(ctx, func() { _ = c.nc.SetWriteDeadline(time.Now()) })
}

// interruptible arranges for a transport with no deadlines to be interrupted
// when ctx is cancelled, returning the function that undoes that and disarms
// expiry.
//
// Without a network connection there are no deadlines to set, so the only way
// to interrupt a blocked read or write is to close the transport, and the
// connection ends with it.
func (c *WSConn) interruptible(ctx context.Context, expiry *wsExpiry) func() bool {
	if c.arranged(ctx) {
		return expiry.disarm
	}
	stop := wsAfterFunc(ctx, func() { _ = c.rwc.Close() })
	return func() bool {
		expiry.disarm()
		return stop()
	}
}

// wsAfterFunc is [context.AfterFunc] for an arrangement that acts on the
// connection, with one difference: once the function it returns has returned,
// f has finished and never runs again.
//
// The stop function of [context.AfterFunc] reports whether it prevented f from
// running and does not wait for an f that has already started. A context
// cancelled at the instant its read or write completes therefore leaves f
// running, or about to run, on a goroutine of its own, and the next operation
// may have cleared the deadline and begun waiting by the time it gets there.
// Moving that operation's deadline into the past ends a healthy connection for
// a cancellation that belonged to an operation already over. Here stopping
// takes the lock f runs under, so it waits for an f in progress, and an f that
// has not begun finds itself stopped when it does.
func wsAfterFunc(ctx context.Context, f func()) (stop func() bool) {
	var mu sync.Mutex
	stopped := false
	cancel := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !stopped {
			f()
		}
	})
	return func() bool {
		prevented := cancel()
		mu.Lock()
		stopped = true
		mu.Unlock()
		return prevented
	}
}

// wsExpiry enforces a deadline on a transport that has none of its own, such
// as the body of the response a [net/http.Client] hands back for a switched
// connection.
//
// A deadline on a network connection interrupts the operation and leaves the
// connection to decide what happens next. Nothing can interrupt a read or a
// write on a transport without one except closing it, so an expiry runs a
// function that ends the connection when its deadline passes, which is what
// the connection would have done on seeing the deadline anyway. The zero value
// is disarmed and ready to use.
type wsExpiry struct {
	mu sync.Mutex
	// timer is the one armed timer, or nil. A timer that fires after it was
	// replaced or disarmed finds it is no longer this one and does nothing,
	// which is what keeps a deadline from ending an operation that finished
	// in time but raced its own timer.
	timer *time.Timer
}

// arm runs expire once deadline passes, unless the expiry is disarmed or armed
// again first. A deadline already in the past fires at once.
func (e *wsExpiry) arm(deadline time.Time, expire func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
	var timer *time.Timer
	timer = time.AfterFunc(time.Until(deadline), func() {
		e.mu.Lock()
		current := e.timer == timer
		if current {
			e.timer = nil
		}
		e.mu.Unlock()
		if current {
			expire()
		}
	})
	e.timer = timer
}

// disarm cancels the armed deadline, if there is one, and reports whether
// there was. It has the signature of the function [context.AfterFunc] returns
// so that it can stand in for one.
func (e *wsExpiry) disarm() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopLocked()
}

// stopLocked is disarm for a caller that already holds the mutex.
func (e *wsExpiry) stopLocked() bool {
	armed := e.timer != nil
	if armed {
		e.timer.Stop()
		e.timer = nil
	}
	return armed
}

// arranged reports whether a context needs no arrangement of its own.
//
// A context that can never be cancelled needs none, which is what keeps the
// close frames and the keepalive pings off the allocator. Neither does the
// handler's own context, because the connection is already watching that one
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
// It must be called before the handler or the keepalive can reach the
// connection, which is what makes the fields it writes safe to read from their
// goroutines afterwards. The register can reach a connection a moment sooner,
// but it only ever closes one, with a context that needs no arrangement, and so
// never reads them.
//
// A served connection watches the handler's context, which [WSConn.cancelOnEnd]
// also cancels when the connection ends for any other reason. Whatever ended
// it then closes the transport once it has finished with it, and closing it
// from here as well could cut short a close frame still on its way out, so
// this only acts on a cancellation that came first.
func (c *WSConn) watch(ctx context.Context) {
	if ctx.Done() == nil {
		return
	}
	c.watched = ctx
	c.unwatch = context.AfterFunc(ctx, func() {
		ended := &WSCloseError{Status: WSStatusAbnormalClosure, Reason: "the request ended"}
		if errors.Is(c.record(ended), ended) {
			_ = c.rwc.Close()
		}
	})
}

// stopWatching undoes what watch arranged, which the handler's own goroutine
// does once it has finished with the connection.
func (c *WSConn) stopWatching() {
	if c.unwatch != nil {
		c.unwatch()
	}
}

// wsCloseDrainLimit bounds how much a closing connection reads from a peer that
// has not stopped sending, which is what keeps a hostile peer from making a
// refusal cost more than a megabyte of reading, on top of the grace period
// that bounds how long it may take.
const wsCloseDrainLimit = 1 << 20

// drain is [WSConn.linger] for a caller that does not hold the read half. It
// gives up immediately when another goroutine is reading, because closing the
// transport will wake that goroutine anyway.
func (c *WSConn) drain() {
	select {
	case c.readSem <- struct{}{}:
		defer c.release(c.readSem)
	default:
		return
	}
	c.linger()
}

// linger is what a connection does between sending its close frame and closing
// the transport. The read half must be held.
//
// The transport cannot simply be closed. A peer that was sending when it was
// refused has bytes in flight, and a socket closed with unread data in its
// receive buffer sends a reset, which the peer's kernel may act on before the
// application has read the close frame that came ahead of it. So this end stops
// sending, which the peer sees as the end of the stream and can answer, and
// reads what arrives until the peer's own close frame, its end of the stream,
// or a bound is reached, dropping every byte as it comes so that none of it is
// held.
//
// Two bounds apply and neither can be extended by the peer: the read deadline
// is set once, to the close grace period, and no more than
// [wsCloseDrainLimit] bytes are read. A peer that answers promptly is not
// held up by either, because its close frame ends the wait.
//
// A client waits for longer, and does not stop sending first. RFC 6455 asks the
// server to close the TCP connection before the client does, so that the
// TIME_WAIT state a closed connection leaves behind is held by the server
// rather than by every client it ever had, and a client that shut down its
// write side would be the first to close. So a client sends nothing more and
// reads on past the server's close frame to the end of the stream, within the
// same two bounds.
func (c *WSConn) linger() {
	if c.closeGrace <= 0 {
		return
	}
	defer c.boundClosing()()
	c.discardInbound()
}

// awaitHangUp is what a client does once it has answered the server's close
// frame: it waits, within the grace period, for the server to close the
// connection, which the specification asks the server to do first. The read
// half must be held.
func (c *WSConn) awaitHangUp() {
	if c.closeGrace <= 0 {
		return
	}
	defer c.boundClosing()()
	_, _ = io.CopyN(io.Discard, c.br, wsCloseDrainLimit)
}

// boundClosing applies the close grace period to the reads of a connection
// that is closing, and on a server stops sending, returning the function that
// lifts the bound again. The read half must be held.
//
// A connection dialled through an [net/http.Client] reaches its peer through
// the body of a response, which has no deadline to set, so its grace period is
// enforced the way its read timeout is: by closing the transport when the time
// is up, which is what this end was about to do anyway.
func (c *WSConn) boundClosing() func() bool {
	deadline := time.Now().Add(c.closeGrace)
	if c.nc == nil {
		c.readExpiry.arm(deadline, func() { _ = c.rwc.Close() })
		return c.readExpiry.disarm
	}
	// Both the plain and the TLS connection can shut down their write side, the
	// latter by sending its close_notify alert. A transport that cannot is left
	// as it is: the wait still keeps the receive buffer empty.
	if half, ok := c.nc.(interface{ CloseWrite() error }); ok && !c.client {
		_ = c.nc.SetWriteDeadline(time.Now().Add(c.closeTimeout()))
		_ = half.CloseWrite()
	}
	_ = c.nc.SetReadDeadline(deadline)
	return wsNothingToStop
}

// wsHeaderSize is how many bytes a frame's header took on the wire, which a
// header that was read is exact about because the length must be written in
// its shortest form.
func wsHeaderSize(header wsframe.Header) int64 {
	size := int64(2)
	switch {
	case header.Length > math.MaxUint16:
		size += 8
	case header.Length >= 126:
		size += 2
	}
	if header.Masked {
		size += 4
	}
	return size
}

// discardInbound reads and drops what the peer sends until its close frame, the
// end of its stream, a deadline or [wsCloseDrainLimit] bytes. A client reads on
// past the server's close frame to the end of the stream.
//
// It follows the frames for as long as it can, which is how the peer's answer is
// recognised, starting from wherever the failure left the stream. A stream that
// stops making sense, or whose position is not known, is dropped as raw bytes
// instead, which costs only the ability to spot a close frame.
func (c *WSConn) discardInbound() {
	budget := int64(wsCloseDrainLimit)
	skip := c.unread
	for budget > 0 {
		switch {
		case skip < 0:
			_, _ = io.CopyN(io.Discard, c.br, budget)
			return
		case skip > 0:
			n, err := io.CopyN(io.Discard, c.br, min(skip, budget))
			budget -= n
			skip -= n
			if err != nil {
				return
			}
		default:
			header, err := wsframe.ReadHeader(c.br)
			var protocol *wsframe.Error
			if errors.As(err, &protocol) {
				// How much of it was read is not known, and the rest is
				// charged in bulk below.
				budget -= wsframe.MaxHeaderSize
				skip = -1
				continue
			}
			if err != nil {
				return
			}
			budget -= wsHeaderSize(header)
			if header.Opcode == wsframe.Close {
				// Its payload is read too, so that not even the peer's goodbye
				// is left unread when the transport is closed.
				n, _ := io.CopyN(io.Discard, c.br, header.Length)
				if !c.client {
					return
				}
				// A client goes on to wait for the server to hang up, which
				// is the end of the stream; see [WSConn.linger].
				budget -= n
				skip = -1
				continue
			}
			skip = header.Length
		}
	}
}

// keepalive pings the peer periodically and ends the connection when it stops
// answering, which is how a connection lost without a close frame is noticed
// before the operating system gets round to noticing it.
//
// It runs unless [WSOptions.PingInterval] turned it off, and it stops as soon
// as the connection does.
//
// A pong is consumed by a read like any other frame, so an unanswered ping
// says something about the peer only when a read was waiting for the answer
// throughout. A handler that only writes, or one that is busy with a message,
// has not looked, and is left alone: closing a healthy peer for that would be
// worse than the silence being checked for. A peer that has really gone
// leaves the handler blocked in its read, so the next round finds it waiting
// and closes the connection then.
//
// What the ping asks is whether the peer is still there, and anything that
// arrives from it after the ping answers that as well as a pong does. That
// matters for a peer part way through sending a large frame, whose pong cannot
// go out until the frame is finished: the bytes of the frame arriving are
// what tell the keepalive it is alive, so only a peer from which nothing at
// all has arrived for the pong timeout is closed.
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
		sent := c.heard.now()
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
		if c.heard.last() < sent && c.reading.Load() > 0 && c.notReading.last() < sent {
			_ = c.Close(WSStatusPolicyViolation, "the peer did not answer a ping")
			return
		}
	}
}
