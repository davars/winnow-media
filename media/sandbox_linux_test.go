//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"errors"
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

// fakeProcessor returns a Processor whose only tool, "tool", is the test
// binary playing a media tool, run through the real sandbox helper.
func fakeProcessor(t *testing.T) *Processor {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &Processor{dir: t.TempDir(), tools: map[string]string{"tool": self}}
	if err := os.Mkdir(p.tmpDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	p.run = p.runTool
	return p
}

func runFake(t *testing.T, p *Processor, args ...string) (Result, error) {
	t.Helper()
	return p.run(context.Background(), "tool", append([]string{"--tool"}, args...)...)
}

func lines(out []byte) []string { return strings.Split(strings.TrimSpace(string(out)), "\n") }

func TestSandboxDeniesSocketCreation(t *testing.T) {
	p := fakeProcessor(t)
	res, err := runFake(t, p, "sockets")
	if err != nil {
		t.Fatal(err)
	}
	eperm := strconv.Itoa(int(syscall.EPERM))
	want := []string{"socket " + eperm, "socketpair " + eperm}
	if got := lines(res.Stdout); !slices.Equal(got, want) {
		t.Errorf("socket calls gave %q, want %q", got, want)
	}
}

func TestFilterCoversThreadsThatExistBeforeItIsInstalled(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(self, "--tool", "tsync").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(out)), "socket "+strconv.Itoa(int(syscall.EPERM)); got != want {
		t.Errorf("the earlier thread's socket call gave %q, want %q", got, want)
	}
}

func TestSandboxLeavesOtherSystemCallsAlone(t *testing.T) {
	p := fakeProcessor(t)
	res, err := runFake(t, p, "syscalls")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "ok" {
		t.Errorf("output %q, want ok", got)
	}
}

func TestSandboxAppliesEachLimitToTheTool(t *testing.T) {
	p := fakeProcessor(t)
	res, err := runFake(t, p, "limits")
	if err != nil {
		t.Fatal(err)
	}
	// Lines of /proc/self/limits: "Max cpu time  600  600  seconds".
	seen := map[string][2]uint64{}
	for _, l := range lines(res.Stdout)[1:] {
		for _, name := range []string{"Max cpu time", "Max address space", "Max file size", "Max open files", "Max processes"} {
			rest, ok := strings.CutPrefix(l, name)
			if !ok {
				continue
			}
			f := strings.Fields(rest)
			cur, err1 := strconv.ParseUint(f[0], 10, 64)
			hard, err2 := strconv.ParseUint(f[1], 10, 64)
			if err1 != nil || err2 != nil {
				cur, hard = 1<<63, 1<<63 // "unlimited"
			}
			seen[name] = [2]uint64{cur, hard}
		}
	}
	for name, w := range map[string]uint64{
		"Max cpu time":      sandboxCPUSeconds,
		"Max address space": sandboxAddressSize,
		"Max file size":     sandboxFileSize,
		"Max open files":    sandboxOpenFiles,
	} {
		if got := seen[name]; got[0] != w || got[1] != w {
			t.Errorf("%s = %v, want %d for both", name, got, w)
		}
	}
	base, err := userTasks(uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	got := seen["Max processes"]
	if got[0] != got[1] || got[0] == 0 || got[0] >= 1<<62 {
		t.Fatalf("Max processes = %v, want a finite limit", got)
	}
	// Tasks of the user may have changed since the helper counted them.
	if got[0] < processHeadroom || got[0] > base+processHeadroom+10000 {
		t.Errorf("Max processes = %d, not near the user's %d tasks plus %d", got[0], base, processHeadroom)
	}
}

func TestSandboxEnvironmentAndArgumentsAreExactlyTheTools(t *testing.T) {
	p := fakeProcessor(t)
	res, err := runFake(t, p, "environ", "one", "two words")
	if err != nil {
		t.Fatal(err)
	}
	// Each run has a directory of its own under the Processor's.
	var tmp string
	got := lines(res.Stdout)
	for i, l := range got {
		if v, ok := strings.CutPrefix(l, "env TMPDIR="); ok {
			tmp = v
			got = slices.Delete(got, i, i+1)
			break
		}
	}
	if filepath.Dir(tmp) != p.tmpDir() || !strings.HasPrefix(filepath.Base(tmp), "run-") {
		t.Errorf("TMPDIR = %q, want a run directory under %s", tmp, p.tmpDir())
	}
	want := []string{
		"env MAGICK_CONFIGURE_PATH=" + p.dir,
		"env MAGICK_TEMPORARY_PATH=" + tmp,
		"env LC_ALL=C",
		"arg --tool", "arg environ", "arg one", "arg two words",
	}
	if slices.Sort(got); !slices.Equal(got, sortedCopy(want)) {
		t.Errorf("tool saw\n%q\nwant\n%q", got, sortedCopy(want))
	}
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

func TestSandboxReportsHowTheToolEnded(t *testing.T) {
	p := fakeProcessor(t)
	if _, err := runFake(t, p, "exit", "0"); err != nil {
		t.Errorf("exit 0: %v", err)
	}
	if _, err := runFake(t, p, "exit", "3"); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("exit 3: err = %v, want exit status 3", err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGKILL, syscall.SIGHUP, syscall.SIGTERM} {
		_, err := runFake(t, p, "kill-self", strconv.Itoa(int(sig)))
		if err == nil || !strings.Contains(err.Error(), "killed by signal: "+sig.String()) {
			t.Errorf("signal %d: err = %v, want killed by signal %s", sig, err, sig)
		}
	}
}

// A tool that fails after a flood on standard error has both ends of it in the
// error: the first line, which names the cause, and the last.
func TestSandboxErrorKeepsTheStartAndEndOfStandardError(t *testing.T) {
	p := fakeProcessor(t)
	res, err := runFake(t, p, "noise", "100000")
	if err == nil {
		t.Fatal("the tool failed but the run did not")
	}
	for _, want := range []string{"exit status 4", "FIRST-LINE", "LAST-LINE", "bytes omitted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err does not contain %q: %.300q", want, err)
		}
	}
	if !strings.Contains(string(res.Stderr), "FIRST-LINE") || !strings.Contains(string(res.Stderr), "LAST-LINE") {
		t.Errorf("Result.Stderr does not hold both ends: %.300q", res.Stderr)
	}
	if limit := stderrHead + stderrTail + 200; len(err.Error()) > limit {
		t.Errorf("the error is %d bytes, more than %d", len(err.Error()), limit)
	}
}

// A compositor that fails after a flood on standard error has both ends of it
// in the error, as a tool has.
func TestSandboxCompositorErrorKeepsTheStartAndEndOfStandardError(t *testing.T) {
	p := fakeProcessor(t)
	err := composeWithScript(t, p, "echo FIRST-LINE >&2\ni=0\nwhile [ $i -lt 400 ]; do echo xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx >&2; i=$((i+1)); done\necho LAST-LINE >&2\nexit 5\n")
	if err == nil {
		t.Fatal("the compositor failed but the run did not")
	}
	for _, want := range []string{"exit status 5", "FIRST-LINE", "LAST-LINE", "bytes omitted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err does not contain %q: %.300q", want, err)
		}
	}
}

func readPids(t *testing.T, path string) []int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// requireGone fails unless no process, not even a zombie, has the pid.
func requireGone(t *testing.T, what string, pid int) {
	t.Helper()
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
		t.Errorf("%s (pid %d) still exists", what, pid)
	}
}

func TestSandboxKillsAndReapsWhatTheToolLeavesRunning(t *testing.T) {
	p := fakeProcessor(t)
	ready := filepath.Join(t.TempDir(), "ready")
	if _, err := runFake(t, p, "spawn", "marker-leftover", ready); err != nil {
		t.Fatal(err)
	}
	pids := readPids(t, ready)
	requireGone(t, "grandchild", pids[0])
}

// Pdeathsig covers the tool only: its own children are reparented when it
// dies and survive if the helper could not clean up after it.
func TestToolDiesWithTheHelper(t *testing.T) {
	p := fakeProcessor(t)
	pidfile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	if _, err := runFake(t, p, "kill-parent", pidfile); err == nil {
		t.Error("a run whose helper was killed succeeded")
	}
	// The tool holds the output pipe, so the run returns only once it is gone.
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the run took %v: the tool outlived its helper", d)
	}
	pid := readPids(t, pidfile)[0]
	deadline := time.Now().Add(5 * time.Second)
	for {
		// A killed process nobody has reaped yet is a zombie.
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil || strings.Contains(string(b), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tool (pid %d) outlived its helper", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCancellingTheContextStopsTheToolAndItsDescendants(t *testing.T) {
	p := fakeProcessor(t)
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.run(ctx, "tool", "--tool", "spawn", "marker-cancel", ready, "hold")
		done <- err
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the tool never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pids := readPids(t, ready)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not return after cancellation")
	}
	requireGone(t, "grandchild", pids[0])
	requireGone(t, "tool", pids[1])
}

func TestSandboxedRunReportsTheToolsUsage(t *testing.T) {
	p := fakeProcessor(t)
	const mib, busy = 300, 400
	res, err := runFake(t, p, "alloc", strconv.Itoa(mib), strconv.Itoa(busy))
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage == nil {
		t.Fatal("no usage")
	}
	// The tool's own resident set and CPU, not the helper's, which is a
	// small process that mostly waits.
	if res.Usage.MaxRSS < mib<<20 || res.Usage.MaxRSS > (mib+100)<<20 {
		t.Errorf("MaxRSS = %d MiB, want %d to %d", res.Usage.MaxRSS>>20, mib, mib+100)
	}
	if res.Usage.CPU < busy*time.Millisecond || res.Usage.CPU > (busy+600)*time.Millisecond {
		t.Errorf("CPU = %v, want %dms to %dms", res.Usage.CPU, busy, busy+600)
	}

	self, _ := os.Executable()
	direct := exec.Command(self, "--tool", "alloc", strconv.Itoa(mib), strconv.Itoa(busy))
	if err := direct.Run(); err != nil {
		t.Fatal(err)
	}
	d := UsageOf(direct.ProcessState)
	if diff := res.Usage.MaxRSS - d.MaxRSS; diff < -(20<<20) || diff > 20<<20 {
		t.Errorf("MaxRSS through the helper %d MiB, directly %d MiB", res.Usage.MaxRSS>>20, d.MaxRSS>>20)
	}
}

func TestNewProcessorRequiresInit(t *testing.T) {
	initCalled.Store(false)
	defer initCalled.Store(true)
	start := time.Now()
	_, err := NewProcessor(context.Background(), HighMemory)
	if err == nil || !strings.Contains(err.Error(), "media.Init") {
		t.Fatalf("err = %v, want one that says to call media.Init", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("refusal took %v", time.Since(start))
	}
}

func TestProbeRefusesWhenAGuardIsMissing(t *testing.T) {
	defer func() { probeFault = "" }()
	if err := probeSandbox(context.Background()); err != nil {
		t.Fatalf("probe of an intact sandbox: %v", err)
	}
	faults := map[string]string{"filter": "socket is not denied"}
	for _, l := range sandboxLimits() {
		faults["limit:"+l.name] = "limit " + l.name + " reads back"
	}
	for fault, want := range faults {
		t.Run(fault, func(t *testing.T) {
			probeFault = fault
			err := probeSandbox(context.Background())
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want one containing %q", err, want)
			}
		})
	}
}

func TestCheckProbe(t *testing.T) {
	good := func() *probeReport {
		r := &probeReport{
			SocketErrno:     int(syscall.EPERM),
			SocketpairErrno: int(syscall.EPERM),
			Limits:          map[string][2]uint64{},
			ProcessBase:     100,
		}
		for _, l := range sandboxLimits() {
			v := l.value
			if l.name == limitProcesses {
				v = 100 + processHeadroom
			}
			r.Limits[l.name] = [2]uint64{v, v}
		}
		return r
	}
	if err := checkProbe(good()); err != nil {
		t.Fatalf("intact report: %v", err)
	}
	if err := checkProbe(nil); err == nil {
		t.Error("nil report accepted")
	}
	for name, mutate := range map[string]func(*probeReport){
		"socket allowed":      func(r *probeReport) { r.SocketErrno = 0 },
		"socket other errno":  func(r *probeReport) { r.SocketErrno = int(syscall.EAFNOSUPPORT) },
		"socketpair allowed":  func(r *probeReport) { r.SocketpairErrno = 0 },
		"limit missing":       func(r *probeReport) { delete(r.Limits, limitCPU) },
		"soft unlimited":      func(r *probeReport) { r.Limits[limitAddress] = [2]uint64{^uint64(0), sandboxAddressSize} },
		"hard unlimited":      func(r *probeReport) { r.Limits[limitFileSize] = [2]uint64{sandboxFileSize, ^uint64(0)} },
		"processes too high":  func(r *probeReport) { r.Limits[limitProcesses] = [2]uint64{1 << 20, 1 << 20} },
		"open files too high": func(r *probeReport) { r.Limits[limitOpenFiles] = [2]uint64{4096, 4096} },
	} {
		r := good()
		mutate(r)
		if err := checkProbe(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r := good()
	r.Limits[limitCPU] = [2]uint64{1, 1}
	if err := checkProbe(r); err != nil {
		t.Errorf("a stricter inherited limit refused: %v", err)
	}
}

// runFilter interprets the filter program for one system call.
func runFilter(prog []sockFilter, arch, nr uint32) uint32 {
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.code {
		case bpfLdAbs:
			acc = map[uint32]uint32{0: nr, 4: arch}[in.k]
		case bpfJeq:
			if acc == in.k {
				pc += int(in.jt)
			} else {
				pc += int(in.jf)
			}
		case bpfJset:
			if acc&in.k != 0 {
				pc += int(in.jt)
			} else {
				pc += int(in.jf)
			}
		case bpfRet:
			return in.k
		}
	}
	panic("program ends without a verdict")
}

func TestFilterProgram(t *testing.T) {
	prog := filterProgram()
	deny := uint32(retErrno | uint32(syscall.EPERM))
	for _, c := range []struct {
		name    string
		arch    uint32
		nr      uint32
		verdict uint32
	}{
		{"socket", auditArch, sysSocket, deny},
		{"socketpair", auditArch, sysSocketpair, deny},
		{"read", auditArch, 0, retAllow},
		{"execve", auditArch, uint32(syscall.SYS_EXECVE), retAllow},
		{"another architecture", 0x40000003, sysSocket, retKill},
		{"another architecture, other call", 0x40000003, 1, retKill},
	} {
		if got := runFilter(prog, c.arch, c.nr); got != c.verdict {
			t.Errorf("%s: verdict %#x, want %#x", c.name, got, c.verdict)
		}
	}
	if rejectX32 {
		for _, nr := range []uint32{sysSocket, 0, 1} {
			if got := runFilter(prog, auditArch, nr|x32Bit); got != retKill {
				t.Errorf("x32 call %d: verdict %#x, want kill", nr, got)
			}
		}
	}
}

// The status pipe and the lifeline are the helper's protocol with the
// Processor. The tool must see only its standard descriptors: a tool that
// inherits the status pipe can forge the helper's report, and a descendant
// that outlives the helper keeps the Processor blocked reading it.
func TestToolSeesOnlyItsStandardDescriptors(t *testing.T) {
	p := fakeProcessor(t)
	res, err := runFake(t, p, "fds")
	if err != nil {
		t.Fatal(err)
	}
	var extra []string
	for _, l := range lines(res.Stdout) {
		// "fd N -> target". The fake tool is a Go program: its runtime opens
		// its cgroup's limits, on the lowest free descriptor, 3; and the directory it lists is closed again
		// before its link can be read.
		f := strings.Fields(l)
		n, err := strconv.Atoi(f[1])
		if err != nil {
			t.Fatalf("unexpected line %q", l)
		}
		ownCgroup := n == 3 && strings.Contains(l, "/sys/fs/cgroup/")
		if n > 2 && !ownCgroup && !strings.Contains(l, "no such file") {
			extra = append(extra, l)
		}
	}
	if len(extra) > 0 {
		t.Errorf("the tool inherited descriptors beyond stdio: %q", extra)
	}
}

// composeWithScript runs composeFrames with a shell script as the external
// compositor. The script's process is killed when the test ends, and the
// context is bound to a timeout.
func composeWithScript(t *testing.T, p *Processor, body string) error {
	t.Helper()
	script := filepath.Join(t.TempDir(), "compositor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	p.tools["compositor"] = script
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	req := composeRequest{external: "compositor", dir: t.TempDir(), path: "x", selected: []int{0}, frames: 1, width: 1, height: 1, inFlight: 1}
	done := make(chan error, 1)
	go func() {
		_, err := p.composeFrames(ctx, req, func(composedFrame) error { return nil })
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("composeFrames did not return within 5s")
		return nil
	}
}

// A compositor whose report the Processor rejects is stopped at once. Once the
// Processor stops reading, nothing consumes the compositor's frames, so one
// left running waits out its consume timeout with the operation blocked on it.
func TestComposeStopsACompositorWhoseReportItRejects(t *testing.T) {
	p := fakeProcessor(t)
	// The sleep stands in for a compositor waiting for its frames to be
	// consumed.
	err := composeWithScript(t, p, "echo garbage\nexec /bin/sleep 20\n")
	if err == nil || !strings.Contains(err.Error(), "compositor reported") {
		t.Errorf("err = %v, want the rejected report", err)
	}
	if got := dirNames(t, p.tmpDir()); len(got) != 0 {
		t.Errorf("the Processor's temporary directory holds %v after the run", got)
	}
}

// An overlong line is the compositor's fault, not the caller's cancellation.
func TestComposeNamesAnOverlongCompositorLine(t *testing.T) {
	p := fakeProcessor(t)
	err := composeWithScript(t, p, "printf '%0100d' 0\nexec /bin/sleep 20\n")
	if err == nil || !strings.Contains(err.Error(), "overlong line") || errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the overlong line and not a cancellation", err)
	}
}

// A compositor that succeeds leaves nothing in the Processor's temporary
// directory either.
func TestComposeRemovesItsRunDirectoryOnSuccess(t *testing.T) {
	p := fakeProcessor(t)
	if err := composeWithScript(t, p, "exit 0\n"); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, p.tmpDir()); len(got) != 0 {
		t.Errorf("the Processor's temporary directory holds %v after the run", got)
	}
}
