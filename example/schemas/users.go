package schemas

import (
	"muzak.dev/framework"
)

// UserOut is the response model for a single user.
type UserOut struct {
	// Username identifies the user.
	Username string `json:"username" doc:"The user's login name"`
	// Email is where the user is reached.
	Email string `json:"email,omitzero" doc:"Where we reach the user"`
}

// UserLookupParams binds the username from the path.
type UserLookupParams struct {
	// Username is read from the path template.
	Username string `path:"username" doc:"The username to look up"`
}

// Validate constrains the username to what the store can hold.
func (in *UserLookupParams) Validate(v *muzak.Validation) {
	v.String(&in.Username).Trim().Lower().Required().MinLen(2).MaxLen(32).
		Matches(`^[a-z0-9_]+$`).
		Message("may only contain lower case letters, digits and underscores")
}

// UserListQuery binds the paging parameters from the query string.
type UserListQuery struct {
	// Limit caps how many users are returned.
	Limit int `query:"limit" doc:"Maximum number of users to return" default:"20"`
	// Cursor continues a previous page.
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous page"`
}

// Validate keeps a client from asking for an unbounded page.
func (in *UserListQuery) Validate(v *muzak.Validation) {
	v.Number(&in.Limit).Between(1, 100)
	v.String(&in.Cursor).MaxLen(128)
}

// UserListOut is the response model for a page of users.
type UserListOut struct {
	// Users is the page of results.
	Users []UserOut `json:"users"`
	// NextCursor continues the listing, and is absent on the last page.
	NextCursor string `json:"next_cursor,omitzero" doc:"Pass as cursor to fetch the next page"`
}

// CreateUserIn is the JSON body for creating a user.
//
// It is the fullest example in this service: transforms, a custom rule, a
// cross-field comparison and a collection with per-element rules.
type CreateUserIn struct {
	// Username is the name the user signs in with.
	Username string `json:"username" doc:"Lower case letters, digits and underscores"`
	// Email is where the user is reached.
	Email string `json:"email" doc:"Where we reach the user"`
	// Password is the user's secret.
	Password string `json:"password" doc:"At least 12 characters"`
	// Confirm must repeat the password.
	Confirm string `json:"confirm_password"`
	// Age is the user's age in years.
	Age int `json:"age"`
	// Role decides what the user may do.
	Role string `json:"role" doc:"One of admin, editor or viewer"`
	// Tags are free-form labels.
	Tags []string `json:"tags,omitzero"`
	// Website is optional, and checked only when supplied.
	Website *string `json:"website,omitzero"`
}

// Validate declares what a valid signup looks like.
func (in *CreateUserIn) Validate(v *muzak.Validation) {
	v.String(&in.Username).Trim().Lower().Required().MinLen(2).MaxLen(32).
		Matches(`^[a-z0-9_]+$`).
		Message("may only contain lower case letters, digits and underscores").
		NotOneOf("admin", "root", "support").
		Message("is reserved")

	v.String(&in.Email).Trim().Lower().Required().Email()

	v.String(&in.Password).Required().MinLen(12).Must(NotACommonPassword)

	v.String(&in.Confirm).Equal(in.Password).Message("must match the password")

	v.Number(&in.Age).Required().Between(18, 120)

	v.String(&in.Role).Required().OneOf("admin", "editor", "viewer")

	v.Slice(&in.Tags).MaxItems(10).Unique().
		Each(validateTag())

	v.String(&in.Website).URL()

	// A cross-field rule is an ordinary Go expression over the model's fields.
	v.When(in.Role == "admin" && in.Age < 21).
		Reject(&in.Role, "an admin must be at least 21")
}
