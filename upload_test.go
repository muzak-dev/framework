package muzak

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// part describes one file a test uploads.
type part struct {
	field       string
	filename    string
	contentType string
	content     string
}

// uploadRequest builds a multipart request carrying the given form values and
// files, in the order they are supplied so that a test can assert on it.
func uploadRequest(t *testing.T, target string, values []string, parts ...part) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for i := 0; i+1 < len(values); i += 2 {
		if err := writer.WriteField(values[i], values[i+1]); err != nil {
			t.Fatalf("WriteField(%q): %v", values[i], err)
		}
	}
	for _, p := range parts {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf("form-data; name=%q; filename=%q", p.field, p.filename))
		if p.contentType != "" {
			header.Set("Content-Type", p.contentType)
		}
		field, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("CreatePart(%q): %v", p.field, err)
		}
		if _, err := io.WriteString(field, p.content); err != nil {
			t.Fatalf("writing part %q: %v", p.field, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

type fileBytesIn struct {
	File []byte `file:"file" doc:"A file read as bytes"`
}

type fileSizeOut struct {
	FileSize int `json:"file_size"`
}

func TestUploadReadsAFileAsBytes(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/files/", func(ctx *Context, in fileBytesIn) (fileSizeOut, error) {
		return fileSizeOut{FileSize: len(in.File)}, nil
	})

	req := uploadRequest(t, "/files/", nil, part{field: "file", filename: "notes.txt", content: "hello upload"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"file_size":12}`)
}

type uploadFileIn struct {
	File File `file:"file" doc:"A file read as an upload"`
}

type uploadFileOut struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Content     string `json:"content"`
}

func TestUploadReadsAFileAsAnUpload(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/uploadfile/", func(ctx *Context, in uploadFileIn) (uploadFileOut, error) {
		content, err := in.File.Bytes()
		if err != nil {
			return uploadFileOut{}, err
		}
		return uploadFileOut{
			Filename:    in.File.Filename,
			ContentType: in.File.ContentType,
			Size:        in.File.Size,
			Content:     string(content),
		}, nil
	})

	req := uploadRequest(t, "/uploadfile/", nil,
		part{field: "file", filename: "report.csv", contentType: "text/csv", content: "a,b,c"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"filename":"report.csv","content_type":"text/csv","size":5,"content":"a,b,c"}`)
}

type multiUploadIn struct {
	Files []File `file:"files"`
}

type multiUploadOut struct {
	Filenames []string `json:"filenames"`
	Sizes     []int64  `json:"sizes"`
}

func TestUploadReadsManyFiles(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/uploadfiles/", func(ctx *Context, in multiUploadIn) (multiUploadOut, error) {
		out := multiUploadOut{
			Filenames: make([]string, len(in.Files)),
			Sizes:     make([]int64, len(in.Files)),
		}
		for i, f := range in.Files {
			out.Filenames[i] = f.Filename
			out.Sizes[i] = f.Size
		}
		return out, nil
	})

	req := uploadRequest(t, "/uploadfiles/", nil,
		part{field: "files", filename: "one.txt", content: "1"},
		part{field: "files", filename: "two.txt", content: "22"},
		part{field: "files", filename: "three.txt", content: "333"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"filenames":["one.txt","two.txt","three.txt"],"sizes":[1,2,3]}`)
}

type multiBytesIn struct {
	Files [][]byte `file:"files"`
}

func TestUploadReadsManyFilesAsBytes(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/files/", func(ctx *Context, in multiBytesIn) (multiUploadOut, error) {
		out := multiUploadOut{Sizes: make([]int64, len(in.Files))}
		for i, content := range in.Files {
			out.Sizes[i] = int64(len(content))
		}
		return out, nil
	})

	req := uploadRequest(t, "/files/", nil,
		part{field: "files", filename: "one.txt", content: "1"},
		part{field: "files", filename: "two.txt", content: "22"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"filenames":[],"sizes":[1,2]}`)
}

type profileIn struct {
	Owner    string `path:"owner"`
	Trace    string `header:"X-Trace" required:"false"`
	Note     string `form:"note"`
	Rank     int    `form:"rank" default:"3"`
	Optional string `form:"optional" required:"false"`
	Avatar   File   `file:"avatar"`
}

type profileOut struct {
	Owner    string `json:"owner"`
	Trace    string `json:"trace"`
	Note     string `json:"note"`
	Rank     int    `json:"rank"`
	Optional string `json:"optional"`
	Avatar   string `json:"avatar"`
}

func TestUploadBindsFormValuesAlongsideFiles(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/profiles/{owner}", func(ctx *Context, in profileIn) (profileOut, error) {
		return profileOut{
			Owner:    in.Owner,
			Trace:    in.Trace,
			Note:     in.Note,
			Rank:     in.Rank,
			Optional: in.Optional,
			Avatar:   in.Avatar.Filename,
		}, nil
	})

	req := uploadRequest(t, "/profiles/muzak", []string{"note", "hello"},
		part{field: "avatar", filename: "face.png", contentType: "image/png", content: "PNG"})
	req.Header.Set("X-Trace", "abc")
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"owner":"muzak","trace":"abc","note":"hello","rank":3,"optional":"","avatar":"face.png"}`)
}

func TestUploadReportsEveryMissingPart(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/profiles/{owner}", func(ctx *Context, in profileIn) (Empty, error) {
		return Empty{}, nil
	})

	req := uploadRequest(t, "/profiles/muzak", nil)
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	got := map[string]string{}
	for _, detail := range decodeError(t, rec).Error.Details {
		got[detail.Location+"."+detail.Field] = detail.Issue
	}
	for _, want := range []string{"form.note", "file.avatar"} {
		if got[want] != "is required" {
			t.Errorf("detail for %s = %q, want \"is required\"\nbody: %s", want, got[want], rec.Body.String())
		}
	}
	if len(got) != 2 {
		t.Errorf("details = %v, want only the required note and avatar", got)
	}
}

type optionalUploadIn struct {
	Avatar File `file:"avatar" required:"false"`
}

func TestUploadLeavesAnAbsentOptionalFileEmpty(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/optional/", func(ctx *Context, in optionalUploadIn) (uploadFileOut, error) {
		if in.Avatar.Present() {
			return uploadFileOut{Filename: in.Avatar.Filename}, nil
		}
		if _, err := in.Avatar.Bytes(); !errors.Is(err, ErrNoFile) {
			t.Errorf("Bytes() on an absent file = %v, want ErrNoFile", err)
		}
		if in.Avatar.Header() != nil {
			t.Error("Header() on an absent file returned headers")
		}
		return uploadFileOut{Filename: "none"}, nil
	})

	built := mustBuild(t, app)
	rec := doRequest(t, built, uploadRequest(t, "/optional/", nil))
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"filename":"none","content_type":"","size":0,"content":""}`)

	rec = doRequest(t, built, uploadRequest(t, "/optional/", nil,
		part{field: "avatar", filename: "face.png", content: "PNG"}))
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"filename":"face.png","content_type":"","size":0,"content":""}`)
}

func TestUploadRejectsAnUnusableMediaType(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/files/", func(ctx *Context, in fileBytesIn) (Empty, error) { return Empty{}, nil })
	built := mustBuild(t, app)

	cases := []struct {
		name        string
		contentType string
		want        string
	}{
		{name: "json", contentType: "application/json", want: `unsupported media type "application/json"`},
		{name: "urlencoded", contentType: "application/x-www-form-urlencoded", want: `unsupported media type "application/x-www-form-urlencoded"`},
		{name: "absent", contentType: "", want: "declared no Content-Type"},
		{name: "malformed", contentType: "multipart/", want: "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/files/", strings.NewReader("{}"))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := doRequest(t, built, req)
			assertStatus(t, rec, http.StatusUnsupportedMediaType)
			if message := decodeError(t, rec).Error.Message; !strings.Contains(message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", message, tc.want)
			}
		})
	}
}

func TestUploadRejectsABodyOverTheUploadLimit(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/files/", func(ctx *Context, in fileBytesIn) (Empty, error) {
		return Empty{}, nil
	}, MaxUploadSize(64))

	req := uploadRequest(t, "/files/", nil,
		part{field: "file", filename: "big.txt", content: strings.Repeat("x", 512)})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
	if message := decodeError(t, rec).Error.Message; !strings.Contains(message, "64 byte limit") {
		t.Errorf("message = %q, want it to name the 64 byte limit", message)
	}
}

func TestUploadRejectsAFileOverTheFileLimit(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/uploadfiles/", func(ctx *Context, in multiUploadIn) (Empty, error) {
		return Empty{}, nil
	}, MaxFileSize(4))

	built := mustBuild(t, app)
	rec := doRequest(t, built, uploadRequest(t, "/uploadfiles/", nil,
		part{field: "files", filename: "small.txt", content: "ok"},
		part{field: "files", filename: "big.txt", content: "far too long"}))
	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
	message := decodeError(t, rec).Error.Message
	if !strings.Contains(message, `uploaded as "files"`) || strings.Contains(message, "big.txt") {
		t.Errorf("message = %q, want it to name the field rather than the client's filename", message)
	}

	rec = doRequest(t, built, uploadRequest(t, "/uploadfiles/", nil,
		part{field: "files", filename: "small.txt", content: "ok"}))
	assertStatus(t, rec, http.StatusOK)
}

func TestUploadLimitsAreInheritedFromTheRouter(t *testing.T) {
	t.Parallel()
	app := New(AppOptions{
		Title:         "Test API",
		Version:       "1.0.0",
		LoggerOptions: LoggerOptions{Format: LogFormatNone},
		MaxUploadSize: 1 << 20,
		MaxFileSize:   1 << 20,
	})
	uploads := NewRouter(MaxFileSize(4), MaxUploadSize(2<<20))
	uploads.Post("/files/", func(ctx *Context, in fileBytesIn) (Empty, error) { return Empty{}, nil })
	app.Include(uploads, WithPrefix("/api"))
	direct := app.Post("/files/", func(ctx *Context, in fileBytesIn) (Empty, error) { return Empty{}, nil })
	built := mustBuild(t, app)

	route := uploads.Routes()[0]
	if route.maxUploadSize != 2<<20 || route.maxFileSize != 4 {
		t.Fatalf("limits = upload %d, file %d; want both from the router that declared them",
			route.maxUploadSize, route.maxFileSize)
	}
	// A route the router did not cover keeps what the application set.
	if direct.maxUploadSize != 1<<20 || direct.maxFileSize != 1<<20 {
		t.Fatalf("uncovered route limits = upload %d, file %d; want the application defaults",
			direct.maxUploadSize, direct.maxFileSize)
	}
	rec := doRequest(t, built, uploadRequest(t, "/api/files/", nil,
		part{field: "file", filename: "big.txt", content: "far too long"}))
	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
}

type formOnlyIn struct {
	Username string `form:"username"`
	Password string `form:"password"`
	Remember bool   `form:"remember" default:"false"`
}

type formOnlyOut struct {
	Username string `json:"username"`
	Remember bool   `json:"remember"`
}

func TestFormRouteAcceptsBothEncodings(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/login/", func(ctx *Context, in formOnlyIn) (formOnlyOut, error) {
		return formOnlyOut{Username: in.Username, Remember: in.Remember}, nil
	})
	built := mustBuild(t, app)

	values := url.Values{"username": {"muzak"}, "password": {"secret"}, "remember": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/login/", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := doRequest(t, built, req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"username":"muzak","remember":true}`)

	rec = doRequest(t, built, uploadRequest(t, "/login/", []string{"username", "muzak", "password", "secret"}))
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"username":"muzak","remember":false}`)
}

func TestFormValuesCannotBeSuppliedInTheQueryString(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/login/", func(ctx *Context, in formOnlyIn) (formOnlyOut, error) {
		return formOnlyOut{Username: in.Username}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/login/?username=spoofed&password=spoofed", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)
}

func TestFormValueReportsAMalformedValue(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/profiles/{owner}", func(ctx *Context, in profileIn) (Empty, error) { return Empty{}, nil })

	req := uploadRequest(t, "/profiles/muzak", []string{"note", "hello", "rank", "high"},
		part{field: "avatar", filename: "face.png", content: "PNG"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	details := decodeError(t, rec).Error.Details
	if len(details) != 1 || details[0].Location != "form" || details[0].Field != "rank" {
		t.Fatalf("details = %+v, want one form failure for rank", details)
	}
}

func TestUploadRejectsAMalformedBody(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/files/", func(ctx *Context, in fileBytesIn) (Empty, error) { return Empty{}, nil })

	req := httptest.NewRequest(http.MethodPost, "/files/", strings.NewReader("not a multipart body"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=nothing")
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusBadRequest)
}

type badFileTypeIn struct {
	File int `file:"file"`
}

type namelessFileIn struct {
	File File `file:""`
}

type mixedBodyIn struct {
	File File   `file:"file"`
	Note string `json:"note"`
}

func TestUploadRegistrationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
		bind func(*App)
	}{
		{
			name: "unsupported field type",
			want: "a file field must be muzak.File",
			bind: func(app *App) {
				app.Post("/x", func(ctx *Context, in badFileTypeIn) (Empty, error) { return Empty{}, nil })
			},
		},
		{
			name: "empty name",
			want: "empty file parameter name",
			bind: func(app *App) {
				app.Post("/x", func(ctx *Context, in namelessFileIn) (Empty, error) { return Empty{}, nil })
			},
		},
		{
			name: "json body mixed with a file",
			want: "would come from a JSON body",
			bind: func(app *App) {
				app.Post("/x", func(ctx *Context, in mixedBodyIn) (Empty, error) { return Empty{}, nil })
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			tc.bind(app)
			if got := buildError(t, app); !strings.Contains(got, tc.want) {
				t.Errorf("build error = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

func TestUploadIsDocumentedAsAForm(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/profiles/{owner}", func(ctx *Context, in profileIn) (Empty, error) { return Empty{}, nil })
	app.Post("/uploadfiles/", func(ctx *Context, in multiUploadIn) (Empty, error) { return Empty{}, nil })
	app.Post("/login/", func(ctx *Context, in formOnlyIn) (Empty, error) { return Empty{}, nil })

	doc, err := mustBuild(t, app).Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}

	body := doc.Paths["/profiles/{owner}"].Post.RequestBody
	if body == nil || !body.Required {
		t.Fatalf("request body = %+v, want a required one", body)
	}
	if _, described := body.Content["application/json"]; described {
		t.Error("a form route was documented as accepting JSON")
	}
	schema := body.Content["multipart/form-data"].Schema
	if schema == nil {
		t.Fatal("the multipart body carries no schema")
	}
	avatar := schema.Properties["avatar"]
	if avatar == nil || avatar.Type != "string" || avatar.Format != "binary" {
		t.Errorf("avatar schema = %+v, want a binary string", avatar)
	}
	if rank := schema.Properties["rank"]; rank == nil || rank.Type != "integer" || rank.Default != "3" {
		t.Errorf("rank schema = %+v, want an integer defaulting to 3", rank)
	}
	if got := fmt.Sprint(schema.Required); got != "[avatar note]" {
		t.Errorf("required = %s, want the avatar and the note", got)
	}
	for _, parameter := range doc.Paths["/profiles/{owner}"].Post.Parameters {
		if parameter.In == "form" || parameter.In == "file" {
			t.Errorf("%q was documented as a %s parameter, but it describes the body", parameter.Name, parameter.In)
		}
	}

	many := doc.Paths["/uploadfiles/"].Post.RequestBody.Content["multipart/form-data"].Schema.Properties["files"]
	if many == nil || many.Type != "array" || many.Items == nil || many.Items.Format != "binary" {
		t.Errorf("files schema = %+v, want an array of binary strings", many)
	}

	login := doc.Paths["/login/"].Post.RequestBody.Content
	if _, described := login["application/x-www-form-urlencoded"]; !described {
		t.Errorf("a route binding only form values was not documented as accepting urlencoded: %v", login)
	}
}

func TestReleaseUploadRemovesTemporaryFiles(t *testing.T) {
	t.Parallel()
	req := uploadRequest(t, "/files/", nil, part{field: "file", filename: "spilled.txt", content: "on disk"})
	// A memory budget of zero sends every part to a temporary file, which is
	// what a large upload does without needing a large upload to prove it.
	if err := req.ParseMultipartForm(0); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	header := req.MultipartForm.File["file"][0]
	opened, err := header.Open()
	if err != nil {
		t.Fatalf("opening the spilled file: %v", err)
	}
	opened.Close()

	releaseUpload(req)
	if _, err := header.Open(); err == nil {
		t.Error("the temporary file outlived the request")
	}
	releaseUpload(req)
}

func TestFileSaveWritesTheContent(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	destination := filepath.Join(t.TempDir(), "saved.txt")
	app.Post("/uploadfile/", func(ctx *Context, in uploadFileIn) (fileSizeOut, error) {
		written, err := in.File.Save(destination)
		if err != nil {
			return fileSizeOut{}, err
		}
		return fileSizeOut{FileSize: int(written)}, nil
	})

	req := uploadRequest(t, "/uploadfile/", nil, part{field: "file", filename: "notes.txt", content: "hello upload"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"file_size":12}`)

	saved, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("reading the saved file: %v", err)
	}
	if string(saved) != "hello upload" {
		t.Errorf("saved content = %q, want %q", saved, "hello upload")
	}

	if _, err := (File{}).Save(destination); !errors.Is(err, ErrNoFile) {
		t.Errorf("Save on an absent file = %v, want ErrNoFile", err)
	}
}

func TestFileOpenReadsIndependently(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/uploadfile/", func(ctx *Context, in uploadFileIn) (uploadFileOut, error) {
		first, err := in.File.Open()
		if err != nil {
			return uploadFileOut{}, err
		}
		defer first.Close()
		if _, err := io.ReadAll(first); err != nil {
			return uploadFileOut{}, err
		}
		// A second reader starts at the beginning, which is what makes an
		// upload readable more than once.
		second, err := in.File.Open()
		if err != nil {
			return uploadFileOut{}, err
		}
		defer second.Close()
		content, err := io.ReadAll(second)
		if err != nil {
			return uploadFileOut{}, err
		}
		return uploadFileOut{Content: string(content), Filename: in.File.Header().Get("Content-Disposition")}, nil
	})

	req := uploadRequest(t, "/uploadfile/", nil, part{field: "file", filename: "notes.txt", content: "twice"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"content":"twice"`) {
		t.Errorf("body = %s, want the content read twice", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "notes.txt") {
		t.Errorf("body = %s, want the part headers to be reachable", rec.Body.String())
	}
}

type validatedUploadIn struct {
	Title  string `form:"title"`
	Avatar File   `file:"avatar"`
}

// Validate declares the rules the form value must pass, which is what proves a
// form field is validated like any other bound field.
func (in *validatedUploadIn) Validate(v *Validation) {
	v.String(&in.Title).Trim().MinLen(4)
}

func TestFormValuesAreValidatedAndReportedAgainstTheForm(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/validated/", func(ctx *Context, in validatedUploadIn) (formOnlyOut, error) {
		return formOnlyOut{Username: in.Title}, nil
	})
	built := mustBuild(t, app)

	rec := doRequest(t, built, uploadRequest(t, "/validated/", []string{"title", "no"},
		part{field: "avatar", filename: "face.png", content: "PNG"}))
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	details := decodeError(t, rec).Error.Details
	if len(details) != 1 || details[0].Location != "form" || details[0].Field != "title" {
		t.Fatalf("details = %+v, want one form failure for title", details)
	}

	// The rules also run their transformations, so the handler sees the
	// trimmed value rather than what the client sent.
	rec = doRequest(t, built, uploadRequest(t, "/validated/", []string{"title", "  hello  "},
		part{field: "avatar", filename: "face.png", content: "PNG"}))
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"username":"hello","remember":false}`)
}

func TestFormRouteRejectsWhatItCannotDecode(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/login/", func(ctx *Context, in formOnlyIn) (Empty, error) {
		return Empty{}, nil
	}, MaxUploadSize(32))
	built := mustBuild(t, app)

	req := httptest.NewRequest(http.MethodPost, "/login/", strings.NewReader(`{"username":"muzak"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := doRequest(t, built, req)
	assertStatus(t, rec, http.StatusUnsupportedMediaType)
	if message := decodeError(t, rec).Error.Message; !strings.Contains(message, "or application/x-www-form-urlencoded") {
		t.Errorf("message = %q, want it to name both encodings the route accepts", message)
	}

	values := url.Values{"username": {strings.Repeat("x", 128)}}
	req = httptest.NewRequest(http.MethodPost, "/login/", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = doRequest(t, built, req)
	assertStatus(t, rec, http.StatusRequestEntityTooLarge)
	if message := decodeError(t, rec).Error.Message; !strings.Contains(message, "32 byte limit") {
		t.Errorf("message = %q, want it to name the 32 byte limit", message)
	}
}

func TestReadingAPartReportsWhatWentWrong(t *testing.T) {
	t.Parallel()
	req := uploadRequest(t, "/files/", nil, part{field: "file", filename: "spilled.txt", content: "on disk"})
	// A memory budget of zero puts the part in a temporary file, which is what
	// makes both failures below reachable.
	if err := req.ParseMultipartForm(0); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	header := req.MultipartForm.File["file"][0]
	header.Size = 4096

	if _, err := readPart(header); err == nil {
		t.Error("reading a part shorter than its recorded size succeeded")
	}
	single := fileBinder{name: "file", kind: fileBytes}
	target := reflect.New(reflect.TypeFor[fileBytesIn]()).Elem()
	if err := single.assign(target.Field(0), []*multipart.FileHeader{header}); err == nil {
		t.Error("binding a []byte from a truncated part succeeded")
	}
	many := fileBinder{name: "files", kind: fileBytesMany}
	manyTarget := reflect.New(reflect.TypeFor[multiBytesIn]()).Elem()
	if err := many.assign(manyTarget.Field(0), []*multipart.FileHeader{header}); err == nil {
		t.Error("binding a [][]byte from a truncated part succeeded")
	}

	releaseUpload(req)
	if _, err := readPart(header); err == nil {
		t.Error("reading a part whose temporary file is gone succeeded")
	}
}

func TestFileSaveReportsAnUnwritableDestination(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	missing := filepath.Join(t.TempDir(), "no-such-directory", "saved.txt")
	app.Post("/uploadfile/", func(ctx *Context, in uploadFileIn) (Empty, error) {
		_, err := in.File.Save(missing)
		return Empty{}, err
	})

	req := uploadRequest(t, "/uploadfile/", nil, part{field: "file", filename: "notes.txt", content: "hello"})
	rec := doRequest(t, mustBuild(t, app), req)
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestUploadDefaultsComeFromTheApplication(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	route := app.Post("/files/", func(ctx *Context, in fileBytesIn) (Empty, error) { return Empty{}, nil })
	mustBuild(t, app)

	if route.maxUploadSize != DefaultMaxUploadSize {
		t.Errorf("upload limit = %d, want the default of %d", route.maxUploadSize, DefaultMaxUploadSize)
	}
	if route.maxFileSize != 0 {
		t.Errorf("file limit = %d, want none until one is declared", route.maxFileSize)
	}
	// The body limit stays what it was, so a JSON route is not widened by the
	// presence of an upload limit.
	if route.maxBodySize != DefaultMaxBodySize {
		t.Errorf("body limit = %d, want the default of %d", route.maxBodySize, DefaultMaxBodySize)
	}
}

// loginIn is a form whose rules constrain its values without declaring them
// required, which is what the requiredness of a form value comes from instead.
type loginIn struct {
	Username string `form:"username"`
	Password string `form:"password"`
	Next     string `form:"next" required:"false"`
}

func (in *loginIn) Validate(v *Validation) {
	v.String(&in.Username).Trim().Lower().MinLen(2)
	v.String(&in.Password).MinLen(8)
}

func TestFormRulesCannotClearAValueTheBinderRequires(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/login/", func(ctx *Context, in loginIn) (formOnlyOut, error) {
		return formOnlyOut{Username: in.Username}, nil
	})
	built := mustBuild(t, app)

	// The runtime rejects a missing password, so the document has to say the
	// body needs one. Rules that constrain a value without declaring it
	// required must not be read as declaring it optional.
	rec := doRequest(t, built, uploadRequest(t, "/login/", []string{"username", "muzak"}))
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	details := decodeError(t, rec).Error.Details
	if len(details) != 1 || details[0].Field != "password" || details[0].Issue != "is required" {
		t.Fatalf("details = %+v, want the missing password reported as required", details)
	}

	doc, err := built.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}
	body := doc.Paths["/login/"].Post.RequestBody
	if !body.Required {
		t.Error("request body is documented as optional, but the route rejects one without a password")
	}
	schema := body.Content["application/x-www-form-urlencoded"].Schema
	if got := fmt.Sprint(schema.Required); got != "[password username]" {
		t.Errorf("required = %s, want the two values the binder demands", got)
	}
	// The rules still reach the properties they do speak for.
	if shortest := schema.Properties["password"].MinLength; shortest == nil || *shortest != 8 {
		t.Errorf("password minLength = %v, want 8", shortest)
	}
	// A value the tag made optional stays optional.
	if slices.Contains(schema.Required, "next") {
		t.Error("an optional form value was documented as required")
	}
}
