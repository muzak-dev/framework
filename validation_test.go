package muzak

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"

	"muzak.dev/framework/validate"
)

// signup is the model the validation tests exercise. It covers every shape the
// engine claims to support: transforms, required and optional fields, a
// pointer field, a collection with element rules, and a cross-field check.
type signup struct {
	Email    string   `json:"email"`
	Password string   `json:"password"`
	Confirm  string   `json:"confirm_password"`
	Age      int      `json:"age"`
	Role     string   `json:"role"`
	Tags     []string `json:"tags,omitzero"`
	Website  *string  `json:"website,omitzero"`
	Referrer string   `query:"ref"`
}

// notACommonPassword is a rule of the kind an application would write.
func notACommonPassword(password string) error {
	if strings.EqualFold(password, "password123456") {
		return errors.New("is too common, choose something less guessable")
	}
	return nil
}

func (in *signup) Validate(v *Validation) {
	v.String(&in.Email).Trim().Lower().Required().Email()
	v.String(&in.Password).Required().MinLen(12).Must(notACommonPassword)
	v.String(&in.Confirm).Equal(in.Password).Message("must match the password")
	v.Number(&in.Age).Between(18, 120)
	v.String(&in.Role).OneOf("admin", "editor", "viewer")
	v.Slice(&in.Tags).MaxItems(3).Each(validate.String().MaxLen(8))
	v.String(&in.Website).URL()
	v.String(&in.Referrer).MaxLen(20)

	v.When(in.Role == "admin" && in.Age < 21).
		Reject(&in.Role, "an admin must be at least 21")
}

// signupApp serves the model so that validation can be exercised through the
// whole stack rather than in isolation.
func signupApp(t *testing.T) *App {
	t.Helper()
	app := New(quietOptions())
	app.Post("/signup", func(ctx *Context, in signup) (signup, error) {
		return in, nil
	})
	return mustBuild(t, app)
}

func TestValidationRejectsAndReports(t *testing.T) {
	t.Parallel()
	app := signupApp(t)

	rec := do(t, app, "POST", "/signup?ref=short", `{
		"email": "not-an-email",
		"password": "short",
		"confirm_password": "different",
		"age": 12,
		"role": "wizard",
		"tags": ["fine", "far-too-long-for-this"],
		"website": "not a url"
	}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	envelope := decodeError(t, rec)
	if envelope.Error.Code != CodeValidationError {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, CodeValidationError)
	}

	got := map[string]string{}
	for _, detail := range envelope.Error.Details {
		got[detail.Field] = detail.Issue
	}
	want := map[string]string{
		"email":            "must be a valid email address",
		"password":         "must be at least 12 characters",
		"confirm_password": "must match the password",
		"age":              "must be between 18 and 120",
		"role":             `must be one of "admin", "editor" or "viewer"`,
		"tags[1]":          "must be at most 8 characters",
		"website":          "must be a valid absolute http or https URL",
	}
	for field, issue := range want {
		if got[field] != issue {
			t.Errorf("issue for %q = %q, want %q", field, got[field], issue)
		}
	}
	if len(got) != len(want) {
		t.Errorf("reported %d fields, want %d\n%s", len(got), len(want), rec.Body.String())
	}
}

func TestValidationTransformsReachTheHandler(t *testing.T) {
	t.Parallel()
	app := signupApp(t)

	rec := do(t, app, "POST", "/signup", `{
		"email": "  Rick.Sanchez@Example.TEST  ",
		"password": "a-long-enough-password",
		"confirm_password": "a-long-enough-password",
		"age": 70,
		"role": "admin"
	}`)
	assertStatus(t, rec, http.StatusOK)

	var out signup
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if out.Email != "rick.sanchez@example.test" {
		t.Errorf("email = %q, want it trimmed and lower-cased", out.Email)
	}
}

func TestValidationOptionalFieldsSkipTheirRules(t *testing.T) {
	t.Parallel()
	app := signupApp(t)

	// Website, tags, age and role are all absent. None of them is Required, so
	// none of their other rules has anything to say.
	rec := do(t, app, "POST", "/signup", `{
		"email": "rick@example.test",
		"password": "a-long-enough-password",
		"confirm_password": "a-long-enough-password"
	}`)
	assertStatus(t, rec, http.StatusOK)
}

func TestValidationNilPointerFieldIsSkipped(t *testing.T) {
	t.Parallel()
	app := signupApp(t)

	rec := do(t, app, "POST", "/signup", `{
		"email": "rick@example.test",
		"password": "a-long-enough-password",
		"confirm_password": "a-long-enough-password",
		"website": null
	}`)
	assertStatus(t, rec, http.StatusOK)
}

func TestValidationPointerFieldIsChecked(t *testing.T) {
	t.Parallel()
	app := signupApp(t)

	rec := do(t, app, "POST", "/signup", `{
		"email": "rick@example.test",
		"password": "a-long-enough-password",
		"confirm_password": "a-long-enough-password",
		"website": "https://example.test"
	}`)
	assertStatus(t, rec, http.StatusOK)
}

func TestValidationCustomRuleAndMessage(t *testing.T) {
	t.Parallel()
	app := signupApp(t)

	rec := do(t, app, "POST", "/signup", `{
		"email": "rick@example.test",
		"password": "password123456",
		"confirm_password": "password123456"
	}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	details := decodeError(t, rec).Error.Details
	if len(details) != 1 {
		t.Fatalf("details = %d entries, want 1\n%s", len(details), rec.Body.String())
	}
	if details[0].Field != "password" || !strings.Contains(details[0].Issue, "too common") {
		t.Errorf("detail = %+v", details[0])
	}
}

func TestValidationCrossFieldCondition(t *testing.T) {
	t.Parallel()
	app := signupApp(t)

	rec := do(t, app, "POST", "/signup", `{
		"email": "rick@example.test",
		"password": "a-long-enough-password",
		"confirm_password": "a-long-enough-password",
		"age": 19,
		"role": "admin"
	}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	details := decodeError(t, rec).Error.Details
	if len(details) != 1 {
		t.Fatalf("details = %d entries, want 1\n%s", len(details), rec.Body.String())
	}
	if details[0].Field != "role" || details[0].Issue != "an admin must be at least 21" {
		t.Errorf("detail = %+v", details[0])
	}
}

// TestValidationReportsTheRightLocation checks that a query parameter is
// reported as one, which comes free from the binding plan.
func TestValidationReportsTheRightLocation(t *testing.T) {
	t.Parallel()
	app := signupApp(t)

	rec := do(t, app, "POST", "/signup?ref="+strings.Repeat("x", 30), `{
		"email": "rick@example.test",
		"password": "a-long-enough-password",
		"confirm_password": "a-long-enough-password"
	}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	details := decodeError(t, rec).Error.Details
	if len(details) != 1 {
		t.Fatalf("details = %d entries, want 1\n%s", len(details), rec.Body.String())
	}
	if details[0].Field != "ref" || details[0].Location != "query" {
		t.Errorf("detail = %+v, want the query parameter named", details[0])
	}
}

// TestValidationDoesNotPileOnBindingFailures checks that a field which could
// not be parsed is reported once, by the binder, rather than twice.
func TestValidationDoesNotPileOnBindingFailures(t *testing.T) {
	t.Parallel()
	type in struct {
		Limit int `query:"limit"`
	}
	app := New(quietOptions())
	app.Get("/search", func(ctx *Context, i in) (rtOut, error) { return rtOut{OK: true}, nil })
	mustBuild(t, app)

	rec := do(t, app, "GET", "/search?limit=abc")
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	details := decodeError(t, rec).Error.Details
	if len(details) != 1 {
		t.Fatalf("details = %d entries, want 1\n%s", len(details), rec.Body.String())
	}
	if details[0].Issue != "must be a valid integer" {
		t.Errorf("issue = %q, want the binder's message", details[0].Issue)
	}
}

// TestGuardsRunBeforeValidation is the ordering that keeps an unauthenticated
// caller from learning the shape of a model by sending it rubbish.
func TestGuardsRunBeforeValidation(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithDependencies(func(ctx *Context) error {
		return NewHTTPError(http.StatusUnauthorized, "unauthorized")
	}))
	app.Post("/signup", func(ctx *Context, in signup) (rtOut, error) { return rtOut{}, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/signup", `{"email":"nonsense"}`)
	assertStatus(t, rec, http.StatusUnauthorized)
	if body := rec.Body.String(); strings.Contains(body, "email") {
		t.Errorf("an unauthenticated caller learned about the model: %s", body)
	}
}

func TestSkipValidation(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/strict", func(ctx *Context, in signup) (rtOut, error) { return rtOut{OK: true}, nil })
	app.Post("/repair", func(ctx *Context, in signup) (rtOut, error) { return rtOut{OK: true}, nil },
		SkipValidation())
	mustBuild(t, app)

	body := `{"email":"not-an-email","password":"x","confirm_password":"x"}`
	assertStatus(t, do(t, app, "POST", "/strict", body), http.StatusUnprocessableEntity)
	assertStatus(t, do(t, app, "POST", "/repair", body), http.StatusOK)
}
