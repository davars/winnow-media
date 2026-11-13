//go:build unix

package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDirectRunner(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal("sh is not available; run the tests under nix develop")
	}
	// The runs have no PATH.
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal("sleep is not available; run the tests under nix develop")
	}
	run := func(ctx context.Context, env []string, script string) (string, toolResult, error) {
		var out, errOut strings.Builder
		res, err := directRunner(ctx, runSpec{Tool: sh, Args: []string{"-c", script}, Env: env, Dir: t.TempDir()}, &out, &errOut)
		return out.String(), res, err
	}
	alive := func(pid int) bool { return syscall.Kill(pid, 0) == nil }
	gone := func(pid int) bool {
		for range 100 {
			if !alive(pid) {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}
	pidOf := func(t *testing.T, s string) int {
		t.Helper()
		pid, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			t.Fatalf("pid %q: %v", s, err)
		}
		return pid
	}

	t.Run("environment, directory and exit status are the spec's", func(t *testing.T) {
		out, res, err := run(context.Background(), []string{"ONE=1"}, `echo "$ONE"; echo "${HOME-unset}"; exit 3`)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Fields(out), []string{"1", "unset"}; !slices.Equal(got, want) {
			t.Errorf("output %q, want %q", got, want)
		}
		if res.Code != 3 || res.Signal != 0 || res.Err != "" {
			t.Errorf("result %+v, want exit code 3", res)
		}
		if res.Usage == nil {
			t.Error("no usage")
		}
	})

	t.Run("a process left running is killed when the tool exits", func(t *testing.T) {
		out, res, err := run(context.Background(), []string{}, sleep+` 60 >/dev/null 2>&1 & echo $!`)
		if err != nil || res.Code != 0 {
			t.Fatalf("res %+v, err %v", res, err)
		}
		if pid := pidOf(t, out); !gone(pid) {
			t.Errorf("left-over process %d survived the call", pid)
		}
	})

	// The sandbox helper kills what the tool leaves as soon as the tool exits;
	// the direct runner must too, even when the left-over process holds the
	// tool's output open, rather than wait out WaitDelay first.
	t.Run("a process holding the output is killed when the tool exits", func(t *testing.T) {
		start := time.Now()
		out, res, err := run(context.Background(), []string{}, sleep+` 60 & echo $!`)
		if err != nil || res.Code != 0 || res.Err != "" {
			t.Fatalf("res %+v, err %v", res, err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("the call took %v: the left-over process was killed only after WaitDelay", took)
		}
		if pid := pidOf(t, out); !gone(pid) {
			t.Errorf("left-over process %d survived the call", pid)
		}
	})

	// A process in a session of its own is out of the group's reach; the run
	// is then an error rather than a wait for its output to end. The tool
	// lingers so that the session exists before the group is killed.
	t.Run("output still held after the group is killed is an error", func(t *testing.T) {
		setsid, err := exec.LookPath("setsid")
		if err != nil {
			t.Fatal("setsid is not available; run the tests under nix develop")
		}
		old := drainTimeout
		drainTimeout = 200 * time.Millisecond
		defer func() { drainTimeout = old }()
		out, _, err := run(context.Background(), []string{}, setsid+" "+sleep+" 60 & echo $!; "+sleep+" 0.3")
		if pid, perr := strconv.Atoi(strings.TrimSpace(out)); perr == nil {
			defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
		}
		if err == nil {
			t.Error("a run whose output stayed open succeeded")
		}
	})

	t.Run("cancelling kills the whole group", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		pidFile := filepath.Join(t.TempDir(), "pid")
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = run(ctx, []string{}, sleep+` 60 & echo $! >`+pidFile+`; wait`)
		}()
		var pid int
		for range 200 {
			if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) != "" {
				pid = pidOf(t, string(b))
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if pid == 0 {
			t.Fatal("the tool never started")
		}
		cancel()
		<-done
		if !gone(pid) {
			t.Errorf("process %d survived the cancel", pid)
		}
	})
}
