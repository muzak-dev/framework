//go:build !unix

package muzak

import (
	"io/fs"
	"os"
)

// openServedDir opens the directory a frontend or static mount serves from
// disk. Every file is reached through an [os.Root], which resolves every path
// inside the directory so that a symbolic link pointing out of it cannot be
// followed out of it.
//
// Unlike on Unix, the root is not held for as long as the application serves:
// on Windows a directory that is open cannot be removed or renamed, so a
// deployment replacing the directory, or a test removing its temporary one,
// would fail for as long as the application lived. The directory is opened
// here once, so that a missing one is reported when the application is
// built, and then for each file, and closed as soon as the file is open.
func openServedDir(dir string) (fs.FS, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	if err := root.Close(); err != nil {
		return nil, err
	}
	return servedDir(dir), nil
}
