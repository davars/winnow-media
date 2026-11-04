// Package media builds and runs the media tools' command lines: bounded WebP
// encodes of still images and animations (a fallback ladder of dimension
// ceilings and frame caps, with the frame mapping each encode holds), and
// animated WebP thumbnails of video. It also offers media operations (Still,
// AnimationFrames, VideoFrames) that turn one staged input into upright,
// sized, sRGB images in an output directory.
//
// The package decides arguments and interprets tool output; it does not run
// programs itself. A caller supplies a Runner and a Stager through an Env, so
// it controls how tools are resolved, how long they run and where scratch
// files live. The same inputs and tool binaries produce the same arguments,
// and so the same output bytes.
package media

import (
	"context"
	"os"
	"syscall"
	"time"
)

// Result is one completed tool run.
type Result struct {
	Stdout []byte
	// Usage is nil when the runner cannot measure it; it is never zero-filled.
	Usage *Usage
}

// Usage is what one tool run consumed.
type Usage struct {
	CPU    time.Duration // user plus system time
	MaxRSS int64         // peak resident set size, in bytes
}

// UsageOf reports the resources an exited process consumed. It returns nil
// when state is nil or carries no rusage.
func UsageOf(state *os.ProcessState) *Usage {
	if state == nil {
		return nil
	}
	ru, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return nil
	}
	return &Usage{
		CPU:    state.UserTime() + state.SystemTime(),
		MaxRSS: maxRSSBytes(ru),
	}
}

// Runner runs one tool to completion and returns its standard output and,
// when it can measure it, its resource usage.
//
// The package passes bare tool names ("magick", "ffmpeg"); resolving them to
// executables is the Runner's business. A non-nil error means the tool failed.
type Runner func(ctx context.Context, tool string, args ...string) (Result, error)

// Stager creates an empty scratch file whose name ends in ext (for example
// ".webp"); the tools choose the output format from the extension. cleanup
// removes the file and is safe to call once; the caller owns it. The tools
// write their output to the returned path.
type Stager func(ext string) (path string, cleanup func(), err error)

// Env is what the package needs from its caller to run tools. Run is
// required by every method; Stage is required by the methods that produce an
// output file (EncodeRung).
type Env struct {
	Run   Runner
	Stage Stager
}
