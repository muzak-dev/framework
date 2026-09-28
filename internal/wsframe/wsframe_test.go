package wsframe

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

func TestOpcodeClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		op      Opcode
		control bool
		valid   bool
		name    string
	}{
		{op: Continuation, control: false, valid: true, name: "continuation"},
		{op: Text, control: false, valid: true, name: "text"},
		{op: Binary, control: false, valid: true, name: "binary"},
		{op: 0x3, control: false, valid: false, name: "0x3"},
		{op: 0x7, control: false, valid: false, name: "0x7"},
		{op: Close, control: true, valid: true, name: "close"},
		{op: Ping, control: true, valid: true, name: "ping"},
		{op: Pong, control: true, valid: true, name: "pong"},
		{op: 0xB, control: true, valid: false, name: "0xb"},
		{op: 0xF, control: true, valid: false, name: "0xf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.op.IsControl(); got != tc.control {
				t.Errorf("IsControl() = %v, want %v", got, tc.control)
			}
			if got := tc.op.IsValid(); got != tc.valid {
				t.Errorf("IsValid() = %v, want %v", got, tc.valid)
			}
			if got := tc.op.String(); got != tc.name {
				t.Errorf("String() = %q, want %q", got, tc.name)
			}
		})
	}
}

func TestReadHeader(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input []byte
		want  Header
	}{
		{
			name:  "empty text frame",
			input: []byte{0x81, 0x00},
			want:  Header{Fin: true, Opcode: Text},
		},
		{
			name:  "unfinished binary fragment",
			input: []byte{0x02, 0x05},
			want:  Header{Opcode: Binary, Length: 5},
		},
		{
			name:  "continuation",
			input: []byte{0x80, 0x01},
			want:  Header{Fin: true, Opcode: Continuation, Length: 1},
		},
		{
			name:  "largest seven bit length",
			input: []byte{0x81, 0x7D},
			want:  Header{Fin: true, Opcode: Text, Length: 125},
		},
		{
			name:  "smallest sixteen bit length",
			input: []byte{0x81, 0x7E, 0x00, 0x7E},
			want:  Header{Fin: true, Opcode: Text, Length: 126},
		},
		{
			name:  "largest sixteen bit length",
			input: []byte{0x81, 0x7E, 0xFF, 0xFF},
			want:  Header{Fin: true, Opcode: Text, Length: math.MaxUint16},
		},
		{
			name:  "smallest sixty four bit length",
			input: []byte{0x81, 0x7F, 0, 0, 0, 0, 0, 0x01, 0x00, 0x00},
			want:  Header{Fin: true, Opcode: Text, Length: math.MaxUint16 + 1},
		},
		{
			name:  "masked frame",
			input: []byte{0x81, 0x83, 0x01, 0x02, 0x03, 0x04},
			want:  Header{Fin: true, Opcode: Text, Masked: true, Mask: [4]byte{1, 2, 3, 4}, Length: 3},
		},
		{
			name:  "close with a payload",
			input: []byte{0x88, 0x02},
			want:  Header{Fin: true, Opcode: Close, Length: 2},
		},
		{
			name:  "ping at the control limit",
			input: []byte{0x89, 0x7D},
			want:  Header{Fin: true, Opcode: Ping, Length: 125},
		},
		{
			name:  "pong",
			input: []byte{0x8A, 0x00},
			want:  Header{Fin: true, Opcode: Pong},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadHeader(bytes.NewReader(tc.input))
			if err != nil {
				t.Fatalf("ReadHeader(% x) = %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("ReadHeader(% x) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

func TestReadHeaderRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		input  []byte
		status uint16
		reason string
	}{
		{
			name:   "reserved data opcode",
			input:  []byte{0x83, 0x00},
			status: StatusProtocolError,
			reason: "reserved opcode 0x3",
		},
		{
			name:   "reserved control opcode",
			input:  []byte{0x8B, 0x00},
			status: StatusProtocolError,
			reason: "reserved opcode 0xb",
		},
		{
			name:   "rsv1 set",
			input:  []byte{0xC1, 0x00},
			status: StatusProtocolError,
			reason: "reserved bit",
		},
		{
			name:   "rsv2 set",
			input:  []byte{0xA1, 0x00},
			status: StatusProtocolError,
			reason: "reserved bit",
		},
		{
			name:   "rsv3 set",
			input:  []byte{0x91, 0x00},
			status: StatusProtocolError,
			reason: "reserved bit",
		},
		{
			name:   "fragmented control frame",
			input:  []byte{0x09, 0x00},
			status: StatusProtocolError,
			reason: "cannot be fragmented",
		},
		{
			name:   "oversized control frame",
			input:  []byte{0x88, 0x7E, 0x00, 0x7E},
			status: StatusProtocolError,
			reason: "more than 125 bytes",
		},
		{
			name:   "sixteen bit length that fits in seven",
			input:  []byte{0x81, 0x7E, 0x00, 0x7D},
			status: StatusProtocolError,
			reason: "fewest bytes",
		},
		{
			name:   "sixty four bit length that fits in sixteen",
			input:  []byte{0x81, 0x7F, 0, 0, 0, 0, 0, 0, 0xFF, 0xFF},
			status: StatusProtocolError,
			reason: "fewest bytes",
		},
		{
			name:   "length with the most significant bit set",
			input:  []byte{0x81, 0x7F, 0x80, 0, 0, 0, 0, 0, 0, 0x01},
			status: StatusProtocolError,
			reason: "most significant bit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadHeader(bytes.NewReader(tc.input))
			var protocol *Error
			if !errors.As(err, &protocol) {
				t.Fatalf("ReadHeader(% x) = %v, want a *wsframe.Error", tc.input, err)
			}
			if protocol.Status != tc.status {
				t.Errorf("status = %d, want %d", protocol.Status, tc.status)
			}
			if !strings.Contains(protocol.Error(), tc.reason) {
				t.Errorf("error = %q, want it to mention %q", protocol, tc.reason)
			}
		})
	}
}

func TestReadHeaderTruncated(t *testing.T) {
	t.Parallel()
	// One full header of every shape, cut at every byte, so that a truncated
	// read is reported as a transport failure rather than mistaken for a
	// protocol violation.
	full := [][]byte{
		{0x81, 0x00},
		{0x81, 0x80, 1, 2, 3, 4},
		{0x81, 0x7E, 0x01, 0x00},
		{0x81, 0xFE, 0x01, 0x00, 1, 2, 3, 4},
		{0x81, 0x7F, 0, 0, 0, 0, 0, 0x01, 0x00, 0x00},
		{0x81, 0xFF, 0, 0, 0, 0, 0, 0x01, 0x00, 0x00, 1, 2, 3, 4},
	}
	for _, header := range full {
		for cut := range len(header) {
			_, err := ReadHeader(bytes.NewReader(header[:cut]))
			switch {
			case cut == 0 && !errors.Is(err, io.EOF):
				t.Errorf("ReadHeader(empty) = %v, want io.EOF", err)
			case cut > 0 && !errors.Is(err, io.ErrUnexpectedEOF):
				t.Errorf("ReadHeader(% x) = %v, want io.ErrUnexpectedEOF", header[:cut], err)
			}
		}
		if _, err := ReadHeader(bytes.NewReader(header)); err != nil {
			t.Errorf("ReadHeader(% x) = %v, want the whole header to decode", header, err)
		}
	}
}

func TestReadHeaderReportsReaderFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("transport is gone")
	for _, prefix := range [][]byte{
		nil,
		{0x81, 0x7E},
		{0x81, 0x7F},
		{0x81, 0x80},
	} {
		reader := io.MultiReader(bytes.NewReader(prefix), iotest.ErrReader(failure))
		if _, err := ReadHeader(reader); !errors.Is(err, failure) {
			t.Errorf("ReadHeader after % x = %v, want the reader's own error", prefix, err)
		}
	}
}

func TestAppendHeaderRoundTrip(t *testing.T) {
	t.Parallel()
	lengths := []int64{0, 1, 124, 125, 126, 127, 1000, math.MaxUint16, math.MaxUint16 + 1, 1 << 20}
	for _, length := range lengths {
		for _, fin := range []bool{false, true} {
			for _, masked := range []bool{false, true} {
				opcode := Binary
				if length <= MaxControlPayload && fin {
					opcode = Ping
				}
				want := Header{
					Fin:    fin,
					Opcode: opcode,
					Masked: masked,
					Length: length,
				}
				if masked {
					want.Mask = [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
				}
				encoded := AppendHeader(nil, want)
				if len(encoded) > MaxHeaderSize {
					t.Fatalf("AppendHeader(%+v) produced %d bytes, more than MaxHeaderSize", want, len(encoded))
				}
				got, err := ReadHeader(bytes.NewReader(encoded))
				if err != nil {
					t.Fatalf("ReadHeader(AppendHeader(%+v)) = %v", want, err)
				}
				if got != want {
					t.Errorf("round trip of %+v = %+v", want, got)
				}
			}
		}
	}
}

func TestAppendHeaderCarriesReservedBits(t *testing.T) {
	t.Parallel()
	// The reserved bits are refused on the way in, so the only way to check
	// that they are written correctly is to inspect the bytes.
	encoded := AppendHeader(nil, Header{RSV1: true, RSV2: true, RSV3: true, Opcode: Text})
	if encoded[0] != 0x71 {
		t.Errorf("first byte = %#x, want %#x", encoded[0], 0x71)
	}
}

func TestAppendHeaderAppends(t *testing.T) {
	t.Parallel()
	dst := []byte{0xAA}
	got := AppendHeader(dst, Header{Fin: true, Opcode: Text, Length: 1})
	if want := []byte{0xAA, 0x81, 0x01}; !bytes.Equal(got, want) {
		t.Errorf("AppendHeader = % x, want % x", got, want)
	}
}

// maskNaive is the definition straight out of the specification, used to check
// the word-at-a-time implementation against something obviously correct.
func maskNaive(key [4]byte, pos int, b []byte) {
	for i := range b {
		b[i] ^= key[(pos+i)%4]
	}
}

func TestMaskMatchesTheSpecification(t *testing.T) {
	t.Parallel()
	key := [4]byte{0x37, 0xFA, 0x21, 0x3D}
	payload := make([]byte, 300)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	for _, length := range []int{0, 1, 3, 4, 5, 7, 8, 9, 15, 16, 63, 64, 127, 255, 300} {
		for pos := range 8 {
			got := append([]byte(nil), payload[:length]...)
			want := append([]byte(nil), payload[:length]...)
			next := Mask(key, pos, got)
			maskNaive(key, pos, want)
			if !bytes.Equal(got, want) {
				t.Fatalf("Mask(pos=%d, len=%d) = % x, want % x", pos, length, got, want)
			}
			if want := (pos + length) % 4; next != want {
				t.Errorf("Mask(pos=%d, len=%d) returned position %d, want %d", pos, length, next, want)
			}
		}
	}
}

func TestMaskIsItsOwnInverse(t *testing.T) {
	t.Parallel()
	key := [4]byte{1, 2, 3, 4}
	original := []byte("the same call both masks and unmasks a payload")
	round := append([]byte(nil), original...)
	Mask(key, 0, round)
	if bytes.Equal(round, original) {
		t.Fatal("masking left the payload unchanged")
	}
	Mask(key, 0, round)
	if !bytes.Equal(round, original) {
		t.Errorf("unmasking produced %q, want %q", round, original)
	}
}

func TestMaskAcrossChunks(t *testing.T) {
	t.Parallel()
	// A payload arriving in pieces must come out the same as one that arrived
	// whole, which is the whole reason Mask takes a position.
	key := [4]byte{0x9A, 0x0B, 0xCD, 0x1E}
	whole := make([]byte, 49)
	for i := range whole {
		whole[i] = byte(i*31 + 7)
	}
	want := append([]byte(nil), whole...)
	maskNaive(key, 0, want)

	for _, sizes := range [][]int{{1, 2, 3, 43}, {7, 7, 7, 28}, {8, 41}, {49}} {
		got := append([]byte(nil), whole...)
		pos, offset := 0, 0
		for _, size := range sizes {
			pos = Mask(key, pos, got[offset:offset+size])
			offset += size
		}
		if offset != len(whole) {
			t.Fatalf("the chunk sizes %v cover %d bytes, want %d", sizes, offset, len(whole))
		}
		if !bytes.Equal(got, want) {
			t.Errorf("chunked by %v = % x, want % x", sizes, got, want)
		}
	}
}

func TestValidStatus(t *testing.T) {
	t.Parallel()
	valid := []uint16{1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011, 1012, 1013, 1014, 3000, 3999, 4000, 4999}
	for _, code := range valid {
		if !ValidStatus(code) {
			t.Errorf("ValidStatus(%d) = false, want true", code)
		}
	}
	invalid := []uint16{0, 1, 999, 1004, 1005, 1006, 1015, 1016, 1100, 2000, 2999, 5000, 65535}
	for _, code := range invalid {
		if ValidStatus(code) {
			t.Errorf("ValidStatus(%d) = true, want false", code)
		}
	}
}

func TestParseClose(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload []byte
		status  uint16
		reason  string
	}{
		{name: "no payload", payload: nil, status: StatusNoStatus},
		{name: "status only", payload: []byte{0x03, 0xE8}, status: 1000},
		{name: "status and reason", payload: []byte{0x03, 0xE9, 'b', 'y', 'e'}, status: 1001, reason: "bye"},
		{name: "application code", payload: []byte{0x0B, 0xB8}, status: 3000},
		{name: "multi byte reason", payload: append([]byte{0x03, 0xE8}, "\xc3\xa9"...), status: 1000, reason: "\xc3\xa9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, reason, err := ParseClose(tc.payload)
			if err != nil {
				t.Fatalf("ParseClose(% x) = %v", tc.payload, err)
			}
			if status != tc.status || reason != tc.reason {
				t.Errorf("ParseClose(% x) = %d, %q, want %d, %q", tc.payload, status, reason, tc.status, tc.reason)
			}
		})
	}
}

func TestParseCloseRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload []byte
		status  uint16
	}{
		{name: "one byte", payload: []byte{0x03}, status: StatusProtocolError},
		{name: "reserved code", payload: []byte{0x03, 0xEE}, status: StatusProtocolError},
		{name: "no status received", payload: []byte{0x03, 0xED}, status: StatusProtocolError},
		{name: "abnormal closure", payload: []byte{0x03, 0xEE}, status: StatusProtocolError},
		{name: "code zero", payload: []byte{0x00, 0x00}, status: StatusProtocolError},
		{name: "invalid utf8 reason", payload: []byte{0x03, 0xE8, 0xFF, 0xFE}, status: StatusInvalidPayload},
		{name: "truncated rune", payload: []byte{0x03, 0xE8, 0xC3}, status: StatusInvalidPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ParseClose(tc.payload)
			var protocol *Error
			if !errors.As(err, &protocol) {
				t.Fatalf("ParseClose(% x) = %v, want a *wsframe.Error", tc.payload, err)
			}
			if protocol.Status != tc.status {
				t.Errorf("status = %d, want %d", protocol.Status, tc.status)
			}
		})
	}
}

func TestAppendClose(t *testing.T) {
	t.Parallel()
	if got := AppendClose(nil, StatusNoStatus, "ignored"); len(got) != 0 {
		t.Errorf("AppendClose(1005) = % x, want an empty payload", got)
	}
	got := AppendClose(nil, StatusNormalClosure, "done")
	if want := []byte{0x03, 0xE8, 'd', 'o', 'n', 'e'}; !bytes.Equal(got, want) {
		t.Errorf("AppendClose = % x, want % x", got, want)
	}
	status, reason, err := ParseClose(got)
	if err != nil || status != StatusNormalClosure || reason != "done" {
		t.Errorf("ParseClose(AppendClose(...)) = %d, %q, %v", status, reason, err)
	}
}

func TestTruncateReason(t *testing.T) {
	t.Parallel()
	if got := TruncateReason("short"); got != "short" {
		t.Errorf("TruncateReason(short) = %q", got)
	}
	long := strings.Repeat("a", MaxCloseReason+10)
	if got := TruncateReason(long); len(got) != MaxCloseReason {
		t.Errorf("TruncateReason produced %d bytes, want %d", len(got), MaxCloseReason)
	}
	// A cut that would land inside a rune must move back to its start, so that
	// what is sent stays valid UTF-8.
	multi := strings.Repeat("a", MaxCloseReason-1) + "\xe2\x82\xac"
	got := TruncateReason(multi)
	if len(got) != MaxCloseReason-1 {
		t.Errorf("TruncateReason cut to %d bytes, want %d", len(got), MaxCloseReason-1)
	}
	if !utf8.ValidString(got) {
		t.Errorf("TruncateReason produced invalid UTF-8: %q", got)
	}
	if payload := AppendClose(nil, StatusNormalClosure, long); len(payload) > MaxControlPayload {
		t.Errorf("AppendClose produced %d bytes, more than a control frame can carry", len(payload))
	}
}

func TestTruncateReasonRepairsInvalidUTF8(t *testing.T) {
	t.Parallel()
	// A reason a caller forwarded from somewhere else may not be UTF-8, and a
	// close frame carrying one obliges the peer to fail the connection rather
	// than read the status, so it is repaired on the way out.
	for _, reason := range []string{"bad \xff byte", "\xc3", strings.Repeat("\xff", MaxCloseReason)} {
		got := TruncateReason(reason)
		if !utf8.ValidString(got) {
			t.Errorf("TruncateReason(%q) = %q, want valid UTF-8", reason, got)
		}
		payload := AppendClose(nil, StatusNormalClosure, reason)
		if len(payload) > MaxControlPayload {
			t.Errorf("AppendClose(%q) produced %d bytes, more than a control frame can carry", reason, len(payload))
		}
		if _, _, err := ParseClose(payload); err != nil {
			t.Errorf("ParseClose(AppendClose(%q)) = %v, want a frame a peer accepts", reason, err)
		}
	}
}

func FuzzReadHeader(f *testing.F) {
	for _, seed := range [][]byte{
		{0x81, 0x00},
		{0x81, 0x83, 1, 2, 3, 4},
		{0x81, 0x7E, 0x01, 0x00},
		{0x81, 0x7F, 0, 0, 0, 0, 0, 0x01, 0x00, 0x00},
		{0x88, 0x02, 0x03, 0xE8},
		{0xFF, 0xFF, 0xFF, 0xFF},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		header, err := ReadHeader(bytes.NewReader(input))
		if err != nil {
			var protocol *Error
			if errors.As(err, &protocol) && !ValidStatus(protocol.Status) {
				t.Errorf("ReadHeader reported the unsendable status %d", protocol.Status)
			}
			return
		}
		if header.Length < 0 {
			t.Errorf("ReadHeader accepted a negative length %d", header.Length)
		}
		if !header.Opcode.IsValid() {
			t.Errorf("ReadHeader accepted the reserved opcode %s", header.Opcode)
		}
		if header.RSV1 || header.RSV2 || header.RSV3 {
			t.Error("ReadHeader accepted a reserved bit")
		}
		if header.Opcode.IsControl() && (!header.Fin || header.Length > MaxControlPayload) {
			t.Errorf("ReadHeader accepted a malformed control frame %+v", header)
		}
		// Whatever was accepted must survive being written back out.
		again, err := ReadHeader(bytes.NewReader(AppendHeader(nil, header)))
		if err != nil {
			t.Fatalf("re-reading an accepted header %+v = %v", header, err)
		}
		if again != header {
			t.Errorf("round trip of %+v = %+v", header, again)
		}
	})
}

func FuzzMask(f *testing.F) {
	f.Add([]byte("hello"), byte(1), byte(2), byte(3), byte(4), 0)
	f.Add([]byte(""), byte(0), byte(0), byte(0), byte(0), 3)
	f.Add(bytes.Repeat([]byte{0xAB}, 137), byte(0xFF), byte(0x00), byte(0x7F), byte(0x80), 2)
	f.Fuzz(func(t *testing.T, payload []byte, k0, k1, k2, k3 byte, pos int) {
		if pos < 0 {
			pos = -pos
		}
		key := [4]byte{k0, k1, k2, k3}
		got := append([]byte(nil), payload...)
		want := append([]byte(nil), payload...)
		next := Mask(key, pos, got)
		maskNaive(key, pos&3, want)
		if !bytes.Equal(got, want) {
			t.Fatalf("Mask(pos=%d) = % x, want % x", pos, got, want)
		}
		if want := (pos&3 + len(payload)) % 4; next != want {
			t.Errorf("Mask returned position %d, want %d", next, want)
		}
		if Mask(key, pos, got); !bytes.Equal(got, payload) {
			t.Errorf("masking twice = % x, want % x", got, payload)
		}
	})
}

func FuzzParseClose(f *testing.F) {
	f.Add([]byte{0x03, 0xE8})
	f.Add([]byte{0x03, 0xE8, 'b', 'y', 'e'})
	f.Add([]byte{0xFF, 0xFF})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		status, reason, err := ParseClose(payload)
		if err != nil {
			var protocol *Error
			if !errors.As(err, &protocol) {
				t.Fatalf("ParseClose returned %T, want a *wsframe.Error", err)
			}
			return
		}
		if status != StatusNoStatus && !ValidStatus(status) {
			t.Errorf("ParseClose accepted the invalid status %d", status)
		}
		if !utf8.ValidString(reason) {
			t.Errorf("ParseClose accepted a reason that is not UTF-8: %q", reason)
		}
	})
}

func BenchmarkMask(b *testing.B) {
	key := [4]byte{0x37, 0xFA, 0x21, 0x3D}
	for _, size := range []int{7, 64, 1 << 10, 64 << 10} {
		payload := make([]byte, size)
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for range b.N {
				Mask(key, 0, payload)
			}
		})
	}
}

func BenchmarkReadHeader(b *testing.B) {
	frame := AppendHeader(nil, Header{Fin: true, Opcode: Text, Masked: true, Length: 1024})
	reader := bytes.NewReader(frame)
	b.ReportAllocs()
	for range b.N {
		reader.Reset(frame)
		if _, err := ReadHeader(reader); err != nil {
			b.Fatalf("ReadHeader = %v", err)
		}
	}
}

func BenchmarkAppendHeader(b *testing.B) {
	buf := make([]byte, 0, MaxHeaderSize)
	header := Header{Fin: true, Opcode: Binary, Length: 1 << 20}
	b.ReportAllocs()
	for range b.N {
		buf = AppendHeader(buf[:0], header)
	}
	_ = buf
}
