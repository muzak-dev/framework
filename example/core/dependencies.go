package core

import (
	"net/http"

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

// GetQueryToken is a guard dependency applied to the whole application.
//
// It rejects any request that does not carry a token query parameter, standing
// in for whatever real check a service would perform. A guard produces no
// value; it either lets the request through or returns the error that becomes
// the response.
func GetQueryToken(ctx *muzak.Context) error {
	if ctx.Query("token") == "" {
		return muzak.NewHTTPError(http.StatusBadRequest, "token is required")
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
		return CurrentUser{}, muzak.NewHTTPError(http.StatusUnauthorized, "unauthorized")
	}
	// A real service would look the token up. This example accepts any bearer
	// token and reports a fixed user, which is enough to show how a resolved
	// value reaches a handler.
	_ = token
	return CurrentUser{Username: "fakecurrentuser"}, nil
}

// SessionOrToken is the caller of a WebSocket route, resolved from either a
// session cookie or a query parameter.
//
// A browser cannot set headers on a WebSocket handshake, so the two places a
// credential can arrive are a cookie the browser attaches itself and a query
// parameter the page puts in the URL. Both are covered here, which is what
// [GetSessionOrToken] exists to show.
type SessionOrToken struct {
	// Value is the credential that was presented.
	Value string
	// FromCookie reports which of the two it came from.
	FromCookie bool
}

// GetSessionOrToken resolves the caller of a WebSocket route.
//
// It runs during the handshake, before a single byte is upgraded, so a caller
// with no credential receives an ordinary JSON error rather than a connection
// that closes a moment later.
func GetSessionOrToken(ctx *muzak.Context) (SessionOrToken, error) {
	if cookie, err := ctx.Cookie("session"); err == nil && cookie.Value != "" {
		return SessionOrToken{Value: cookie.Value, FromCookie: true}, nil
	}
	if token := ctx.Query("token"); token != "" {
		return SessionOrToken{Value: token}, nil
	}
	return SessionOrToken{}, muzak.NewHTTPError(http.StatusUnauthorized,
		"a session cookie or a token query parameter is required")
}
