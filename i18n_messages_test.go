package muzak

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"muzak.dev/framework/i18n"
	"muzak.dev/framework/validate"
)

// everyRule exercises one rule of each kind, so that the messages the framework
// produces can be compared with the ones it ships translations for.
type everyRule struct {
	Blank     string    `json:"blank"`
	Short     string    `json:"short"`
	Long      string    `json:"long"`
	Exact     string    `json:"exact"`
	Email     string    `json:"email"`
	URL       string    `json:"url"`
	UUID      string    `json:"uuid"`
	Pattern   string    `json:"pattern"`
	OneOf     string    `json:"one_of"`
	NotOneOf  string    `json:"not_one_of"`
	Equal     string    `json:"equal"`
	Prefix    string    `json:"prefix"`
	Suffix    string    `json:"suffix"`
	Contains  string    `json:"contains"`
	Min       int       `json:"min"`
	Max       int       `json:"max"`
	Between   int       `json:"between"`
	Positive  int       `json:"positive"`
	Negative  int       `json:"negative"`
	Multiple  int       `json:"multiple"`
	FewItems  []string  `json:"few_items"`
	ManyItems []string  `json:"many_items"`
	Repeated  []string  `json:"repeated"`
	Before    time.Time `json:"before"`
	After     time.Time `json:"after"`
	InBetween time.Time `json:"in_between"`
}

func (in *everyRule) Validate(v *Validation) {
	epoch := time.Unix(0, 0).UTC()
	v.String(&in.Blank).Required()
	v.String(&in.Short).MinLen(5)
	v.String(&in.Long).MaxLen(2)
	v.String(&in.Exact).Len(4)
	v.String(&in.Email).Email()
	v.String(&in.URL).URL()
	v.String(&in.UUID).UUID()
	v.String(&in.Pattern).Matches(`^[0-9]+$`)
	v.String(&in.OneOf).OneOf("red", "green")
	v.String(&in.NotOneOf).NotOneOf("taken")
	v.String(&in.Equal).Equal("secret")
	v.String(&in.Prefix).Prefix("pre")
	v.String(&in.Suffix).Suffix("post")
	v.String(&in.Contains).Contains("mid")
	v.Number(&in.Min).Min(10)
	v.Number(&in.Max).Max(1)
	v.Number(&in.Between).Between(10, 20)
	v.Number(&in.Positive).Positive()
	v.Number(&in.Negative).Negative()
	v.Number(&in.Multiple).MultipleOf(7)
	v.Slice(&in.FewItems).MinItems(3)
	v.Slice(&in.ManyItems).MaxItems(1)
	v.Slice(&in.Repeated).Unique()
	v.Time(&in.Before).Before(epoch)
	v.Time(&in.After).After(time.Now().Add(time.Hour))
	v.Time(&in.InBetween).Between(epoch, epoch.Add(time.Hour))
}

// badRequest is a body that breaks every rule declared above.
const badRequest = `{
	"blank": "",
	"short": "ab",
	"long": "abcdef",
	"exact": "ab",
	"email": "nope",
	"url": "nope",
	"uuid": "nope",
	"pattern": "abc",
	"one_of": "blue",
	"not_one_of": "taken",
	"equal": "wrong",
	"prefix": "nope",
	"suffix": "nope",
	"contains": "nope",
	"min": 1,
	"max": 5,
	"between": 50,
	"positive": -1,
	"negative": 1,
	"multiple": 5,
	"few_items": ["a"],
	"many_items": ["a", "b", "c"],
	"repeated": ["a", "a"],
	"before": "2200-01-01T00:00:00Z",
	"after": "1971-01-01T00:00:00Z",
	"in_between": "2200-01-01T00:00:00Z"
}`

// rulesApp serves the model above, with i18n configured however the caller says.
func rulesApp(t *testing.T, opts I18nOptions) *App {
	t.Helper()
	options := quietOptions()
	options.I18n = opts
	app := New(options)
	app.Post("/rules", func(*Context, everyRule) (struct{}, error) { return struct{}{}, nil })
	return mustBuild(t, app)
}

// TestEnglishIsUnchangedWhenTranslated is the central claim of this feature.
//
// Every message the framework produces exists twice: as the Go wording that has
// always produced it, and as a key in the locale the framework ships. If the
// two ever disagree, turning internationalization on would silently reword an
// application's error responses. This sends the same broken request to an
// application with a translation store and to one without, and requires the two
// bodies to be identical field for field.
func TestEnglishIsUnchangedWhenTranslated(t *testing.T) {
	t.Parallel()
	plain := rulesApp(t, I18nOptions{})
	translated := rulesApp(t, I18nOptions{Store: i18n.Builtin()})

	before := decodeError(t, do(t, plain, http.MethodPost, "/rules", badRequest))
	after := decodeError(t, do(t, translated, http.MethodPost, "/rules", badRequest))

	if len(before.Error.Details) == 0 {
		t.Fatal("the request broke no rules, so this test proves nothing")
	}
	if before.Error.Message != after.Error.Message {
		t.Errorf("the summary changed when i18n was turned on:\n without: %q\n    with: %q",
			before.Error.Message, after.Error.Message)
	}
	if len(before.Error.Details) != len(after.Error.Details) {
		t.Fatalf("the number of details changed: %d without i18n, %d with",
			len(before.Error.Details), len(after.Error.Details))
	}
	for i := range before.Error.Details {
		was, now := before.Error.Details[i], after.Error.Details[i]
		if was.Field != now.Field || was.Location != now.Location || was.Issue != now.Issue {
			t.Errorf("the detail for %q changed when i18n was turned on:\n without: %+v\n    with: %+v",
				was.Field, was, now)
		}
	}
}

// TestEveryRuleHasATranslation is the drift guard: a rule added without a
// message in the shipped locale fails here rather than reaching a client as a
// marker.
func TestEveryRuleHasATranslation(t *testing.T) {
	t.Parallel()
	store := i18n.Builtin()

	for _, kind := range validate.Kinds() {
		if !store.Exists("en", "errors.messages."+string(kind)) {
			t.Errorf("the rule %q has no message in the locale the framework ships", kind)
		}
	}

	// The same for the sentence behind each HTTP status.
	for status := range statusMessages {
		key := "muzak.http." + strconv.Itoa(status)
		if !store.Exists("en", key) {
			t.Errorf("status %d has no sentence at %s", status, key)
		}
		if got := store.T("en", key); got != statusMessages[status] {
			t.Errorf("the sentence for %d differs:\n    Go: %q\n locale: %q",
				status, statusMessages[status], got)
		}
	}

	// And for the two summaries in the envelope.
	if got := store.T("en", "muzak.validation.summary"); got != validationMessage {
		t.Errorf("the validation summary differs:\n    Go: %q\n locale: %q", validationMessage, got)
	}
	if got := store.T("en", "muzak.validation.internal"); got != internalMessage {
		t.Errorf("the internal summary differs:\n    Go: %q\n locale: %q", internalMessage, got)
	}
}

// TestNoMachineCodesInTheLocale keeps the classifiers out of the translated
// material. A client branches on them, so a locale file that "translated" one
// would break every client that reads it.
func TestNoMachineCodesInTheLocale(t *testing.T) {
	t.Parallel()
	store := i18n.Builtin()
	for _, code := range []string{
		CodeValidationError, CodeNotFound, CodeInternalError, CodeBadRequest,
	} {
		if store.Exists("en", "muzak.codes."+code) || store.Exists("en", code) {
			t.Errorf("the locale holds a translation for the machine code %q, want none", code)
		}
	}
}

// spanish is a locale file in Spanish, written without accents so that the Go
// source holding it stays plain ASCII. One key uses a numeric escape, which is
// how a locale file carries anything else.
const spanish = `
es:
  errors:
    messages:
      blank: "es obligatorio"
      email: "debe ser una direccion de correo v\u00e1lida"
      too_short:
        one: "debe tener al menos 1 caracter"
        other: "debe tener al menos %{count} caracteres"
    attributes:
      email:
        blank: "hace falta un correo"
    models:
      enrolment:
        attributes:
          email:
            blank: "hace falta el correo de registro"
  muzak:
    http:
      403: "No tienes acceso a este recurso."
    validation:
      summary: "La solicitud no pudo ser validada."
    binding:
      integer: "debe ser un numero entero valido"
`

// spanishStore chains a Spanish locale in front of the one the framework ships,
// which is how an application supplies one language without restating the
// English of every rule it did not translate.
func spanishStore(t *testing.T) *i18n.Store {
	t.Helper()
	own, err := i18n.Load(
		fstest.MapFS{"locales/es.yml": &fstest.MapFile{Data: []byte(spanish)}}, "locales")
	if err != nil {
		t.Fatalf("loading the Spanish locale: %v", err)
	}
	store, err := i18n.New(i18n.StoreOptions{
		Backend: i18n.NewChain(own.Backend(), i18n.Builtin().Backend()),
	})
	if err != nil {
		t.Fatalf("chaining the stores: %v", err)
	}
	return store
}

// enrolment is a model whose failures are scoped by name, so the lookup chain
// can be shown to prefer the narrowest wording available.
type enrolment struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (in *enrolment) Validate(v *Validation) {
	v.String(&in.Email).Required()
	v.String(&in.Name).Required()
}

// spanishApp serves the enrolment model with the Spanish locale installed.
func spanishApp(t *testing.T) *App {
	t.Helper()
	options := quietOptions()
	options.I18n = I18nOptions{Store: spanishStore(t)}
	app := New(options)
	app.Post("/enrol", func(*Context, enrolment) (struct{}, error) { return struct{}{}, nil })
	app.Get("/forbidden", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, Forbidden("")
	})
	app.Get("/count", func(*Context, struct {
		Limit int `query:"limit"`
	}) (struct{}, error) {
		return struct{}{}, nil
	})
	return mustBuild(t, app)
}

// inSpanish sends a request asking for Spanish and returns the envelope.
func inSpanish(t *testing.T, app *App, method, target, body string) ErrorResponse {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept-Language", "es")
	return decodeError(t, doRequest(t, app, req))
}

// TestValidationMessagesAreTranslated is what the whole feature is for: a
// client that asks for Spanish is told what is wrong with its request in
// Spanish, by a framework whose messages were written in English.
func TestValidationMessagesAreTranslated(t *testing.T) {
	t.Parallel()
	app := spanishApp(t)
	body := inSpanish(t, app, http.MethodPost, "/enrol", `{"email": "", "name": ""}`)

	if body.Error.Message != "La solicitud no pudo ser validada." {
		t.Errorf("the summary is %q, want the Spanish one", body.Error.Message)
	}

	// The name has no wording of its own, so it takes the message every blank
	// field gets. The email has three, and the narrowest wins.
	byField := map[string]string{}
	for _, detail := range body.Error.Details {
		byField[detail.Field] = detail.Issue
	}
	if got := byField["name"]; got != "es obligatorio" {
		t.Errorf("the name says %q, want the general Spanish message", got)
	}
	if got := byField["email"]; got != "hace falta el correo de registro" {
		t.Errorf("the email says %q, want the message scoped to this model and field", got)
	}
}

// TestMessageScopesNarrowestFirst walks the four levels a message is looked up
// under, removing one at a time.
func TestMessageScopesNarrowestFirst(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		locale string
		want   string
	}{
		{
			name: "the model and the field together",
			locale: `es:
  errors:
    messages:
      blank: general
    attributes:
      email:
        blank: by field
    models:
      enrolment:
        attributes:
          email:
            blank: by model and field
`,
			want: "by model and field",
		},
		{
			name: "the model alone",
			locale: `es:
  errors:
    messages:
      blank: general
    attributes:
      email:
        blank: by field
    models:
      enrolment:
        blank: by model
`,
			want: "by model",
		},
		{
			name: "the field alone",
			locale: `es:
  errors:
    messages:
      blank: general
    attributes:
      email:
        blank: by field
`,
			want: "by field",
		},
		{
			name: "nothing but the rule",
			locale: `es:
  errors:
    messages:
      blank: general
`,
			want: "general",
		},
		{
			name: "nothing at all falls back to the framework's English",
			locale: `es:
  unrelated: x
`,
			want: "is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			own, err := i18n.Load(fstest.MapFS{
				"locales/es.yml": &fstest.MapFile{Data: []byte(tc.locale)},
			}, "locales")
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			store, err := i18n.New(i18n.StoreOptions{
				Backend: i18n.NewChain(own.Backend(), i18n.Builtin().Backend()),
			})
			if err != nil {
				t.Fatalf("chaining: %v", err)
			}

			options := quietOptions()
			options.I18n = I18nOptions{Store: store}
			app := New(options)
			app.Post("/enrol", func(*Context, enrolment) (struct{}, error) { return struct{}{}, nil })
			mustBuild(t, app)

			body := inSpanish(t, app, http.MethodPost, "/enrol", `{"email": "", "name": "ok"}`)
			if len(body.Error.Details) != 1 {
				t.Fatalf("details = %v, want exactly the email", body.Error.Details)
			}
			if got := body.Error.Details[0].Issue; got != tc.want {
				t.Errorf("the email says %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTranslatedPluralForm checks that a count both prints and chooses, which
// is the one thing a translated message needs that a formatted one does not.
func TestTranslatedPluralForm(t *testing.T) {
	t.Parallel()
	store := spanishStore(t)
	cases := map[int]string{
		1: "debe tener al menos 1 caracter",
		8: "debe tener al menos 8 caracteres",
	}
	for count, want := range cases {
		if got := store.T("es", "errors.messages.too_short", "count", count); got != want {
			t.Errorf("too_short with a count of %d = %q, want %q", count, got, want)
		}
	}
}

// TestTranslatedNonASCII proves a locale file carries characters this
// repository's own source may not, by writing them as escapes.
func TestTranslatedNonASCII(t *testing.T) {
	t.Parallel()
	got := spanishStore(t).T("es", "errors.messages.email")
	if !strings.Contains(got, "\u00e1lida") {
		t.Errorf("the Spanish message is %q, want the accented word", got)
	}
}

// TestStatusMessagesAreTranslated covers the twenty standard outcomes, which an
// application reaches for without writing a message at all.
func TestStatusMessagesAreTranslated(t *testing.T) {
	t.Parallel()
	app := spanishApp(t)
	body := inSpanish(t, app, http.MethodGet, "/forbidden", "")
	if body.Error.Message != "No tienes acceso a este recurso." {
		t.Errorf("the 403 says %q, want the Spanish sentence", body.Error.Message)
	}
	if body.Error.Code != CodeForbidden {
		t.Errorf("the code is %q, want it untranslated", body.Error.Code)
	}

	// A message the caller wrote is the caller's own words, and is not replaced
	// by a translation of the standard sentence.
	options := quietOptions()
	options.I18n = I18nOptions{Store: spanishStore(t)}
	own := New(options)
	own.Get("/mine", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, Forbidden("only editors may do that")
	})
	mustBuild(t, own)
	body = inSpanish(t, own, http.MethodGet, "/mine", "")
	if body.Error.Message != "only editors may do that" {
		t.Errorf("a message written by hand became %q, want it left alone", body.Error.Message)
	}
}

// TestBinderMessagesAreTranslated covers a value that could not be read at all,
// which is reported before any rule runs.
func TestBinderMessagesAreTranslated(t *testing.T) {
	t.Parallel()
	app := spanishApp(t)
	body := inSpanish(t, app, http.MethodGet, "/count?limit=abc", "")
	if len(body.Error.Details) != 1 {
		t.Fatalf("details = %v, want exactly the limit", body.Error.Details)
	}
	if got := body.Error.Details[0].Issue; got != "debe ser un numero entero valido" {
		t.Errorf("the limit says %q, want the Spanish message", got)
	}
}

// TestMessageKeyOverride covers an application naming its own translation for
// one rule on one field.
func TestMessageKeyOverride(t *testing.T) {
	t.Parallel()
	own, err := i18n.Load(fstest.MapFS{"locales/es.yml": &fstest.MapFile{
		Data: []byte(`es:
  errors:
    password:
      too_short: "la clave necesita %{count} caracteres"
    booking:
      ends_before_it_starts: "el final va despues del principio"
`),
	}}, "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	store, err := i18n.New(i18n.StoreOptions{
		Backend: i18n.NewChain(own.Backend(), i18n.Builtin().Backend()),
	})
	if err != nil {
		t.Fatalf("chaining: %v", err)
	}

	options := quietOptions()
	options.I18n = I18nOptions{Store: store}
	app := New(options)
	app.Post("/signup", func(*Context, keyed) (struct{}, error) { return struct{}{}, nil })
	mustBuild(t, app)

	body := inSpanish(t, app, http.MethodPost, "/signup", `{"password": "ab", "start": 5, "end": 1}`)
	byField := map[string]string{}
	for _, detail := range body.Error.Details {
		byField[detail.Field] = detail.Issue
	}
	if got := byField["password"]; got != "la clave necesita 12 caracteres" {
		t.Errorf("the password says %q, want the named translation with its count", got)
	}
	if got := byField["end"]; got != "el final va despues del principio" {
		t.Errorf("the cross-field check says %q, want the named translation", got)
	}

	// With no translation store the same rules read as English, which is what
	// an application that has not been localized still gets.
	plain := New(quietOptions())
	plain.Post("/signup", func(*Context, keyed) (struct{}, error) { return struct{}{}, nil })
	mustBuild(t, plain)
	body = decodeError(t, do(t, plain, http.MethodPost, "/signup", `{"password": "ab", "start": 5, "end": 1}`))
	byField = map[string]string{}
	for _, detail := range body.Error.Details {
		byField[detail.Field] = detail.Issue
	}
	if got := byField["password"]; got != "must be at least 12 characters" {
		t.Errorf("with no store the password says %q, want the rule's English", got)
	}
	if got := byField["end"]; got != "errors.booking.ends_before_it_starts" {
		t.Errorf("with no store the cross-field check says %q, want the key it named", got)
	}
}

// keyed is a model that names its own translations, both for a rule and for a
// check written as an ordinary condition.
type keyed struct {
	Password string `json:"password"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

func (in *keyed) Validate(v *Validation) {
	v.String(&in.Password).MinLen(12).MessageKey("errors.password.too_short")
	v.When(in.End < in.Start).RejectKey(&in.End, "errors.booking.ends_before_it_starts")
}

// TestTranslationFallsBackToEnglish covers the paths where nothing translates a
// failure, which is what an application gets for every rule it has not
// localized yet.
func TestTranslationFallsBackToEnglish(t *testing.T) {
	t.Parallel()
	// A store holding a locale with nothing in it at all, so that every lookup
	// misses and every message has to fall through.
	own, err := i18n.Load(fstest.MapFS{
		"locales/es.yml": &fstest.MapFile{Data: []byte("es:\n  unrelated: x\n")},
	}, "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	options := quietOptions()
	options.I18n = I18nOptions{Store: own}
	app := New(options)
	app.Post("/enrol", func(*Context, enrolment) (struct{}, error) { return struct{}{}, nil })
	app.Get("/forbidden", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, Forbidden("")
	})
	app.Get("/count", func(*Context, struct {
		Limit int `query:"limit"`
	}) (struct{}, error) {
		return struct{}{}, nil
	})
	app.Post("/keyed", func(*Context, keyed) (struct{}, error) { return struct{}{}, nil })
	mustBuild(t, app)

	// A rule with no translation anywhere keeps the English the framework has
	// always produced, rather than becoming a marker.
	body := inSpanish(t, app, http.MethodPost, "/enrol", `{"email": "", "name": ""}`)
	for _, detail := range body.Error.Details {
		if detail.Issue != "is required" {
			t.Errorf("the %s says %q, want the framework's English", detail.Field, detail.Issue)
		}
	}
	if body.Error.Message != validationMessage {
		t.Errorf("the summary is %q, want the English one", body.Error.Message)
	}

	// The same for a status sentence and for a value that could not be read.
	if got := inSpanish(t, app, http.MethodGet, "/forbidden", "").Error.Message; got != statusMessages[403] {
		t.Errorf("the 403 says %q, want the English sentence", got)
	}
	binding := inSpanish(t, app, http.MethodGet, "/count?limit=abc", "")
	if got := binding.Error.Details[0].Issue; got != "must be a valid integer" {
		t.Errorf("the limit says %q, want the English message", got)
	}

	// A key nobody translated falls back to the rule's own English, and a
	// rejection's key falls back to the key, which names what to add.
	keyedBody := inSpanish(t, app, http.MethodPost, "/keyed", `{"password": "ab", "start": 5, "end": 1}`)
	byField := map[string]string{}
	for _, detail := range keyedBody.Error.Details {
		byField[detail.Field] = detail.Issue
	}
	if got := byField["password"]; got != "must be at least 12 characters" {
		t.Errorf("an untranslated key gave %q, want the rule's English", got)
	}
	if got := byField["end"]; got != "errors.booking.ends_before_it_starts" {
		t.Errorf("an untranslated rejection gave %q, want the key it named", got)
	}
}

// TestInternalErrorIsTranslated covers the last of the three summaries, which
// is the one a handler reaches by failing unexpectedly.
func TestInternalErrorIsTranslated(t *testing.T) {
	t.Parallel()
	own, err := i18n.Load(fstest.MapFS{"locales/es.yml": &fstest.MapFile{
		Data: []byte("es:\n  muzak:\n    validation:\n      internal: \"Algo salio mal.\"\n"),
	}}, "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	options := quietOptions()
	options.I18n = I18nOptions{Store: own}
	app := New(options)
	app.Get("/boom", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, errors.New("a database went away")
	})
	mustBuild(t, app)

	body := inSpanish(t, app, http.MethodGet, "/boom", "")
	if body.Error.Message != "Algo salio mal." {
		t.Errorf("the internal error says %q, want the Spanish sentence", body.Error.Message)
	}
	if strings.Contains(body.Error.Message, "database") {
		t.Error("the internal error disclosed what actually failed")
	}
}

// TestHTTPErrorWithMessageKey covers an application naming a translation for an
// error it raises itself.
func TestHTTPErrorWithMessageKey(t *testing.T) {
	t.Parallel()
	own, err := i18n.Load(fstest.MapFS{"locales/es.yml": &fstest.MapFile{
		Data: []byte("es:\n  errors:\n    access:\n      denied: \"Acceso denegado.\"\n"),
	}}, "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	options := quietOptions()
	options.I18n = I18nOptions{Store: own}
	app := New(options)
	app.Get("/editors", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, Forbidden("only editors may do that").
			WithMessageKey("errors.access.denied")
	})
	app.Get("/untranslated", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, Forbidden("only editors may do that").
			WithMessageKey("errors.access.nothing_here")
	})
	mustBuild(t, app)

	if got := inSpanish(t, app, http.MethodGet, "/editors", "").Error.Message; got != "Acceso denegado." {
		t.Errorf("the message is %q, want the named translation", got)
	}
	// The message already written stays the fallback for a key nobody
	// translated, so an error is never left without one.
	if got := inSpanish(t, app, http.MethodGet, "/untranslated", "").Error.Message; got != "only editors may do that" {
		t.Errorf("the message is %q, want the fallback that was written by hand", got)
	}
}

// TestDetailsWithNothingToTranslate covers the shape a custom error takes when
// it carries details of its own, which have no rule behind them.
func TestDetailsWithNothingToTranslate(t *testing.T) {
	t.Parallel()
	options := quietOptions()
	options.I18n = I18nOptions{Store: spanishStore(t)}
	app := New(options)
	app.Get("/custom", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, BadRequest("something is off").WithDetails(
			ErrorDetail{Field: "thing", Location: "body", Issue: "is not right"},
		)
	})
	mustBuild(t, app)

	body := inSpanish(t, app, http.MethodGet, "/custom", "")
	if len(body.Error.Details) != 1 || body.Error.Details[0].Issue != "is not right" {
		t.Errorf("details = %v, want the one written by hand, untouched", body.Error.Details)
	}
}

// everyFormat exercises one rule of each kind added alongside the formats, so
// that the wording they produce can be compared with the wording the shipped
// locale carries for them.
type everyFormat struct {
	Secure   string    `json:"secure"`
	Scheme   string    `json:"scheme"`
	Host     string    `json:"host"`
	Address  string    `json:"address"`
	V4       string    `json:"v4"`
	V6       string    `json:"v6"`
	Network  string    `json:"network"`
	Hardware string    `json:"hardware"`
	Reserved string    `json:"reserved"`
	Blank    string    `json:"blank"`
	Control  string    `json:"control"`
	Letters  string    `json:"letters"`
	Alnum    string    `json:"alnum"`
	Digits   string    `json:"digits"`
	Plain    string    `json:"plain"`
	Slug     string    `json:"slug"`
	Hex      string    `json:"hex"`
	Colour   string    `json:"colour"`
	Encoded  string    `json:"encoded"`
	Document string    `json:"document"`
	Version  string    `json:"version"`
	Phone    string    `json:"phone"`
	Language string    `json:"language"`
	Zone     string    `json:"zone"`
	Country  string    `json:"country"`
	Currency string    `json:"currency"`
	Echo     string    `json:"echo"`
	Small    string    `json:"small"`
	Large    string    `json:"large"`
	Above    int       `json:"above"`
	Below    int       `json:"below"`
	NotNeg   int       `json:"not_neg"`
	NotPos   int       `json:"not_pos"`
	Whole    float64   `json:"whole"`
	Port     int       `json:"port"`
	Choice   int       `json:"choice"`
	Exactly  []string  `json:"exactly"`
	Filled   []string  `json:"filled"`
	Needs    []string  `json:"needs"`
	Forbids  []string  `json:"forbids"`
	Gone     time.Time `json:"gone"`
	Coming   time.Time `json:"coming"`
	Near     time.Time `json:"near"`
}

func (in *everyFormat) Validate(v *Validation) {
	v.String(&in.Secure).HTTPS()
	v.String(&in.Scheme).URLWithSchemes("s3")
	v.String(&in.Host).Host()
	v.String(&in.Address).IP()
	v.String(&in.V4).IPv4()
	v.String(&in.V6).IPv6()
	v.String(&in.Network).CIDR()
	v.String(&in.Hardware).MAC()
	v.String(&in.Reserved).MatchesNot(`^tmp-`)
	v.String(&in.Blank).NotBlank()
	v.String(&in.Control).NoControl()
	v.String(&in.Letters).Alpha()
	v.String(&in.Alnum).Alphanumeric()
	v.String(&in.Digits).Numeric()
	v.String(&in.Plain).ASCII()
	v.String(&in.Slug).Slug()
	v.String(&in.Hex).Hex()
	v.String(&in.Colour).HexColour()
	v.String(&in.Encoded).Base64()
	v.String(&in.Document).JSON()
	v.String(&in.Version).Semver()
	v.String(&in.Phone).E164()
	v.String(&in.Language).LanguageTag()
	v.String(&in.Zone).Timezone()
	v.String(&in.Country).CountryCode()
	v.String(&in.Currency).CurrencyCode()
	v.String(&in.Echo).EqualFold("secret")
	v.String(&in.Small).MinBytes(8)
	v.String(&in.Large).MaxBytes(2)
	v.Number(&in.Above).GreaterThan(10)
	v.Number(&in.Below).LessThan(1)
	v.Number(&in.NotNeg).NonNegative()
	v.Number(&in.NotPos).NonPositive()
	v.Number(&in.Whole).Whole()
	v.Number(&in.Port).Port()
	v.Number(&in.Choice).OneOf(1, 2)
	v.Slice(&in.Exactly).Items(3)
	v.Slice(&in.Filled).NotEmpty()
	v.Slice(&in.Needs).Contains("read")
	v.Slice(&in.Forbids).Excludes("*")
	v.Time(&in.Gone).Past()
	v.Time(&in.Coming).Future()
	v.Time(&in.Near).Within(time.Minute)
}

// brokenFormats is a body that breaks every rule the model above declares.
const brokenFormats = `{
	"secure": "http://x.dev", "scheme": "https://x.dev", "host": "-bad.example",
	"address": "nope", "v4": "::1", "v6": "127.0.0.1", "network": "10.0.0.0",
	"hardware": "nope", "reserved": "tmp-1", "blank": "   ", "control": "a\rb",
	"letters": "a1", "alnum": "a 1", "digits": "12a", "plain": "caf\u00e9",
	"slug": "Not A Slug", "hex": "ghij", "colour": "fff", "encoded": "***",
	"document": "{", "version": "1.4", "phone": "0555", "language": "pt_BR",
	"zone": "Mars/Olympus", "country": "tr", "currency": "try", "echo": "other",
	"small": "ab", "large": "abcd",
	"above": 1, "below": 5, "not_neg": -1, "not_pos": 1, "whole": 1.5,
	"port": 70000, "choice": 5,
	"exactly": ["a"], "filled": [], "needs": ["write"], "forbids": ["*"],
	"gone": "2200-01-01T00:00:00Z", "coming": "1971-01-01T00:00:00Z",
	"near": "1971-01-01T00:00:00Z"
}`

// TestEnglishIsUnchangedForTheNewRules holds the rules added alongside the
// formats to the same contract as the ones that came before.
//
// Every message exists twice: as the Go wording that produces it, and as a key
// in the shipped locale. If the two disagree, an application that turns
// internationalization on has its error responses silently reworded, which is
// the one thing this feature must never do.
func TestEnglishIsUnchangedForTheNewRules(t *testing.T) {
	t.Parallel()

	build := func(store Translator) *App {
		options := quietOptions()
		options.I18n = I18nOptions{Store: store}
		app := New(options)
		app.Post("/formats", func(*Context, everyFormat) (struct{}, error) {
			return struct{}{}, nil
		})
		return mustBuild(t, app)
	}

	before := decodeError(t, do(t, build(nil), http.MethodPost, "/formats", brokenFormats))
	after := decodeError(t, do(t, build(i18n.Builtin()), http.MethodPost, "/formats", brokenFormats))

	// Every rule the model declares has to have produced a failure, or the
	// comparison below would be comparing nothing.
	if len(before.Error.Details) < 40 {
		t.Fatalf("only %d rules failed, want every one of them", len(before.Error.Details))
	}
	if len(before.Error.Details) != len(after.Error.Details) {
		t.Fatalf("the number of details changed: %d without i18n, %d with",
			len(before.Error.Details), len(after.Error.Details))
	}
	for i := range before.Error.Details {
		was, now := before.Error.Details[i], after.Error.Details[i]
		if was.Field != now.Field || was.Issue != now.Issue {
			t.Errorf("the message for %q changed when i18n was turned on:\n without: %q\n    with: %q",
				was.Field, was.Issue, now.Issue)
		}
	}
}

// A translation that names a value the framework's call site never passes
// fails to render, and the default exception handler answers a failure with
// the empty string. The response then went out with a blank message, where the
// English the framework would otherwise have produced is what it should say.
func TestATranslationThatCannotRenderFallsBackToEnglish(t *testing.T) {
	t.Parallel()
	own, err := i18n.Load(fstest.MapFS{"locales/es.yml": &fstest.MapFile{Data: []byte(`es:
  errors:
    messages:
      blank: "%{oops} vacio"
  muzak:
    http:
      403: "%{oops}"
    validation:
      summary: "%{oops}"
`)}}, "locales")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	options := quietOptions()
	options.I18n = I18nOptions{Store: own}
	app := New(options)
	app.Post("/enrol", func(*Context, enrolment) (struct{}, error) { return struct{}{}, nil })
	app.Get("/forbidden", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, Forbidden("")
	})
	app.Get("/keyed", func(*Context, struct{}) (struct{}, error) {
		return struct{}{}, Forbidden("no entry").WithMessageKey("errors.messages.blank")
	})
	mustBuild(t, app)

	body := inSpanish(t, app, http.MethodPost, "/enrol", `{"email": "", "name": ""}`)
	if body.Error.Message != validationMessage {
		t.Errorf("the summary is %q, want the English one", body.Error.Message)
	}
	for _, detail := range body.Error.Details {
		if detail.Issue != "is required" {
			t.Errorf("the %s says %q, want the framework's English", detail.Field, detail.Issue)
		}
	}
	if got := inSpanish(t, app, http.MethodGet, "/forbidden", "").Error.Message; got != statusMessages[403] {
		t.Errorf("the 403 says %q, want the English sentence", got)
	}
	if got := inSpanish(t, app, http.MethodGet, "/keyed", "").Error.Message; got != "no entry" {
		t.Errorf("a keyed message says %q, want the error's own", got)
	}
}
