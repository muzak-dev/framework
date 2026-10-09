package muzak

import (
	"io"
	"net/http"
	"testing"
)

// A client dispatches an event only when it has a data field. An event with
// none, the zero SSEEvent or one with only a name, is applied for its id and
// retry and otherwise dropped, by a browser and by SSEReader alike, which the
// documentation used to say the opposite of. And an empty Text wrote no data
// field, so a stream of text could not send an empty message at all;
// EmptyData writes one with nothing in it.

func TestSSEEventDispatchesOnlyWithADataField(t *testing.T) {
	t.Parallel()
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			for _, event := range []SSEEvent[itemOut]{
				{},
				{Name: "ping"},
				{Name: "empty", Text: ""},
				{Name: "empty", EmptyData: true},
				{Text: "", EmptyData: true},
				{Text: "line", EmptyData: true},
				{Data: &itemOut{Name: "a"}, EmptyData: true},
				{Name: "marker", Text: "x"},
			} {
				if err := stream.SendEvent(event); err != nil {
					return err
				}
			}
			return nil
		}, WithSSE(SSEOptions{KeepAlive: -1}))
	})

	response, err := http.Get(server.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	const want = "\n" +
		"event: ping\n\n" +
		"event: empty\n\n" +
		"event: empty\ndata: \n\n" +
		"data: \n\n" +
		"data: line\n\n" +
		"data: {\"name\":\"a\"}\n\n" +
		"event: marker\ndata: x\n\n"
	if string(wire) != want {
		t.Errorf("the stream wrote %q, want %q", wire, want)
	}

	reader := openStream(t, server.URL, "/stream")
	var got []SSEMessage
	for range 5 {
		got = append(got, nextEvent(t, reader))
	}
	wantMessages := []SSEMessage{
		{Name: "empty", Data: ""},
		{Name: "", Data: ""},
		{Data: "line"},
		{Data: `{"name":"a"}`},
		{Name: "marker", Data: "x"},
	}
	for i := range wantMessages {
		if got[i] != wantMessages[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], wantMessages[i])
		}
	}
}
