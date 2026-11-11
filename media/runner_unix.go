//go:build unix

package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// drainTimeout is how long the direct runner waits for a tool's output to
// end after the tool's process group has been killed; tests lower it.
var drainTimeout = 5 * time.Second

// directRunner runs the process itself, in a process group of its own that is
// killed when ctx ends and as soon as the process exits, so nothing it left
// running survives the call. The tool writes to pipes the runner reads, so
// that a left-over process holding them open cannot delay the return.
func directRunner(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return toolResult{}, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return toolResult{}, err
	}
	cmd := exec.CommandContext(ctx, spec.Tool, spec.Args...)
	cmd.Env, cmd.Dir = spec.Env, spec.Dir
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = directProcAttr()
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err = cmd.Start()
	_, _ = outW.Close(), errW.Close()
	if err != nil {
		_, _ = outR.Close(), errR.Close()
		return toolResult{Err: "starting the tool: " + err.Error()}, nil
	}
	var copies sync.WaitGroup
	for _, c := range []struct {
		from *os.File
		to   io.Writer
	}{{outR, stdout}, {errR, stderr}} {
		copies.Add(1)
		go func() {
			defer copies.Done()
			_, _ = io.Copy(c.to, c.from)
		}()
	}
	waitErr := cmd.Wait()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	drained := make(chan struct{})
	go func() { copies.Wait(); close(drained) }()
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		_, _ = outR.Close(), errR.Close()
		<-drained
		return toolResult{}, fmt.Errorf("the tool's output was still open %v after its process group was killed", drainTimeout)
	}
	_, _ = outR.Close(), errR.Close()

	state := cmd.ProcessState
	if state == nil {
		return toolResult{}, fmt.Errorf("the tool's exit status was lost (is SIGCHLD ignored?): %w", waitErr)
	}
	var res toolResult
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		res.Err = "waiting for the tool: " + waitErr.Error()
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		res.Signal = int(ws.Signal())
	} else {
		res.Code = state.ExitCode()
	}
	res.Usage = UsageOf(state)
	return res, nil
}
