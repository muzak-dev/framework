package muzak

import (
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// engineImports are the packages that make up the translation engine.
//
// Nothing in the root package may reach them. The framework talks to a
// translator through an interface, so an application that does not translate
// links neither the engine, nor the YAML parser it loads locale files with, nor
// the table of plural rules it counts by.
var engineImports = []string{
	"muzak.dev/framework/i18n",
	"muzak.dev/framework/internal/yaml",
}

// TestTheRootPackageDoesNotLinkTheEngine is the structural half of the promise
// that internationalization is optional.
//
// It is a source-level check rather than a behavioural one because that is
// where the property lives: Go links a package when something imports it, so
// the only way to keep the engine out of a binary that does not want it is for
// no reachable file to name it. A single import added here would put the YAML
// parser and ninety plural rules into every Muzak binary ever built, and
// nothing else in the suite would notice.
func TestTheRootPackageDoesNotLinkTheEngine(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++

		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				// coverage: an import path that will not unquote would not have
				// compiled, so the parse above would have failed first.
				continue
			}
			if slices.Contains(engineImports, path) {
				t.Errorf("%s imports %s; the root package must reach the translation engine "+
					"only through the Translator interface, so that a binary which does not "+
					"translate does not carry it", name, path)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no source files were checked, so this test proves nothing")
	}
}

// TestNoMiddlewareWhenI18nIsOff is the runtime half: with no store configured,
// the locale middleware is not in the chain at all, so a request pays nothing
// for a feature it is not using.
func TestNoMiddlewareWhenI18nIsOff(t *testing.T) {
	t.Parallel()
	off := New(quietOptions())
	on := New(func() AppOptions {
		options := quietOptions()
		options.I18n = i18nOptions(t)
		return options
	}())

	if len(on.middleware) != len(off.middleware)+1 {
		t.Errorf("a localized application has %d middleware and a plain one %d, "+
			"want exactly one more", len(on.middleware), len(off.middleware))
	}
}

// plainInput is a model with a rule, so that turning i18n off can be shown not
// to change what a rejected request looks like.
type plainInput struct {
	Email string `json:"email"`
}

func (in *plainInput) Validate(v *Validation) {
	v.String(&in.Email).Required().Email()
}

// TestTheEnvelopeIsUnchangedWhenI18nIsOff pins the response an application
// produces with no store configured.
//
// The envelope is written out here rather than compared against another
// application, because what has to stay the same is not "the same as some other
// build" but "the same as it has always been". A change to any of these bytes
// is a breaking change to every client already parsing them.
func TestTheEnvelopeIsUnchangedWhenI18nIsOff(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/users", func(*Context, plainInput) (struct{}, error) { return struct{}{}, nil })
	mustBuild(t, app)

	rec := do(t, app, http.MethodPost, "/users", `{"email": ""}`)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	// The request identifier is the one member that differs between runs, so
	// the envelope is checked field by field rather than byte for byte.
	body := decodeError(t, rec)
	if body.Error.Code != CodeValidationError {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeValidationError)
	}
	if body.Error.Message != "The request could not be validated." {
		t.Errorf("message = %q, want the fixed English summary", body.Error.Message)
	}
	if body.Error.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", body.Error.Status)
	}
	if len(body.Error.Details) != 1 {
		t.Fatalf("details = %v, want exactly one", body.Error.Details)
	}
	if detail := body.Error.Details[0]; detail.Field != "email" ||
		detail.Location != "body" || detail.Issue != "is required" {
		t.Errorf("detail = %+v, want email/body/is required", detail)
	}
	if body.RequestID == "" {
		t.Error("the envelope carries no request identifier")
	}

	// Neither header is written for an application that does not translate.
	for _, header := range []string{"Content-Language", "Vary"} {
		if got := rec.Header().Get(header); got != "" {
			t.Errorf("%s = %q with i18n off, want nothing", header, got)
		}
	}
}

// TestTheZeroValueIsOff states the contract of the options struct itself, since
// that is what an application relies on when it leaves the field out.
func TestTheZeroValueIsOff(t *testing.T) {
	t.Parallel()
	var zero I18nOptions
	if zero.enabled() {
		t.Error("the zero I18nOptions reports itself enabled, want off")
	}
	if err := zero.validate(); err != nil {
		t.Errorf("the zero I18nOptions failed validation with %v, want no error", err)
	}
	if filled := zero.withDefaults(); filled.enabled() || len(filled.Sources) != 0 {
		t.Errorf("the zero I18nOptions gained defaults: %+v", filled)
	}
}

// TestDefaultsAreFilledOnce checks that an enabled configuration picks up
// everything a source needs without the application naming it.
func TestDefaultsAreFilledOnce(t *testing.T) {
	t.Parallel()
	filled := i18nOptions(t).withDefaults()

	if len(filled.Sources) != 1 || filled.Sources[0] != LocaleFromAcceptLanguage {
		t.Errorf("Sources = %v, want the Accept-Language header alone", filled.Sources)
	}
	if filled.Query != "locale" || filled.Header != "X-Locale" || filled.Cookie != "locale" {
		t.Errorf("the named sources defaulted to %q, %q, %q", filled.Query, filled.Header, filled.Cookie)
	}
	if filled.DefaultLocale != "en" {
		t.Errorf("DefaultLocale = %q, want the store's own default", filled.DefaultLocale)
	}
	if !slices.Contains(filled.AvailableLocales, "es") {
		t.Errorf("AvailableLocales = %v, want the locales the store holds", filled.AvailableLocales)
	}
}
