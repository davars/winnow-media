// Package media builds and runs the media tools' command lines: still and
// animation encodes for face detection, and video thumbnail frames.
//
// The package decides arguments and interprets tool output; it does not run
// programs itself. A caller supplies a Runner and a Stager through an Env, so
// it controls how tools are resolved, how long they run and where scratch
// files live. The same inputs and tool binaries produce the same arguments,
// and so the same output bytes.
package media

import "context"

// Runner runs one tool to completion and returns its standard output.
//
// The package passes bare tool names ("magick", "ffmpeg"); resolving them to
// executables is the Runner's business. A non-nil error means the tool failed.
type Runner func(ctx context.Context, tool string, args ...string) ([]byte, error)

// Stager creates an empty scratch file whose base name matches pattern, in
// the form os.CreateTemp accepts. cleanup removes the file and is safe to call
// once; the caller owns it. The tools write their output to the returned
// path.
type Stager func(pattern string) (path string, cleanup func(), err error)

// Env is what the package needs from its caller to run tools. Run is
// required by every method; Stage is required by the methods that produce an
// output file (EncodeRung).
type Env struct {
	Run   Runner
	Stage Stager
}
