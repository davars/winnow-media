// Package tool locates the external programs package media runs.
//
// A program that uses media need not call it: media resolves its own tools.
// What matters is where they come from. A binary linked with the ldflags of
// the module's Nix flake (lib.ldflags) finds them in the flake's pinned tool
// directory and nowhere else; any other build finds them on PATH, so the
// tools are then whatever the host provides.
package tool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Set by the linker, as the flake's lib.ldflags does:
// -X github.com/davars/winnow-media/tool.toolDir=<dir> and .flakeRev=<rev>.
// Empty in other builds.
var (
	toolDir  string
	flakeRev string
)

// Resolve returns the path of the named tool. name must be a bare file name.
//
// When a tool directory was set at link time, the result is <dir>/<name>, and
// Resolve fails if that file is missing or not executable; it never falls
// back to PATH. Otherwise the result comes from exec.LookPath.
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

// FlakeRevision returns the revision of the flake that built the tool
// directory, as set at link time: a commit hash, a dirty tree's revision, or
// "unknown". It is empty when nothing was set. It identifies the tool
// binaries, which output bytes depend on.
func FlakeRevision() string { return flakeRev }
