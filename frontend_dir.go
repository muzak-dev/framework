package muzak

import (
	"io/fs"
	"os"
)

// servedDir is a directory served through an [os.Root] opened for each call
// and closed before the call returns, so that nothing holds the directory
// between calls. It is what [openServedDir] returns where an open directory
// cannot be removed or renamed; see frontend_dir_other.go.
type servedDir string

// Open opens name in the directory. The file stays open once the root it was
// reached through is closed, as a file does once its directory's handle is.
func (d servedDir) Open(name string) (fs.File, error) {
	root, err := os.OpenRoot(string(d))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.FS().Open(name)
}

// Stat describes name in the directory without opening it for reading.
func (d servedDir) Stat(name string) (fs.FileInfo, error) {
	root, err := os.OpenRoot(string(d))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return fs.Stat(root.FS(), name)
}
