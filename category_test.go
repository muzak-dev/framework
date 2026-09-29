package muzak

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// categoryBody is an input with a JSON body, for the routes below that are not
// plain reads.
type categoryBody struct {
	Name string `json:"name"`
}

func withBody(_ *Context, _ categoryBody) (Empty, error) { return Empty{}, nil }

// documentOf builds an application and returns its OpenAPI document parsed as
// generic JSON, which is what a documentation tool reads: an assertion made
// against it is an assertion about the wire form, not about a Go struct.
func documentOf(t *testing.T, app *App) map[string]any {
	t.Helper()
	mustBuild(t, app)
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the rendered document is not valid JSON: %v", err)
	}
	return parsed
}

// operationIn digs one operation out of a parsed document.
func operationIn(t *testing.T, doc map[string]any, method, path string) map[string]any {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	op, _ := item[strings.ToLower(method)].(map[string]any)
	if op == nil {
		t.Fatalf("the document has no %s %s", method, path)
	}
	return op
}

// extensions returns the category and title an operation carries, and whether
// each key is present at all, which is a different question from whether it is
// empty.
func extensions(op map[string]any) (category, title string, hasCategory, hasTitle bool) {
	c, hasCategory := op["x-category"]
	ti, hasTitle := op["x-title"]
	category, _ = c.(string)
	title, _ = ti.(string)
	return
}

func TestCategoryIsInheritedByEveryRouteBeneathARouter(t *testing.T) {
	t.Parallel()
	billing := NewRouter(WithCategory("Billing"))
	billing.Get("/invoices", noop)
	inner := NewRouter()
	inner.Get("/refunds", noop)
	billing.Include(inner, WithPrefix("/nested"))

	app := New(quietOptions())
	app.Include(billing, WithPrefix("/billing"))
	doc := documentOf(t, app)

	for _, path := range []string{"/billing/invoices", "/billing/nested/refunds"} {
		category, _, has, _ := extensions(operationIn(t, doc, "GET", path))
		if !has || category != "Billing" {
			t.Errorf("GET %s x-category = %q (present %v), want Billing", path, category, has)
		}
	}
}

func TestAnInnerCategoryReplacesAnOuterOne(t *testing.T) {
	t.Parallel()
	inner := NewRouter(WithCategory("Inner"))
	inner.Get("/own", noop)
	inner.Get("/route-wins", noop, WithCategory("Route"))

	plain := NewRouter()
	plain.Get("/inherits", noop)

	// The options given where a router is included sit between the parent and
	// the child, so the child's own declaration still wins over them, and a
	// child that declares none takes them.
	own := NewRouter(WithCategory("Own"))
	own.Get("/x", noop)
	bare := NewRouter()
	bare.Get("/x", noop)

	outer := NewRouter(WithCategory("Outer"))
	outer.Get("/top", noop)
	outer.Include(inner, WithPrefix("/inner"))
	outer.Include(plain, WithPrefix("/plain"))
	outer.Include(own, WithPrefix("/own-wins"), WithCategory("AtInclude"))
	outer.Include(bare, WithPrefix("/at-include"), WithCategory("AtInclude"))

	app := New(quietOptions(), WithCategory("App"))
	rootRoute := app.Get("/root", noop)
	app.Include(outer, WithPrefix("/outer"))
	doc := documentOf(t, app)

	want := map[string]string{
		"/root":                   "App",
		"/outer/top":              "Outer",
		"/outer/inner/own":        "Inner",
		"/outer/inner/route-wins": "Route",
		"/outer/plain/inherits":   "Outer",
		"/outer/own-wins/x":       "Own",
		"/outer/at-include/x":     "AtInclude",
	}
	for path, category := range want {
		got, _, has, _ := extensions(operationIn(t, doc, "GET", path))
		if !has || got != category {
			t.Errorf("GET %s x-category = %q (present %v), want %q", path, got, has, category)
		}
	}
	if rootRoute.Category != "App" {
		t.Errorf("Route.Category = %q, want App", rootRoute.Category)
	}
}

func TestManyRoutersMayShareOneCategory(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	for _, prefix := range []string{"/invoices", "/refunds", "/plans"} {
		r := NewRouter(WithCategory("Billing"))
		r.Get("/list", noop)
		app.Include(r, WithPrefix(prefix))
	}
	doc := documentOf(t, app)
	for _, path := range []string{"/invoices/list", "/refunds/list", "/plans/list"} {
		if category, _, _, _ := extensions(operationIn(t, doc, "GET", path)); category != "Billing" {
			t.Errorf("GET %s x-category = %q, want Billing", path, category)
		}
	}
}

func TestRouteCategoryAndTitleAreExposedOnTheRoute(t *testing.T) {
	t.Parallel()
	r := NewRouter(WithCategory("  Billing  "))
	rt := r.Get("/me", noop, Title("  Fetch User Profile "))
	bare := r.Get("/bare", noop)
	app := New(quietOptions())
	app.Include(r)
	mustBuild(t, app)
	// The value is trimmed, so two spellings that differ only in surrounding
	// spaces cannot become two headings.
	if rt.Category != "Billing" || rt.Title != "Fetch User Profile" {
		t.Errorf("Route.Category, Route.Title = %q, %q; want the trimmed values", rt.Category, rt.Title)
	}
	if bare.Title != "" {
		t.Errorf("a route that set no title has Title %q", bare.Title)
	}
}

func TestTheDocumentCarriesCategoryAndTitleAsExactExtensions(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/me", noop, WithCategory("Billing"), Title("Fetch User Profile"), Summary("Read me"))
	app.Get("/only-title", noop, Title("Only Title"))
	app.Get("/only-category", noop, WithCategory("Only Category"))
	mustBuild(t, app)
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want []string
		not  []string
	}{
		{"/me", []string{`"x-category":"Billing"`, `"x-title":"Fetch User Profile"`}, nil},
		{"/only-title", []string{`"x-title":"Only Title"`}, []string{"x-category"}},
		{"/only-category", []string{`"x-category":"Only Category"`}, []string{"x-title"}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(doc.Paths[tc.path].Get)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.want {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s: %s lacks %s", tc.path, raw, want)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(string(raw), not) {
				t.Errorf("%s: %s carries %s, which was never set", tc.path, raw, not)
			}
		}
	}

	// The whole set of keys, so that one sneaking in beside them is noticed.
	raw, err := json.Marshal(doc.Paths["/only-title"].Get)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	slices.Sort(names)
	if want := []string{"operationId", "responses", "x-title"}; !slices.Equal(names, want) {
		t.Errorf("/only-title carries the keys %v, want %v", names, want)
	}
	if got := string(keys["x-title"]); got != `"Only Title"` {
		t.Errorf("x-title = %s", got)
	}
}

func TestAnApplicationThatSetsNeitherDocumentsNothingNew(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/a", noop, Summary("A"), WithTags("t"))
	app.Post("/b", withBody)
	app.WS("/ws", wsEcho)
	app.SSE("/sse", streamItems("x"))
	mustBuild(t, app)
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"x-category", "x-title"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("the document mentions %s though nothing set it", key)
		}
	}
	// A category is not a tag: the tag list is what WithTags alone produced.
	if len(doc.Tags) != 1 || doc.Tags[0].Name != "t" {
		t.Errorf("tags = %+v, want only t", doc.Tags)
	}
}

func TestACategoryIsNotATagAndLeavesTagsAlone(t *testing.T) {
	t.Parallel()
	r := NewRouter(WithCategory("Billing"), WithTags("invoices"))
	r.Get("/x", noop, WithTags("extra"))
	app := New(quietOptions())
	app.Include(r)
	mustBuild(t, app)
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Paths["/x"].Get.Tags; !slices.Equal(got, []string{"invoices", "extra"}) {
		t.Errorf("operation tags = %v, want [invoices extra]", got)
	}
	for _, tag := range doc.Tags {
		if tag.Name == "Billing" {
			t.Error("the category was listed as a tag")
		}
	}
}

func TestCategoryAndTitleReachGenericWebSocketAndSSERoutes(t *testing.T) {
	t.Parallel()
	r := NewRouter(WithCategory("Live"))
	r.Post("/generic", withBody, Title("Generic Route"))
	r.WS("/ws", wsEcho, Title("Open A Socket"))
	r.SSE("/sse", streamItems("x"), Title("Follow Changes"), WithCategory("Streams"))
	app := New(quietOptions())
	app.Include(r)
	doc := documentOf(t, app)

	for _, tc := range []struct{ method, path, category, title string }{
		{"POST", "/generic", "Live", "Generic Route"},
		{"GET", "/ws", "Live", "Open A Socket"},
		{"GET", "/sse", "Streams", "Follow Changes"},
	} {
		category, title, hasC, hasT := extensions(operationIn(t, doc, tc.method, tc.path))
		if !hasC || !hasT || category != tc.category || title != tc.title {
			t.Errorf("%s %s = category %q, title %q; want %q, %q", tc.method, tc.path, category, title, tc.category, tc.title)
		}
	}
}

func TestCategoryAndTitleSurviveVersionedExpansion(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Versioning = VersioningOptions{Type: VersioningURI}
	app := New(opts)
	app.Get("/things", noop, WithVersion("1", "2"), WithCategory("Things"), Title("List Things"))
	doc := documentOf(t, app)
	for _, path := range []string{"/v1/things", "/v2/things"} {
		category, title, _, _ := extensions(operationIn(t, doc, "GET", path))
		if category != "Things" || title != "List Things" {
			t.Errorf("GET %s = %q, %q; want the route's own", path, category, title)
		}
	}
}

func TestBuiltInDocumentationRoutesCarryNothing(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithCategory("Everything"))
	app.Get("/x", noop, Title("X"))
	doc := documentOf(t, app)
	// The documentation page and the document itself are served by the
	// framework, not registered as routes, so an application-wide category
	// reaches neither and the document describes only the application's own.
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) != 1 {
		t.Errorf("the document describes %d paths, want only the application's own: %v", len(paths), paths)
	}
	if category, title, _, _ := extensions(operationIn(t, doc, "GET", "/x")); category != "Everything" || title != "X" {
		t.Errorf("GET /x = %q, %q", category, title)
	}
}

func TestACategoryOrTitleMustBeAUsableLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  string // empty means the value must be accepted
	}{
		{"plain", "Billing", ""},
		{"inner spaces", "User Accounts", ""},
		{"non-ascii", "Fa\u00e7ade", ""},
		{"empty", "", "empty"},
		{"spaces only", "   ", "empty"},
		{"tab", "Bill\ting", "control character or line break"},
		{"newline", "Bill\ning", "control character or line break"},
		{"trailing newline", "Billing\n", "control character or line break"},
		{"carriage return", "Bill\ring", "control character or line break"},
		{"nul", "Bill\x00ing", "control character or line break"},
		{"escape", "\x1b[2Jing", "control character or line break"},
		{"c1 control", "Bill\u0085ing", "control character or line break"},
		{"line separator", "Bill\u2028ing", "control character or line break"},
		{"bidi override", "Bill\u202eing", "control character or line break"},
		{"invalid utf-8", "Bill\xffing", "not valid UTF-8"},
	}
	for _, tc := range cases {
		t.Run("category "+tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			app.Get("/x", noop, WithCategory(tc.value))
			assertLabel(t, app, tc.want, "WithCategory", "GET /x")
		})
		t.Run("title "+tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			app.Get("/x", noop, Title(tc.value))
			assertLabel(t, app, tc.want, "Title", "GET /x")
		})
	}
}

// assertLabel builds an application and checks that it was refused for the
// reason wanted, naming the option and the place, or accepted when want is
// empty.
func assertLabel(t *testing.T, app *App, want, option, place string) {
	t.Helper()
	if want == "" {
		mustBuild(t, app)
		return
	}
	got := buildError(t, app)
	if !strings.Contains(got, want) || !strings.Contains(got, option) || !strings.Contains(got, place) {
		t.Errorf("Build() = %q, want it to name %s and %q and mention %q", got, option, place, want)
	}
}

func TestCategoryAndTitleLengthLimitsCountCharacters(t *testing.T) {
	t.Parallel()
	// A two-byte character, so that a limit counted in bytes would refuse a
	// value that is within it.
	for _, tc := range []struct {
		name string
		opt  func(string) RouteOption
		max  int
	}{
		{"WithCategory", func(v string) RouteOption { return WithCategory(v) }, 64},
		{"Title", func(v string) RouteOption { return Title(v) }, 120},
	} {
		app := New(quietOptions())
		app.Get("/ok", noop, tc.opt(strings.Repeat("\u00e9", tc.max)))
		mustBuild(t, app)

		app = New(quietOptions())
		app.Get("/long", noop, tc.opt(strings.Repeat("\u00e9", tc.max+1)))
		got := buildError(t, app)
		if !strings.Contains(got, tc.name) || !strings.Contains(got, "GET /long") || !strings.Contains(got, "over the limit") {
			t.Errorf("%s: Build() = %q", tc.name, got)
		}
	}
}

func TestARouterCategoryIsCheckedAndTheRouterIsNamed(t *testing.T) {
	t.Parallel()

	t.Run("included router", func(t *testing.T) {
		t.Parallel()
		bad := NewRouter(WithCategory(" "))
		bad.Get("/x", noop)
		app := New(quietOptions())
		app.Include(bad, WithPrefix("/billing"))
		assertLabel(t, app, "empty", "WithCategory", `the router mounted at "/billing"`)
	})
	t.Run("option at the include", func(t *testing.T) {
		t.Parallel()
		child := NewRouter()
		child.Get("/x", noop)
		app := New(quietOptions())
		app.Include(child, WithPrefix("/plans"), WithCategory("bad\nname"))
		assertLabel(t, app, "control character", "WithCategory", `the router mounted at "/plans"`)
	})
	t.Run("root router names its first route", func(t *testing.T) {
		t.Parallel()
		app := New(quietOptions(), WithCategory(strings.Repeat("x", 65)))
		app.Get("/first", noop)
		assertLabel(t, app, "over the limit", "WithCategory", "GET /first")
	})
	t.Run("nested router", func(t *testing.T) {
		t.Parallel()
		inner := NewRouter(WithCategory(""))
		inner.Get("/x", noop)
		outer := NewRouter(WithPrefix("/outer"))
		outer.Include(inner, WithPrefix("/inner"))
		app := New(quietOptions())
		app.Include(outer)
		assertLabel(t, app, "empty", "WithCategory", `"/outer/inner"`)
	})
}

func TestAnInvalidRouterCategoryIsReportedOnce(t *testing.T) {
	t.Parallel()
	bad := NewRouter(WithCategory(""))
	bad.Get("/a", noop)
	bad.Get("/b", noop)
	app := New(quietOptions())
	app.Include(bad, WithPrefix("/x"))
	got := buildError(t, app)
	// The routes beneath only inherit the value, so the router is what is
	// wrong and it is reported alone.
	if n := strings.Count(got, "WithCategory"); n != 1 {
		t.Errorf("the error names WithCategory %d times, want 1: %q", n, got)
	}
}

func TestTitleNeedNotBeUnique(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/a", noop, Title("Same"))
	app.Get("/b", noop, Title("Same"))
	mustBuild(t, app)
}
