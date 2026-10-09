package muzak

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

// TestContextLoggerCarriesTheRequest is the regression test for a request
// logger that was the application's logger and nothing more. Its
// documentation promised the method, path and request identifier on every
// record, which is what lets a line written deep in a handler be joined to
// the access log line of its request; none of them was there.
func TestContextLoggerCarriesTheRequest(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Get("/items/{id}", func(c *Context, _ Empty) (rtOut, error) {
		first := c.Logger()
		if c.Logger() != first {
			t.Error("Logger built a second logger for the same request")
		}
		first.Info("from the handler", "item", c.PathValue("id"))
		return rtOut{OK: true}, nil
	})
	mustBuild(t, app)

	// Two requests in turn, so that the second borrows the pooled Context the
	// first returned and must not inherit its attributes.
	var ids []string
	for _, target := range []string{"/items/plumbus", "/items/fleeb"} {
		rec := do(t, app, "GET", target)
		ids = append(ids, rec.Header().Get(HeaderRequestID))
	}

	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record["msg"] == "from the handler" {
			records = append(records, record)
		}
	}
	if len(records) != 2 {
		t.Fatalf("found %d records from the handler, want 2:\n%s", len(records), logs.String())
	}
	for i, item := range []string{"plumbus", "fleeb"} {
		record := records[i]
		if record["method"] != "GET" || record["path"] != "/items/"+item || record["item"] != item {
			t.Errorf("record %d = %v, want method GET and path /items/%s", i, record, item)
		}
		if ids[i] == "" || record[RequestIDKey] != ids[i] {
			t.Errorf("record %d request_id = %v, want %q", i, record[RequestIDKey], ids[i])
		}
	}
}
