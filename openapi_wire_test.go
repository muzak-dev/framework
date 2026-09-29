package muzak

import (
	"net/http"
	"regexp"
	"testing"
	"time"
)

// TestDurationSchemaMatchesWhatTheRuntimeReads holds the description of a
// time.Duration to the text the binder and the JSON codec actually read.
//
// JSON Schema's "duration" format is ISO 8601, which "1500ms" is not, so a
// client or a gateway that checks formats would refuse the very value the
// document tells it to send. The text is described by its pattern instead.
func TestDurationSchemaMatchesWhatTheRuntimeReads(t *testing.T) {
	t.Parallel()
	schema := newSchemaBuilder().inline(durationType)
	if schema.Type != "string" || schema.Format != "" || schema.Pattern == "" {
		t.Fatalf("schema = %+v, want a string described by a pattern and no format", schema)
	}
	pattern := regexp.MustCompile(schema.Pattern)
	for _, text := range []string{
		"1500ms", "1.5s", "2h45m", "-3m", "+5s", "0", "1h", "100ns", "5us", "5\u00b5s", "5\u03bcs",
		".5s", "1.s", "1h30m15.5s", "0s", "-0",
		"", "5", "ms", "1 s", "1d", "PT1.5S", "1h 30m", "1.5.2s", "--1s", "1s-", "soon",
	} {
		_, err := time.ParseDuration(text)
		if accepted := pattern.MatchString(text); accepted != (err == nil) {
			t.Errorf("%q: the pattern says %v, ParseDuration says %v", text, accepted, err == nil)
		}
	}
}

type bytesIn struct {
	Data []byte `query:"data"`
	Sig  []byte `header:"X-Sig"`
}

// TestByteSliceParameterIsDescribedAsWhatItBinds shows a []byte parameter for
// what it is: the binder reads each value as one byte, so `?data=65` is
// []byte{65}, where a JSON body carries the same type as base64 text.
func TestByteSliceParameterIsDescribedAsWhatItBinds(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/b", func(ctx *Context, in bytesIn) (bytesIn, error) { return in, nil })
	mustBuild(t, app)

	rec := do(t, app, "POST", "/b?data=65&data=66", "")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"Data":"QUI=","Sig":""}`)
	assertStatus(t, do(t, app, "POST", "/b?data=QUI=", ""), http.StatusUnprocessableEntity)

	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	for _, parameter := range doc.Paths["/b"].Post.Parameters {
		schema := parameter.Schema
		if schema.Type != "array" || schema.Items == nil || schema.Items.Type != "integer" ||
			schema.Items.Minimum == nil || *schema.Items.Minimum != 0 || schema.Items.Maximum == nil || *schema.Items.Maximum != 255 {
			t.Errorf("%s %s = %+v (items %+v), want an array of integers from 0 to 255", parameter.In, parameter.Name, schema, schema.Items)
		}
		if schema.Format == "byte" {
			t.Errorf("%s %s is described as base64, which is not what the binder reads", parameter.In, parameter.Name)
		}
	}
}
