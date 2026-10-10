package core

import (
	"muzak.dev/framework"
)

// CurrentUser is the authenticated caller.
//
// It is produced once per request by [GetCurrentUser] and read inside a handler
// with muzak.From[core.CurrentUser](ctx), where the type is checked by the
// compiler and no cast is written anywhere.
type CurrentUser struct {
	// Username identifies the caller.
	Username string
}

// GetQueryToken is a guard dependency applied where every router is included.
//
// It rejects any request that does not carry a token query parameter, standing
// in for whatever real check a service would perform. A guard produces no
// value; it either lets the request through or returns the error that becomes
// the response.
func GetQueryToken(ctx *muzak.Context) error {
	if ctx.Query("token") == "" {
		return muzak.BadRequest("token is required")
	}
	return nil
}

// GetTokenHeader returns the guard applied where the admin router is included.
//
// The comparison is constant-time, so the response timing does not reveal how
// much of the supplied header was correct.
func GetTokenHeader(settings Settings) muzak.Guard {
	return muzak.RequireHeaderToken("X-Token", settings.AdminToken)
}

// GetCurrentUser is a value dependency that resolves the caller from the
// Authorization header.
//
// A route declares it with muzak.Needs, and the resolved value lives on the
// request context until that context is released, so two concurrent requests
// never see each other's user.
func GetCurrentUser(ctx *muzak.Context) (CurrentUser, error) {
	token, present := muzak.BearerToken(ctx)
	if !present {
		return CurrentUser{}, muzak.Unauthorized("")
	}
	// A real service would look the token up. This example accepts any bearer
	// token and reports a fixed user, which is enough to show how a resolved
	// value reaches a handler.
	_ = token
	return CurrentUser{Username: "fakecurrentuser"}, nil
}

// Caller is who is on the other end of a WebSocket route.
type Caller struct {
	// Username identifies the caller.
	Username string
	// FromToken reports that the caller presented a token query parameter
	// rather than a signed-in session.
	FromToken bool
}

// GetCaller resolves the caller of a WebSocket route from the session a
// sign-in stored, or from a token query parameter.
//
// A browser cannot set headers on a WebSocket handshake, so the two places a
// credential can arrive are the session cookie the browser attaches itself
// and a query parameter the page puts in the URL. Both are covered here, which
// is what [GetCaller] exists to show. It runs during the handshake, before a
// single byte is upgraded, so a caller with no credential receives an
// ordinary JSON error rather than a connection that closes a moment later.
func GetCaller(ctx *muzak.Context) (Caller, error) {
	if user, ok := muzak.SessionGet[string](ctx.Session(), "user"); ok {
		return Caller{Username: user}, nil
	}
	if token := ctx.Query("token"); token != "" {
		// A real service would verify the token. This example accepts any
		// token and reports a fixed caller, as GetCurrentUser does.
		return Caller{Username: "fakecurrentuser", FromToken: true}, nil
	}
	return Caller{}, muzak.Unauthorized("sign in, or pass a token query parameter")
}
