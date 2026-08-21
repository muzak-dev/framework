package badele

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
	if limit.Schema.Default != "20" {
		t.Errorf("default = %v, want 20", limit.Schema.Default)
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
		{"defaulted", func(s *Schema) bool { return s.Default == "x" }, "a default"},
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
	if !strings.Contains(second, "badele") {
		t.Errorf("the qualified name %q does not carry the package", second)
	}
}

func TestShortPackage(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"github.com/example/project/internal/models", "models"},
		{"badele", "badele"},
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
	if doc.Info.Title != "Badele API" || doc.Info.Version != "0.1.0" {
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
