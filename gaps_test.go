package muzak

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"muzak.dev/framework/internal/radix"
)

// TestEmbeddedStructPlanErrorsPropagate covers the error return inside the
// recursive half of collectFields.
func TestEmbeddedStructPlanErrorsPropagate(t *testing.T) {
	t.Parallel()
	type broken struct {
		Bad chan int `query:"bad"`
	}
	type outer struct {
		broken
		Fine string `query:"fine"`
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, in outer) (rtOut, error) { return rtOut{}, nil })

	if got := buildError(t, app); !strings.Contains(got, "cannot be bound") {
		t.Errorf("build error = %q, want the nested field type to be reported", got)
	}
}

// TestParamLookupWhenNothingWasCaptured covers the defensive branch that
// reports an absent path parameter, which routing itself prevents.
func TestParamLookupWhenNothingWasCaptured(t *testing.T) {
	t.Parallel()
	binder := &paramBinder{source: srcPath, name: "id"}
	ctx := &Context{params: radix.Params{}}

	if raw, present := binder.lookup(ctx, nil); present || raw != nil {
		t.Errorf("lookup = %v, %v; want nothing", raw, present)
	}
}

// TestEnvFileUnopenableIsReported covers the read failure that is not a missing
// file, which a file the process cannot open produces.
func TestEnvFileUnopenableIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can open a file with no permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.env")
	if err := os.WriteFile(path, []byte("KEY=value\n"), 0o000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := readEnvFile(path)
	if err == nil {
		t.Fatal("readEnvFile succeeded on an unreadable file")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want a permission failure rather than a missing file", err)
	}
}

// TestDecodeIssueForAnUnclassifiedFailure covers the branch that reports a
// decoding failure carrying neither an unknown name nor a JSON kind.
func TestDecodeIssueForAnUnclassifiedFailure(t *testing.T) {
	t.Parallel()
	type unsupported struct {
		Ch chan int `json:"ch"`
	}
	err := json.Unmarshal([]byte(`{"ch":1}`), &unsupported{})
	if err == nil {
		t.Fatal("decoding into a channel field succeeded, want an error")
	}
	var semantic *json.SemanticError
	if !errors.As(err, &semantic) {
		t.Fatalf("error is %T, want a *json.SemanticError", err)
	}
	if semantic.JSONKind != 0 {
		t.Fatalf("JSONKind = %q, want the unclassified case", string(semantic.JSONKind))
	}

	// A failure with no JSON kind is the type's, not the request's, and the
	// binder answers it as a server error rather than as a detail.
	if !typeFault(err) {
		t.Error("typeFault = false for a channel field, want the type blamed")
	}
	if typeFault(errors.New("jsontext: unexpected EOF")) {
		t.Error("typeFault = true for a syntax error, want the request blamed")
	}
	if !typeFault(&json.SemanticError{GoType: reflect.TypeFor[**chan int]()}) {
		t.Error("typeFault = false for a pointer to a channel, want the type blamed")
	}
	detail := decodeIssue(err, reflect.TypeFor[unsupported]())
	if detail.Field != "ch" {
		t.Errorf("field = %q, want %q", detail.Field, "ch")
	}
	if strings.Contains(detail.Issue, "chan") {
		t.Errorf("the issue leaked a Go type: %q", detail.Issue)
	}
}

// TestLifecycleStartAndStopBothFail covers the path that joins a failed
// start-up with a failure to unwind it.
func TestLifecycleStartAndStopBothFail(t *testing.T) {
	t.Parallel()
	stubborn := &recorder{name: "stubborn", stopErr: errors.New("could not close")}
	broken := &recorder{name: "broken", startErr: errors.New("could not open")}

	app := New(quietOptions(), WithLifecycle(stubborn, broken))
	mustBuild(t, app)

	err := app.StartLifecycle(context.Background())
	if err == nil {
		t.Fatal("StartLifecycle succeeded, want an error")
	}
	message := err.Error()
	if !strings.Contains(message, "could not open") {
		t.Errorf("the start failure is missing:\n%s", message)
	}
	if !strings.Contains(message, "could not close") {
		t.Errorf("the unwind failure is missing:\n%s", message)
	}
}

// TestNewLoggerAutoChoosesConsoleForACharacterDevice covers the branch that
// picks the human-readable format when the output looks like a terminal.
func TestNewLoggerAutoChoosesConsoleForACharacterDevice(t *testing.T) {
	device, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer device.Close()

	if !isTerminal(device) {
		t.Skipf("%s is not reported as a character device on this platform", os.DevNull)
	}
	logger := NewLogger(LoggerOptions{Format: LogFormatAuto, Output: device})
	if _, isConsole := logger.Handler().(*consoleHandler); !isConsole {
		t.Errorf("handler = %T, want the console handler for a character device", logger.Handler())
	}
}

// TestDiscardHandlerSatisfiesTheInterface covers the handler installed by
// LogFormatNone, whose methods are never reached through a logger because it
// reports itself disabled.
func TestDiscardHandlerSatisfiesTheInterface(t *testing.T) {
	t.Parallel()
	var handler slog.Handler = discardHandler{}

	if handler.Enabled(context.Background(), slog.LevelError) {
		t.Error("the discarding handler reports itself enabled")
	}
	record := slog.NewRecord(time.Now(), slog.LevelError, "dropped", 0)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Errorf("Handle = %v, want nil", err)
	}
	if handler.WithAttrs([]slog.Attr{slog.String("k", "v")}) != handler {
		t.Error("WithAttrs returned a different handler")
	}
	if handler.WithGroup("g") != handler {
		t.Error("WithGroup returned a different handler")
	}
}

func TestConsoleHandlerNoOpDerivations(t *testing.T) {
	t.Parallel()
	logger, _ := consoleLogger(t, nil)
	handler := logger.Handler()

	if handler.WithAttrs(nil) != handler {
		t.Error("WithAttrs(nil) returned a new handler, wasting an allocation")
	}
	if handler.WithGroup("") != handler {
		t.Error(`WithGroup("") returned a new handler, wasting an allocation`)
	}
}

func TestAppendAttrSkipsEmptyGroups(t *testing.T) {
	t.Parallel()
	logger, _ := consoleLogger(t, nil)
	handler := logger.Handler().(*consoleHandler)

	got := handler.appendAttr(nil, slog.Attr{Key: "empty", Value: slog.GroupValue()}, nil)
	if len(got) != 0 {
		t.Errorf("an empty group rendered %q, want nothing", got)
	}
}

func TestJSONFieldNameSkipsLocatedFields(t *testing.T) {
	t.Parallel()
	type located struct {
		FromQuery string `query:"q" json:"q"`
		FromBody  string `json:"body"`
		Ignored   string `json:"-"`
	}
	typ := reflect.TypeFor[located]()

	if name, _ := jsonFieldName(typ.Field(0)); name != "" {
		t.Errorf("a located field is described as body member %q", name)
	}
	if name, _ := jsonFieldName(typ.Field(1)); name != "body" {
		t.Errorf("a body field = %q, want %q", name, "body")
	}
	if name, _ := jsonFieldName(typ.Field(2)); name != "" {
		t.Errorf(`a field tagged "-" is described as %q`, name)
	}
}

func TestRouteLevelSharedOptions(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/documented", okHandler,
		WithResponseDoc(http.StatusTeapot, "I'm a teapot"),
		WithTags("route-tag"))
	app.Get("/hidden", okHandler, Hidden())
	app.Get("/deprecated", okHandler, Deprecated())
	mustBuild(t, app)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	documented := doc.Paths["/documented"].Get
	if _, described := documented.Responses["418"]; !described {
		t.Errorf("a route-level response doc is missing: %v", documented.Responses)
	}
	if len(documented.Tags) != 1 || documented.Tags[0] != "route-tag" {
		t.Errorf("tags = %v", documented.Tags)
	}
	if _, described := doc.Paths["/hidden"]; described {
		t.Error("a route-level Hidden did not hide the route")
	}
	if !doc.Paths["/deprecated"].Get.Deprecated {
		t.Error("a route-level Deprecated did not mark the route")
	}
	// A hidden route stays routable.
	assertStatus(t, do(t, app, "GET", "/hidden"), http.StatusOK)
}

// TestFinalizeSkipsRoutesThatFailedRegistration covers the branch that leaves a
// route alone once registration has already reported why it is unusable.
func TestFinalizeSkipsRoutesThatFailedRegistration(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("no-leading-slash", okHandler)
	app.Get("/fine", okHandler)

	message := buildError(t, app)
	if !strings.Contains(message, "no-leading-slash") {
		t.Errorf("build error = %q", message)
	}
	// The bad route contributed exactly one problem, not a second from
	// finalize trying to resolve it anyway.
	if got := strings.Count(message, "no-leading-slash"); got != 1 {
		t.Errorf("the broken route was reported %d times, want once:\n%s", got, message)
	}
}

// TestListenReportsLifecycleFailures covers the path where components refuse to
// start and no socket is opened.
func TestListenReportsLifecycleFailures(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	app := New(opts, WithLifecycle(&recorder{name: "broken", startErr: errors.New("no")}))
	app.Get("/x", okHandler)

	if err := app.RunContext(context.Background()); err == nil {
		t.Fatal("RunContext succeeded despite a component that cannot start")
	}
	if addr := app.Addr(); addr != "" {
		t.Errorf("a listener was opened at %q despite the failure", addr)
	}
}

// TestServePageStillWorksWithoutAnAcceptHeader is a small guard that the docs
// routes do not depend on content negotiation.
func TestDocsRoutesIgnoreAcceptHeaders(t *testing.T) {
	t.Parallel()
	app := mustBuild(t, newPublicDocsApp())

	req := httptest.NewRequest("GET", "/docs", nil)
	req.Header.Set("Accept", "text/plain")
	assertStatus(t, doRequest(t, app, req), http.StatusOK)

	req = httptest.NewRequest("GET", "/openapi.json", nil)
	req.Header.Set("Accept", "text/plain")
	assertStatus(t, doRequest(t, app, req), http.StatusOK)
}
