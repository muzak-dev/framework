package muzak_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"muzak.dev/framework"
)

// ExampleNewClient shows the default client refusing the cloud metadata
// service, which is what a URL supplied from outside usually aims at.
func ExampleNewClient() {
	client := muzak.NewClient(muzak.ClientOptions{})
	defer client.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	_, err := client.Do(req)

	var refused *muzak.AddressRefusedError
	if errors.As(err, &refused) {
		fmt.Println("refused:", refused.Reason)
	}
	// Output: refused: an IPv4 link-local address, where cloud metadata services live
}

// ExampleClient_GetJSON calls a service on the local network, which a client
// reaches only once it is told private networks are its business.
func ExampleClient_GetJSON() {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"username":"rick"}`))
	}))
	defer upstream.Close()

	client := muzak.NewClient(muzak.ClientOptions{AllowPrivateNetworks: true})
	defer client.Close()

	user, err := client.GetJSON[UserOut](context.Background(), upstream.URL)
	fmt.Println(user.Username, err)
	// Output: rick <nil>
}
