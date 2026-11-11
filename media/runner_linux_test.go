//go:build linux && (amd64 || arm64)

package media

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The kernel kills what the direct runner started when the program dies.
func TestDirectRunnerToolDiesWithTheProgram(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	if err := exec.Command(self, "--tool", "direct-parent", pidFile).Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("tool %d survived the death of the program that ran it", pid)
}

// A program that ignores SIGCHLD has its children reaped by the kernel; the
// direct runner reports that instead of dereferencing a missing exit status.
func TestDirectRunnerReportsALostExitStatus(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(self, "--tool", "ignore-sigchld").CombinedOutput()
	if err != nil {
		t.Fatalf("the program died: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "err=true code=0" {
		t.Errorf("output %q, want an error", got)
	}
}
