package muzak

import (
	"net/http"
	"testing"
	"testing/fstest"

	"muzak.dev/framework/i18n"
)

// quietThread has the shape of the thread model the limit tests count, without
// the counter, so these tests can run beside them without disturbing it.
type quietThread struct {
	Name  string       `json:"name"`
	Reply *quietThread `json:"reply"`
}

func (in *quietThread) Validate(v *Validation) {
	v.String(&in.Name).Required()
	v.Nested(in.Reply)
}

// TestEnglishIsUnchangedForTheValidationLimits holds the two details the
// validation limits add, the marker closing a report that was cut short and
// the refusal of a model nested too deep, to the contract every other message
// keeps: turning internationalization on must not reword them, so each needs a
// key in the shipped locale that says exactly what the Go wording says.
func TestEnglishIsUnchangedForTheValidationLimits(t *testing.T) {
	t.Parallel()

	build := func(store Translator) *App {
		options := quietOptions()
		options.I18n = I18nOptions{Store: store}
		app := New(options)
		app.Post("/tags", func(*Context, requiredTags) (Empty, error) { return Empty{}, nil })
		app.Post("/threads", func(*Context, quietThread) (Empty, error) { return Empty{}, nil })
		return mustBuild(t, app)
	}
	// The limits' own details, identified by their English, since a kind is
	// not part of the response.
	requests := map[string]struct{ body, issue string }{
		"/tags": {
			`{"tags":` + jsonArray(MaxValidationDetails+5, func(int) string { return `""` }) + `}`,
			"has more problems than the 100 listed; correct these and send it again to see the rest",
		},
		"/threads": {threadBody(MaxNestedDepth+3, `"x"`), "must not be nested more than 32 levels deep"},
	}
	for path, request := range requests {
		body := request.body
		before := decodeError(t, do(t, build(nil), http.MethodPost, path, body)).Error.Details
		after := decodeError(t, do(t, build(i18n.Builtin()), http.MethodPost, path, body)).Error.Details
		if len(before) == 0 || len(before) != len(after) {
			t.Fatalf("%s: %d details without i18n, %d with", path, len(before), len(after))
		}
		last, translated := before[len(before)-1], after[len(after)-1]
		if last.Issue != request.issue {
			t.Fatalf("%s: the last detail is %+v, want the limit's own detail", path, last)
		}
		if last.Issue != translated.Issue {
			t.Errorf("%s: the message changed when i18n was turned on:\n without: %q\n    with: %q",
				path, last.Issue, translated.Issue)
		}
	}
}

// TestValidationLimitsAreTranslated pins that the limits' details are looked up
// by their kind like every other rule, with the limit passed as the count, so
// an application can word them in its own language.
func TestValidationLimitsAreTranslated(t *testing.T) {
	t.Parallel()

	const spanishLimits = `es:
  errors:
    messages:
      too_many_problems: "tiene mas de %{count} errores"
      too_deep: "no puede anidarse mas de %{count} niveles"
`
	own, err := i18n.Load(
		fstest.MapFS{"locales/es.yml": &fstest.MapFile{Data: []byte(spanishLimits)}}, "locales")
	if err != nil {
		t.Fatalf("loading the locale: %v", err)
	}
	store, err := i18n.New(i18n.StoreOptions{Backend: i18n.NewChain(own.Backend(), i18n.Builtin().Backend())})
	if err != nil {
		t.Fatalf("chaining the stores: %v", err)
	}
	options := quietOptions()
	options.I18n = I18nOptions{Store: store}
	app := New(options)
	app.Post("/tags", func(*Context, requiredTags) (Empty, error) { return Empty{}, nil })
	app.Post("/threads", func(*Context, quietThread) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	for path, want := range map[string]struct{ body, issue string }{
		"/tags": {
			`{"tags":` + jsonArray(MaxValidationDetails+5, func(int) string { return `""` }) + `}`,
			"tiene mas de 100 errores",
		},
		"/threads": {threadBody(MaxNestedDepth+3, `"x"`), "no puede anidarse mas de 32 niveles"},
	} {
		details := inSpanish(t, app, http.MethodPost, path, want.body).Error.Details
		if got := details[len(details)-1].Issue; got != want.issue {
			t.Errorf("%s: the limit's detail reads %q, want %q", path, got, want.issue)
		}
	}
}
