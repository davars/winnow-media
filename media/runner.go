package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// runSpec is one process to run: the tool's path, arguments, whole
// environment and working directory.
type runSpec struct {
	Tool string
	Args []string
	Env  []string
	Dir  string
	// Input, when not nil, is a regular file the process inherits as
	// descriptor 3, open for reading. The caller keeps it open until the run
	// ends; the runner does not close it.
	Input *os.File
}

// toolResult is how a run ended, when the runner delivered a report.
type toolResult struct {
	// Err is set when the runner itself failed: the sandbox could not be
	// applied, or the tool or its descendants could not be started or cleared.
	Err string `json:",omitempty"`
	// Signal is the signal that killed the tool, or 0.
	Signal int
	// Code is the tool's exit code when Signal is 0.
	Code int
	// Usage is what the tool and its descendants consumed, or nil when the
	// runner could not measure it.
	Usage *Usage
}

// runner starts one process, with its standard output and error going to the
// writers, and waits for it. It returns how the run ended, with the usage of
// the tool and its descendants; the error is non-nil when no outcome was
// delivered. Cancelling ctx kills the process and everything it started.
type runner func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error)

// toolRun runs one tool by name to completion, with in, when not nil, as its
// input descriptor.
type toolRun func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error)

// runTool runs one tool through the Processor's runner, with in, when not nil,
// as its input descriptor.
func (p *Processor) runTool(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
	path, ok := p.tools[name]
	if !ok {
		return runResult{}, fmt.Errorf("unknown tool %q", name)
	}
	if err := requireInput(in, args); err != nil {
		return runResult{}, err
	}
	tmp, err := p.runTempDir()
	if err != nil {
		return runResult{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	var stdout bytes.Buffer
	var stderr headTailBuffer
	res, err := p.start(ctx, runSpec{Tool: path, Args: args, Env: p.environ(tmp), Dir: p.dir, Input: in}, &stdout, &stderr)
	tail := bytes.TrimSpace(stderr.bytes())
	return runResult{Stdout: stdout.Bytes(), Stderr: tail, Usage: res.Usage}, runError(ctx, name, res, err, tail, p.cpuLimit)
}

// cpuSlack is how far short of the CPU limit a killed run's measured CPU time
// may fall and still count as having reached it: the larger of 250 ms and 5%
// of the limit. The kernel enforces RLIMIT_CPU on tick-sampled CPU time while
// wait4 reports precise run time, so a killed run's measured time can fall
// short by a share of the limit that grows with load and thread count. A
// SIGKILL from anything else within the last 5% of the budget is reported as
// ErrTimeLimit.
func cpuSlack(limit time.Duration) time.Duration {
	return max(250*time.Millisecond, limit/20)
}

// reachedCPULimit reports whether a run ended because it reached the
// sandbox's CPU-time limit. The limit's soft and hard values are equal, so the
// kernel kills with SIGKILL; SIGXCPU is what a soft limit alone sends. Either
// signal alone could come from anything else, so the run's measured CPU time
// must also have reached the limit. limit is 0 when no limit is in force.
func reachedCPULimit(res toolResult, limit time.Duration) bool {
	sig := syscall.Signal(res.Signal)
	return limit > 0 && (sig == syscall.SIGKILL || sig == syscall.SIGXCPU) &&
		res.Usage != nil && res.Usage.CPU+cpuSlack(limit) >= limit
}

// runError turns the outcome of one run into an error, or nil. A run that
// failed after starting is a *ToolError carrying tail as its Stderr; one the
// sandbox killed at cpuLimit also wraps ErrTimeLimit.
func runError(ctx context.Context, name string, res toolResult, err error, tail []byte, cpuLimit time.Duration) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%s: %w", name, ctx.Err())
	case err != nil:
		return fmt.Errorf("%s: %w", name, err)
	case res.Err != "":
		return fmt.Errorf("%s: %s", name, res.Err)
	case res.Signal != 0:
		te := &ToolError{Tool: name, ExitCode: -1, Signal: syscall.Signal(res.Signal), Stderr: tail}
		if reachedCPULimit(res, cpuLimit) {
			return fmt.Errorf("%w: %w", ErrTimeLimit, te)
		}
		return te
	case res.Code != 0:
		return &ToolError{Tool: name, ExitCode: res.Code, Stderr: tail}
	}
	return nil
}

// composeFrames runs a frame compositor through the Processor's runner, under
// the same limits as any tool: the GIF compositor, which is this executable
// started again with compositorEnv set, or the external tool the request
// names; in is its input, when not nil. onFrame is called, in order, with the
// path of each frame as the compositor finishes it; if it fails the compositor
// is stopped and its error returned. The usage returned covers the compositor.
func (p *Processor) composeFrames(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
	if err := requireInput(in, req.args()); err != nil {
		return nil, err
	}
	tmp, err := p.runTempDir()
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	spec := runSpec{Args: req.args(), Dir: p.dir, Input: in}
	name := "gif compositor"
	if req.external != "" {
		path, ok := p.tools[req.external]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", req.external)
		}
		spec.Tool, spec.Env, name = path, p.environ(tmp), req.external
	} else {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		spec.Tool, spec.Env = exe, []string{compositorEnv + "=1"}
	}
	parent := ctx
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	lines := &frameLines{dir: req.dir, onFrame: onFrame}
	var stderr headTailBuffer
	res, err := p.start(ctx, spec, stopOnError{lines, cancel}, &stderr)
	usage := res.Usage
	if lines.err != nil {
		return usage, lines.err
	}
	runErr := runError(parent, name, res, err, bytes.TrimSpace(stderr.bytes()), p.cpuLimit)
	if te := (*ToolError)(nil); errors.As(runErr, &te) && bytes.Contains(te.Stderr, []byte(notConsumedMessage)) {
		runErr = fmt.Errorf("%w: %w", ErrTimeLimit, runErr)
	}
	return usage, runErr
}

// stopOnError is a writer that calls stop when its writes fail, so that a
// producer whose output is refused is cancelled instead of left running with
// nobody reading it.
type stopOnError struct {
	w    io.Writer
	stop func()
}

func (s stopOnError) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil {
		s.stop()
	}
	return n, err
}
