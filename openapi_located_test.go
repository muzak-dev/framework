package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sameNameIn binds a query parameter and a body member under one name, with
// rules that disagree about it.
type sameNameIn struct {
	QueryName string `query:"name"`
	BodyName  string `json:"name"`
}

func (in *sameNameIn) Validate(v *Validation) {
	v.String(&in.QueryName).MinLen(2).MaxLen(50)
	v.String(&in.BodyName).Required().MaxLen(5).Matches(`^[a-z]+$`)
}

// TestOpenAPIConstraintsAreKeyedByLocationAndName holds the document to what
// the rules say about each field. A query parameter and a body member may share
// a name, and the rules of one must not be written onto the other.
func TestOpenAPIConstraintsAreKeyedByLocationAndName(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/things", func(ctx *Context, in sameNameIn) (rtOut, error) { return rtOut{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	op := doc.Paths["/things"].Post

	if len(op.Parameters) != 1 || op.Parameters[0].Name != "name" || op.Parameters[0].In != "query" {
		t.Fatalf("parameters = %+v", op.Parameters)
	}
	param := op.Parameters[0]
	if param.Required {
		t.Error("the query parameter is required, on the strength of the body member's Required rule")
	}
	if param.Schema.MaxLength == nil || *param.Schema.MaxLength != 50 || param.Schema.MinLength == nil || *param.Schema.MinLength != 2 {
		t.Errorf("the query parameter's length bounds = %v..%v, want 2..50", param.Schema.MinLength, param.Schema.MaxLength)
	}
	if param.Schema.Pattern != "" {
		t.Errorf("the query parameter carries the body member's pattern %q", param.Schema.Pattern)
	}

	body := op.RequestBody.Content["application/json"].Schema
	member := body.Properties["name"]
	if member == nil {
		t.Fatalf("the body has no name member: %+v", body.Properties)
	}
	if member.MaxLength == nil || *member.MaxLength != 5 || member.Pattern == "" {
		t.Errorf("the body member lost its own rules: %+v", member)
	}
	if member.MinLength != nil {
		t.Errorf("the body member carries the query parameter's minLength %d", *member.MinLength)
	}
	if len(body.Required) != 1 || body.Required[0] != "name" {
		t.Errorf("required = %v, want [name]", body.Required)
	}
}

// TestOpenAPIQueryRuleDoesNotTouchTheBodyMember is the reverse: a rule on the
// query parameter must not set or clear anything on the body member.
func TestOpenAPIQueryRuleDoesNotTouchTheBodyMember(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/things", func(ctx *Context, in reverseIn) (rtOut, error) { return rtOut{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	op := doc.Paths["/things"].Post
	body := op.RequestBody.Content["application/json"].Schema
	// The body member's rules do not say Required, so it is optional; the
	// query parameter is required by its tag. Each keeps its own.
	if len(body.Required) != 0 {
		t.Errorf("required = %v, want none: the body member is optional", body.Required)
	}
	if !op.Parameters[0].Required {
		t.Error("the query parameter is not required")
	}
	if body.Properties["name"].MaxLength != nil {
		t.Error("the query parameter's MaxLen reached the body member")
	}
	if got := op.Parameters[0].Schema.MinLength; got != nil {
		t.Errorf("the body member's MinLen reached the query parameter: %d", *got)
	}
}

type reverseIn struct {
	QueryName string `query:"name" required:"true"`
	BodyName  string `json:"name"`
}

func (in *reverseIn) Validate(v *Validation) {
	v.String(&in.QueryName).MaxLen(9)
	v.String(&in.BodyName).MinLen(3)
}

// TestFailedBindingOfOneLocationDoesNotHideTheOthersName covers the runtime
// side of the same confusion: a query value that could not be parsed must not
// silence the failures of a body member that happens to share its name.
func TestFailedBindingOfOneLocationDoesNotHideTheOthersName(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/things", func(ctx *Context, in failedIn) (rtOut, error) { return rtOut{}, nil })
	mustBuild(t, app)

	req := httptest.NewRequest(http.MethodPost, "/things?name=abc", strings.NewReader(`{"name":"much too long"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	got := map[string]bool{}
	for _, d := range decodeError(t, rec).Error.Details {
		got[d.Location+"."+d.Field] = true
	}
	for _, want := range []string{"query.name", "body.name"} {
		if !got[want] {
			t.Errorf("no failure for %s in %v", want, got)
		}
	}
}

type failedIn struct {
	QueryName int    `query:"name"`
	BodyName  string `json:"name"`
}

func (in *failedIn) Validate(v *Validation) {
	v.Number(&in.QueryName).Min(1)
	v.String(&in.BodyName).MaxLen(5)
}

type formAndQueryIn struct {
	QueryName string `query:"name"`
	FormName  string `form:"name"`
}

func (in *formAndQueryIn) Validate(v *Validation) {
	v.String(&in.QueryName).MaxLen(7)
	v.String(&in.FormName).MinLen(3)
}

// TestOpenAPIFormRulesStayOnTheFormValue keeps the rules of a form value and of
// a query parameter of the same name apart.
func TestOpenAPIFormRulesStayOnTheFormValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/forms", func(ctx *Context, in formAndQueryIn) (rtOut, error) { return rtOut{}, nil })
	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document = %v", err)
	}
	op := doc.Paths["/forms"].Post
	query := op.Parameters[0].Schema
	if query.MaxLength == nil || *query.MaxLength != 7 || query.MinLength != nil {
		t.Errorf("query parameter schema = %+v", query)
	}
	form := op.RequestBody.Content["multipart/form-data"].Schema.Properties["name"]
	if form.MinLength == nil || *form.MinLength != 3 || form.MaxLength != nil {
		t.Errorf("form value schema = %+v", form)
	}
}
