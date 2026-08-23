package handlers

import (
	"muzak.dev/framework"
	"muzak.dev/framework/example/schemas"
)

// FileSize reports the size of a file read into memory. Binding it as []byte is
// what makes that the whole handler.
func FileSize(ctx *muzak.Context, in schemas.FileBytesIn) (schemas.FileOut, error) {
	return schemas.FileOut{FileSize: len(in.File)}, nil
}

// UploadFile reports what a client sent with one file. Nothing is read here:
// muzak.File carries the metadata and leaves the content where the parser put
// it, so a handler that only needs the name never touches the bytes.
func UploadFile(ctx *muzak.Context, in schemas.UploadFileIn) (schemas.UploadFileOut, error) {
	return schemas.UploadFileOut{
		Filename:    in.File.Filename,
		ContentType: in.File.ContentType,
		Size:        in.File.Size,
		Note:        in.Note,
	}, nil
}

// UploadFiles reports every file sent under one name.
func UploadFiles(ctx *muzak.Context, in schemas.MultiUploadIn) (schemas.MultiUploadOut, error) {
	out := schemas.MultiUploadOut{Filenames: make([]string, len(in.Files))}
	for i, file := range in.Files {
		out.Filenames[i] = file.Filename
		out.TotalSize += file.Size
	}
	return out, nil
}

// UploadForm serves the page that posts to UploadFiles. Returning muzak.HTML
// is what bypasses JSON encoding; the document is written as it stands.
//
// The token is in the form's action because this application guards every
// route with one. A real form would carry it in a cookie or a header instead.
func UploadForm(ctx *muzak.Context, _ muzak.Empty) (muzak.HTML, error) {
	return muzak.HTML(`<body>
<form action="/uploadfiles/?token=jessica" enctype="multipart/form-data" method="post">
<input name="files" type="file" multiple>
<input type="submit">
</form>
</body>`), nil
}
