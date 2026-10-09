package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"muzak.dev/framework/i18n"
)

type repeatedIn struct {
	Limit    int      `query:"limit"`
	Tags     []string `query:"tag"`
	Optional *[]int   `query:"opt"`
	Tenant   string   `header:"X-Tenant"`
}

type repeatedFormIn struct {
	Name string `form:"name"`
}

// TestRepeatedScalarParameterIsRefused covers a scalar parameter sent more
// than once. The binder took the first value and dropped the rest, while
// FastAPI, Rails and most proxies take the last: when the component that
// checks a request and the one that serves it disagree about which duplicate
// counts, a request can pass the check with one value and be served with the
// other. Refusing the ambiguity leaves nothing to disagree about.
func TestRepeatedScalarParameterIsRefused(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/r", func(_ *Context, in repeatedIn) (repeatedIn, error) { return in, nil })
	app.Post("/f", func(_ *Context, in repeatedFormIn) (repeatedFormIn, error) { return in, nil })
	mustBuild(t, app)

	refused := func(name string, req *http.Request, location, field string) {
		t.Helper()
		rec := doRequest(t, app, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422\nbody: %s", name, rec.Code, rec.Body.String())
			return
		}
		details := decodeError(t, rec).Error.Details
		if len(details) != 1 || details[0].Location != location || details[0].Field != field || details[0].Issue != "must be given only once" {
			t.Errorf("%s: details = %+v, want %s %s given only once", name, details, location, field)
		}
	}
	refused("query", httptest.NewRequest(http.MethodGet, "/r?limit=1&limit=1000", nil), "query", "limit")
	refused("empty query", httptest.NewRequest(http.MethodGet, "/r?limit=&limit=", nil), "query", "limit")

	header := httptest.NewRequest(http.MethodGet, "/r", nil)
	header.Header.Add("X-Tenant", "acme")
	header.Header.Add("X-Tenant", "evil")
	refused("header", header, "header", "X-Tenant")

	form := httptest.NewRequest(http.MethodPost, "/f", strings.NewReader("name=a&name=b"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	refused("form", form, "form", "name")

	// A list still takes every value, behind a pointer too, and a scalar sent
	// once binds as before.
	rec := do(t, app, http.MethodGet, "/r?limit=5&tag=a&tag=b&opt=1&opt=2")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"Limit":5,"Tags":["a","b"],"Optional":[1,2],"Tenant":""}`)
}

// TestRepeatedScalarMessageIsTranslated holds the new message to the rule the
// others keep: it is looked up under the binder's own key.
func TestRepeatedScalarMessageIsTranslated(t *testing.T) {
	t.Parallel()
	own, err := i18n.Load(fstest.MapFS{"locales/es.yml": &fstest.MapFile{Data: []byte(`es:
  muzak:
    binding:
      repeated: "debe darse una sola vez"
`)}}, "locales")
	if err != nil {
		t.Fatal(err)
	}
	store, err := i18n.New(i18n.StoreOptions{Backend: i18n.NewChain(own.Backend(), i18n.Builtin().Backend())})
	if err != nil {
		t.Fatal(err)
	}
	for locale, want := range map[string]string{"es": "debe darse una sola vez", "en": "must be given only once"} {
		options := quietOptions()
		options.I18n = I18nOptions{Store: store}
		app := New(options)
		app.Get("/r", func(_ *Context, in repeatedIn) (repeatedIn, error) { return in, nil })
		mustBuild(t, app)
		req := httptest.NewRequest(http.MethodGet, "/r?limit=1&limit=2", nil)
		req.Header.Set("Accept-Language", locale)
		details := decodeError(t, doRequest(t, app, req)).Error.Details
		if len(details) != 1 || details[0].Issue != want {
			t.Errorf("%s: details = %+v, want %q", locale, details, want)
		}
	}
}
