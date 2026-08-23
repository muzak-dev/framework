package routers

import (
	"muzak.dev/framework"
	"muzak.dev/framework/example/handlers"
)

// Uploads returns the router for the endpoints that accept files.
//
// The two limits are declared once for the whole router, so a route added here
// later cannot forget them: MaxUploadSize bounds what the server will read at
// all, and MaxFileSize bounds any single file inside it.
func Uploads() *muzak.Router {
	r := muzak.NewRouter(
		muzak.WithTags("uploads"),
		muzak.MaxUploadSize(32<<20),
		muzak.MaxFileSize(10<<20),
	)

	r.Get("/upload", handlers.UploadForm,
		muzak.Summary("Serve a form that posts files"))

	r.Post("/files/", handlers.FileSize,
		muzak.Summary("Report the size of a file read as bytes"))

	r.Post("/uploadfile/", handlers.UploadFile,
		muzak.Summary("Report what was sent with one file"))

	r.Post("/uploadfiles/", handlers.UploadFiles,
		muzak.Summary("Report what was sent with several files"))

	return r
}
