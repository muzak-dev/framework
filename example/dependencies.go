package main

import (
	"badele"
)

// CurrentUser is the authenticated caller, resolved once per request by
// [GetCurrentUser] and read by handlers with badele.From[CurrentUser].
type CurrentUser struct {
	// Username identifies the caller.
	Username string
}

// GetQueryToken is a guard dependency applied to the whole application. It
// rejects any request that does not carry a token query parameter, standing in
// for whatever real check an application would perform.
func GetQueryToken(ctx *badele.Context) error {
	if ctx.Query("token") == "" {
		return badele.NewHTTPError(400, "token is required")
	}
	return nil
}

// GetTokenHeader is a guard dependency applied where the admin router is
// included. It checks a shared secret in constant time, so that the response
// timing does not reveal how much of the header was correct.
var GetTokenHeader = badele.RequireHeaderToken("X-Token", "coneofsilence")

// GetCurrentUser is a value dependency that resolves the caller from the
// Authorization header. Handlers that declare it with badele.Needs receive the
// result through badele.From[CurrentUser], with the type checked at compile
// time and no cast written anywhere.
func GetCurrentUser(ctx *badele.Context) (CurrentUser, error) {
	token, present := badele.BearerToken(ctx)
	if !present {
		return CurrentUser{}, badele.NewHTTPError(401, "unauthorized")
	}
	// A real application would look the token up. This example accepts any
	// bearer token and reports a fixed user, which is enough to show how a
	// resolved value reaches a handler.
	_ = token
	return CurrentUser{Username: "fakecurrentuser"}, nil
}
