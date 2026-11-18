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
	"runtime/debug"
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

const modulePath = "github.com/davars/winnow-media"

// Dir returns the tool directory set at link time, the path in the Nix store
// that Resolve looks in, or "" for a build that finds its tools on PATH. The
// directory's store path is derived from the packages whose programs it
// links, so binaries with equal non-empty Dir run the same tool binaries.
// An empty Dir identifies nothing: PATH holds whatever the host has.
func Dir() string { return toolDir }

// ModuleVersion returns this module's version as the running binary was
// built, from its build information: a tag ("v1.1.0"), a pseudo-version
// ("v1.1.1-0.20261118120000-0123456789ab", whose last element names the
// commit), or "(devel)". It is "(devel)" when the module was built from its
// own tree or replaced by a local directory, and empty when the build
// information does not list the module, as in a build without module
// information. Equal tags or pseudo-versions mean the same library source;
// "(devel)" and the empty string identify no source. The flake revision of a
// pinned build, FlakeRevision, changes with every commit of the flake, and
// Dir only with the tools.
func ModuleVersion() string {
	info, _ := debug.ReadBuildInfo()
	return moduleVersion(info)
}

// moduleVersion is this module's version in info, or empty.
func moduleVersion(info *debug.BuildInfo) string {
	if info == nil {
		return ""
	}
	if info.Main.Path == modulePath {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path != modulePath {
			continue
		}
		if dep.Replace != nil {
			return dep.Replace.Version
		}
		return dep.Version
	}
	return ""
}
