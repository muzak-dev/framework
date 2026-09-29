package muzak

import (
	"net/http"
	"strings"
	"testing"
)

// A refusal for capacity is documented: the client of a generated SDK is told a
// stream can answer 503, which it does for a server that is draining and for
// one at its limit.
func TestSSEOpenAPIDocumentsTheRefusalForCapacity(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.SSE("/stream", streamItems("x"))
	app.SSE("/own", streamItems("x"), WithResponseDoc(http.StatusServiceUnavailable, "Come back later."))
	mustBuild(t, app)
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	if _, described := doc.Paths["/stream"].Get.Responses["503"]; !described {
		t.Errorf("a stream does not document 503: %v", doc.Paths["/stream"].Get.Responses)
	}
	if got := doc.Paths["/own"].Get.Responses["503"].Description; !strings.Contains(got, "Come back later.") {
		t.Errorf("the route's own 503 description was replaced with %q", got)
	}
}
