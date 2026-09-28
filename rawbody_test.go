package muzak

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The body a signature covers, kept as it arrived. The point of every test here
// is that the bytes are the client's, not a re-encoding of them.

// signedBody is JSON whose whitespace and member order no encoder would
// reproduce. If any test below sees a normalised version of it, the feature is
// not doing its job.
const signedBody = `{  "name" : "hertus" ,
   "count":3 }`

type captureIn struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type captureOut struct {
	Name string `json:"name"`
	Raw  string `json:"raw"`
	OK   bool   `json:"ok"`
}

func captureHandler(ctx *Context, in captureIn) (captureOut, error) {
	raw, ok := ctx.RawBody()
	return captureOut{Name: in.Name, Raw: string(raw), OK: ok}, nil
}

func TestCaptureBodyGivesTheHandlerTheBytesAndTheBoundInput(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", captureHandler, CaptureBody())
	mustBuild(t, app)

	res := do(t, app, "POST", "/x", signedBody)
	assertStatus(t, res, http.StatusOK)

	out := decodeCapture(t, res)
	if !out.OK {
		t.Fatal("the route declared CaptureBody and RawBody reported otherwise")
	}
	// The decoded value and the original bytes, from one request.
	if out.Name != "hertus" {
		t.Errorf("name = %q; binding did not run after capture", out.Name)
	}
	// Byte for byte, whitespace and all. Contains() would pass on a normalised
	// body that merely happened to include the same substring.
	if out.Raw != signedBody {
		t.Errorf("raw  = %q\nwant = %q", out.Raw, signedBody)
	}
}

// decodeCapture reads the handler's answer, so assertions compare the raw body
// against the original rather than against its JSON escaping.
func decodeCapture(t *testing.T, rec *httptest.ResponseRecorder) captureOut {
	t.Helper()
	var out captureOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not the expected shape: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

// TestRawBodyReportsWhenTheRouteDidNotCapture is why RawBody returns two
// values. A helper shared with a route that does not capture must not verify a
// signature against an empty body and pass.
func TestRawBodyReportsWhenTheRouteDidNotCapture(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", captureHandler)
	mustBuild(t, app)

	res := do(t, app, "POST", "/x", signedBody)
	assertStatus(t, res, http.StatusOK)

	if out := decodeCapture(t, res); out.OK {
		t.Fatal("a route without CaptureBody reported a captured body")
	}
}

// TestCaptureBodyReachesAGuard is the ordering that matters: a signature is
// checked before anything decodes the request.
func TestCaptureBodyReachesAGuard(t *testing.T) {
	t.Parallel()

	secret := []byte("shhh")
	sign := func(body string) string {
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(body))
		return hex.EncodeToString(mac.Sum(nil))
	}

	verify := func(ctx *Context) error {
		raw, ok := ctx.RawBody()
		if !ok {
			return NewHTTPError(http.StatusInternalServerError, "this route does not capture its body")
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(raw)
		if !hmac.Equal([]byte(ctx.Header("X-Sig")), []byte(hex.EncodeToString(mac.Sum(nil)))) {
			return Unauthorized("the signature does not match")
		}
		return nil
	}

	app := New(quietOptions())
	app.Post("/x", captureHandler, CaptureBody(), WithDependencies(verify))
	mustBuild(t, app)

	signed := func(sig string) *http.Request {
		req := httptest.NewRequest("POST", "/x", strings.NewReader(signedBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Sig", sig)
		return req
	}

	assertStatus(t, doRequest(t, app, signed(sign(signedBody))), http.StatusOK)

	// The same body signed as a re-encoding of itself fails, which is the whole
	// reason the original has to be kept.
	reencoded := `{"name":"hertus","count":3}`
	assertStatus(t, doRequest(t, app, signed(sign(reencoded))), http.StatusUnauthorized)
}

// TestCaptureBodyWorksWithAnEmptyInput covers the route that binds nothing and
// would otherwise have its body drained.
func TestCaptureBodyWorksWithAnEmptyInput(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", func(ctx *Context, _ Empty) (captureOut, error) {
		raw, ok := ctx.RawBody()
		return captureOut{Raw: string(raw), OK: ok}, nil
	}, CaptureBody())
	mustBuild(t, app)

	res := do(t, app, "POST", "/x", signedBody)
	assertStatus(t, res, http.StatusOK)

	out := decodeCapture(t, res)
	if !out.OK {
		t.Fatal("an Empty input drained the body despite CaptureBody")
	}
	if out.Raw != signedBody {
		t.Errorf("raw  = %q\nwant = %q", out.Raw, signedBody)
	}
}

// TestCaptureBodyRefusesABodyOverTheRouteLimit: refused with the size error,
// not truncated into a signature failure that reads like an attack.
func TestCaptureBodyRefusesABodyOverTheRouteLimit(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/x", captureHandler, CaptureBody(), MaxBodySize(32))
	mustBuild(t, app)

	res := do(t, app, "POST", "/x", `{"name":"`+strings.Repeat("x", 64)+`"}`)
	assertStatus(t, res, http.StatusRequestEntityTooLarge)
}

// TestCaptureBodyAtExactlyTheLimitIsAccepted guards the off-by-one in the
// limit+1 read.
func TestCaptureBodyAtExactlyTheLimitIsAccepted(t *testing.T) {
	t.Parallel()
	body := `{"name":"abc","count":1}`

	app := New(quietOptions())
	app.Post("/x", captureHandler, CaptureBody(), MaxBodySize(int64(len(body))))
	mustBuild(t, app)

	res := do(t, app, "POST", "/x", body)
	assertStatus(t, res, http.StatusOK)
	if out := decodeCapture(t, res); out.Raw != body {
		t.Errorf("a body exactly at the limit was not captured whole: %q", out.Raw)
	}
}

// TestCaptureBodyOnARouterAppliesToEveryRoute: it is a shared option, so a
// router of webhook receivers declares it once.
func TestCaptureBodyOnARouterAppliesToEveryRoute(t *testing.T) {
	t.Parallel()
	r := NewRouter(CaptureBody())
	r.Post("/a", captureHandler)
	r.Post("/b", captureHandler)

	app := New(quietOptions())
	app.Include(r)
	mustBuild(t, app)

	for _, path := range []string{"/a", "/b"} {
		res := do(t, app, "POST", path, signedBody)
		assertStatus(t, res, http.StatusOK)
		if out := decodeCapture(t, res); !out.OK || out.Raw != signedBody {
			t.Errorf("%s did not inherit CaptureBody", path)
		}
	}
}

// TestCaptureBodyWithNoBodyIsStillCaptured separates "no body was sent" from
// "this route does not capture", which are different answers to a handler
// deciding whether it may verify a signature.
func TestCaptureBodyWithNoBodyIsStillCaptured(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (captureOut, error) {
		raw, ok := ctx.RawBody()
		return captureOut{Raw: string(raw), OK: ok}, nil
	}, CaptureBody())
	mustBuild(t, app)

	res := do(t, app, "GET", "/x", "")
	assertStatus(t, res, http.StatusOK)

	out := decodeCapture(t, res)
	if !out.OK {
		t.Error("a bodyless request reported no capture; that is a different answer")
	}
	if out.Raw != "" {
		t.Errorf("raw = %q, want empty", out.Raw)
	}
}

// TestCaptureBodyDoesNotLeakBetweenRequests is the pooled-Context hazard: the
// bytes must be released with the Context, not carried into the next request.
func TestCaptureBodyDoesNotLeakBetweenRequests(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/capture", captureHandler, CaptureBody())
	app.Post("/plain", captureHandler)
	mustBuild(t, app)

	assertStatus(t, do(t, app, "POST", "/capture", signedBody), http.StatusOK)

	// The same Context comes back out of the pool for this one.
	res := do(t, app, "POST", "/plain", `{"name":"other","count":1}`)
	assertStatus(t, res, http.StatusOK)
	if out := decodeCapture(t, res); out.OK || out.Raw != "" {
		t.Fatalf("a pooled Context carried the previous request's body: %+v", out)
	}
}

// The documented way to remove the limit is a negative AppOptions.MaxBodySize,
// and a limit of math.MaxInt64 is the other way an operator says the same. Both
// used to fail closed: MaxBytesReader clamps a negative limit to zero and
// refuses every body, and limit+1 overflowed the capture's read.
func TestAnUnlimitedBodyIsReadWhole(t *testing.T) {
	t.Parallel()
	for name, limit := range map[string]int64{"negative": -1, "maximum": math.MaxInt64} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.MaxBodySize = limit
			app := New(opts)
			app.Post("/plain", captureHandler)
			app.Post("/captured", captureHandler, CaptureBody())
			mustBuild(t, app)

			body := `{"name":"` + strings.Repeat("x", 2<<20) + `"}`
			for _, path := range []string{"/plain", "/captured"} {
				res := do(t, app, "POST", path, body)
				assertStatus(t, res, http.StatusOK)
				if out := decodeCapture(t, res); len(out.Name) != 2<<20 {
					t.Errorf("%s: bound %d bytes of the name, want %d", path, len(out.Name), 2<<20)
				}
			}
		})
	}
}
