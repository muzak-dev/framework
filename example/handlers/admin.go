package handlers

import (
	"muzak.dev/framework"
	"muzak.dev/framework/example/schemas"
)

// AdminAction performs the privileged action.
func AdminAction(ctx *muzak.Context, in schemas.AdminActionIn) (schemas.AdminActionOut, error) {
	return schemas.AdminActionOut{
		Name:    in.Name,
		Message: "Admin getting schwifty",
	}, nil
}
