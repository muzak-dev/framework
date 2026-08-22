// Package wsframe implements the WebSocket wire format of RFC 6455.
//
// It is the layer below Badele's WebSocket connections: it reads and writes
// frame headers, applies the masking transformation, and decodes close
// payloads. It knows nothing about handshakes, connections or messages, which
// is what makes the rules it enforces testable one at a time.
//
// # Frames
//
// A frame is a header of two to fourteen bytes followed by a payload:
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-------+-+-------------+-------------------------------+
//	|F|R|R|R| opcode|M| Payload len |    Extended payload length    |
//	|I|S|S|S|  (4)  |A|     (7)     |             (16/64)           |
//	|N|V|V|V|       |S|             |   (if payload len==126/127)   |
//	| |1|2|3|       |K|             |                               |
//	+-+-+-+-+-------+-+-------------+ - - - - - - - - - - - - - - - +
//	|     Extended payload length continued, if payload len == 127  |
//	+ - - - - - - - - - - - - - - - +-------------------------------+
//	|                               | Masking-key, if MASK set to 1 |
//	+-------------------------------+-------------------------------+
//
// [ReadHeader] decodes that prefix and refuses every header the specification
// forbids: a reserved opcode, a reserved bit set when no extension was
// negotiated, a fragmented or oversized control frame, and a payload length
// written in more bytes than it needs. Each refusal carries the close status
// the peer is owed for it, so the connection layer never has to decide which
// violation maps to which code.
//
// # Masking
//
// Every frame a client sends is masked with a four byte key, which exists to
// stop a hostile page from steering a payload through a caching proxy that
// mistakes it for a request. [Mask] applies the transformation in place, eight
// bytes at a time, and takes the position within the message so that a payload
// arriving in several reads is unmasked as one continuous stream.
package wsframe
