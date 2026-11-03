// Package tool locates the external programs the media library runs.
//
// Release builds point the package at a Nix-built tool directory with
// -ldflags -X; the directory's store path then appears in the binary, so
// Nix's reference scan carries the tool closure into the binary's runtime
// closure.
package tool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Set by the linker: -X github.com/davars/winnow-media/tool.toolDir=<dir>
// and .flakeRev=<rev>. Empty in development builds.
var (
	toolDir  string
	flakeRev string
)

// Resolve returns the path of the named tool.
//
// When a tool directory was injected at link time, the result is
// <dir>/<name> and Resolve fails if that file is missing or not executable;
// it never falls back to PATH in that case. With no injected directory
// (development) the result comes from exec.LookPath. name must be a bare
// file name.
func Resolve(name string) (string, error) {
	if name == "" || name != filepath.Base(name) {
		return "", fmt.Errorf("tool: invalid tool name %q", name)
	}
	if toolDir == "" {
		return exec.LookPath(name)
	}
	p := filepath.Join(toolDir, name)
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("tool: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("tool: %s is not an executable file", p)
	}
	return p, nil
}

// FlakeRevision returns the flake revision injected at link time, or the
// empty string in a development build.
func FlakeRevision() string { return flakeRev }
