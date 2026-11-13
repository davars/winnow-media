package media

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

// The sentinel errors: test for them with errors.Is. The package
// documentation's Errors section sets them side by side and says what else a
// call can return.
var (
	// ErrUnsupportedType is returned by Open when the file's type, as its
	// bytes or OpenOptions.Type name it, is not in Config.Types, or when its
	// bytes are of no type the package recognizes. Retrying the same file on
	// the same Processor cannot succeed.
	ErrUnsupportedType = errors.New("unsupported media type")

	// ErrTooLarge is returned when an input, or the work a call asks of it,
	// is over a limit: one of Config.Caps, a fixed limit listed at Caps, or
	// Resources.Disk for an animation too large to write as one file.
	// Retrying the same call on the same Processor cannot succeed; a smaller
	// selection, or a Processor with higher limits, may.
	ErrTooLarge = errors.New("input too large")

	// ErrTimeLimit is returned when a tool run reached a time limit:
	// ImageMagick's limit for one image (Resources.TimeLimit), the sandbox's
	// CPU-time limit (Resources.CPUTime), or a frame compositor's wait of
	// Resources.TimeLimit for a frame to be consumed. It wraps the run's
	// *ToolError when the tool failed. A retry can succeed with larger limits
	// or on a less loaded host.
	ErrTimeLimit = errors.New("time limit exceeded")

	// ErrClosed is returned by Open on a closed Processor and by Encode on a
	// closed Source. Retrying cannot succeed; open a new Source or create a
	// new Processor.
	ErrClosed = errors.New("closed")

	// ErrInputChanged is returned when the file is not the one that was
	// opened, or has been modified since: by Source.Encode, and by
	// Processor.Open when the file is replaced while it is being opened. It
	// is the expected result when a file is replaced under a long-lived
	// Source. Retrying on the same Source cannot succeed; opening the file
	// again can.
	ErrInputChanged = errors.New("input changed")

	// ErrInvalidRequest is returned when the request is wrong rather than the
	// file's content: an input path that is not a regular file (a directory,
	// FIFO or device), no Encoding or one with a value out of range, a Region
	// outside the frame, a FrameSelection that does not apply to the Source's
	// kind or
	// names a frame or time that does not exist, an OpenOptions.Orientation
	// out of range or given for a video, an output directory that is missing
	// or not empty, or several images of a still written as one animation.
	// Retrying the same call cannot succeed.
	ErrInvalidRequest = errors.New("invalid request")
)

// invalid returns an error wrapping ErrInvalidRequest, with the message.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

// ToolError is a tool run that failed: the tool exited with a non-zero status
// or was killed by a signal. Find it with errors.As. The usual cause is a
// file the decoder cannot read, but a ToolError alone does not say so. A tool
// killed at the sandbox's CPU limit also wraps ErrTimeLimit.
type ToolError struct {
	// Tool names the tool, such as "magick" or "ffmpeg".
	Tool string
	// ExitCode is the tool's exit status, or -1 when a signal killed it.
	ExitCode int
	// Signal is the signal that killed the tool, or 0.
	Signal syscall.Signal
	// Stderr is the start and end of the tool's standard error: at most the
	// first 512 bytes and the last 4 KiB, with a note of what was dropped
	// between them. It can quote the input, so it is for operators' logs
	// only, and Error leaves it out.
	Stderr []byte
}

// Error names the tool and how it ended, and nothing of its output.
func (e *ToolError) Error() string {
	if e.Signal != 0 {
		return fmt.Sprintf("%s: killed by signal: %s", e.Tool, e.Signal)
	}
	return fmt.Sprintf("%s: exit status %d", e.Tool, e.ExitCode)
}

// Stderr returns the standard error of the failed tool run that err wraps,
// which is the Stderr field of its *ToolError, or nil when err wraps none.
// ToolError.Stderr says what the bytes may contain and who may see them.
func Stderr(err error) []byte {
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
