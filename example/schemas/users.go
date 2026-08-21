package schemas

// UserOut is the response model for a single user.
type UserOut struct {
	// Username identifies the user.
	Username string `json:"username" doc:"The user's login name"`
}

// UserLookupParams binds the username from the path.
type UserLookupParams struct {
	// Username is read from the path template.
	Username string `path:"username" doc:"The username to look up"`
}

// UserListQuery binds the paging parameters from the query string.
type UserListQuery struct {
	// Limit caps how many users are returned.
	Limit int `query:"limit" doc:"Maximum number of users to return" default:"20"`
	// Cursor continues a previous page.
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous page"`
}

// UserListOut is the response model for a page of users.
type UserListOut struct {
	// Users is the page of results.
	Users []UserOut `json:"users"`
	// NextCursor continues the listing, and is absent on the last page.
	NextCursor string `json:"next_cursor,omitzero" doc:"Pass as cursor to fetch the next page"`
}
