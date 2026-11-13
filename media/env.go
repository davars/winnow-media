package media

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
)

// runResult is one completed tool run.
type runResult struct {
	Stdout []byte
	// Stderr is the start and end of the tool's standard error (the first 512
	// bytes and the last 4 KiB, with a note of the bytes dropped). ImageMagick
	// exits 0 when it hits its time limit and reports it there and nowhere
	// else, so every magick run is checked against it for ErrTimeLimit.
	Stderr []byte
	// Usage is nil when the runner cannot measure it; it is never zero-filled.
	Usage *Usage
}

// Usage is what the tools of one call consumed: CPU is summed over every tool
// run of the call, and MaxRSS is the largest peak of any one run. Both cover
// the tools and their descendants, never the calling program. A call reports
// it also when it fails; it is zero when no tool ran.
type Usage struct {
	CPU    time.Duration // user plus system time
	MaxRSS int64         // largest resident set of the tool or any descendant, in bytes
}

// usageOf reports the resources an exited process consumed. It returns nil
// when state is nil or carries no rusage.
func usageOf(state *os.ProcessState) *Usage {
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

// toolEnv is what the decoding and encoding code needs to run tools: run
// receives bare tool names ("magick", "ffmpeg", "webp-frames") and compose runs
// the frame compositor. Both give the tool, as its input descriptor, the file
// the Processor opened the input as.
type toolEnv struct {
	run     func(ctx context.Context, name string, args ...string) (runResult, error)
	compose composer

	// resources are the limits the runs are held to and caps those of the
	// gate.
	resources Resources
	caps      caps
}

// pinnedTo returns e with every failed run checked against the descriptor f,
// which holds the input as id described it. A run that failed while f no
// longer matches id, or has no links left, returns an error wrapping
// ErrInputChanged in place of the tool's, because a tool cannot always say
// that its input was replaced. when ends the message: "while it was opened" or
// "since it was opened".
func (e toolEnv) pinnedTo(f *os.File, id fileID, when string) toolEnv {
	changed := func(ctx context.Context, err error) error {
		if err == nil || ctx.Err() != nil {
			return err
		}
		if info, serr := f.Stat(); serr == nil && (idOf(info) != id || unlinked(info)) {
			return fmt.Errorf("source: %w %s", ErrInputChanged, when)
		}
		return err
	}
	run, compose := e.run, e.compose
	e.run = func(ctx context.Context, name string, args ...string) (runResult, error) {
		res, err := run(ctx, name, args...)
		return res, changed(ctx, err)
	}
	e.compose = func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) error {
		return changed(ctx, compose(ctx, req, onFrame))
	}
	return e
}

// threads is the thread count of the ffmpeg and ffprobe runs.
func (e toolEnv) threads() int { return e.resources.Threads }

// consumeTimeout is how long the compositor waits for a written frame to be
// consumed.
func (e toolEnv) consumeTimeout() time.Duration {
	if composeTimeout != 0 {
		return composeTimeout
	}
	return e.resources.TimeLimit
}

// composer composites the selected frames of a GIF or animated WebP in the
// sandbox and calls onFrame with each as it is written.
type composer func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) error
