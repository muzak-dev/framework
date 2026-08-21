package badele

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"badele/validate"
)

// locatedIn declares a rule for a field bound from every place a value can come
// from, so that the rules and the binder are exercised against the same model.
//
// Rules identify a field by its address, which is all the model says. Where the
// value came from is the binder's business, and these tests are what hold the
// two together: a failure has to name the header or cookie the client actually
// sent, not the Go field or the JSON name it would have had in a body.
type locatedIn struct {
	Slug   string   `path:"slug"`
	Limit  int      `query:"limit" default:"5"`
	Mode   string   `header:"X-Mode"`
	Region string   `cookie:"region"`
	Tags   []string `header:"X-Tag"`
	Trace  *string  `header:"X-Trace"`
}

// Validate declares one rule set per field, covering the transforms, the
// element rules and the optional pointer alike.
func (in *locatedIn) Validate(v *Validation) {
	v.String(&in.Slug).MinLen(3)
	v.Number(&in.Limit).Between(1, 50)
	v.String(&in.Mode).Trim().Lower().OneOf("fast", "slow")
	v.String(&in.Region).Trim().Lower().OneOf("eu", "us")
	v.Slice(&in.Tags).MaxItems(2).Each(validate.String().MaxLen(4))
	v.String(&in.Trace).MinLen(6)
}

// locatedApp builds the application these tests share.
func locatedApp(t *testing.T) *App {
	t.Helper()
	app := New(quietOptions())
	app.Get("/things/{slug}", func(ctx *Context, in locatedIn) (locatedIn, error) {
		return in, nil
	})
	return mustBuild(t, app)
}

// locatedRequest builds a request carrying one value for each location.
func locatedRequest(target, mode, region, trace string, tags ...string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if mode != "" {
		req.Header.Set("X-Mode", mode)
	}
	if trace != "" {
		req.Header.Set("X-Trace", trace)
	}
	for _, tag := range tags {
		req.Header.Add("X-Tag", tag)
	}
	if region != "" {
		req.AddCookie(&http.Cookie{Name: "region", Value: region})
	}
	return req
}

func TestValidationNamesTheLocationOfEveryBoundField(t *testing.T) {
	t.Parallel()
	req := locatedRequest("/things/ab?limit=99", "sideways", "asia", "abc", "alpha", "beta", "gamma")
	rec := doRequest(t, locatedApp(t), req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	got := map[string]string{}
	for _, detail := range decodeError(t, rec).Error.Details {
		got[detail.Location+"."+detail.Field] = detail.Issue
	}
	// A client fixing this request needs to know which part of it to change,
	// so every location has to name itself and use the name that was sent.
	want := map[string]string{
		"path.slug":      "must be at least 3 characters",
		"query.limit":    "must be between 1 and 50",
		"header.X-Mode":  `must be one of "fast" or "slow"`,
		"cookie.region":  `must be one of "eu" or "us"`,
		"header.X-Tag":   "must have at most 2 items",
		"header.X-Trace": "must be at least 6 characters",
	}
	for key, issue := range want {
		if got[key] != issue {
			t.Errorf("detail for %s = %q, want %q\nbody: %s", key, got[key], issue, rec.Body.String())
		}
	}
	if len(got) != len(want) {
		t.Errorf("details = %v, want exactly one per located field", got)
	}
}

func TestValidationTransformsAHeaderAndACookieBeforeTheHandlerRuns(t *testing.T) {
	t.Parallel()
	// The rules trim and lower both values, so a header the client sent padded
	// and shouting still satisfies OneOf and reaches the handler cleaned.
	req := locatedRequest("/things/widget", "  FAST  ", "  Eu ", "")
	rec := doRequest(t, locatedApp(t), req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"Slug":"widget","Limit":5,"Mode":"fast","Region":"eu","Tags":[],"Trace":null}`)
}

func TestValidationSkipsAHeaderThatFailedToBind(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	type retriesIn struct {
		Retries int `header:"X-Retries"`
	}
	app.Get("/retry", func(ctx *Context, in retriesIn) (Empty, error) { return Empty{}, nil })

	req := httptest.NewRequest(http.MethodGet, "/retry", nil)
	req.Header.Set("X-Retries", "not-a-number")
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	// A value that could not be parsed has nothing further to say, so the
	// range rule must not pile a second complaint onto the same header.
	details := decodeError(t, rec).Error.Details
	if len(details) != 1 || details[0].Location != "header" || details[0].Field != "X-Retries" {
		t.Fatalf("details = %+v, want one header failure for X-Retries", details)
	}
	if details[0].Issue != "must be a valid integer" {
		t.Errorf("issue = %q, want the binder's message rather than the rule's", details[0].Issue)
	}
}

func TestValidationSkipsAnAbsentOptionalHeader(t *testing.T) {
	t.Parallel()
	// Neither X-Trace nor X-Mode was sent. An optional field that is empty
	// skips its rules, so the request succeeds rather than being told its
	// missing trace is too short.
	rec := doRequest(t, locatedApp(t), locatedRequest("/things/widget", "", "eu", ""))
	assertStatus(t, rec, http.StatusOK)
}

func TestValidationConstrainsHeaderAndCookieParametersInTheDocument(t *testing.T) {
	t.Parallel()
	doc, err := locatedApp(t).Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}

	parameters := map[string]Parameter{}
	for _, parameter := range doc.Paths["/things/{slug}"].Get.Parameters {
		parameters[parameter.In+"."+parameter.Name] = parameter
	}

	// What the rules enforce is what the document promises, for a header and a
	// cookie just as much as for a query parameter.
	if enum := parameters["header.X-Mode"].Schema.Enum; len(enum) != 2 {
		t.Errorf("X-Mode enum = %v, want the two accepted modes", enum)
	}
	if enum := parameters["cookie.region"].Schema.Enum; len(enum) != 2 {
		t.Errorf("region enum = %v, want the two accepted regions", enum)
	}
	tags := parameters["header.X-Tag"].Schema
	if tags.MaxItems == nil || *tags.MaxItems != 2 {
		t.Errorf("X-Tag maxItems = %v, want 2", tags.MaxItems)
	}
	// The element rule lands on the array's items rather than on the array.
	if tags.Items == nil || tags.Items.MaxLength == nil || *tags.Items.MaxLength != 4 {
		t.Errorf("X-Tag items = %+v, want a maxLength of 4 on the elements", tags.Items)
	}
}

// requiredHeaderIn declares the same demand twice, once to the binder and once
// to the rules.
type requiredHeaderIn struct {
	Key string `header:"X-Api-Key" required:"true"`
}

func (in *requiredHeaderIn) Validate(v *Validation) {
	v.String(&in.Key).Required().MinLen(8)
}

func TestValidationDoesNotDoubleReportARequiredHeader(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/keyed", func(ctx *Context, in requiredHeaderIn) (Empty, error) { return Empty{}, nil })

	rec := doRequest(t, mustBuild(t, app), httptest.NewRequest(http.MethodGet, "/keyed", nil))
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	details := decodeError(t, rec).Error.Details
	if len(details) != 1 {
		t.Fatalf("details = %+v, want the missing header reported once", details)
	}
	if details[0].Location != "header" || details[0].Field != "X-Api-Key" || details[0].Issue != "is required" {
		t.Errorf("detail = %+v, want one header failure saying it is required", details[0])
	}
}

// groupedHeaders is a reusable set of headers, embedded rather than repeated.
type groupedHeaders struct {
	Mode string `header:"X-Mode"`
}

// groupedCookies is the cookie half of the same idea.
type groupedCookies struct {
	Region string `cookie:"region"`
}

type groupedValidatedIn struct {
	groupedHeaders
	groupedCookies
}

// Validate binds rules to fields promoted from the embedded groups, which is
// the case where a field's address sits at an offset inside another struct.
func (in *groupedValidatedIn) Validate(v *Validation) {
	v.String(&in.Mode).OneOf("fast", "slow")
	v.String(&in.Region).OneOf("eu", "us")
}

func TestValidationReachesFieldsPromotedFromEmbeddedGroups(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/grouped", func(ctx *Context, in groupedValidatedIn) (Empty, error) { return Empty{}, nil })

	req := httptest.NewRequest(http.MethodGet, "/grouped", nil)
	req.Header.Set("X-Mode", "sideways")
	req.AddCookie(&http.Cookie{Name: "region", Value: "asia"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	// A promoted field is identified by its offset within the outer struct, so
	// this is what proves grouping does not cost a model its field origins.
	got := map[string]string{}
	for _, detail := range decodeError(t, rec).Error.Details {
		got[detail.Location] = detail.Field
	}
	if got["header"] != "X-Mode" || got["cookie"] != "region" {
		t.Errorf("details = %v, want the header and cookie named as the client sent them", got)
	}
}
