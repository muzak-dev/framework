package core

import (
	"net/http"

	"badele"
)

// CurrentUser is the authenticated caller.
//
// It is produced once per request by [GetCurrentUser] and read inside a handler
// with badele.From[core.CurrentUser](ctx), where the type is checked by the
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
func GetQueryToken(ctx *badele.Context) error {
	if ctx.Query("token") == "" {
		return badele.NewHTTPError(http.StatusBadRequest, "token is required")
	}
	return nil
}

// GetTokenHeader returns the guard applied where the admin router is included.
//
// The comparison is constant-time, so the response timing does not reveal how
// much of the supplied header was correct.
func GetTokenHeader(settings Settings) badele.Guard {
	return badele.RequireHeaderToken("X-Token", settings.AdminToken)
}

// GetCurrentUser is a value dependency that resolves the caller from the
// Authorization header.
//
// A route declares it with badele.Needs, and the resolved value lives on the
// request context until that context is released, so two concurrent requests
// never see each other's user.
func GetCurrentUser(ctx *badele.Context) (CurrentUser, error) {
	token, present := badele.BearerToken(ctx)
	if !present {
		return CurrentUser{}, badele.NewHTTPError(http.StatusUnauthorized, "unauthorized")
	}
	// A real service would look the token up. This example accepts any bearer
	// token and reports a fixed user, which is enough to show how a resolved
	// value reaches a handler.
	_ = token
	return CurrentUser{Username: "fakecurrentuser"}, nil
}
