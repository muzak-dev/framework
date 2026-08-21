// Package core holds the pieces every other package in the service depends on:
// its configuration, its guard and value dependencies, and the resources whose
// lifetime the application manages.
//
// Nothing here imports the handlers, the schemas or the routers, which keeps
// the dependency direction one way and makes each of those packages testable on
// its own.
package core

import (
	"badele"
)

// Settings is the service configuration, read once at start-up from the
// environment and from a .env file when one is present.
//
// It is published to every handler with badele.WithSingleton, so a handler
// reads it with badele.From[core.Settings](ctx) rather than through a package
// level variable.
type Settings struct {
	// AppName titles the API in the generated documentation.
	AppName string `env:"APP_NAME" default:"Awesome API"`
	// AdminEmail is who to contact about the API.
	AdminEmail string `env:"ADMIN_EMAIL" default:"admin@example.com"`
	// ItemsPerUser caps how many items one user may hold.
	ItemsPerUser int `env:"ITEMS_PER_USER" default:"50"`
	// Addr is the address the server listens on.
	Addr string `env:"ADDR" default:":8080"`
	// AdminToken guards the admin subtree. It is marked secret so that a
	// malformed value never appears in a start-up error.
	AdminToken string `env:"ADMIN_TOKEN" default:"coneofsilence" secret:"true"`
}

// LoadSettings reads the configuration, stopping the process if a required
// value is missing.
//
// A service that cannot read its own configuration has nothing useful to do, so
// failing loudly at start-up beats starting in an undefined state. Every
// problem is reported together, so a first run in a new environment lists all
// the missing variables at once.
func LoadSettings() Settings {
	return badele.MustLoadConfig[Settings](badele.EnvFile(".env"))
}
