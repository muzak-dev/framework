package testclient_test

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// sseItemOut is the model the streams in these tests carry.
type sseItemOut struct {
	Name string `json:"name"`
}

// sseStreamIn is the input of the streaming route, so that the test covers a
// stream that binds something as well as one that binds nothing.
type sseStreamIn struct {
	Room  string `path:"room"`
	Token string `query:"token" required:"true"`
}

// ssePromptIn is the body a posted stream answers.
type ssePromptIn struct {
	Text string `json:"text"`
}

// newSSEApp builds an application with the event stream routes these tests
// drive.
func newSSEApp() *muzak.App {
	app := muzak.New(muzak.AppOptions{
		Title:         "Event Stream Test API",
		Version:       "1.0.0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	})

	app.SSE("/rooms/{room}/stream", func(_ *muzak.Context, in sseStreamIn, stream *muzak.SSEStream[sseItemOut]) error {
		for i := range 3 {
			item := sseItemOut{Name: in.Room + "/" + in.Token + "/" + strconv.Itoa(i)}
			if err := stream.SendEvent(muzak.SSEEvent[sseItemOut]{
				Name: "item_update",
				ID:   strconv.Itoa(i),
				Data: &item,
			}); err != nil {
				return err
			}
		}
		return nil
	}, muzak.WithSSE(muzak.SSEOptions{KeepAlive: 5 * time.Millisecond}))

	app.SSEHandle(http.MethodPost, "/chat/stream", func(_ *muzak.Context, in ssePromptIn, stream *muzak.SSEStream[muzak.Empty]) error {
		for word := range strings.SplitSeq(in.Text, " ") {
			if err := stream.SendEvent(muzak.SSEEvent[muzak.Empty]{Name: "token", Text: word}); err != nil {
				return err
			}
		}
		return stream.SendEvent(muzak.SSEEvent[muzak.Empty]{Name: "done", Text: "[DONE]"})
	})

	app.SSE("/idle/stream", func(_ *muzak.Context, _ muzak.Empty, stream *muzak.SSEStream[sseItemOut]) error {
		<-stream.Context().Done()
		return stream.Err()
	}, muzak.WithSSE(muzak.SSEOptions{KeepAlive: 5 * time.Millisecond}))

	return app
}

func TestClientSSE(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newSSEApp())
	stream := client.SSE("/rooms/lobby/stream", testclient.Query("token", "jessica"))

	stream.Response.AssertStatus(http.StatusOK)
	stream.Response.AssertHeader("Content-Type", "text/event-stream; charset=utf-8")

	for i := range 3 {
		item := stream.Decode[sseItemOut]()
		if want := "lobby/jessica/" + strconv.Itoa(i); item.Name != want {
			t.Errorf("event %d = %q, want %q", i, item.Name, want)
		}
	}
	if got := stream.LastEventID(); got != "2" {
		t.Errorf("LastEventID() = %q, want %q", got, "2")
	}
}

func TestClientSSEEventNames(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newSSEApp())
	stream := client.SSE("/rooms/lobby/stream", testclient.Query("token", "jessica"))

	if message := stream.Next(); message.Name != "item_update" {
		t.Errorf("event name = %q, want %q", message.Name, "item_update")
	}
}

func TestClientSSEOverPost(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newSSEApp())
	stream := client.SSEDo(http.MethodPost, "/chat/stream",
		testclient.JSON(ssePromptIn{Text: "hello there"}))

	for _, want := range []string{"hello", "there", "[DONE]"} {
		if message := stream.Next(); message.Data != want {
			t.Errorf("token = %q, want %q", message.Data, want)
		}
	}
}

func TestClientSSERefused(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newSSEApp())

	// The token is required, so the request never becomes a stream at all.
	stream, response := client.TrySSE(http.MethodGet, "/rooms/lobby/stream")
	if stream != nil {
		t.Fatal("a refused request was accepted as a stream")
	}
	response.AssertStatus(http.StatusUnprocessableEntity)
	response.AssertErrorCode(muzak.CodeValidationError)
}

func TestClientSSEEndOfStream(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newSSEApp())
	stream := client.SSE("/rooms/lobby/stream", testclient.Query("token", "jessica"))

	for range 3 {
		stream.Next()
	}
	if _, err := stream.TryNext(); !errors.Is(err, muzak.ErrSSEStreamEnded) {
		t.Errorf("TryNext() = %v, want the stream to have ended", err)
	}
}

func TestClientSSEKeepComments(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newSSEApp())
	stream := client.SSE("/idle/stream", testclient.KeepComments())

	if message := stream.Next(); message.Comment == "" {
		t.Errorf("message = %+v, want the keepalive a quiet stream is held open with", message)
	}
	// Closing is what tells the handler its client is gone.
	stream.Close()
	if _, err := stream.TryNext(); err == nil {
		t.Error("the stream carried on after it was closed")
	}
}
