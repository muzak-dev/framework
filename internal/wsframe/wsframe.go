package wsframe

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Opcode says what a frame carries. RFC 6455 section 5.2 defines six of the
// sixteen possible values and reserves the rest.
type Opcode byte

const (
	// Continuation carries the next fragment of the message an earlier data
	// frame began.
	Continuation Opcode = 0x0
	// Text carries a fragment of a message whose payload is UTF-8.
	Text Opcode = 0x1
	// Binary carries a fragment of a message whose payload is arbitrary bytes.
	Binary Opcode = 0x2
	// Close asks for the connection to be closed, optionally carrying a status
	// code and a reason.
	Close Opcode = 0x8
	// Ping asks the peer to answer with a Pong carrying the same payload.
	Ping Opcode = 0x9
	// Pong answers a Ping.
	Pong Opcode = 0xA
)

// IsControl reports whether the opcode names a control frame, which is the
// meaning of the high bit of the four bit opcode field.
func (op Opcode) IsControl() bool { return op&0x8 != 0 }

// IsValid reports whether the opcode is one the specification defines.
func (op Opcode) IsValid() bool {
	switch op {
	case Continuation, Text, Binary, Close, Ping, Pong:
		return true
	default:
		return false
	}
}

// String names the opcode for a message, falling back to its number for the
// reserved values that only ever appear in a rejection.
func (op Opcode) String() string {
	switch op {
	case Continuation:
		return "continuation"
	case Text:
		return "text"
	case Binary:
		return "binary"
	case Close:
		return "close"
	case Ping:
		return "ping"
	case Pong:
		return "pong"
	default:
		return "0x" + strconv.FormatUint(uint64(op), 16)
	}
}

// Close status codes used by this package when it refuses a frame. The full
// set lives in the muzak package, which is what a handler sees; these are the
// few the codec itself has to name.
const (
	// StatusNormalClosure reports a connection closed because its purpose was
	// fulfilled.
	StatusNormalClosure uint16 = 1000
	// StatusProtocolError reports a frame that breaks the wire format.
	StatusProtocolError uint16 = 1002
	// StatusNoStatus stands for a close frame that carried no status code. It
	// is recorded locally and must never be written to the wire.
	StatusNoStatus uint16 = 1005
	// StatusAbnormalClosure stands for a connection lost without a close
	// frame. It is recorded locally and must never be written to the wire.
	StatusAbnormalClosure uint16 = 1006
	// StatusInvalidPayload reports a payload that is not what its opcode
	// promised, such as text that is not UTF-8.
	StatusInvalidPayload uint16 = 1007
	// StatusMessageTooBig reports a message larger than the receiver accepts.
	StatusMessageTooBig uint16 = 1009
	// StatusTLSHandshake stands for a TLS handshake that failed. It is
	// recorded locally and must never be written to the wire.
	StatusTLSHandshake uint16 = 1015
)

// MaxHeaderSize is the largest a frame header can be: two fixed bytes, eight
// for a 64 bit length and four for a masking key.
const MaxHeaderSize = 14

// MaxControlPayload is the largest payload a control frame may carry, which is
// what keeps a ping or a close from being fragmented.
const MaxControlPayload = 125

// MaxCloseReason is the longest close reason that fits in a control frame,
// once the two byte status code is accounted for.
const MaxCloseReason = MaxControlPayload - 2

// Header is the decoded prefix of one frame.
type Header struct {
	// Fin reports whether this frame completes its message.
	Fin bool
	// RSV1, RSV2 and RSV3 are the reserved bits. They carry meaning only for a
	// negotiated extension, and Muzak negotiates none, so a set bit is a
	// protocol violation.
	RSV1, RSV2, RSV3 bool
	// Opcode says what the frame carries.
	Opcode Opcode
	// Masked reports whether the payload is masked. Every frame from a client
	// must be, and no frame from a server may be.
	Masked bool
	// Mask is the masking key, meaningful only when Masked is set.
	Mask [4]byte
	// Length is the payload length in bytes.
	Length int64
}

// Error is a protocol violation found while decoding, carrying the close
// status the peer is owed for it.
//
// The reason is written to be sent as the close reason as well as read in a
// log, so it stays short, plain and free of internal state.
type Error struct {
	// Status is the close status code the violation maps to.
	Status uint16
	// Reason explains what was wrong with the frame.
	Reason string
}

// Error implements the error interface.
func (e *Error) Error() string { return "wsframe: " + e.Reason }

// ReadHeader reads and validates one frame header from r.
//
// It returns an *[Error] for a header the specification forbids, and whatever
// r reported for a header that could not be read at all, so a caller can tell
// a peer that violated the protocol from one that simply went away. A stream
// that ends between frames gives [io.EOF] and one that ends inside a header
// gives [io.ErrUnexpectedEOF].
func ReadHeader(r io.Reader) (Header, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:2]); err != nil {
		return Header{}, err
	}
	h := Header{
		Fin:    buf[0]&0x80 != 0,
		RSV1:   buf[0]&0x40 != 0,
		RSV2:   buf[0]&0x20 != 0,
		RSV3:   buf[0]&0x10 != 0,
		Opcode: Opcode(buf[0] & 0x0F),
		Masked: buf[1]&0x80 != 0,
	}
	if !h.Opcode.IsValid() {
		return h, &Error{Status: StatusProtocolError, Reason: "reserved opcode " + h.Opcode.String()}
	}
	if h.RSV1 || h.RSV2 || h.RSV3 {
		// A reserved bit means an extension is in use. Muzak negotiates none,
		// so the peer is sending something this connection agreed not to.
		return h, &Error{Status: StatusProtocolError, Reason: "a reserved bit is set but no extension was negotiated"}
	}

	switch length := int64(buf[1] & 0x7F); length {
	case 126:
		if _, err := io.ReadFull(r, buf[:2]); err != nil {
			return h, unexpected(err)
		}
		h.Length = int64(binary.BigEndian.Uint16(buf[:2]))
		if h.Length < 126 {
			return h, errNotMinimal
		}
	case 127:
		if _, err := io.ReadFull(r, buf[:8]); err != nil {
			return h, unexpected(err)
		}
		unsigned := binary.BigEndian.Uint64(buf[:8])
		if unsigned > math.MaxInt64 {
			// The specification reserves the most significant bit, so a length
			// that sets it is malformed rather than merely enormous.
			return h, &Error{Status: StatusProtocolError, Reason: "the payload length has its most significant bit set"}
		}
		h.Length = int64(unsigned)
		if h.Length <= math.MaxUint16 {
			return h, errNotMinimal
		}
	default:
		h.Length = length
	}

	if h.Opcode.IsControl() {
		if !h.Fin {
			return h, &Error{Status: StatusProtocolError, Reason: "a control frame cannot be fragmented"}
		}
		if h.Length > MaxControlPayload {
			return h, &Error{Status: StatusProtocolError, Reason: "a control frame cannot carry more than 125 bytes"}
		}
	}
	if h.Masked {
		if _, err := io.ReadFull(r, h.Mask[:]); err != nil {
			return h, unexpected(err)
		}
	}
	return h, nil
}

// unexpected reports an end of stream reached part way through a header as
// what it is. A clean end between frames is io.EOF and means the peer stopped
// sending; one inside a header means the peer vanished mid-frame, and the two
// deserve to be told apart.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// errNotMinimal reports a length written in more bytes than it needs, which
// the specification forbids so that one payload size has one encoding.
var errNotMinimal = &Error{
	Status: StatusProtocolError,
	Reason: "the payload length is not encoded in the fewest bytes",
}

// AppendHeader appends the wire form of h to dst and returns the extended
// slice, so that a header can be built into a caller's scratch buffer without
// allocating.
func AppendHeader(dst []byte, h Header) []byte {
	first := byte(h.Opcode) & 0x0F
	if h.Fin {
		first |= 0x80
	}
	if h.RSV1 {
		first |= 0x40
	}
	if h.RSV2 {
		first |= 0x20
	}
	if h.RSV3 {
		first |= 0x10
	}
	mask := byte(0)
	if h.Masked {
		mask = 0x80
	}
	switch {
	case h.Length <= 125:
		//nolint:gosec // the case guard is what makes the conversion exact
		dst = append(dst, first, mask|byte(h.Length))
	case h.Length <= math.MaxUint16:
		dst = append(dst, first, mask|126)
		dst = binary.BigEndian.AppendUint16(dst, uint16(h.Length))
	default:
		dst = append(dst, first, mask|127)
		dst = binary.BigEndian.AppendUint64(dst, uint64(h.Length))
	}
	if h.Masked {
		dst = append(dst, h.Mask[:]...)
	}
	return dst
}

// Mask applies the masking transformation to b in place and returns the
// position the next call should continue from.
//
// The transformation is its own inverse, so the same call both masks an
// outgoing payload and unmasks an incoming one. Passing the position within
// the message is what lets a payload that arrives in several reads be treated
// as one stream: the key rotates with the position rather than restarting.
func Mask(key [4]byte, pos int, b []byte) int {
	if len(b) == 0 {
		return pos & 3
	}
	pos &= 3
	// Rotating the key here means the loops below never have to know where in
	// the message this chunk sits.
	rotated := uint32(key[pos]) |
		uint32(key[(pos+1)&3])<<8 |
		uint32(key[(pos+2)&3])<<16 |
		uint32(key[(pos+3)&3])<<24
	word := uint64(rotated) | uint64(rotated)<<32

	n := len(b)
	for len(b) >= 8 {
		binary.LittleEndian.PutUint64(b, binary.LittleEndian.Uint64(b)^word)
		b = b[8:]
	}
	// Whatever is left starts at a multiple of eight bytes, so the rotation
	// still lines up and the tail can index the key directly.
	for i := range b {
		// The truncation is the point: each byte of the key is selected by
		// shifting it down to the low eight bits.
		b[i] ^= byte(rotated >> (8 * (i & 3))) //nolint:gosec // see above
	}
	return (pos + n) & 3
}

// ValidStatus reports whether a close status code may appear in a close frame
// received from a peer.
//
// The registered codes run from 1000 to 1014, minus the three that describe a
// local condition and can therefore never be sent: 1004 is unassigned, 1005
// stands for a close frame with no code at all, and 1006 for a connection lost
// without one. Everything from 3000 to 4999 is available to libraries and
// applications, and everything else is reserved.
func ValidStatus(code uint16) bool {
	switch {
	case code >= 3000 && code <= 4999:
		return true
	case code >= 1000 && code <= 1014:
		return code != 1004 && code != StatusNoStatus && code != StatusAbnormalClosure
	default:
		return false
	}
}

// ParseClose decodes the payload of a close frame into its status and reason.
//
// A close frame may carry nothing at all, which is reported as
// [StatusNoStatus] rather than as an error, because it is a legitimate way to
// close. Anything else that is not a valid code followed by valid UTF-8 is an
// *[Error] carrying the status the peer is owed.
func ParseClose(payload []byte) (status uint16, reason string, err error) {
	switch len(payload) {
	case 0:
		return StatusNoStatus, "", nil
	case 1:
		return 0, "", &Error{Status: StatusProtocolError, Reason: "a close payload of one byte cannot carry a status code"}
	}
	status = binary.BigEndian.Uint16(payload)
	if !ValidStatus(status) {
		return 0, "", &Error{Status: StatusProtocolError, Reason: "the close status code is reserved or unassigned"}
	}
	if !utf8.Valid(payload[2:]) {
		return 0, "", &Error{Status: StatusInvalidPayload, Reason: "the close reason is not valid UTF-8"}
	}
	return status, string(payload[2:]), nil
}

// AppendClose appends the payload of a close frame to dst.
//
// A status of [StatusNoStatus] produces an empty payload, which is how the
// absence of a code is expressed on the wire; the code itself must never be
// sent. The reason is truncated to what a control frame can carry.
func AppendClose(dst []byte, status uint16, reason string) []byte {
	if status == StatusNoStatus {
		return dst
	}
	dst = binary.BigEndian.AppendUint16(dst, status)
	return append(dst, TruncateReason(reason)...)
}

// TruncateReason shortens a close reason to what a control frame can carry,
// cutting on a rune boundary so that the result stays valid UTF-8.
//
// A reason that is not valid UTF-8 to begin with has each invalid byte replaced
// by U+FFFD first, because a peer must fail a connection whose close frame
// carries one, which would cost it the status the frame exists to deliver.
func TruncateReason(reason string) string {
	if !utf8.ValidString(reason) {
		reason = strings.ToValidUTF8(reason, "\uFFFD")
	}
	if len(reason) <= MaxCloseReason {
		return reason
	}
	cut := MaxCloseReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}
