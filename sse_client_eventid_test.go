package muzak

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// An id field does not move the identifier a reader resumes from until the
// event it belongs to is complete. The HTML specification keeps it in a
// buffer that becomes the stream's last event ID only when a blank line
// dispatches the event, and Muzak's own server writes the id before the data,
// so a reader that took it on sight and then lost the connection, timed out or
// refused the event for its size would resume past an event it never
// delivered, and that event would be lost.

func TestSSEReaderTakesAnIdentifierOnlyFromACompleteEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// open is how the stream ends after its one complete event.
		open func(t *testing.T) (io.ReadCloser, SSEDialOptions)
		// cut is what the second Next must report.
		cut error
	}{
		{
			name: "the connection drops in the middle of the next event",
			open: func(*testing.T) (io.ReadCloser, SSEDialOptions) {
				return io.NopCloser(strings.NewReader("id: 4\ndata: four\n\nid: 5\ndata: fi")), SSEDialOptions{}
			},
			cut: ErrSSEStreamEnded,
		},
		{
			name: "the connection drops after the next event's id",
			open: func(*testing.T) (io.ReadCloser, SSEDialOptions) {
				return io.NopCloser(strings.NewReader("id: 4\ndata: four\n\nid: 5\n")), SSEDialOptions{}
			},
			cut: ErrSSEStreamEnded,
		},
		{
			name: "the next event does not finish arriving in time",
			open: func(t *testing.T) (io.ReadCloser, SSEDialOptions) {
				pr, pw := io.Pipe()
				t.Cleanup(func() { _ = pw.Close() })
				go func() {
					_, _ = pw.Write([]byte("id: 4\ndata: four\n\n"))
					_, _ = pw.Write([]byte("id: 5\ndata: part"))
				}()
				return pr, SSEDialOptions{ReadTimeout: 50 * time.Millisecond}
			},
			cut: ErrSSEStreamEnded,
		},
		{
			name: "the next event is larger than the reader accepts",
			open: func(*testing.T) (io.ReadCloser, SSEDialOptions) {
				body := "id: 4\ndata: four\n\nid: 5\ndata: " + strings.Repeat("x", 4<<10) + "\n\n"
				return io.NopCloser(strings.NewReader(body)), SSEDialOptions{ReadLimit: 1 << 10}
			},
			cut: errSSEEventTooLarge,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, opts := tc.open(t)
			reader := newSSEReader(body, opts)
			t.Cleanup(func() { _ = reader.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
			defer cancel()

			message, err := reader.Next(ctx)
			if err != nil || message.ID != "4" || message.Data != "four" {
				t.Fatalf("Next() = %+v, %v; want the complete event with identifier 4", message, err)
			}
			if _, err := reader.Next(ctx); !errors.Is(err, tc.cut) {
				t.Errorf("Next() = %v, want %v", err, tc.cut)
			}
			if got := reader.LastEventID(); got != "4" {
				t.Errorf("LastEventID() = %q, want %q: the event that carried 5 was never delivered", got, "4")
			}
		})
	}
}

func TestSSEReaderKeepsTheIdentifierBufferAcrossEvents(t *testing.T) {
	t.Parallel()
	// What the buffer holds outlives the event that set it, so an event with
	// no id of its own is dispatched under the last one, and an event with an
	// id and no data, which is not delivered, still moves it.
	tests := []struct {
		name   string
		resume string
		body   string
		want   []string
		last   string
	}{
		{
			name: "an event without an id carries the one before it",
			body: "id: 7\ndata: a\n\ndata: b\n\n",
			want: []string{"7", "7"},
			last: "7",
		},
		{
			name: "an id with no data is applied without an event",
			body: "id: 8\n\ndata: c\n\n",
			want: []string{"8"},
			last: "8",
		},
		{
			name:   "the identifier resumed from carries until the stream sends one",
			resume: "3",
			body:   "data: d\n\nid: 9\ndata: e\n\n",
			want:   []string{"3", "9"},
			last:   "9",
		},
		{
			name:   "an id the stream never finishes leaves the one resumed from",
			resume: "3",
			body:   "data: d\n\nid: 9\n",
			want:   []string{"3"},
			last:   "3",
		},
		{
			name: "an empty id clears it once its event is complete",
			body: "id: 7\ndata: a\n\nid\ndata: b\n\n",
			want: []string{"7", ""},
			last: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader := newSSEReader(io.NopCloser(strings.NewReader(tc.body)), SSEDialOptions{LastEventID: tc.resume})
			var got []string
			for _, message := range readAll(t, reader) {
				got = append(got, message.ID)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("identifiers = %q, want %q", got, tc.want)
			}
			if last := reader.LastEventID(); last != tc.last {
				t.Errorf("LastEventID() = %q, want %q", last, tc.last)
			}
		})
	}
}
