package media

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

// The errors below are sentinels: test for them with errors.Is. A call's
// error text is not part of the contract and can change in any release; the
// sentinels, ToolError and the context's own errors are what callers branch
// on. An error that wraps none of them says nothing about whether a retry
// can succeed.
var (
	// ErrUnsupportedType is returned, wrapped with the media type found in
	// the input's bytes, when that type is not in Config.Types or the bytes
	// are of no type the package recognizes. Retrying the same call on the
	// same file cannot succeed.
	ErrUnsupportedType = errors.New("unsupported media type")

	// ErrTooLarge is returned, wrapped with the cap and the values that
	// exceeded it, when an input, or the work a call asks of it, is larger
	// than a Processor accepts, and when a header read reaches ImageMagick's
	// list length ceiling (2048 scenes). Retrying the same call on the same
	// Processor cannot succeed; a smaller selection or a Processor with
	// higher Caps, or with a larger Resources.Disk where a refusal depends on
	// it, may.
	ErrTooLarge = errors.New("input too large")

	// ErrTimeLimit is returned, wrapped with the *ToolError of the run, when it failed, when a
	// tool run reached a time limit: ImageMagick's per-image limit
	// (Resources.TimeLimit), the sandbox's CPU-time limit
	// (Resources.CPUTime), or a frame compositor waiting longer than
	// Resources.TimeLimit for a frame to be consumed. ImageMagick often exits
	// 0 when it reaches its limit, with the work unfinished, so such a run's
	// output is never used. A retry can succeed with larger limits or on a
	// less loaded host.
	ErrTimeLimit = errors.New("time limit exceeded")

	// ErrClosed is returned by a Source or Processor used after Close.
	// Retrying cannot succeed; open a new Source or create a new Processor.
	ErrClosed = errors.New("closed")

	// ErrInputChanged is returned when the file is not the one that was
	// opened: by Source.Encode, and by Processor.Open when the file is
	// replaced while it is being opened. It is the expected result when a file
	// is replaced under a long-lived Source. Retrying the same Source cannot
	// succeed; opening the file again can.
	ErrInputChanged = errors.New("input changed")

	// ErrInvalidRequest is returned when the request is wrong rather than the
	// file: an Encoding with a value out of range, a Region outside the
	// frame, a FrameSelection that does not apply to the Source's kind or
	// names a frame or time that does not exist, an OpenOptions.Orientation
	// out of range or given for a video, an output directory that is missing
	// or not empty, or a still written as one animation. Retrying the same
	// call cannot succeed.
	ErrInvalidRequest = errors.New("invalid request")
)

// invalid returns an error wrapping ErrInvalidRequest, with the message.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// ToolError is a tool run that failed: the tool exited with a non-zero
// status or was killed by a signal. Find it with errors.As. A tool killed at
// the sandbox's CPU limit also wraps ErrTimeLimit.
type ToolError struct {
	// Tool names the tool, such as "magick" or "ffmpeg".
	Tool string
	// ExitCode is the tool's exit status, or -1 when a signal killed it.
	ExitCode int
	// Signal is the signal that killed the tool, or 0.
	Signal syscall.Signal
	// Stderr is the start and end of the tool's standard error (the first
	// 512 bytes and the last 4 KiB, with a note of the bytes dropped). It can
	// quote the input and is kept out of Error.
	Stderr []byte
}

// Error names the tool and how it ended, and nothing of its output.
func (e *ToolError) Error() string {
	if e.Signal != 0 {
		return fmt.Sprintf("%s: killed by signal: %s", e.Tool, e.Signal)
	}
	return fmt.Sprintf("%s: exit status %d", e.Tool, e.ExitCode)
}

// stderrOf returns the standard error of the tool run err wraps, or nil.
func stderrOf(err error) []byte {
	var te *ToolError
	if errors.As(err, &te) {
		return te.Stderr
	}
	return nil
}

// withoutPath returns err without the path of the file operation that
// failed, keeping its cause: errors.Is(err, fs.ErrNotExist) still holds.
func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
