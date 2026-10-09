package handlers

import (
	"crypto/subtle"

	"muzak.dev/framework"
	"muzak.dev/framework/example/schemas"
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
func Login(ctx *muzak.Context, in schemas.LoginIn) (schemas.LoginOut, error) {
	expected, known := accounts[in.Username]

	// The comparison runs even for an unknown account, and the same answer is
	// given either way. Returning "no such user" would turn this endpoint into
	// a way to enumerate accounts, and returning early would let its timing do
	// the same thing more quietly.
	matches := subtle.ConstantTimeCompare([]byte(expected), []byte(in.Password)) == 1
	if !known || !matches {
		return schemas.LoginOut{}, muzak.Unauthorized("the username or password is incorrect")
	}

	session := ctx.Session()
	// Regenerating comes first, before anything that says who the user is.
	// The browser may arrive carrying a session someone else chose for it,
	// planted through a sibling subdomain or a scripting bug; signing in on top of
	// that session would hand whoever planted it a signed-in one. A new
	// session makes whatever they hold worthless at the moment it would have
	// become valuable.
	if err := session.Regenerate(); err != nil {
		return schemas.LoginOut{}, err
	}
	// Only the username goes in: the session travels with every request, and
	// the cookie that carries it is encrypted, HttpOnly, Secure and SameSite,
	// all of which the framework sets without being asked.
	if err := session.Set("user", in.Username); err != nil {
		return schemas.LoginOut{}, err
	}

	return schemas.LoginOut{
		Username: in.Username,
		Next:     in.Next,
	}, nil
}

// Logout ends the session: the cookie is removed from the browser, and a
// value set afterwards would begin a new session rather than continue this
// one.
func Logout(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
	ctx.Session().Destroy()
	return muzak.Empty{}, nil
}

// LoginForm serves the page that posts to Login.
//
// It is a plain form with no enctype, so the browser posts it as
// application/x-www-form-urlencoded. The route accepts that without being told
// to, because it binds form values and no files. A form is exactly what
// another site can submit in the user's name, which is why the application
// refuses a state-changing request from another origin; see
// AppOptions.CrossOriginProtection in cmd/main.go.
func LoginForm(ctx *muzak.Context, _ muzak.Empty) (muzak.HTML, error) {
	return muzak.HTML(`<body>
<form action="/login/?token=jessica" method="post">
<label>Username <input name="username" autocomplete="username"></label>
<label>Password <input name="password" type="password" autocomplete="current-password"></label>
<input type="hidden" name="next" value="/feed">
<input type="submit" value="Sign in">
</form>
</body>`), nil
}
