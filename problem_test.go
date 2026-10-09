package muzak

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"muzak.dev/framework/i18n"
)

// problemIn is a model with rules, so a request can fail validation.
type problemIn struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (in *problemIn) Validate(v *Validation) {
	v.String(&in.Name).Required()
	v.String(&in.Email).Required()
}

// secretStatusError is a StatusCoder whose own text must never reach a client
// at a server-side status.
type secretStatusError struct{}

func (secretStatusError) Error() string   { return "upstream password=hunter2 refused" }
func (secretStatusError) HTTPStatus() int { return http.StatusServiceUnavailable }

// problemApp is an application rendering problem details, with a route for
// each outcome the tests below look at.
func problemApp(t *testing.T, opts *ProblemOptions, configure func(*AppOptions)) (*App, *syncBuffer) {
	t.Helper()
	logger, logs := captureLogger(t)
	options := quietOptions()
	options.Logger = logger
	options.ProblemDetails = opts
	if configure != nil {
		configure(&options)
	}
	app := New(options)
	app.Post("/people", func(*Context, problemIn) (Empty, error) { return Empty{}, nil })
	app.Get("/people/{id}", func(*Context, struct {
		ID int `path:"id"`
	}) (Empty, error) {
		return Empty{}, NotFound("")
	})
	app.Get("/pay", func(*Context, Empty) (Empty, error) {
		return Empty{}, PaymentRequired("the card was declined").WithCode("card_declined")
	})
	app.Get("/odd", func(*Context, Empty) (Empty, error) {
		return Empty{}, NewHTTPError(http.StatusTeapot, "short and stout").WithCode("a b/c")
	})
	app.Get("/fault", func(*Context, Empty) (Empty, error) {
		return Empty{}, fmt.Errorf("query failed: dsn=postgres://admin:hunter2@db/prod")
	})
	app.Get("/upstream", func(*Context, Empty) (Empty, error) {
		return Empty{}, fmt.Errorf("calling billing: %w", secretStatusError{})
	})
	app.Get("/wrapped", func(*Context, Empty) (Empty, error) {
		return Empty{}, ServiceUnavailable("billing is down").Wrap(errors.New("dial tcp 10.0.0.5: hunter2"))
	})
	app.Get("/panic", func(*Context, Empty) (Empty, error) { panic("secret hunter2") })
	app.Get("/private", func(*Context, Empty) (Empty, error) { return Empty{}, nil },
		WithDependencies(RequireBearerToken("s3cret")))
	app.Get("/limited", func(*Context, Empty) (Empty, error) { return Empty{}, nil },
		RateLimit(Quota{Name: "problem", Limit: 1, Window: time.Minute}))
	return mustBuild(t, app), logs
}

// decodeProblem reads a problem out of a response, checking it is sent as
// one, and returns it with the raw members for a check of the exact set.
func decodeProblem(t *testing.T, res *httptest.ResponseRecorder) (Problem, map[string]any) {
	t.Helper()
	if got := res.Header().Get("Content-Type"); got != ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", got, ProblemContentType)
	}
	var problem Problem
	if err := json.Unmarshal(res.Body.Bytes(), &problem); err != nil {
		t.Fatalf("the body is not a problem: %v\n%s", err, res.Body.String())
	}
	var members map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &members); err != nil {
		t.Fatal(err)
	}
	return problem, members
}

// TestProblemDetailsWireFormat checks every member of a problem for the common
// outcomes, and that nothing else is sent.
func TestProblemDetailsWireFormat(t *testing.T) {
	t.Parallel()
	app, _ := problemApp(t, &ProblemOptions{}, nil)

	res := do(t, app, "GET", "/people/7")
	assertStatus(t, res, http.StatusNotFound)
	problem, members := decodeProblem(t, res)
	id := res.Header().Get(HeaderRequestID)
	want := Problem{
		Type: "about:blank", Title: "Not Found", Status: 404, Detail: statusMessages[404],
		Instance: "urn:uuid:" + id, Code: CodeNotFound, RequestID: id,
	}
	if fmt.Sprint(problem) != fmt.Sprint(want) {
		t.Errorf("the problem is\n %+v\nwant\n %+v", problem, want)
	}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if got := strings.Join(keys, ","); got != "code,detail,instance,request_id,status,title,type" {
		t.Errorf("the members are %s", got)
	}
	if res.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", res.Header().Get("Cache-Control"))
	}

	res = do(t, app, "POST", "/people", `{"name":""}`)
	assertStatus(t, res, http.StatusUnprocessableEntity)
	problem, members = decodeProblem(t, res)
	if problem.Code != CodeValidationError || problem.Title != "Unprocessable Entity" || problem.Detail != validationMessage {
		t.Errorf("the validation problem is %+v", problem)
	}
	if len(problem.Errors) != 2 || problem.Errors[0].Field != "name" || problem.Errors[0].Location != "body" || problem.Errors[0].Issue == "" {
		t.Errorf("the errors are %+v", problem.Errors)
	}
	if _, ok := members["details"]; ok {
		t.Error("the problem carries the envelope's details member as well")
	}

	// A path parameter that is not an integer.
	assertStatus(t, do(t, app, "GET", "/people/x"), http.StatusUnprocessableEntity)
}

// TestProblemDetailsKeepHeaders checks the headers a refusal relies on survive
// the change of format: Allow on a 405, Retry-After on a 429 and
// WWW-Authenticate on a 401.
func TestProblemDetailsKeepHeaders(t *testing.T) {
	t.Parallel()
	app, _ := problemApp(t, &ProblemOptions{}, nil)

	res := do(t, app, "DELETE", "/pay")
	assertStatus(t, res, http.StatusMethodNotAllowed)
	if res.Header().Get("Allow") == "" {
		t.Error("the 405 lost its Allow header")
	}
	if p, _ := decodeProblem(t, res); p.Code != CodeMethodNotAllowed || p.Title != "Method Not Allowed" {
		t.Errorf("the 405 is %+v", p)
	}

	do(t, app, "GET", "/limited")
	res = do(t, app, "GET", "/limited")
	assertStatus(t, res, http.StatusTooManyRequests)
	if res.Header().Get("Retry-After") == "" {
		t.Error("the 429 lost its Retry-After header")
	}
	if p, _ := decodeProblem(t, res); p.Code != CodeTooManyRequests || p.Title != "Too Many Requests" {
		t.Errorf("the 429 is %+v", p)
	}

	res = do(t, app, "GET", "/private")
	assertStatus(t, res, http.StatusUnauthorized)
	if res.Header().Get("WWW-Authenticate") == "" {
		t.Error("the 401 lost its WWW-Authenticate header")
	}
	decodeProblem(t, res)
}

// TestProblemDetailsTypes builds each type from the base and the code, escaping
// a code that is not a path segment, and leaves the type blank without a base.
func TestProblemDetailsTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		base, target, want string
	}{
		{"", "/pay", "about:blank"},
		{"https://errors.example.com/", "/pay", "https://errors.example.com/card_declined"},
		{"https://errors.example.com/", "/odd", "https://errors.example.com/a%20b%2Fc"},
		{"https://errors.example.com/", "/people/1", "https://errors.example.com/not_found"},
		{"urn:example:problems:", "/fault", "urn:example:problems:internal_error"},
	} {
		app, _ := problemApp(t, &ProblemOptions{TypeBase: tc.base}, nil)
		if p, _ := decodeProblem(t, do(t, app, "GET", tc.target)); p.Type != tc.want {
			t.Errorf("base %q, GET %s: type %q, want %q", tc.base, tc.target, p.Type, tc.want)
		}
	}
}

// TestProblemDetailsLeakNothing holds the format to the promises the default
// envelope makes about a server-side fault: the client reads a fixed sentence
// and the log gets the cause.
func TestProblemDetailsLeakNothing(t *testing.T) {
	t.Parallel()
	app, logs := problemApp(t, &ProblemOptions{TypeBase: "https://errors.example.com/"}, nil)
	for target, status := range map[string]int{
		"/fault":    http.StatusInternalServerError,
		"/upstream": http.StatusServiceUnavailable,
		"/panic":    http.StatusInternalServerError,
	} {
		res := do(t, app, "GET", target)
		assertStatus(t, res, status)
		p, _ := decodeProblem(t, res)
		if strings.Contains(res.Body.String(), "hunter2") || p.Detail != internalMessage {
			t.Errorf("GET %s leaked: %s", target, res.Body.String())
		}
	}
	res := do(t, app, "GET", "/wrapped")
	if p, _ := decodeProblem(t, res); p.Detail != "billing is down" || strings.Contains(res.Body.String(), "hunter2") {
		t.Errorf("GET /wrapped = %s", res.Body.String())
	}
	if strings.Count(logs.String(), "hunter2") < 4 {
		t.Errorf("the causes were not logged:\n%s", logs.String())
	}
}

// TestProblemDetailsAreTranslated renders the title and the detail in the
// request's locale through the hooks every other message uses.
func TestProblemDetailsAreTranslated(t *testing.T) {
	t.Parallel()
	app, _ := problemApp(t, &ProblemOptions{}, func(o *AppOptions) {
		o.I18n = I18nOptions{Store: interopStore(t)}
	})
	ask := func(method, target, body string) (*httptest.ResponseRecorder, Problem) {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept-Language", "es")
		res := doRequest(t, app, req)
		p, _ := decodeProblem(t, res)
		return res, p
	}
	res, p := ask("GET", "/people/1", "")
	if p.Title != "No encontrado" || p.Detail != "No se encontr\u00f3 el recurso solicitado." {
		t.Errorf("the Spanish 404 is %+v", p)
	}
	if res.Header().Get("Content-Language") != "es" {
		t.Errorf("Content-Language = %q", res.Header().Get("Content-Language"))
	}
	if p.Code != CodeNotFound || p.Type != "about:blank" {
		t.Errorf("a machine member was translated: %+v", p)
	}
	if _, p = ask("POST", "/people", `{}`); p.Title != "Entidad no procesable" || p.Detail != "La solicitud no pudo ser validada." {
		t.Errorf("the Spanish 422 is %+v", p)
	}
	// A status with no translation falls back to the English phrase.
	if _, p = ask("GET", "/pay", ""); p.Title != "Payment Required" {
		t.Errorf("the untranslated title is %q", p.Title)
	}
}

// TestProblemTitlesMatchTheStandardPhrases keeps the shipped titles equal to
// the phrases net/http knows each status by, so turning a store on changes no
// English title, and covers every status the framework classifies.
func TestProblemTitlesMatchTheStandardPhrases(t *testing.T) {
	t.Parallel()
	store := i18n.Builtin()
	statuses := []int{http.StatusMisdirectedRequest}
	for status := range statusCodes {
		statuses = append(statuses, status)
	}
	for _, status := range statuses {
		key := "muzak.status." + strconv.Itoa(status)
		if !store.Exists("en", key) {
			t.Errorf("status %d has no title at %s", status, key)
			continue
		}
		if got := store.T("en", key); got != http.StatusText(status) {
			t.Errorf("the title of %d is %q, want %q", status, got, http.StatusText(status))
		}
	}
}

// TestProblemInstanceNeverCarriesThePath keeps a path holding personal data out
// of the member meant to identify the occurrence.
func TestProblemInstanceNeverCarriesThePath(t *testing.T) {
	t.Parallel()
	app, _ := problemApp(t, &ProblemOptions{}, nil)
	res := do(t, app, "GET", "/users/jane.doe@example.com")
	p, _ := decodeProblem(t, res)
	if !strings.HasPrefix(p.Instance, "urn:uuid:") || strings.Contains(p.Instance, "jane") {
		t.Errorf("instance = %q", p.Instance)
	}
	if got := problemInstance("not-a-uuid\r\n"); got != "" {
		t.Errorf("an identifier that is not a UUID became the instance %q", got)
	}
	if got := problemInstance(""); got != "" {
		t.Errorf("no identifier became the instance %q", got)
	}
}

// TestProblemDetailsCoverEveryRefusal checks the refusals that happen outside
// a route are problems too: a refused host, a mount's guard and a panic in a
// middleware.
func TestProblemDetailsCoverEveryRefusal(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.ProblemDetails = &ProblemOptions{TypeBase: "https://errors.example.com/"}
	options.AllowedHosts = []string{"example.com"}
	app := New(options)
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/middleware-panic" {
				panic("middleware secret hunter2")
			}
			next.ServeHTTP(w, r)
		})
	})
	app.Mount("/metrics", &mountRecorder{}, WithDependencies(RequireBearerToken("s3cret")))
	mustBuild(t, app)

	res := doRequest(t, app, requestFor("GET", "/x", "evil.com"))
	if p, _ := decodeProblem(t, res); p.Status != 421 || p.Code != CodeMisdirectedRequest || p.Title != "Misdirected Request" ||
		p.Type != "https://errors.example.com/misdirected_request" {
		t.Errorf("the refused host is %+v", p)
	}
	res = doRequest(t, app, requestFor("GET", "/metrics", "example.com"))
	if p, _ := decodeProblem(t, res); p.Status != 401 || res.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("the mount's refusal is %+v", p)
	}
	res = doRequest(t, app, requestFor("GET", "/middleware-panic", "example.com"))
	assertStatus(t, res, http.StatusInternalServerError)
	p, _ := decodeProblem(t, res)
	if p.Code != CodeInternalError || p.Detail != internalMessage || p.Type != "https://errors.example.com/internal_error" ||
		p.RequestID == "" || p.Instance != "urn:uuid:"+p.RequestID || strings.Contains(res.Body.String(), "hunter2") {
		t.Errorf("a panic in middleware is %s", res.Body.String())
	}
}

// TestDefaultEnvelopeStaysTheDefault shows an application that does not ask
// for problems gets the envelope and the document it always had.
func TestDefaultEnvelopeStaysTheDefault(t *testing.T) {
	t.Parallel()
	app, _ := problemApp(t, nil, nil)
	res := do(t, app, "GET", "/people/1")
	if res.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", res.Header().Get("Content-Type"))
	}
	if body := decodeError(t, res); body.Error.Code != CodeNotFound {
		t.Errorf("the envelope is %+v", body)
	}
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Components.Schemas["Problem"]; ok {
		t.Error("the document describes a Problem the application never sends")
	}
	if _, ok := doc.Components.Schemas["ErrorResponse"]; !ok {
		t.Error("the document no longer describes the envelope")
	}
}

// TestProblemDetailsInTheDocument checks every error response the document
// describes is a problem, under its own media type, and the envelope is gone.
func TestProblemDetailsInTheDocument(t *testing.T) {
	t.Parallel()
	app, _ := problemApp(t, &ProblemOptions{}, nil)
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	problem, ok := doc.Components.Schemas["Problem"]
	if !ok {
		t.Fatal("the document has no Problem schema")
	}
	for _, member := range []string{"type", "title", "status", "detail", "instance", "code", "errors", "request_id"} {
		if _, ok := problem.Properties[member]; !ok {
			t.Errorf("the Problem schema has no %q member", member)
		}
	}
	if _, ok := doc.Components.Schemas["ErrorResponse"]; ok {
		t.Error("the document still describes the envelope")
	}
	checked := 0
	for path, item := range doc.Paths {
		for _, op := range []*Operation{item.Get, item.Post} {
			if op == nil {
				continue
			}
			for code, response := range op.Responses {
				if code != "default" && code[0] != '4' && code[0] != '5' {
					continue
				}
				media, ok := response.Content[ProblemContentType]
				if !ok || len(response.Content) != 1 || media.Schema.Ref != componentPrefix+"Problem" {
					t.Errorf("%s %s: the error response is %+v", path, code, response.Content)
				}
				checked++
			}
		}
	}
	if checked < 10 {
		t.Errorf("only %d error responses were checked", checked)
	}
}

// TestProblemDetailsWithARendererOfYourOwn composes ProblemDetails into a
// renderer of the application's own: the renderer runs, and the document
// still says errors are problems.
func TestProblemDetailsWithARendererOfYourOwn(t *testing.T) {
	t.Parallel()
	base := ProblemDetails(ProblemOptions{TypeBase: "https://errors.example.com/"})
	app, _ := problemApp(t, &ProblemOptions{}, func(o *AppOptions) {
		o.ErrorRenderer = func(ctx *Context, err error) (int, any) {
			ctx.SetHeader("X-Rendered-By", "mine")
			return base(ctx, err)
		}
	})
	res := do(t, app, "GET", "/people/1")
	if res.Header().Get("X-Rendered-By") != "mine" {
		t.Error("the application's own renderer did not run")
	}
	if p, _ := decodeProblem(t, res); p.Type != "https://errors.example.com/not_found" {
		t.Errorf("the composed renderer produced %+v", p)
	}
	doc, _ := app.Document()
	if _, ok := doc.Components.Schemas["Problem"]; !ok {
		t.Error("the document does not describe problems")
	}
}

// TestProblemDetailsRendererOutsideTheFramework calls the renderer with a
// Context the framework did not make, as a renderer of the application's own
// might in a test, which must not panic for want of a response to set a
// header on.
func TestProblemDetailsRendererOutsideTheFramework(t *testing.T) {
	t.Parallel()
	status, body := ProblemDetails(ProblemOptions{})(&Context{}, NotFound(""))
	p, ok := body.(Problem)
	if status != http.StatusNotFound || !ok || p.Code != CodeNotFound || p.Instance != "" {
		t.Errorf("the renderer answered %d %+v", status, body)
	}
}

// TestProblemDetailsAbortAStartedResponse checks a panic after the response
// started is still an abort, not a problem appended to the body.
func TestProblemDetailsAbortAStartedResponse(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.ProblemDetails = &ProblemOptions{}
	app := New(options)
	app.Use(func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))
			panic("after the first write")
		})
	})
	mustBuild(t, app)
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler { //nolint:errorlint // recover yields any
				t.Errorf("a panic after the response started became %v", recovered)
			}
		}()
		app.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	}()
	if strings.Contains(rec.Body.String(), "type") {
		t.Errorf("a problem was appended to a started body: %q", rec.Body.String())
	}
}

// TestProblemDetailsWithAnUnencodableMessage falls back to the fixed problem
// when the renderer's body cannot be encoded, as the envelope falls back to
// its fixed form.
func TestProblemDetailsWithAnUnencodableMessage(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.ProblemDetails = &ProblemOptions{}
	app := New(options)
	app.Get("/bad", func(*Context, Empty) (Empty, error) { return Empty{}, BadRequest("not utf-8 \xff") })
	mustBuild(t, app)
	res := do(t, app, "GET", "/bad")
	assertStatus(t, res, http.StatusInternalServerError)
	if p, _ := decodeProblem(t, res); p.Code != CodeInternalError || p.Detail != internalMessage {
		t.Errorf("the fallback is %+v", p)
	}
}

// TestProblemOptionsBuildErrors refuses a base no client could resolve the
// same way, or that a code appended to it would land inside the wrong part of.
func TestProblemOptionsBuildErrors(t *testing.T) {
	t.Parallel()
	for base, bad := range map[string]bool{
		"https://errors.example.com/":   false,
		"urn:example:errors:":           false,
		"tag:example.com,2026:problem:": false,
		"errors/":                       true,
		"/errors/":                      true,
		"//errors.example.com/":         true,
		"https://errors.example.com/?a": true,
		"https://errors.example.com/?":  true,
		"https://errors.example.com/#":  true,
		"https://errors example.com/":   true,
		"https://errors.example.com/\n": true,
		"https://\u00e9rrors.example/":  true,
		"%zz":                           true,
	} {
		options := quietOptions()
		options.ProblemDetails = &ProblemOptions{TypeBase: base}
		err := New(options).Build()
		if got := err != nil; got != bad {
			t.Errorf("TypeBase %q: Build() = %v, want refused = %v", base, err, bad)
		}
		if bad && err != nil && !strings.Contains(err.Error(), "ProblemDetails.TypeBase") {
			t.Errorf("TypeBase %q: the error does not name the option: %v", base, err)
		}
	}
}
