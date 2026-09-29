package muzak

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tripwireBody fails the test the moment anything reads it.
type tripwireBody struct{ t *testing.T }

func (b tripwireBody) Read([]byte) (int, error) {
	b.t.Error("the body was read although its declared length was over the limit")
	return 0, errors.New("read")
}

func (tripwireBody) Close() error { return nil }

// A request that declares a length over the route's limit is refused before
// any of its body is read. A client that sent Expect: 100-continue was
// otherwise told to go ahead by the first read, and 50 MiB were on the wire
// before the 413 came.
func TestDeclaredLengthOverTheLimitIsRefusedUnread(t *testing.T) {
	t.Parallel()
	type formIn struct {
		S string `form:"s"`
	}
	app := New(quietOptions())
	app.Post("/json", func(ctx *Context, _ struct {
		S string `json:"s"`
	}) (Empty, error) {
		return Empty{}, nil
	}, MaxBodySize(1024))
	app.Post("/form", func(ctx *Context, _ formIn) (Empty, error) { return Empty{}, nil }, MaxBodySize(1024))
	built := mustBuild(t, app)

	tests := []struct{ name, path, contentType string }{
		{"json", "/json", "application/json"},
		{"urlencoded", "/form", "application/x-www-form-urlencoded"},
		{"multipart", "/form", "multipart/form-data; boundary=x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Body = tripwireBody{t}
			req.ContentLength = 50 << 20
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Expect", "100-continue")
			assertStatus(t, doRequest(t, built, req), http.StatusRequestEntityTooLarge)
		})
	}

	// A body that declares no length is still bounded as it is read, and one
	// inside the limit is unaffected.
	req := httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"s":"`+strings.Repeat("a", 4096)+`"}`))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	assertStatus(t, doRequest(t, built, req), http.StatusRequestEntityTooLarge)

	req = httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"s":"ok"}`))
	req.Header.Set("Content-Type", "application/json")
	assertStatus(t, doRequest(t, built, req), http.StatusOK)
}
