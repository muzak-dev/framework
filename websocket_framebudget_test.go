package muzak

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWebSocketFramesThatCarryNoMessageAreBudgeted(t *testing.T) {
	t.Parallel()
	// The frame count that stops an endless run of pings is kept per message,
	// so a peer that completes one empty message every 65535 frames never
	// meets it, and the server answers every ping while the message quota
	// sees one message per round. What a frame costs is not what it carries,
	// so pings, pongs and empty fragments are counted against the connection
	// however many messages complete in between.
	key := []byte{1, 2, 3, 4}
	const perRound = wsMaxFramesPerMessage - 1

	pings := func() []byte {
		var round []byte
		for range perRound {
			round = append(round, frameHeader(true, opPing, 0, key)...)
		}
		// One empty message, which resets what a single read counts.
		return append(round, frameHeader(true, opText, 0, key)...)
	}
	fragments := func() []byte {
		round := frameHeader(false, opText, 0, key)
		for range perRound - 2 {
			round = append(round, frameHeader(false, opContinuation, 0, key)...)
		}
		return append(round, frameHeader(true, opContinuation, 0, key)...)
	}

	for name, round := range map[string][]byte{"pings": pings(), "empty fragments": fragments()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ended := make(chan error, 1)
			_, server := newWSTestApp(t, func(app *App) {
				app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
					for {
						if _, _, err := conn.Read(context.Background()); err != nil {
							ended <- err
							return nil
						}
					}
				})
			})
			conn := dialWS(t, server.URL, "/ws")

			go func() {
				// The server closes part way through, so a refused write is
				// the expected end of this rather than a failure.
				_ = conn.conn.SetWriteDeadline(time.Now().Add(wsTestTimeout))
				for range 4 {
					if _, err := conn.conn.Write(round); err != nil {
						return
					}
				}
			}()

			pongs := 0
			for {
				_, opcode, payload, err := conn.tryRecv()
				if err != nil {
					// The close frame may be lost to the reset that follows
					// a server closing on unread input; that the connection
					// ended is what matters.
					break
				}
				if opcode == opClose {
					if !strings.Contains(string(payload), "too many frames") {
						t.Errorf("close payload = %q, want it to name the abuse", payload)
					}
					break
				}
				if opcode == opPong {
					pongs++
				}
			}
			select {
			case err := <-ended:
				if status, ok := WSCloseStatus(err); !ok || status != WSStatusPolicyViolation {
					t.Errorf("read error = %v, want the peer closed for a policy violation", err)
				}
			case <-time.After(wsTestTimeout):
				t.Fatal("the connection was never closed")
			}
			if pongs > wsMaxFramesPerMessage {
				t.Errorf("%d pings were answered, want the connection closed before the budget of %d was passed", pongs, wsMaxFramesPerMessage)
			}
		})
	}
}
