package badele

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// benchStream returns a stream writing into a discarding response, so that a
// benchmark measures the engine rather than the socket underneath it.
func benchStream(b *testing.B, opts SSEOptions) *sseStream {
	b.Helper()
	c := &Context{
		w: asResponseWriter(flushingDiscard{newDiscardWriter()}),
		r: httptest.NewRequest(http.MethodGet, "/stream", nil),
	}
	stream := newSSEStream(c, opts.withDefaults())
	b.Cleanup(stream.cancel)
	return stream
}

// flushingDiscard is the benchmarks' discarding writer with a flush, which is
// the one thing beyond writing that a stream asks of a response.
type flushingDiscard struct{ *discardWriter }

func (flushingDiscard) FlushError() error { return nil }

func BenchmarkSSESend(b *testing.B) {
	stream := benchStream(b, SSEOptions{})
	typed := &SSEStream[itemOut]{core: stream}
	item := itemOut{Name: "Plumbus"}

	b.ReportAllocs()
	for b.Loop() {
		if err := typed.Send(item); err != nil {
			b.Fatalf("Send = %v", err)
		}
	}
}

func BenchmarkSSESendEvent(b *testing.B) {
	stream := benchStream(b, SSEOptions{})
	typed := &SSEStream[itemOut]{core: stream}
	item := itemOut{Name: "Plumbus"}

	b.ReportAllocs()
	for b.Loop() {
		if err := typed.SendEvent(SSEEvent[itemOut]{Name: "item_update", ID: "1", Data: &item}); err != nil {
			b.Fatalf("SendEvent = %v", err)
		}
	}
}

func BenchmarkSSESendText(b *testing.B) {
	stream := benchStream(b, SSEOptions{})
	typed := &SSEStream[Empty]{core: stream}

	b.ReportAllocs()
	for b.Loop() {
		if err := typed.SendEvent(SSEEvent[Empty]{Name: "token", Text: "plumbus"}); err != nil {
			b.Fatalf("SendEvent = %v", err)
		}
	}
}

func BenchmarkSSEEncodeEvent(b *testing.B) {
	// The encoding alone, with nothing of the response path in it.
	buf := make([]byte, 0, 256)
	payload := []byte(`{"name":"Plumbus"}`)
	frame := sseFrame{name: "item_update", id: "1"}

	b.ReportAllocs()
	for b.Loop() {
		buf = frame.appendFields(buf[:0])
		buf = appendSSELines(buf, "data: ", payload)
		buf = append(buf, '\n')
	}
}

func BenchmarkSSEParseEvent(b *testing.B) {
	const body = "event: item_update\nid: 1\ndata: {\"name\":\"Plumbus\"}\n\n"
	reader := newSSEReader(io.NopCloser(&repeatingReader{chunk: body}), SSEDialOptions{})

	b.ReportAllocs()
	for b.Loop() {
		if _, err := reader.Next(b.Context()); err != nil {
			b.Fatalf("Next = %v", err)
		}
	}
}

// repeatingReader yields one chunk over and over, which is a stream that never
// ends without needing a server to produce it.
type repeatingReader struct {
	chunk string
	at    int
}

func (r *repeatingReader) Read(p []byte) (int, error) {
	n := copy(p, r.chunk[r.at:])
	r.at += n
	if r.at == len(r.chunk) {
		r.at = 0
	}
	return n, nil
}
