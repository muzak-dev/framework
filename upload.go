package muzak

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"reflect"
)

// File is one file received in a multipart request.
//
// A field tagged `file:"name"` and typed File is bound from the part the
// client sent under that name, with the metadata copied out of the part
// header and the content left where the parser put it:
//
//	type UploadFileIn struct {
//		File muzak.File `file:"file" doc:"A file read as an upload"`
//	}
//
// The content is reachable through [File.Open] for streaming and through
// [File.Bytes] for the whole thing at once. Both are only valid while the
// handler runs: a file that spilled to a temporary file is removed once the
// handler returns, so anything that must outlive the request has to be copied
// out of it first, with [File.Save] or otherwise.
type File struct {
	// Filename is the name the client reported for the file. It is arbitrary
	// client-supplied text and must never be used as a path, a database key or
	// anything else with meaning on the server without being checked first.
	Filename string
	// ContentType is the media type declared for the part, which is likewise
	// what the client claimed rather than what the bytes contain. It is empty
	// when the part carried no Content-Type header.
	ContentType string
	// Size is the number of bytes the file holds.
	Size int64

	// header is the part this file was bound from, which is what makes the
	// content reachable. It is nil for a File that was never bound, which is
	// what an optional file field holds when the client sent nothing.
	header *multipart.FileHeader
}

// newFile builds the File a handler sees from the part the parser produced.
func newFile(header *multipart.FileHeader) File {
	return File{
		Filename:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		Size:        header.Size,
		header:      header,
	}
}

// ErrNoFile reports an operation on a File that was never bound from a
// request, which is what an optional file field holds when the client sent
// nothing under its name.
var ErrNoFile = errors.New("muzak: no file was uploaded for this field")

// Present reports whether a file was actually uploaded. It is only ever false
// for a field marked `required:"false"`, since a required file that is missing
// fails the request before the handler runs.
func (f File) Present() bool { return f.header != nil }

// Header returns the MIME headers of the part the file arrived in, for the
// occasional client that sends more than a filename and a content type. It
// returns nil when no file was uploaded.
func (f File) Header() textproto.MIMEHeader {
	if f.header == nil {
		return nil
	}
	return f.header.Header
}

// Open returns a reader over the file's content, positioned at the start. The
// caller owns the returned file and must close it. Opening the same File more
// than once is allowed, and each reader has its own position.
//
// It returns [ErrNoFile] when no file was uploaded.
func (f File) Open() (multipart.File, error) {
	if f.header == nil {
		return nil, ErrNoFile
	}
	return f.header.Open()
}

// Bytes reads the whole file into memory. The result is a fresh slice sized to
// the file, so it stays valid after the request ends.
//
// It returns [ErrNoFile] when no file was uploaded. Prefer [File.Open] for
// anything large enough that holding all of it at once matters.
func (f File) Bytes() ([]byte, error) {
	if f.header == nil {
		return nil, ErrNoFile
	}
	return readPart(f.header)
}

// Save copies the file to the given path, creating or truncating it, and
// returns the number of bytes written.
//
// The path is the caller's to choose, and [File.Filename] is not a safe
// source for one: a client may send "../../etc/passwd" or a name that means
// something to the local filesystem. Build the destination from a directory
// the server controls and a name the server generates.
func (f File) Save(path string) (int64, error) {
	src, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.Create(path) //nolint:gosec // the destination is the caller's to choose, and the doc comment says why it must not be built from Filename
	if err != nil {
		return 0, err
	}
	written, err := io.Copy(dst, src)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	return written, err
}

// readPart reads one uploaded part in full. The buffer is allocated to the
// size the parser recorded, so reading a file costs exactly one allocation.
func readPart(header *multipart.FileHeader) ([]byte, error) {
	f, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	content := make([]byte, header.Size)
	if _, err := io.ReadFull(f, content); err != nil {
		return nil, err
	}
	return content, nil
}

// fileKind is the shape of the field a file is bound into, resolved once when
// the route is registered.
type fileKind uint8

const (
	// fileOne binds a single [File].
	fileOne fileKind = iota
	// fileMany binds every part sent under the name as a []File.
	fileMany
	// fileBytes reads a single file into a []byte.
	fileBytes
	// fileBytesMany reads every part sent under the name into a [][]byte.
	fileBytesMany
)

// fileBinder binds one struct field from the uploaded parts sent under one
// name.
type fileBinder struct {
	index    []int
	name     string
	doc      string
	required bool
	kind     fileKind
}

var (
	fileType      = reflect.TypeFor[File]()
	fileSliceType = reflect.TypeFor[[]File]()
	bytesType     = reflect.TypeFor[[]byte]()
	bytesManyType = reflect.TypeFor[[][]byte]()
)

// newFileBinder compiles the binder for one field tagged `file:"name"`,
// rejecting at registration time any field type a multipart part cannot be
// read into.
func newFileBinder(f reflect.StructField, index []int, name string) (fileBinder, error) {
	if name == "" {
		return fileBinder{}, fmt.Errorf("field %s declares an empty file parameter name", f.Name)
	}
	var kind fileKind
	switch f.Type {
	case fileType:
		kind = fileOne
	case fileSliceType:
		kind = fileMany
	case bytesType:
		kind = fileBytes
	case bytesManyType:
		kind = fileBytesMany
	default:
		return fileBinder{}, fmt.Errorf("field %s (file %q) has type %s; a file field must be muzak.File, []muzak.File, []byte or [][]byte",
			f.Name, name, f.Type)
	}

	b := fileBinder{index: index, name: name, doc: f.Tag.Get(tagDoc), kind: kind}
	// An uploaded file is the point of the request that carries it, so it is
	// required unless the field says otherwise. That matches how the JSON body
	// is treated and differs from a query parameter, which is optional by
	// default because filters and cursors usually are.
	if explicit, declared := f.Tag.Lookup(tagRequired); declared {
		b.required = explicit == "true"
	} else {
		b.required = true
	}
	return b, nil
}

// multi reports whether the binder accepts more than one part under its name,
// which is what tells the generated document to describe an array.
func (b *fileBinder) multi() bool {
	return b.kind == fileMany || b.kind == fileBytesMany
}

// assign writes the uploaded parts into the field, reading their content for
// the byte-slice kinds and leaving it where it is for the File kinds.
func (b *fileBinder) assign(dst reflect.Value, headers []*multipart.FileHeader) error {
	switch b.kind {
	case fileOne:
		dst.Set(reflect.ValueOf(newFile(headers[0])))
	case fileMany:
		files := make([]File, len(headers))
		for i, header := range headers {
			files[i] = newFile(header)
		}
		dst.Set(reflect.ValueOf(files))
	case fileBytes:
		content, err := readPart(headers[0])
		if err != nil {
			return err
		}
		dst.SetBytes(content)
	default:
		contents := make([][]byte, len(headers))
		for i, header := range headers {
			content, err := readPart(header)
			if err != nil {
				return err
			}
			contents[i] = content
		}
		dst.Set(reflect.ValueOf(contents))
	}
	return nil
}

// maxMultipartMemory bounds how much of a multipart body is held in memory
// while it is parsed. Parts beyond it spill to temporary files, which are
// removed once the handler returns. It is deliberately well below the default
// upload limit so that a handful of concurrent large uploads cannot be turned
// into memory pressure.
const maxMultipartMemory int64 = 10 << 20

// bindMultipart reads a form-encoded body and binds the form values and
// uploaded files it carries.
func (p *bindPlan) bindMultipart(c *Context, dst reflect.Value, route *Route, verr *ValidationError) error {
	mediaType, err := requestMediaType(c.r)
	if err != nil {
		return err
	}

	switch {
	case mediaType == "multipart/form-data":
		if err := parseMultipart(c, route); err != nil {
			return err
		}
	case mediaType == "application/x-www-form-urlencoded" && len(p.files) == 0:
		// A route that binds only form values is reachable from a plain HTML
		// form, which posts urlencoded unless it is told otherwise. A route
		// that expects a file is not: urlencoded cannot carry one.
		if err := parseURLEncodedForm(c, route); err != nil {
			return err
		}
	default:
		return p.unsupportedMediaType(mediaType)
	}

	bindParams(p.form, c, dst, nil, verr)

	for i := range p.files {
		b := &p.files[i]
		headers := uploadedParts(c.r, b.name)
		if len(headers) == 0 {
			if b.required {
				verr.add(srcFile.String(), b.name, "is required")
			}
			continue
		}
		if err := checkFileSizes(b, headers, route); err != nil {
			return err
		}
		if err := b.assign(fieldByIndex(dst, b.index), headers); err != nil {
			// coverage: the parser has already read every part by this point,
			// so reading one back only fails if the filesystem holding a
			// spilled part fails under us. TestReadingAPartReportsWhatWentWrong
			// covers the reading itself against a part that cannot be read.
			return NewHTTPErrorf(http.StatusBadRequest, "the uploaded file %q could not be read", b.name).Wrap(err)
		}
	}
	return nil
}

// uploadedParts returns the parts sent under a name, or nothing at all when
// the body carried no files.
func uploadedParts(r *http.Request, name string) []*multipart.FileHeader {
	if r.MultipartForm == nil {
		// coverage: a plan with a file binder only ever reaches this after a
		// multipart body has been parsed, which leaves MultipartForm set. The
		// guard keeps a future encoding that carries no files from turning
		// into a nil dereference here.
		return nil
	}
	return r.MultipartForm.File[name]
}

// checkFileSizes enforces the per-file limit. The parts have already been
// read by this point, so the limit bounds what a handler is handed rather than
// what the server accepts; [MaxUploadSize] is what bounds the latter.
func checkFileSizes(b *fileBinder, headers []*multipart.FileHeader, route *Route) error {
	if route.maxFileSize <= 0 {
		return nil
	}
	for _, header := range headers {
		if header.Size > route.maxFileSize {
			// The field name is named rather than the client's filename,
			// which is text the client chose and this message is sent back.
			return NewHTTPErrorf(http.StatusRequestEntityTooLarge,
				"a file uploaded as %q exceeds the %d byte limit for a single file on this route", b.name, route.maxFileSize)
		}
	}
	return nil
}

// parseMultipart reads and parses a multipart body, bounding it by the route's
// upload limit.
func parseMultipart(c *Context, route *Route) error {
	memory := maxMultipartMemory
	if route.maxUploadSize > 0 {
		c.r.Body = http.MaxBytesReader(c.w, c.r.Body, route.maxUploadSize)
		memory = min(memory, route.maxUploadSize)
	}
	if err := c.r.ParseMultipartForm(memory); err != nil {
		return uploadReadError(err, route.maxUploadSize, "upload")
	}
	return nil
}

// parseURLEncodedForm reads a urlencoded body, bounding it by the same limit a
// multipart body would be bound by so that one route has one limit.
func parseURLEncodedForm(c *Context, route *Route) error {
	if route.maxUploadSize > 0 {
		c.r.Body = http.MaxBytesReader(c.w, c.r.Body, route.maxUploadSize)
	}
	if err := c.r.ParseForm(); err != nil {
		return uploadReadError(err, route.maxUploadSize, "form")
	}
	return nil
}

// uploadReadError turns a parse failure into the response it deserves, which
// is 413 when the body outgrew its limit and 400 when it was malformed.
func uploadReadError(err error, limit int64, what string) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return NewHTTPErrorf(http.StatusRequestEntityTooLarge,
			"the %s exceeds the %d byte limit for this route", what, limit).Wrap(err)
	}
	return NewHTTPErrorf(http.StatusBadRequest, "the %s could not be read", what).Wrap(err)
}

// unsupportedMediaType reports that a form route was sent something it cannot
// decode, naming what it does accept.
func (p *bindPlan) unsupportedMediaType(mediaType string) error {
	accepted := "multipart/form-data"
	if len(p.files) == 0 {
		accepted += " or application/x-www-form-urlencoded"
	}
	if mediaType == "" {
		return NewHTTPErrorf(http.StatusUnsupportedMediaType,
			"this route accepts %s, and the request declared no Content-Type", accepted)
	}
	return NewHTTPErrorf(http.StatusUnsupportedMediaType,
		"unsupported media type %q; this route accepts %s", mediaType, accepted)
}

// releaseUpload removes the temporary files a multipart body spilled to disk.
//
// net/http does the same once the response is finished, but only for requests
// it served itself. Doing it here means a request driven through the handler
// directly, as a test does, leaves nothing behind either.
func releaseUpload(r *http.Request) {
	if r.MultipartForm != nil {
		_ = r.MultipartForm.RemoveAll()
	}
}
