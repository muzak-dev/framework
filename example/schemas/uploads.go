package schemas

import (
	"muzak.dev/framework"
)

// FileBytesIn binds one upload straight into memory, which suits a file small
// enough that holding all of it at once is not a decision worth thinking
// about.
type FileBytesIn struct {
	File []byte `file:"file" doc:"A file read as bytes"`
}

// FileOut reports what was received without echoing any of it back.
type FileOut struct {
	FileSize int `json:"file_size"`
}

// UploadFileIn binds one upload alongside a form value, which is what an HTML
// form with a file input and a text input sends.
type UploadFileIn struct {
	File muzak.File `file:"file" doc:"A file read as an upload"`
	Note string     `form:"note" doc:"An optional note filed with the upload" required:"false"`
}

// UploadFileOut reports the metadata the client sent with the file. None of it
// is trusted: the filename is echoed rather than used as a path.
type UploadFileOut struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Note        string `json:"note"`
}

// MultiUploadIn binds every file sent under one name.
type MultiUploadIn struct {
	Files []muzak.File `file:"files" doc:"One or more files"`
}

// MultiUploadOut lists what arrived.
type MultiUploadOut struct {
	Filenames []string `json:"filenames"`
	TotalSize int64    `json:"total_size"`
}
