package muzak

import (
	"net/http"
	"testing"
)

// The handshake answers 400, 403 and 503 as well as 101 and 426, so a client
// generated from the document has to be told to expect them.
func TestWebSocketOpenAPIDocumentsWhatAHandshakeCanBeRefusedWith(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.WS("/ws", wsEcho)
	app.WS("/open", wsEcho, WithWebSocket(WSOptions{InsecureSkipOriginCheck: true}))
	app.WS("/own", wsEcho, WithResponseDoc(http.StatusForbidden, "Only staff may connect."))
	mustBuild(t, app)
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}

	for _, code := range []string{"101", "400", "403", "426", "503"} {
		if _, described := doc.Paths["/ws"].Get.Responses[code]; !described {
			t.Errorf("/ws does not document %s", code)
		}
	}
	// A route that skips the origin check cannot answer 403 for it.
	if _, described := doc.Paths["/open"].Get.Responses["403"]; described {
		t.Error("a route without an origin check documents 403")
	}
	// What a route declared for itself is not replaced by the default wording.
	if got := doc.Paths["/own"].Get.Responses["403"].Description; got != "Only staff may connect." {
		t.Errorf("the route's own 403 description was replaced with %q", got)
	}
}
