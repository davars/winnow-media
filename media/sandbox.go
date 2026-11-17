package media

import (
	"os"
	"path/filepath"
	"strings"
)

// helperEnv names the environment variable that marks a process as the
// sandbox helper. Only the helper's own environment carries it; the tool it
// runs gets exactly Processor.environ.
const helperEnv = "WINNOW_MEDIA_HELPER"

// The Processor starts this executable again as its sandbox helper and as the
// GIF compositor, each marked in its environment.
func init() {
	if os.Getenv(helperEnv) != "" {
		helperMain()
		os.Exit(1)
	}
	if os.Getenv(compositorEnv) != "" {
		compositorMain()
	}
}

// confinement is the set of file accesses a confined run keeps; every other
// access to the filesystem fails with EACCES. A path that does not exist on
// the host is skipped.
type confinement struct {
	// Exec are files and directories whose files may be read and executed:
	// the programs and the libraries they load.
	Exec []string
	// Read are files and directories that may be read.
	Read []string
	// Write are files and directories that may be read and written. Within a
	// directory, files may be created, truncated and removed, and linked into
	// the directory from within it.
	Write []string
	// Input says that the file the run inherits as its input descriptor may
	// be read, and no other file beside it.
	Input bool
}

// The standing allowances of a render run, besides its program and what the
// run is given: where fontconfig and the C library look for fonts and the
// locale, the process and CPU information the runtime reads, and the devices;
// docs/measurements.md#rendering says how they were found.
var (
	renderReadPaths = []string{
		"/etc/fonts", "/usr/share/fonts", "/usr/local/share/fonts", "/usr/share/fontconfig",
		"/usr/lib/locale", "/etc/localtime", "/etc/ld.so.cache",
		"/proc", "/sys/devices/system/cpu", "/dev/urandom",
	}
	renderWritePaths = []string{"/dev/null"}
	// systemLibraryPaths hold the programs and libraries of a tool that is
	// not in the Nix store.
	systemLibraryPaths = []string{"/usr", "/lib", "/lib64"}
)

// nixStore is where the programs and libraries of the flake's tools are.
const nixStore = "/nix/store"

// renderConfinement returns the confinement of a run of the program tool
// that reads one input and writes only into runDir; extraRead are further
// directories it may read, such as the directory of a policy file. A tool
// that resolves into the Nix store may execute the store, and any other the
// system's library directories and its own.
func renderConfinement(tool, runDir string, extraRead ...string) *confinement {
	c := &confinement{
		Read:  append(append([]string{}, renderReadPaths...), extraRead...),
		Write: append([]string{runDir}, renderWritePaths...),
		Input: true,
	}
	resolved, err := filepath.EvalSymlinks(tool)
	if err != nil {
		resolved = tool
	}
	if resolved == nixStore || strings.HasPrefix(resolved, nixStore+"/") {
		c.Exec = []string{nixStore}
	} else {
		c.Exec = append([]string{filepath.Dir(resolved)}, systemLibraryPaths...)
	}
	return c
}
