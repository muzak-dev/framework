package routers

import (
	"badele"
	"badele-example/handlers"
)

// Uploads returns the router for the endpoints that accept files.
//
// The two limits are declared once for the whole router, so a route added here
// later cannot forget them: MaxUploadSize bounds what the server will read at
// all, and MaxFileSize bounds any single file inside it.
func Uploads() *badele.Router {
	r := badele.NewRouter(
		badele.WithTags("uploads"),
		badele.MaxUploadSize(32<<20),
		badele.MaxFileSize(10<<20),
	)

	r.Get("/upload", handlers.UploadForm,
		badele.Summary("Serve a form that posts files"))

	r.Post("/files/", handlers.FileSize,
		badele.Summary("Report the size of a file read as bytes"))

	r.Post("/uploadfile/", handlers.UploadFile,
		badele.Summary("Report what was sent with one file"))

	r.Post("/uploadfiles/", handlers.UploadFiles,
		badele.Summary("Report what was sent with several files"))

	return r
}
