//go:build !linux

package media

import (
	"os"
	"path/filepath"
)

// inputRef is how a tool names the input at path. /dev/fd/N duplicates the
// descriptor here, and a duplicate shares its offset, which parallel tool
// runs cannot share; so a tool is given an extensionless link in dir to the
// path, and the descriptor guards only what Go reads.
func inputRef(dir, path string) (string, error) {
	link := filepath.Join(dir, "input")
	if err := os.Symlink(path, link); err != nil {
		return "", err
	}
	return link, nil
}
