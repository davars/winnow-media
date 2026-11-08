// Package media builds and runs the media tools' command lines: bounded WebP
// encodes of still images and animations (a fallback ladder of dimension
// ceilings and frame caps, with the frame mapping each encode holds), and
// animated WebPs of a video's samples. It also offers media operations that
// turn one input file into upright, sized, sRGB images in an output directory.
//
// The operations (Processor.Still, Processor.AnimationFrames,
// Processor.VideoFrames and their AnimationFramesAt, VideoFramesAt and
// VideoFrameRun variants) run the tools themselves, for untrusted input: a
// Processor checks the file's size and type before any tool runs, pins the
// decoder to the type found, and runs the tools with a fixed environment under
// a generated ImageMagick policy that it has read back from the tool.
//
// The rest of the package decides arguments and interprets tool output
// without running programs. A caller supplies a Runner and a Stager through
// an Env, so it controls how tools are resolved, how long they run and where
// scratch files live. The same inputs and tool binaries produce the same
// arguments, and so the same output bytes.
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
	// Stderr is the start and end of the tool's standard error (a Processor
	// keeps the first 512 bytes and the last 4 KiB, with a note of the bytes
	// dropped), or nil when the runner does not capture it. ImageMagick reports a run that hit its time
	// limit there and nowhere else, so a runner that leaves it nil leaves
	// only the size check of the encodes to notice.
	Stderr []byte
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

// Scratcher creates a new empty directory for intermediate files. cleanup
// removes it and what is in it and is safe to call once; the caller owns it.
type Scratcher func() (dir string, cleanup func(), err error)

// Env is what the package needs from its caller to run tools. Run is
// required by every method; Stage is required by the methods that produce an
// output file (EncodeRung, Source.Encode); Scratch is required by Open and
// EncodeRung, which keep decoded frames in it until the source is closed.
type Env struct {
	Run     Runner
	Stage   Stager
	Scratch Scratcher
	// Memory selects ImageMagick's memory limits; the zero value is
	// HighMemory. A Processor sets it from its own profile.
	Memory MemoryProfile

	// compose runs the frame compositor; it is set only by a Processor.
	compose composer
}

// composer composites the selected frames of a GIF or animated WebP in the
// sandbox and calls onFrame with each as it is written.
type composer func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) error
