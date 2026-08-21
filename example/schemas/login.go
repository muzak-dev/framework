package schemas

import (
	"badele"
)

// LoginIn is the sign-in form.
//
// The two fields carry no location tag beyond `form`, so this route reads a
// form body rather than JSON and accepts both encodings a browser can produce:
// application/x-www-form-urlencoded, which a plain HTML form posts, and
// multipart/form-data, which one with an enctype does.
type LoginIn struct {
	Username string `form:"username" doc:"The account to sign in to"`
	Password string `form:"password" doc:"The account's password"`

	// Next is where to send the browser afterwards. A form value is required
	// like the body is, so an optional one says so.
	Next string `form:"next" required:"false" doc:"Where to redirect after signing in"`
}

// Validate bounds the credentials before any comparison is attempted.
//
// The rules are deliberately shape-only. A password that is too short is worth
// rejecting outright, but nothing here may hint at whether the account exists;
// that answer belongs to the handler, which gives the same one either way.
func (in *LoginIn) Validate(v *badele.Validation) {
	v.String(&in.Username).Trim().Lower().MinLen(2).MaxLen(32)
	v.String(&in.Password).MinLen(8).MaxLen(128)
}

// LoginOut reports the established session.
//
// The session identifier is also set as a cookie. It appears here so that a
// non-browser client has somewhere to read it from, and nothing else about the
// account is disclosed.
type LoginOut struct {
	Username  string `json:"username"`
	SessionID string `json:"session_id"`
	Next      string `json:"next,omitzero"`
}
