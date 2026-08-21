// Package users exposes the user-facing routes as a router the application
// mounts wherever it likes.
//
// Nothing here knows about the prefix, tags or guards it will run under; those
// are the application's decision, applied where the router is included.
package users

import (
	"badele"
)

// UserOut is the response model for a single user. Because a handler returns
// this type, it is the only thing that can ever reach the client: a field that
// is not declared here cannot leak, and the compiler enforces that.
type UserOut struct {
	// Username identifies the user.
	Username string `json:"username" doc:"The user's login name"`
}

// LookupParams binds the username from the path.
type LookupParams struct {
	// Username is read from the path template.
	Username string `path:"username" doc:"The username to look up"`
}

// ListQuery binds the paging parameters from the query string.
type ListQuery struct {
	// Limit caps how many users are returned.
	Limit int `query:"limit" doc:"Maximum number of users to return" default:"20"`
	// Cursor continues a previous page.
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous page"`
}

// ListOut is the response model for a page of users.
type ListOut struct {
	// Users is the page of results.
	Users []UserOut `json:"users"`
	// NextCursor continues the listing, and is absent on the last page.
	NextCursor string `json:"next_cursor,omitzero" doc:"Pass as cursor to fetch the next page"`
}

// NewRouter returns the users router, tagged so that its operations are
// grouped together in the generated documentation.
func NewRouter() *badele.Router {
	r := badele.NewRouter(badele.WithTags("users"))

	r.Get("/users/", list, badele.Summary("List users"))
	r.Get("/users/me", currentUser, badele.Summary("Read the authenticated user"))
	r.Get("/users/{username}", read, badele.Summary("Read a user by name"))

	return r
}

// list returns a page of users.
func list(ctx *badele.Context, in ListQuery) (ListOut, error) {
	users := make([]UserOut, 0, 2)
	for _, name := range []string{"rick", "morty"} {
		if len(users) == in.Limit {
			break
		}
		users = append(users, UserOut{Username: name})
	}
	return ListOut{Users: users}, nil
}

// currentUser reports the authenticated user. The route takes nothing from the
// request, which badele.Empty states explicitly.
func currentUser(ctx *badele.Context, _ badele.Empty) (UserOut, error) {
	return UserOut{Username: "fakecurrentuser"}, nil
}

// read returns one user by name.
func read(ctx *badele.Context, in LookupParams) (UserOut, error) {
	return UserOut{Username: in.Username}, nil
}
