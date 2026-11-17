package media

import "os"

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
