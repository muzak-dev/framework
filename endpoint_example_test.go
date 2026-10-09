package muzak_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"muzak.dev/framework"
)

// GetItemIn and ItemOut would live in a package the service and its callers
// both import, beside the endpoint.
type GetItemIn struct {
	ID     string `path:"id"`
	Expand bool   `query:"expand"`
}

type ItemOut struct {
	ID       string `json:"id"`
	Expanded bool   `json:"expanded"`
}

var GetItem = muzak.NewEndpoint[GetItemIn, ItemOut](http.MethodGet, "/items/{id}",
	muzak.Summary("Fetch an item"))

// ExampleNewEndpoint declares an endpoint once, implements it on a service
// and calls it from Go, with both sides typed by the same declaration.
func ExampleNewEndpoint() {
	app := muzak.New(muzak.AppOptions{LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone}})
	app.Implement(GetItem, func(_ *muzak.Context, in GetItemIn) (ItemOut, error) {
		if in.ID == "missing" {
			return ItemOut{}, muzak.NotFound("no such item")
		}
		return ItemOut{ID: in.ID, Expanded: in.Expand}, nil
	})
	server := httptest.NewServer(app)
	defer server.Close()

	client := muzak.NewClient(muzak.ClientOptions{AllowPrivateNetworks: true, BaseURL: server.URL})
	defer client.Close()

	item, err := GetItem.Call(context.Background(), client, GetItemIn{ID: "a b?", Expand: true})
	fmt.Println(item.ID, item.Expanded, err)

	_, err = GetItem.Call(context.Background(), client, GetItemIn{ID: "missing"})
	var remote *muzak.RemoteError
	if errors.As(err, &remote) {
		fmt.Println(remote.StatusCode, remote.Code, remote.Message)
	}

	_, err = GetItem.Call(context.Background(), client, GetItemIn{ID: ".."})
	fmt.Println(errors.Is(err, muzak.ErrCallRefused))
	// Output:
	// a b? true <nil>
	// 404 not_found no such item
	// true
}
