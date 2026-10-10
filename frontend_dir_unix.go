//go:build unix

package muzak

import (
	"io/fs"
	"os"
)

// openServedDir opens the directory a frontend or static mount serves from
// disk, through an [os.Root], which resolves every path inside the directory
// so that a symbolic link pointing out of it cannot be followed out of it.
//
// The root is held open for as long as the application serves, which costs a
// request nothing to reach. On Unix an open directory keeps nothing from
// renaming or removing it, so a deployment that replaces the directory is not
// held up by the application still serving the old one.
func openServedDir(dir string) (fs.FS, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return root.FS(), nil
}
