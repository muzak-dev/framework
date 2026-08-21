package handlers

import (
	"badele"
	"badele-example/schemas"
)

// AdminAction performs the privileged action.
func AdminAction(ctx *badele.Context, in schemas.AdminActionIn) (schemas.AdminActionOut, error) {
	return schemas.AdminActionOut{
		Name:    in.Name,
		Message: "Admin getting schwifty",
	}, nil
}
