package handlers

import (
	"badele"
	"badele-example/schemas"
)

// ListUsers returns a page of users, honouring the requested limit.
func ListUsers(ctx *badele.Context, in schemas.UserListQuery) (schemas.UserListOut, error) {
	known := []string{"rick", "morty"}

	users := make([]schemas.UserOut, 0, len(known))
	for _, name := range known {
		if len(users) == in.Limit {
			break
		}
		users = append(users, schemas.UserOut{Username: name})
	}
	return schemas.UserListOut{Users: users}, nil
}

// CurrentUser reports the authenticated user. The route reads nothing from the
// request, which badele.Empty states explicitly.
func CurrentUser(ctx *badele.Context, _ badele.Empty) (schemas.UserOut, error) {
	return schemas.UserOut{Username: "fakecurrentuser"}, nil
}

// ReadUser returns one user by name.
func ReadUser(ctx *badele.Context, in schemas.UserLookupParams) (schemas.UserOut, error) {
	return schemas.UserOut{Username: in.Username}, nil
}
