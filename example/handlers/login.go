package handlers

import (
	"crypto/subtle"
	"net/http"
	"uuid"

	"badele"
	"badele-example/schemas"
)

// accounts stands in for the user store a real service would query. The values
// are plain here only because there is nothing to protect; a real one holds a
// password hash and compares against that.
var accounts = map[string]string{
	"muzak": "correct-horse-battery",
	"rick":  "wubbalubbadubdub",
}

// Login establishes a session from a submitted form.
//
// By the time this runs the form has been read, the username trimmed and
// lower-cased and both lengths checked, so the handler is left with the one
// decision that is actually its own.
func Login(ctx *badele.Context, in schemas.LoginIn) (schemas.LoginOut, error) {
	expected, known := accounts[in.Username]

	// The comparison runs even for an unknown account, and the same answer is
	// given either way. Returning "no such user" would turn this endpoint into
	// a way to enumerate accounts, and returning early would let its timing do
	// the same thing more quietly.
	matches := subtle.ConstantTimeCompare([]byte(expected), []byte(in.Password)) == 1
	if !known || !matches {
		return schemas.LoginOut{}, badele.NewHTTPError(
			http.StatusUnauthorized, "the username or password is incorrect")
	}

	session := uuid.NewV4().String()
	ctx.SetCookie(&http.Cookie{
		Name:  "session_id",
		Value: session,
		Path:  "/",
		// HttpOnly keeps the session out of reach of scripts, and SameSite
		// keeps it off cross-site requests. Secure belongs here too once this
		// is served over TLS, which is why it is named rather than omitted.
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   false,
		MaxAge:   3600,
	})

	return schemas.LoginOut{
		Username:  in.Username,
		SessionID: session,
		Next:      in.Next,
	}, nil
}

// LoginForm serves the page that posts to Login.
//
// It is a plain form with no enctype, so the browser posts it as
// application/x-www-form-urlencoded. The route accepts that without being told
// to, because it binds form values and no files.
func LoginForm(ctx *badele.Context, _ badele.Empty) (badele.HTML, error) {
	return badele.HTML(`<body>
<form action="/login/?token=jessica" method="post">
<label>Username <input name="username" autocomplete="username"></label>
<label>Password <input name="password" type="password" autocomplete="current-password"></label>
<input type="hidden" name="next" value="/feed">
<input type="submit" value="Sign in">
</form>
</body>`), nil
}
