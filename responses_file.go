package muzak

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FileResponse is a response that serves one file from a filesystem, with
// [http.ServeContent]: a Range request is answered with the ranges asked for,
// and If-Modified-Since, If-Unmodified-Since and If-Range are answered from
// the file's modification time.
//
//	var uploads, _ = os.OpenRoot("/var/lib/app/uploads")
//
//	r.Get("/files/{name...}", func(ctx *muzak.Context, in FileIn) (muzak.FileResponse, error) {
//		return muzak.FileResponse{FS: uploads.FS(), Name: in.Name, Download: true}, nil
//	})
//
// Name is often what a client asked for, so it is checked before anything is
// opened, and a name that fails is answered 404 as a file that is not there
// would be: it must be valid for [fs.ValidPath], so "../" and an absolute path
// are refused; it must not name a dotfile or anything inside a dot directory,
// so ".env" and ".git/config" are refused; it must hold no backslash and no
// control character, NUL included; and on Windows it must not name a drive,
// a stream, a reserved device such as CON or NUL, a segment ending in a dot
// or a space, or an 8.3 short name such as GIT~1, each of which Windows
// resolves to something other than what it spells. A directory, a device, a
// pipe and anything else that is not a regular file is answered 404 as well.
//
// The check covers the name and nothing else. [os.DirFS] follows a symbolic
// link wherever it points, so a link planted inside the directory serves
// whatever file it names; serve a directory on disk through the FS of an
// [os.Root], opened with [os.OpenRoot], which refuses to leave it.
//
// The type is taken from the extension, with the table [Router.Static] uses,
// and never from the content: a file with no extension, or one the table does
// not know, is application/octet-stream, which a browser saves rather than
// renders. The response carries "X-Content-Type-Options: nosniff", and an
// HTML, SVG or other XML file is sent under "Content-Security-Policy:
// sandbox" unless AllowActiveContent is set, because a file a client
// uploaded would otherwise run its script as the application.
//
// The file is always closed. The status follows [Status] and
// [Context.SetStatus]: 200 is answered by [http.ServeContent], 204 and 304
// send no body, and any other status sends the whole file under it without
// reading the conditional or range headers. [AutoETag] does not apply, since
// [http.ServeContent] answers the conditional headers itself.
type FileResponse struct {
	// FS is the filesystem the file is read from. Prefer the FS of an
	// [os.Root] to [os.DirFS], which follows symbolic links out of its
	// directory. A response with no FS fails with a 500.
	FS fs.FS

	// Name is the file's path within FS, slash-separated and relative, as
	// [fs.ValidPath] requires. See the type's documentation for the names that
	// are refused.
	Name string

	// ContentType overrides the media type the extension gives, and is
	// checked as [Bytes.ContentType] is.
	ContentType string

	// Download asks the browser to save the file rather than display it, with
	// "Content-Disposition: attachment". The name offered is Filename, or the
	// last element of Name when Filename is empty.
	Download bool

	// Filename is the name a client is offered to save the file under, which
	// is sent inline when Download is not set. It is cleaned before it is
	// sent: control characters, line breaks, double quotes and the Unicode
	// controls that reorder text, which would let "invoice<U+202E>fdp.exe"
	// display as "invoiceexe.pdf", are removed, a slash or backslash becomes
	// "_", and it is cut to 255 bytes. A name with characters outside ASCII is
	// sent twice, as RFC 6266 asks: as ASCII in filename and in full, encoded
	// as RFC 8187 describes, in filename*.
	Filename string

	// AllowActiveContent serves HTML, SVG, XHTML and other XML files without
	// "Content-Security-Policy: sandbox". See
	// [StaticOptions.AllowActiveContent] for what relaxing it costs: an
	// attacker's markup running as the application.
	AllowActiveContent bool
}

// writeFile writes a [FileResponse].
func (c *Context) writeFile(out FileResponse) error {
	if c.w.written {
		return c.settle(nil)
	}
	if out.FS == nil {
		return fmt.Errorf("muzak: %s %s returned a muzak.FileResponse with no FS; set it to the filesystem the file is read from",
			c.r.Method, c.route.pathOrRequest(c.r))
	}
	contentType := contentTypeFor(out.Name)
	if out.ContentType != "" {
		named, err := c.responseContentType(out.ContentType)
		if err != nil {
			return err
		}
		contentType = named
	}
	if !validFileName(out.Name) {
		return NotFound("")
	}
	file, err := out.FS.Open(out.Name)
	if err != nil {
		return fileNotFound(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fileNotFound(err)
	}
	if !info.Mode().IsRegular() {
		// A directory is never listed, and a device or a pipe is not a file
		// that has an end: /dev/zero behind a link would be read for as long
		// as the client cared to wait.
		return NotFound("")
	}
	// Released once the file is known to be there and before anything is
	// written, so a missing file rolls back and a failed release can still
	// change the answer; see [Context.settle].
	if err := c.settle(nil); err != nil {
		return err
	}
	status := clampStatus(c.status)
	if bodiless(status) {
		c.w.WriteHeader(status)
		return nil
	}

	filename := out.Filename
	if filename == "" && out.Download {
		filename = path.Base(out.Name)
	}
	c.describeBody(contentType, out.Download, filename)
	if !out.AllowActiveContent && isActiveContent(contentType) {
		// Added rather than set, as a static mount adds it: a browser enforces
		// every policy it is sent, so this can only make one stricter.
		c.w.Header().Add("Content-Security-Policy", "sandbox")
	}

	content, seekable := file.(io.ReadSeeker)
	size := info.Size()
	if !seekable {
		// ServeContent needs to seek, so a file from a filesystem whose files
		// cannot, such as a zip archive, is read into memory up to the bound a
		// static mount uses, and streamed without ranges past it.
		head, err := io.ReadAll(io.LimitReader(file, maxBufferedFrontendFile+1))
		if err != nil {
			return fmt.Errorf("muzak: reading the file a muzak.FileResponse named for %s %s failed: %w",
				c.r.Method, c.route.pathOrRequest(c.r), err)
		}
		if len(head) > maxBufferedFrontendFile {
			if size <= int64(len(head)) {
				// The size the filesystem reported is already contradicted,
				// so it is not promised to the client.
				size = 0
			}
			return c.sendBody(status, io.MultiReader(bytes.NewReader(head), file), size)
		}
		content, size = bytes.NewReader(head), int64(len(head))
	}
	if status != http.StatusOK {
		// ServeContent always answers 200, 206 or a condition's status, so a
		// file sent as an error page or a creation is sent whole instead.
		return c.sendBody(status, content, size)
	}
	http.ServeContent(c.w, c.r, out.Name, info.ModTime(), content)
	return nil
}

// fileNotFound turns a failure to open or describe a file into the response
// for a file that is not there. Only a file that does not exist goes unlogged;
// anything else, a permission or a link an [os.Root] refused to follow out of
// it, is kept as the cause, so the client learns nothing about the server's
// filesystem and the operator learns why.
func fileNotFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return NotFound("")
	}
	return NotFound("").Wrap(err)
}

// maxFileNameLength bounds the name a [FileResponse] opens. A path beyond it
// is not one any filesystem keeps, and checking it costs a pass over it.
const maxFileNameLength = 4096

// windowsNames reports whether this platform resolves names the way Windows
// does, where a drive, a device and a segment ending in a dot each open
// something other than what they spell. It is a constant so the checks it
// guards cost nothing where they cannot matter.
const windowsNames = runtime.GOOS == "windows"

// validFileName reports whether a [FileResponse] may open name. Each check is
// a pass over the name, so the whole is linear in it and bounded by
// [maxFileNameLength].
func validFileName(name string) bool {
	if len(name) > maxFileNameLength || name == "." || !fs.ValidPath(name) {
		return false
	}
	for i := 0; i < len(name); i++ {
		// A backslash is refused everywhere, not only where Windows reads it as
		// a separator: an FS may hand the name to one that does, and a name
		// that means one file on Linux and another on Windows is not one a
		// handler meant.
		if name[i] < ' ' || name[i] == 0x7f || name[i] == '\\' {
			return false
		}
	}
	if anySegment(name, func(segment string, _ bool) bool { return strings.HasPrefix(segment, ".") }) {
		return false
	}
	return !windowsNames || !windowsUnsafeName(name)
}

// windowsUnsafeName reports whether any segment of name is one Windows would
// resolve to something other than the file it spells: anything with a colon,
// which names a drive or an alternate data stream; a segment ending in a dot
// or a space, which Windows drops before looking it up, so "secret.txt."
// opens "secret.txt"; a reserved device name, with or without an extension;
// and an 8.3 short name, which opens the long name it abbreviates.
//
// It is written for every platform so that it can be tested on every one, and
// consulted only on Windows; see [windowsNames].
func windowsUnsafeName(name string) bool {
	if strings.ContainsRune(name, ':') {
		return true
	}
	return anySegment(name, func(segment string, _ bool) bool {
		if segment == "" {
			// Only a name fs.ValidPath refuses has one, and that is refused
			// already; there is nothing here to resolve.
			return false
		}
		last := segment[len(segment)-1]
		return last == '.' || last == ' ' || reservedDeviceName(segment) || isShortName(segment)
	})
}

// reservedDeviceName reports whether a path segment names a Windows device,
// such as CON, NUL, COM1 or LPT1, which Windows opens whatever extension
// follows it and whatever spaces trail it. The list is the one
// [path/filepath.IsLocal] uses on Windows, the superscript digits that
// Windows treats as ordinary ones included.
func reservedDeviceName(segment string) bool {
	base, _, _ := strings.Cut(segment, ".")
	base = strings.TrimRight(base, " ")
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) < 4 {
		return false
	}
	prefix := strings.ToUpper(base[:3])
	if prefix != "COM" && prefix != "LPT" {
		return false
	}
	digit, size := utf8.DecodeRuneInString(base[3:])
	if 3+size != len(base) {
		return false
	}
	// Superscript one, two and three, which Windows reads as the digits.
	return ('1' <= digit && digit <= '9') || digit == 0xb9 || digit == 0xb2 || digit == 0xb3
}

// maxDispositionName bounds the filename a Content-Disposition offers, in
// bytes, which is the longest name most filesystems can store.
const maxDispositionName = 255

// contentDisposition builds the Content-Disposition header that offers a body
// as a file, or returns "" when neither a download nor a name was asked for.
//
// The name is cleaned by [cleanFilename], so nothing in it can end the header,
// close the quoted string or reorder how it is displayed. A name that is
// plain ASCII is sent once, in filename; any other is sent as an ASCII
// stand-in in filename and in full in filename*, as RFC 6266 section 4.3
// recommends, so a client that reads only the first still saves something
// sensible.
func contentDisposition(download bool, filename string) string {
	kind := "inline"
	if download {
		kind = "attachment"
	}
	name := cleanFilename(filename)
	if name == "" {
		if download {
			return kind
		}
		return ""
	}
	fallback := asciiFilename(name)
	value := kind + `; filename="` + fallback + `"`
	if fallback != name {
		value += "; filename*=UTF-8''" + encodeExtValue(name)
	}
	return value
}

// cleanFilename removes from a name what must not reach a header or a save
// dialog: bytes that are not UTF-8, control characters (CR, LF and NUL among
// them), format characters such as the bidirectional overrides that make a
// name display other than it is, line and paragraph separators, and double
// quotes. A slash or a backslash becomes "_", since a name offered for saving
// is a single name. Surrounding spaces are trimmed and the result is cut to
// [maxDispositionName] bytes on a character boundary.
//
// It stops reading once the result is full, so a long name costs no more than
// a short one.
func cleanFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if b.Len() >= maxDispositionName {
			break
		}
		switch {
		case r == utf8.RuneError, r == '"', r == 0x2028, r == 0x2029,
			unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		case r == '/' || r == '\\':
			r = '_'
		}
		if b.Len()+utf8.RuneLen(r) > maxDispositionName {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// asciiFilename returns the stand-in sent in a filename parameter: every
// character outside printable ASCII, and every backslash and percent sign,
// becomes "_". RFC 6266 appendix D warns that some clients decode a percent
// sign in this parameter and others do not, which is why the name is then
// sent exactly in filename* instead.
func asciiFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < ' ' || r > '~' || r == '\\' || r == '%' || r == '"' {
			r = '_'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// encodeExtValue percent-encodes a UTF-8 name as the value-chars of an RFC 8187
// ext-value, leaving only attr-char unencoded.
func encodeExtValue(name string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAttrChar(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// isAttrChar reports whether c is an attr-char of RFC 8187 section 3.2.1.
func isAttrChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}
