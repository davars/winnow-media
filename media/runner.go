package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
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
	return runResult{Stdout: stdout.Bytes(), Stderr: tail, Usage: res.Usage}, runError(ctx, name, res, err, tail)
}

// runError turns the outcome of one run into an error, or nil.
func runError(ctx context.Context, name string, res toolResult, err error, tail []byte) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%s: %w: %s", name, ctx.Err(), tail)
	case err != nil:
		return fmt.Errorf("%s: %w: %s", name, err, tail)
	case res.Err != "":
		return fmt.Errorf("%s: %s: %s", name, res.Err, tail)
	case res.Signal != 0:
		return fmt.Errorf("%s: killed by signal: %s: %s", name, syscall.Signal(res.Signal), tail)
	case res.Code != 0:
		return fmt.Errorf("%s: exit status %d: %s", name, res.Code, tail)
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
	return usage, runError(parent, name, res, err, bytes.TrimSpace(stderr.bytes()))
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
