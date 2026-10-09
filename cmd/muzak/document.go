package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"muzak.dev/framework"
)

// maxDocumentBytes is the most of a response read as a document, which is
// the most muzak.ReadDocument reads, so a server that sends more is cut off
// by the client rather than read to its end.
const maxDocumentBytes = 16 << 20

// documentSource is where routes and ts read a document from: a URL, a file,
// or standard input.
type documentSource struct {
	url  string
	file string
}

func (s *documentSource) flags(fs *flag.FlagSet) {
	fs.StringVar(&s.url, "url", "", "fetch the document from this http or https `URL`, such as http://localhost:8080/openapi.json")
	fs.StringVar(&s.file, "file", "", "read the document from this `file`, or from standard input when it is -")
}

// load reads the document the flags name.
func (s *documentSource) load(ctx context.Context, c *console, cmd *command) (*muzak.Document, error) {
	switch {
	case s.url != "" && s.file != "":
		return nil, usagef(cmd, "%s reads one document, and was given both -url and -file", cmd.name)
	case s.url != "":
		return fetchDocument(ctx, c, cmd, s.url)
	case s.file != "":
		return readDocumentFile(c, s.file)
	}
	return nil, usagef(cmd, "%s needs a document, from -url or -file", cmd.name)
}

// readDocumentFile reads a document from a file, or from standard input when
// the name is "-".
func readDocumentFile(c *console, name string) (*muzak.Document, error) {
	if name == "-" {
		doc, err := muzak.ReadDocument(c.stdin)
		if err != nil {
			return nil, inDocument("standard input", err)
		}
		return doc, nil
	}
	f, err := os.Open(c.path(name))
	if err != nil {
		return nil, fmt.Errorf("muzak: the document cannot be opened: %w", err)
	}
	defer func() { _ = f.Close() }()
	doc, err := muzak.ReadDocument(f)
	if err != nil {
		return nil, inDocument(name, err)
	}
	return doc, nil
}

// fetchDocument fetches a document over HTTP.
//
// The client is muzak.Client, so every wait has a bound, the response is
// read to at most maxDocumentBytes, and redirects are followed only so far
// and never from https down to http. It is allowed to connect to loopback and
// private addresses, which a client of a URL from somewhere else should never
// be: a developer points this at their own application, which listens on
// localhost or inside their network, and the URL is theirs. Cloud metadata
// addresses stay refused even so, since no document is served there and a
// redirect to one is how credentials would be read.
func fetchDocument(ctx context.Context, c *console, cmd *command, raw string) (*muzak.Document, error) {
	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, usagef(cmd, "-url is %q, and it has to be an absolute http or https URL, such as http://localhost:8080/openapi.json", raw)
	}
	client := muzak.NewClient(muzak.ClientOptions{
		AllowPrivateNetworks: true,
		Timeout:              c.fetchTimeout,
		MaxResponseBytes:     maxDocumentBytes,
		// There is no request this one is made on behalf of, so there is no
		// identity to carry along.
		Propagate: func(context.Context, http.Header) {},
	})
	defer func() { _ = client.Close() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		// coverage: the URL was parsed above and the method is a constant,
		// which leaves nothing for NewRequest to refuse.
		return nil, fmt.Errorf("muzak: the request could not be built: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	origin := target.Scheme + "://" + target.Host
	resp, err := client.Do(req)
	if err != nil {
		return nil, fetchError(c, origin, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("muzak: %s answered with status %d rather than the document", printable(origin), resp.StatusCode)
	}
	doc, err := muzak.ReadDocument(resp.Body)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fetchError(c, origin, err)
	}
	if err != nil {
		return nil, inDocument(origin, err)
	}
	return doc, nil
}

// fetchError explains a fetch that failed. One that ran out of time says so
// in this command's terms, since the client's own message names an option
// the command line does not have.
func fetchError(c *console, origin string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("muzak: %s did not send the document within %s", printable(origin), c.fetchTimeout)
	}
	return err
}

// inDocument names the document an error from muzak.ReadDocument is about.
func inDocument(name string, err error) error {
	return fmt.Errorf("muzak: %s: %s", printable(name), strings.TrimPrefix(err.Error(), "muzak: "))
}

// writeOutput writes the bytes to standard output, or to a file in a way
// that never leaves half of them behind.
func writeOutput(c *console, name string, data []byte) error {
	if name == "" || name == "-" {
		_, err := c.stdout.Write(data)
		return err
	}
	if err := writeFileAtomic(c.path(name), data); err != nil {
		return fmt.Errorf("muzak: %s could not be written: %w", printable(name), err)
	}
	return nil
}

// writeFileAtomic writes a file by writing a temporary file beside it and
// renaming it into place, so that a reader, a build watching the file or an
// interrupted run never sees half of it. A symbolic link at the path is
// replaced rather than written through. A file that is there already keeps
// its permissions, and a new one is readable by all, as a source file is.
func writeFileAtomic(name string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), name)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}
