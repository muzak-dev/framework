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

// CreateUser registers a user.
//
// By the time this runs the model has been validated, so the handler can take
// the input at face value: the email is trimmed and lower-cased, the password
// is long enough, and the role is one of the three it is allowed to be.
func CreateUser(ctx *badele.Context, in schemas.CreateUserIn) (schemas.UserOut, error) {
	return schemas.UserOut{Username: in.Username, Email: in.Email}, nil
}
