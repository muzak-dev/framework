package muzak_test

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// SignInIn is a sign-in form.
type SignInIn struct {
	Username string `form:"username"`
	Password string `form:"password"`
}

// SignInOut names who signed in.
type SignInOut struct {
	Username string `json:"username"`
}

// signIn checks a password and starts a session for the user.
func signIn(ctx *muzak.Context, in SignInIn) (SignInOut, error) {
	if in.Username != "rick" || subtle.ConstantTimeCompare([]byte(in.Password), []byte("wubbalubbadubdub")) != 1 {
		return SignInOut{}, muzak.Unauthorized("the username or password is incorrect")
	}
	s := ctx.Session()
	// A new session for the signed-in user, so that an identifier anyone
	// planted or saw before this moment is worth nothing now.
	if err := s.Regenerate(); err != nil {
		return SignInOut{}, err
	}
	if err := s.Set("user", in.Username); err != nil {
		return SignInOut{}, err
	}
	return SignInOut{Username: in.Username}, nil
}

// whoAmI answers with the signed-in user.
func whoAmI(ctx *muzak.Context, _ muzak.Empty) (SignInOut, error) {
	user, ok := muzak.SessionGet[string](ctx.Session(), "user")
	if !ok {
		return SignInOut{}, muzak.Unauthorized("sign in first")
	}
	return SignInOut{Username: user}, nil
}

// signOut ends the session.
func signOut(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
	ctx.Session().Destroy()
	return muzak.Empty{}, nil
}

// newSignInApp is an application that signs users in with a session.
func newSignInApp() *muzak.App {
	app := muzak.New(muzak.AppOptions{
		Title:         "Accounts",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
		Sessions: &muzak.SessionOptions{
			// In production, read from the environment or a secret manager.
			Secrets: []string{"an example secret of at least 32 bytes"},
		},
		// Required with sessions: the cookie is sent with requests other
		// pages make, so where a state-changing request came from is checked.
		CrossOriginProtection: &muzak.CrossOriginOptions{},
	})
	app.Post("/sign-in", signIn)
	app.Get("/me", whoAmI)
	app.Post("/sign-out", signOut)
	return app
}

// ExampleContext_Session signs a user in, regenerating the session first,
// reads who is signed in, and signs out.
func ExampleContext_Session() {
	app := newSignInApp()
	if err := app.Build(); err != nil {
		panic(err)
	}
	// Output:
}

// TestSignInFlow drives the example through the test client, whose cookie
// jar plays the browser: the session survives between requests, a forged
// sign-in from another site is refused, and signing out ends it.
func TestSignInFlow(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newSignInApp())
	client.Get("/me").AssertStatus(http.StatusUnauthorized)

	form := testclient.Body("application/x-www-form-urlencoded", strings.NewReader("username=rick&password=wubbalubbadubdub"))
	client.Post("/sign-in", form).AssertStatus(http.StatusOK)
	client.Get("/me").AssertStatus(http.StatusOK).AssertJSON(`{"username":"rick"}`)

	client.Post("/sign-out",
		testclient.Header("Origin", "https://evil.test"),
		testclient.Header("Sec-Fetch-Site", "cross-site"),
	).AssertStatus(http.StatusForbidden).AssertErrorCode(muzak.CodeCrossOriginRequest)
	client.Get("/me").AssertStatus(http.StatusOK)

	client.Post("/sign-out").AssertStatus(http.StatusOK)
	client.Get("/me").AssertStatus(http.StatusUnauthorized)
}
