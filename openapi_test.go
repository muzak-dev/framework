package muzak

import (
	"encoding/json/v2"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"
)

func TestOpenAPIDocument(t *testing.T) {
	t.Parallel()
	app := newIntegrationApp()
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}

	if doc.OpenAPI != OpenAPIVersion {
		t.Errorf("openapi = %q, want %q", doc.OpenAPI, OpenAPIVersion)
	}
	if doc.Info.Title != "Test API" || doc.Info.Version != "1.0.0" {
		t.Errorf("info = %+v", doc.Info)
	}

	for _, path := range []string{"/users/", "/users/me", "/users/{username}", "/items/{id}", "/items/", "/admin/"} {
		if _, described := doc.Paths[path]; !described {
			t.Errorf("the document does not describe %s", path)
		}
	}

	admin := doc.Paths["/admin/"].Post
	if admin == nil {
		t.Fatal("POST /admin/ is not described")
	}
	if admin.Summary != "Admin action" {
		t.Errorf("summary = %q", admin.Summary)
	}
	if _, described := admin.Responses["201"]; !described {
		t.Errorf("the declared status is not described: %v", admin.Responses)
	}
	if _, described := admin.Responses["418"]; !described {
		t.Errorf("WithResponseDoc(418) is not described: %v", admin.Responses)
	}
	if _, described := admin.Responses["default"]; !described {
		t.Error("no default response is described")
	}

	tags := map[string]bool{}
	for _, tag := range doc.Tags {
		tags[tag.Name] = true
	}
	for _, want := range []string{"users", "items", "admin"} {
		if !tags[want] {
			t.Errorf("the document does not list the %q tag", want)
		}
	}
}

func TestOpenAPIParameters(t *testing.T) {
	t.Parallel()
	app := newIntegrationApp()
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}

	byName := map[string]Parameter{}
	for _, p := range doc.Paths["/users/"].Get.Parameters {
		byName[p.Name] = p
	}
	limit, described := byName["limit"]
	if !described {
		t.Fatalf("the limit parameter is not described: %v", byName)
	}
	if limit.In != "query" {
		t.Errorf("in = %q, want query", limit.In)
	}
	if limit.Description != "Max results" {
		t.Errorf("description = %q, want the doc tag", limit.Description)
	}
	if limit.Required {
		t.Error("a parameter with a default is marked required")
	}
	if limit.Schema.Default != int64(20) {
		t.Errorf("default = %#v, want the integer 20", limit.Schema.Default)
	}
	if limit.Schema.Type != "integer" {
		t.Errorf("type = %v, want integer", limit.Schema.Type)
	}

	username := doc.Paths["/users/{username}"].Get.Parameters[0]
	if !username.Required {
		t.Error("a path parameter is not marked required")
	}
	if username.In != "path" {
		t.Errorf("in = %q, want path", username.In)
	}
}

func TestOpenAPIRequestAndResponseBodies(t *testing.T) {
	t.Parallel()
	app := newIntegrationApp()
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}

	post := doc.Paths["/items/"].Post
	if post.RequestBody == nil {
		t.Fatal("POST /items/ has no request body")
	}
	if !post.RequestBody.Required {
		t.Error("the request body is not marked required")
	}
	schema := post.RequestBody.Content["application/json"].Schema
	if schema.Ref == "" {
		t.Errorf("the body schema is inline rather than a reference: %+v", schema)
	}

	name := strings.TrimPrefix(schema.Ref, componentPrefix)
	body, described := doc.Components.Schemas[name]
	if !described {
		t.Fatalf("the referenced schema %q is missing from components", name)
	}
	if _, has := body.Properties["name"]; !has {
		t.Errorf("the body schema has no name property: %+v", body.Properties)
	}
	if len(body.Required) != 1 || body.Required[0] != "name" {
		t.Errorf("required = %v, want [name] (async carries omitzero)", body.Required)
	}
}

func TestOpenAPIMixedInputBodySchema(t *testing.T) {
	t.Parallel()
	type mixed struct {
		ID    string `path:"id"`
		Query string `query:"q"`
		Name  string `json:"name"`
		Note  string `json:"note,omitzero"`
	}
	app := New(quietOptions())
	app.Put("/things/{id}", func(ctx *Context, in mixed) (rtOut, error) { return rtOut{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}

	body := doc.Paths["/things/{id}"].Put.RequestBody.Content["application/json"].Schema
	if body.Ref != "" {
		t.Fatalf("a mixed input produced a reference rather than an inline object: %+v", body)
	}
	for _, located := range []string{"ID", "Query", "id", "q"} {
		if _, present := body.Properties[located]; present {
			t.Errorf("the body schema includes the located field %q", located)
		}
	}
	if _, present := body.Properties["name"]; !present {
		t.Errorf("the body schema is missing name: %+v", body.Properties)
	}
	if len(body.Required) != 1 || body.Required[0] != "name" {
		t.Errorf("required = %v, want [name]", body.Required)
	}
}

func TestOpenAPIEmptyOutputHasNoContent(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Delete("/things/{id}", func(ctx *Context, in struct {
		ID string `path:"id"`
	}) (Empty, error) {
		return Empty{}, nil
	}, Status(http.StatusNoContent))

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	response := doc.Paths["/things/{id}"].Delete.Responses["204"]
	if response.Content != nil {
		t.Errorf("a route returning Empty describes content: %+v", response.Content)
	}
}

func TestSchemaGeneration(t *testing.T) {
	t.Parallel()

	type inner struct {
		Value string `json:"value"`
	}
	type everything struct {
		Str        string            `json:"str" doc:"A string"`
		Int        int               `json:"int"`
		Int32      int32             `json:"int32"`
		Uint       uint              `json:"uint"`
		Float32    float32           `json:"float32"`
		Float64    float64           `json:"float64"`
		Bool       bool              `json:"bool"`
		Time       time.Time         `json:"time"`
		UUID       uuid.UUID         `json:"uuid"`
		Duration   time.Duration     `json:"duration"`
		Bytes      []byte            `json:"bytes"`
		Strings    []string          `json:"strings"`
		Array      [3]int            `json:"array"`
		Map        map[string]int    `json:"map"`
		Nested     inner             `json:"nested"`
		Pointer    *inner            `json:"pointer"`
		PtrScalar  *int              `json:"ptr_scalar"`
		Any        any               `json:"any"`
		Omitted    string            `json:"omitted,omitzero"`
		Defaulted  string            `json:"defaulted" default:"x"`
		Renamed    string            `json:"renamed_field"`
		Untagged   string            //nolint:revive // deliberately untagged
		Ignored    string            `json:"-"`
		unexported string            //nolint:unused // proves unexported fields are skipped
		Func       map[string]string `json:"func"`
	}

	builder := newSchemaBuilder()
	ref := builder.schemaFor(reflect.TypeFor[everything]())
	if ref.Ref == "" {
		t.Fatalf("a named struct was not hoisted into components: %+v", ref)
	}
	schema := builder.schemas["everything"]
	if schema == nil {
		t.Fatalf("components = %v", builder.schemas)
	}

	tests := []struct {
		property string
		check    func(*Schema) bool
		want     string
	}{
		{"str", func(s *Schema) bool { return s.Type == "string" && s.Description == "A string" }, "a described string"},
		{"int", func(s *Schema) bool { return s.Type == "integer" && s.Format == "int64" }, "int64"},
		{"int32", func(s *Schema) bool { return s.Format == "int32" }, "int32"},
		{"uint", func(s *Schema) bool { return s.Type == "integer" }, "integer"},
		{"float32", func(s *Schema) bool { return s.Type == "number" && s.Format == "float" }, "float"},
		{"float64", func(s *Schema) bool { return s.Type == "number" && s.Format == "double" }, "double"},
		{"bool", func(s *Schema) bool { return s.Type == "boolean" }, "boolean"},
		{"time", func(s *Schema) bool { return s.Type == "string" && s.Format == "date-time" }, "date-time"},
		{"uuid", func(s *Schema) bool { return s.Type == "string" && s.Format == "uuid" }, "uuid"},
		{"duration", func(s *Schema) bool { return s.Type == "string" && s.Format == "duration" }, "duration"},
		{"bytes", func(s *Schema) bool { return s.Type == "string" && s.Format == "byte" }, "base64"},
		{"strings", func(s *Schema) bool { return s.Type == "array" && s.Items != nil }, "an array"},
		{"array", func(s *Schema) bool { return s.Type == "array" }, "an array"},
		{"map", func(s *Schema) bool { return s.Type == "object" && s.AdditionalProperties != nil }, "a map"},
		{"nested", func(s *Schema) bool { return s.Ref != "" }, "a reference"},
		{"pointer", func(s *Schema) bool { return len(s.AnyOf) == 2 }, "a nullable reference"},
		{"ptr_scalar", func(s *Schema) bool { types, ok := s.Type.([]string); return ok && len(types) == 2 }, "a nullable scalar"},
		{"any", func(s *Schema) bool { return s.Type == nil }, "an unconstrained value"},
		{"defaulted", func(s *Schema) bool { return s.Default == nil }, "no default, which only a request body member has"},
		{"Untagged", func(s *Schema) bool { return s.Type == "string" }, "the field name"},
		{"func", func(s *Schema) bool { return s.Type == "object" }, "a map"},
	}
	for _, tc := range tests {
		t.Run(tc.property, func(t *testing.T) {
			property, described := schema.Properties[tc.property]
			if !described {
				t.Fatalf("no schema for %q; properties are %v", tc.property, keysOf(schema.Properties))
			}
			if !tc.check(property) {
				t.Errorf("%q = %+v, want %s", tc.property, property, tc.want)
			}
		})
	}

	for _, absent := range []string{"-", "Ignored", "unexported"} {
		if _, present := schema.Properties[absent]; present {
			t.Errorf("%q should not be described", absent)
		}
	}
	if _, present := schema.Properties["renamed_field"]; !present {
		t.Error("a renamed field is not described under its JSON name")
	}
	for _, optional := range []string{"omitted", "pointer", "ptr_scalar", "defaulted"} {
		if sliceContains(schema.Required, optional) {
			t.Errorf("%q is marked required but is optional", optional)
		}
	}
	if !sliceContains(schema.Required, "str") {
		t.Errorf("required = %v, want it to include str", schema.Required)
	}
}

// keysOf lists a map's keys for a failure message.
func keysOf(m map[string]*Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sliceContains reports whether a slice holds a value.
func sliceContains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// TestSchemaHandlesRecursiveTypes checks that a type containing itself
// terminates instead of recursing forever.
func TestSchemaHandlesRecursiveTypes(t *testing.T) {
	t.Parallel()
	type node struct {
		Name     string  `json:"name"`
		Parent   *node   `json:"parent"`
		Children []*node `json:"children"`
	}
	builder := newSchemaBuilder()
	ref := builder.schemaFor(reflect.TypeFor[node]())
	if ref.Ref != componentPrefix+"node" {
		t.Fatalf("ref = %q", ref.Ref)
	}
	schema := builder.schemas["node"]
	if schema.Properties["children"].Items.Ref != componentPrefix+"node" {
		t.Errorf("the recursive child is not a reference: %+v", schema.Properties["children"])
	}
}

// TestSchemaNameCollisions covers two identically named types from different
// packages sharing one document.
func TestSchemaNameCollisions(t *testing.T) {
	t.Parallel()
	builder := newSchemaBuilder()
	first := builder.schemaFor(reflect.TypeFor[Contact]())
	// A second, distinct type with the same name must not overwrite the first.
	builder.names["Contact"] = reflect.TypeFor[License]()
	second := builder.nameFor(reflect.TypeFor[Contact]())

	if first.Ref != componentPrefix+"Contact" {
		t.Errorf("the first type = %q", first.Ref)
	}
	if second == "Contact" {
		t.Error("the colliding name was reused")
	}
	// The qualifier is the last element of the package path, so a type in
	// muzak.dev/framework is disambiguated as "framework.Contact".
	if !strings.Contains(second, "framework") {
		t.Errorf("the qualified name %q does not carry the package", second)
	}
}

func TestShortPackage(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"github.com/example/project/internal/models", "models"},
		{"muzak.dev/framework", "framework"},
		// A path with no separator at all is already its own last element.
		{"muzak", "muzak"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := shortPackage(tc.in); got != tc.want {
			t.Errorf("shortPackage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeSchemaName(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"models.User", "models.User"},
		{"a-b_c", "a-b_c"},
		{"has space", "has_space"},
		{"weird/slash", "weird_slash"},
	}
	for _, tc := range tests {
		if got := sanitizeSchemaName(tc.in); got != tc.want {
			t.Errorf("sanitizeSchemaName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSchemaEmbeddedStructsArePromoted(t *testing.T) {
	t.Parallel()
	type base struct {
		ID string `json:"id"`
	}
	type tagged struct {
		Value string `json:"value"`
	}
	type composed struct {
		base
		tagged `json:"tagged"`
		Name   string `json:"name"`
	}
	builder := newSchemaBuilder()
	builder.schemaFor(reflect.TypeFor[composed]())
	schema := builder.schemas["composed"]

	if _, promoted := schema.Properties["id"]; !promoted {
		t.Errorf("an untagged embedded struct was not promoted: %v", keysOf(schema.Properties))
	}
	if _, nested := schema.Properties["tagged"]; !nested {
		t.Errorf("an embedded struct with a json name was not nested: %v", keysOf(schema.Properties))
	}
}

func TestSchemaEmptyStructHasNoProperties(t *testing.T) {
	t.Parallel()
	type blank struct{}
	builder := newSchemaBuilder()
	builder.schemaFor(reflect.TypeFor[blank]())
	if got := builder.schemas["blank"]; got.Properties != nil {
		t.Errorf("an empty struct described properties: %+v", got.Properties)
	}
}

func TestSchemaForPointerToNamedStruct(t *testing.T) {
	t.Parallel()
	type target struct {
		Value string `json:"value"`
	}
	builder := newSchemaBuilder()
	direct := builder.schemaFor(reflect.TypeFor[target]())
	viaPointer := builder.schemaFor(reflect.TypeFor[*target]())
	if direct.Ref != viaPointer.Ref {
		t.Errorf("a pointer produced %q, want the same reference as %q", viaPointer.Ref, direct.Ref)
	}
}

func TestSchemaForUnrepresentableTypes(t *testing.T) {
	t.Parallel()
	builder := newSchemaBuilder()
	schema := builder.inline(reflect.TypeFor[chan int]())
	if !strings.Contains(schema.Description, "no JSON representation") {
		t.Errorf("schema = %+v, want a description explaining the omission", schema)
	}
}

func TestNullable(t *testing.T) {
	t.Parallel()

	widened := nullable(&Schema{Type: "string"})
	types, ok := widened.Type.([]string)
	if !ok || len(types) != 2 || types[0] != "string" || types[1] != "null" {
		t.Errorf("nullable(string) = %+v", widened)
	}

	fromRef := nullable(&Schema{Ref: componentPrefix + "Thing"})
	if len(fromRef.AnyOf) != 2 || fromRef.AnyOf[0].Ref == "" || fromRef.AnyOf[1].Type != "null" {
		t.Errorf("nullable(ref) = %+v", fromRef)
	}

	// A schema with no scalar type to widen is left alone.
	untouched := nullable(&Schema{})
	if untouched.Type != nil {
		t.Errorf("nullable(empty) = %+v", untouched)
	}
}

func TestIntFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeFor[int8](), "int32"},
		{reflect.TypeFor[int16](), "int32"},
		{reflect.TypeFor[int32](), "int32"},
		{reflect.TypeFor[int64](), "int64"},
		{reflect.TypeFor[int](), "int64"},
		{reflect.TypeFor[uint8](), "int32"},
	}
	for _, tc := range tests {
		if got := intFormat(tc.typ); got != tc.want {
			t.Errorf("intFormat(%s) = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

func TestDocPath(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"/users", "/users"},
		{"/users/{id}", "/users/{id}"},
		{"/files/{path...}", "/files/{path}"},
	}
	for _, tc := range tests {
		route := &Route{Path: tc.in}
		if got := route.docPath(); got != tc.want {
			t.Errorf("docPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOrDefault(t *testing.T) {
	t.Parallel()
	if got := orDefault("", "fallback"); got != "fallback" {
		t.Errorf("orDefault(empty) = %q", got)
	}
	if got := orDefault("value", "fallback"); got != "value" {
		t.Errorf("orDefault(value) = %q", got)
	}
}

func TestPathItemSet(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"GET", "PUT", "POST", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
		item := &PathItem{}
		if !item.set(method, &Operation{OperationID: method}) {
			t.Errorf("set(%q) reported no slot", method)
		}
	}
	item := &PathItem{}
	if item.set("PURGE", &Operation{}) {
		t.Error("set(PURGE) claimed a slot that OpenAPI does not define")
	}
}

// TestUndescribableMethodsAreRoutableButUndocumented pins the behaviour for a
// method OpenAPI has no slot for.
func TestUndescribableMethodsAreRoutableButUndocumented(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Handle("PURGE", "/cache", okHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "PURGE", "/cache"), http.StatusOK)
	if item, described := app.spec.Paths["/cache"]; described && item.Get != nil {
		t.Errorf("PURGE was described in the document: %+v", item)
	}
}

func TestDocumentMarshalIsDeterministic(t *testing.T) {
	t.Parallel()
	doc, err := newIntegrationApp().Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	first, err := doc.Marshal()
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}
	for range 5 {
		again, err := doc.Marshal()
		if err != nil {
			t.Fatalf("Marshal = %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("Marshal is not deterministic, so a generated document cannot be diffed")
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(first, &parsed); err != nil {
		t.Fatalf("the rendered document is not valid JSON: %v", err)
	}
}

func TestDocumentReportsBuildFailures(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("bad-path", okHandler)
	if _, err := app.Document(); err == nil {
		t.Fatal("Document succeeded on an application that cannot build")
	}
}

func TestDocumentIsNilWhenDocsAreDisabled(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.DisableDocs = true
	app := New(opts)
	app.Get("/x", okHandler)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	if doc != nil {
		t.Errorf("Document = %+v, want nil when documentation is disabled", doc)
	}
}

func TestOpenAPIInfoDefaultsAndExtras(t *testing.T) {
	t.Parallel()
	opts := AppOptions{
		LoggerOptions: LoggerOptions{Format: LogFormatNone},
		OpenAPIOptions: OpenAPIOptions{
			Description:    "A described API.",
			TermsOfService: "https://example.test/terms",
			Contact:        &Contact{Name: "Team", Email: "team@example.test"},
			License:        &License{Name: "Apache 2.0", Identifier: "Apache-2.0"},
			Servers:        []Server{{URL: "https://api.example.test", Description: "production"}},
		},
	}
	app := New(opts)
	app.Get("/x", okHandler)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	if doc.Info.Title != "Muzak API" || doc.Info.Version != "0.1.0" {
		t.Errorf("defaults = %q / %q", doc.Info.Title, doc.Info.Version)
	}
	if doc.Info.Description != "A described API." || doc.Info.TermsOfService == "" {
		t.Errorf("info = %+v", doc.Info)
	}
	if doc.Info.Contact == nil || doc.Info.License == nil {
		t.Errorf("contact/license = %+v / %+v", doc.Info.Contact, doc.Info.License)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "https://api.example.test" {
		t.Errorf("servers = %+v", doc.Servers)
	}
}

func TestOperationIDsAreDerivedAndUnique(t *testing.T) {
	t.Parallel()
	app := newIntegrationApp()
	mustBuild(t, app)

	seen := map[string]bool{}
	for _, route := range app.routes {
		if route.OperationID == "" {
			t.Errorf("%s %s has no operation id", route.Method, route.Path)
		}
		if seen[route.OperationID] {
			t.Errorf("operation id %q is used twice", route.OperationID)
		}
		seen[route.OperationID] = true
	}
}

func TestDeprecatedAppearsInTheDocument(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/old", okHandler, Deprecated())
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	if !doc.Paths["/old"].Get.Deprecated {
		t.Error("the route is not marked deprecated")
	}
}

func TestValidationResponseIsDocumentedOnlyWhenInputIsBound(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/nothing", okHandler)
	app.Get("/something", func(ctx *Context, in struct {
		Q string `query:"q"`
	}) (rtOut, error) {
		return rtOut{}, nil
	})

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	if _, described := doc.Paths["/nothing"].Get.Responses["422"]; described {
		t.Error("a route that binds nothing describes a validation failure")
	}
	if _, described := doc.Paths["/something"].Get.Responses["422"]; !described {
		t.Error("a route that binds input does not describe a validation failure")
	}
}

// TestTagsAreDescribedAndOrdered covers what [OpenAPIOptions.Tags] adds to the
// tags a route names: a sentence of explanation, and the running order the
// documentation is presented in.
func TestTagsAreDescribedAndOrdered(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Tags = []Tag{
		{Name: "items", Description: "Everything the catalogue holds."},
		{Name: "admin", Description: "Operations that need a staff token."},
		{Name: "unused", Description: "Described but carried by no route."},
	}

	app := New(opts)
	app.Get("/feed", okHandler, WithTags("feed"))
	app.Get("/admin", okHandler, WithTags("admin"))
	app.Get("/items", okHandler, WithTags("items"))
	mustBuild(t, app)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}

	// The described tags lead, in the order they were declared rather than the
	// order the routes were registered; the undescribed one follows.
	want := []Tag{
		{Name: "items", Description: "Everything the catalogue holds."},
		{Name: "admin", Description: "Operations that need a staff token."},
		{Name: "feed"},
	}
	if len(doc.Tags) != len(want) {
		t.Fatalf("tags = %+v, want %+v", doc.Tags, want)
	}
	for i, tag := range want {
		if doc.Tags[i] != tag {
			t.Errorf("tag %d = %+v, want %+v", i, doc.Tags[i], tag)
		}
	}
}

// TestTagsAreListedOncePerName checks that a tag several routes carry, or one
// described twice, is listed once.
func TestTagsAreListedOncePerName(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Tags = []Tag{{Name: "items", Description: "First."}, {Name: "items", Description: "Second."}}

	app := New(opts)
	app.Get("/items", okHandler, WithTags("items"))
	app.Post("/items", okHandler, WithTags("items"))
	mustBuild(t, app)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	if len(doc.Tags) != 1 || doc.Tags[0].Description != "First." {
		t.Errorf("tags = %+v, want the first description, listed once", doc.Tags)
	}
}

// TestHiddenRoutesDoNotContributeTags checks that a tag carried only by a
// route left out of the document does not appear in it.
func TestHiddenRoutesDoNotContributeTags(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/open", okHandler, WithTags("open"))
	app.Get("/secret", okHandler, WithTags("secret"), Hidden())
	mustBuild(t, app)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	if len(doc.Tags) != 1 || doc.Tags[0].Name != "open" {
		t.Errorf("tags = %+v, want only the tag a documented route carries", doc.Tags)
	}
}

// TestRouteLevelTagsReachTheDocument checks that a tag declared on one route
// groups that operation, both where the route is the only thing that names a
// tag and where it adds one to what its router contributes.
func TestRouteLevelTagsReachTheDocument(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Tags = []Tag{{Name: "audit", Description: "Written from the route itself."}}

	app := New(opts)
	admin := NewRouter(WithPrefix("/admin"), WithTags("admin"))
	admin.Post("/actions", okHandler, WithTags("audit"))
	admin.Get("/actions", okHandler)

	loose := NewRouter()
	loose.Post("/note", okHandler, WithTags("notes"))

	app.Include(admin)
	app.Include(loose)
	mustBuild(t, app)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}

	// The route's own tag adds to the one it inherited, so the operation is
	// grouped under both.
	tagged := doc.Paths["/admin/actions"].Post.Tags
	if len(tagged) != 2 || tagged[0] != "admin" || tagged[1] != "audit" {
		t.Errorf("the route's tags = %v, want [admin audit]", tagged)
	}
	if inherited := doc.Paths["/admin/actions"].Get.Tags; len(inherited) != 1 || inherited[0] != "admin" {
		t.Errorf("a route that declares none = %v, want [admin]", inherited)
	}
	// A router that declares no tags leaves its route grouped by its own.
	if only := doc.Paths["/note"].Post.Tags; len(only) != 1 || only[0] != "notes" {
		t.Errorf("a route-only tag = %v, want [notes]", only)
	}

	// Each of them is listed once in the document, described where the
	// application described it.
	var names []string
	for _, tag := range doc.Tags {
		names = append(names, tag.Name)
		if tag.Name == "audit" && tag.Description != "Written from the route itself." {
			t.Errorf("the route-level tag lost its description: %+v", tag)
		}
	}
	if strings.Join(names, ",") != "audit,admin,notes" {
		t.Errorf("tags = %v, want the described one first, then first-use order", names)
	}
}

// respError is the model a route documents for its failures, standing in for
// an application's own error envelope rather than Muzak's.
type respError struct {
	Message string `json:"message" doc:"What went wrong"`
	Code    string `json:"code" doc:"A machine-readable classifier"`
}

// respConflict is a second model, so that one operation can be seen carrying a
// different schema at two different status codes.
type respConflict struct {
	Existing string `json:"existing"`
}

// TestResponseModelsAreDocumentedPerStatusCode covers what
// [WithResponseModel] adds over [WithResponseDoc]: a schema of the
// application's own choosing at each status code, alongside the one the
// handler's return type describes.
func TestResponseModelsAreDocumentedPerStatusCode(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/users", okHandler,
		Status(http.StatusCreated),
		WithResponseModel[respError](http.StatusBadRequest, "The request was malformed"),
		WithResponseModel[respConflict](http.StatusConflict, "A user with that name exists"),
		WithResponseModel[respError](http.StatusInternalServerError, ""),
	)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	op := doc.Paths["/users"].Post

	// The declared status still describes the handler's return type: a model
	// for a failure does not displace the one the route succeeds with.
	created, described := op.Responses["201"]
	if !described {
		t.Fatalf("the declared status is not described: %v", op.Responses)
	}
	if ref := created.Content["application/json"].Schema.Ref; ref != componentPrefix+"rtOut" {
		t.Errorf("201 schema = %q, want the handler's return type", ref)
	}

	for _, want := range []struct {
		code, schema, description string
	}{
		{"400", "respError", "The request was malformed"},
		{"409", "respConflict", "A user with that name exists"},
		// An empty description falls back to the standard reason phrase.
		{"500", "respError", "Internal Server Error"},
	} {
		response, described := op.Responses[want.code]
		if !described {
			t.Errorf("%s is not described: %v", want.code, op.Responses)
			continue
		}
		if response.Description != want.description {
			t.Errorf("%s description = %q, want %q", want.code, response.Description, want.description)
		}
		if ref := response.Content["application/json"].Schema.Ref; ref != componentPrefix+want.schema {
			t.Errorf("%s schema = %q, want a reference to %s", want.code, ref, want.schema)
		}
	}

	// The models are described once, in the components section, exactly as a
	// return type would be.
	schema, present := doc.Components.Schemas["respError"]
	if !present {
		t.Fatalf("the error model is not in the components section: %v", doc.Components.Schemas)
	}
	if schema.Properties["code"].Description != "A machine-readable classifier" {
		t.Errorf("the model lost its doc tags: %+v", schema.Properties)
	}
}

// TestResponseDocKeepsTheErrorEnvelope covers the outcome a route documents
// without naming a model, which is answered with Muzak's own error response
// and so is described as one.
func TestResponseDocKeepsTheErrorEnvelope(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/items", okHandler,
		WithResponseDoc(http.StatusNotFound, ""),
		WithResponseModel[respError](http.StatusGone, "The item was deleted"),
	)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	op := doc.Paths["/items"].Get

	missing := op.Responses["404"]
	if ref := missing.Content["application/json"].Schema.Ref; ref != componentPrefix+"ErrorResponse" {
		t.Errorf("404 schema = %q, want the error envelope", ref)
	}
	if missing.Description != "Not Found" {
		t.Errorf("404 description = %q, want the standard reason phrase", missing.Description)
	}
	if ref := op.Responses["410"].Content["application/json"].Schema.Ref; ref != componentPrefix+"respError" {
		t.Errorf("410 schema = %q, want the declared model", ref)
	}
}

// TestResponseModelEmptyAndHTML covers the two output types a response model
// shares with a handler's return type: one that carries no body, and one that
// carries markup rather than JSON.
func TestResponseModelEmptyAndHTML(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/feed", okHandler,
		WithResponseModel[Empty](http.StatusNotModified, "The feed has not changed"),
		WithResponseModel[HTML](http.StatusServiceUnavailable, "A maintenance page"),
	)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	op := doc.Paths["/feed"].Get

	if content := op.Responses["304"].Content; content != nil {
		t.Errorf("an Empty model describes content: %v", content)
	}
	page, carried := op.Responses["503"].Content["text/html"]
	if !carried {
		t.Fatalf("an HTML model is not described as text/html: %v", op.Responses["503"].Content)
	}
	if page.Schema.Type != "string" {
		t.Errorf("the HTML schema = %v, want a string", page.Schema.Type)
	}
}

// TestResponseModelsAreInheritedAndOverridden covers a model declared on a
// router, which every route beneath it carries, and what happens when a route
// declares its own for the same status code.
func TestResponseModelsAreInheritedAndOverridden(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	api := NewRouter(WithResponseModel[respError](http.StatusNotFound, "Nothing at that path"))
	api.Get("/items", okHandler)
	api.Get("/users", okHandler,
		WithResponseModel[respConflict](http.StatusNotFound, "No such user"))
	app.Include(api)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}

	inherited := doc.Paths["/items"].Get.Responses["404"]
	if ref := inherited.Content["application/json"].Schema.Ref; ref != componentPrefix+"respError" {
		t.Errorf("the inherited 404 schema = %q, want the router's model", ref)
	}
	own := doc.Paths["/users"].Get.Responses["404"]
	if ref := own.Content["application/json"].Schema.Ref; ref != componentPrefix+"respConflict" {
		t.Errorf("the route's own 404 schema = %q, want it to win over the router's", ref)
	}
	if own.Description != "No such user" {
		t.Errorf("the route's own 404 description = %q", own.Description)
	}
}

// TestDocumentedResponseStatusIsValidated covers a code that is not a status
// code at all, which would otherwise become a response key no client could
// ever receive.
func TestDocumentedResponseStatusIsValidated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opt  RouteOption
	}{
		{name: "WithResponseDoc", opt: WithResponseDoc(999, "impossible")},
		{name: "WithResponseModel", opt: WithResponseModel[respError](42, "impossible")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			app.Get("/x", okHandler, tc.opt)
			if msg := buildError(t, app); !strings.Contains(msg, "not a valid HTTP status code") {
				t.Errorf("Build() = %q, want it to name the invalid status", msg)
			}
		})
	}
}
