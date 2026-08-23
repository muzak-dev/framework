package muzak

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
)

// benchWSConns returns a connected pair over an in-memory pipe, with a reader
// on the far end so that a benchmark measures the engine rather than a peer
// that never collects what it is sent.
func benchWSConns(b *testing.B, opts WSOptions) (*WSConn, *WSConn) {
	b.Helper()
	settings := opts.withDefaults()
	serverSide, clientSide := net.Pipe()
	server := newWSConn(serverSide, bufio.NewReader(serverSide), false, "", settings)
	client := newWSConn(clientSide, bufio.NewReader(clientSide), true, "", settings)
	b.Cleanup(func() {
		_ = serverSide.Close()
		_ = clientSide.Close()
	})
	return server, client
}

// benchWSMessages returns the payload sizes the benchmarks run over: one that
// fits the scratch buffer and goes out in a single write, and one that does not.
var benchWSMessages = []struct {
	name string
	size int
}{
	{name: "64B", size: 64},
	{name: "1KiB", size: 1 << 10},
	{name: "64KiB", size: 64 << 10},
}

func BenchmarkWebSocketWrite(b *testing.B) {
	for _, tc := range benchWSMessages {
		payload := make([]byte, tc.size)
		for i := range payload {
			payload[i] = byte(i)
		}
		// The server does not mask, so its payload never has to be copied; the
		// client does, which is what the two cases measure against each other.
		for _, end := range []string{"server", "client"} {
			b.Run(end+"/"+tc.name, func(b *testing.B) {
				server, client := benchWSConns(b, WSOptions{ReadLimit: 1 << 20})
				sender, receiver := server, client
				if end == "client" {
					sender, receiver = client, server
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					for {
						if _, _, err := receiver.Read(context.Background()); err != nil {
							return
						}
					}
				}()

				b.SetBytes(int64(tc.size))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if err := sender.WriteBinary(context.Background(), payload); err != nil {
						b.Fatalf("WriteBinary = %v", err)
					}
				}
				b.StopTimer()
				_ = sender.rwc.Close()
				<-done
			})
		}
	}
}

func BenchmarkWebSocketRead(b *testing.B) {
	for _, tc := range benchWSMessages {
		b.Run(tc.name, func(b *testing.B) {
			payload := make([]byte, tc.size)
			server, client := benchWSConns(b, WSOptions{ReadLimit: 1 << 20})
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					if err := client.WriteBinary(context.Background(), payload); err != nil {
						return
					}
				}
			}()

			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := server.Read(context.Background()); err != nil {
					b.Fatalf("Read = %v", err)
				}
			}
			b.StopTimer()
			_ = server.rwc.Close()
			<-done
		})
	}
}

func BenchmarkWebSocketRoundTrip(b *testing.B) {
	// One message each way per iteration, which is what a request and reply
	// conversation costs end to end.
	server, client := benchWSConns(b, WSOptions{ReadLimit: 1 << 20})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			typ, payload, err := server.Read(context.Background())
			if err != nil {
				return
			}
			if err := server.Write(context.Background(), typ, payload); err != nil {
				return
			}
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := client.WriteText(context.Background(), "ping"); err != nil {
			b.Fatalf("WriteText = %v", err)
		}
		if _, err := client.ReadText(context.Background()); err != nil {
			b.Fatalf("ReadText = %v", err)
		}
	}
	b.StopTimer()
	_ = client.rwc.Close()
	<-done
}

// wsDiscard is a transport that accepts everything and returns nothing, so
// that a benchmark can measure the framing alone with no peer in the way.
type wsDiscard struct{}

func (wsDiscard) Read([]byte) (int, error)    { return 0, io.EOF }
func (wsDiscard) Write(p []byte) (int, error) { return len(p), nil }
func (wsDiscard) Close() error                { return nil }

func BenchmarkWebSocketFrameWrite(b *testing.B) {
	// The write path with no transport behind it, which is what the framing
	// itself costs. The three contexts are the three a handler can pass: one
	// that can never be cancelled, the request's own, which the connection
	// already watches, and one derived per message, which it does not.
	contexts := []string{"background", "request", "derived"}
	for _, tc := range benchWSMessages {
		payload := make([]byte, tc.size)
		for _, name := range contexts {
			b.Run(name+"/"+tc.name, func(b *testing.B) {
				transport := wsDiscard{}
				conn := newWSConn(transport, bufio.NewReader(transport), false, "", WSOptions{}.withDefaults())
				ctx := context.Background()
				if name != "background" {
					cancellable, cancel := context.WithCancel(ctx)
					defer cancel()
					ctx = cancellable
				}
				if name == "request" {
					conn.watch(ctx)
					defer conn.stopWatching()
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if err := conn.WriteBinary(ctx, payload); err != nil {
						b.Fatalf("WriteBinary = %v", err)
					}
				}
			})
		}
	}
}

func BenchmarkWebSocketAcceptKey(b *testing.B) {
	// The digest every handshake computes, which is the only cryptography on
	// the path and the only part of the header checks worth measuring.
	b.ReportAllocs()
	for range b.N {
		if _, err := wsAcceptKey(testWSKey); err != nil {
			b.Fatalf("wsAcceptKey = %v", err)
		}
	}
}
