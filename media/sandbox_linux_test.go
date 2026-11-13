//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"errors"
	"io"
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
func fakeProcessor(t *testing.T) *Processor { return fakeProcessorWith(t, Resources{}) }

func fakeProcessorWith(t *testing.T, r Resources) *Processor {
	t.Helper()
	r, err := r.resolve()
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &Processor{dir: t.TempDir(), tools: map[string]string{"tool": self}, cpuLimit: r.CPUTime}
	if err := os.Mkdir(p.tmpDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	p.start = sandboxRunner(rlimitsFor(r))
	p.run = p.runTool
	return p
}

func runFake(t *testing.T, p *Processor, args ...string) (runResult, error) {
	t.Helper()
	return p.run(context.Background(), nil, "tool", append([]string{"--tool"}, args...)...)
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

// toolLimits parses the limits the fake tool prints from /proc/self/limits.
func toolLimits(t *testing.T, p *Processor) map[string][2]uint64 {
	t.Helper()
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
	return seen
}

func TestSandboxAppliesEachLimitToTheTool(t *testing.T) {
	seen := toolLimits(t, fakeProcessor(t))
	for name, w := range map[string]uint64{
		"Max cpu time":      1200,
		"Max address space": 8 << 30,
		"Max file size":     5 << 30,
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

// The sandbox's CPU, address space and file size limits follow Resources, in
// the tool's runs and in the probe's report.
func TestSandboxLimitsFollowResources(t *testing.T) {
	t.Parallel()
	r := Resources{Memory: 1 << 30, Map: 1 << 30, Disk: 2 << 30, TimeLimit: 50 * time.Second, CPUTime: 100 * time.Second}
	want := map[string]uint64{
		"Max cpu time":      100,
		"Max address space": 7 << 30,
		"Max file size":     5 << 29,
	}
	seen := toolLimits(t, fakeProcessorWith(t, r))
	for name, w := range want {
		if got := seen[name]; got[0] != w || got[1] != w {
			t.Errorf("tool: %s = %v, want %d for both", name, got, w)
		}
	}

	limits := rlimitsFor(Resources{}.withDefaults())
	if got := rlimitsFor(r.withDefaults()); got == limits || got != (rlimits{CPU: 100, Address: 7 << 30, FileSize: 5 << 29}) {
		t.Errorf("rlimitsFor = %+v", got)
	}
	res, err := runHelper(context.Background(), helperConfig{Probe: true, Limits: rlimitsFor(r.withDefaults())}, io.Discard, io.Discard)
	if err != nil || res.Probe == nil {
		t.Fatalf("probe: %v", err)
	}
	for name, w := range map[string]uint64{limitCPU: 100, limitAddress: 7 << 30, limitFileSize: 5 << 29} {
		if got := res.Probe.Limits[name]; got[0] != w || got[1] != w {
			t.Errorf("probe: limit %s = %v, want %d", name, got, w)
		}
	}
	if err := probeSandbox(context.Background(), rlimitsFor(r.withDefaults())); err != nil {
		t.Errorf("probe under custom resources: %v", err)
	}
	// A probe that reads back the defaults does not satisfy custom limits.
	report := *res.Probe
	report.Limits = map[string][2]uint64{}
	for k, v := range res.Probe.Limits {
		report.Limits[k] = v
	}
	report.Limits[limitCPU] = [2]uint64{300, 300}
	if err := checkProbe(&report, rlimitsFor(r.withDefaults())); err == nil {
		t.Error("a CPU limit above the one asked for was accepted")
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
// ToolError: the first line, which names the cause, and the last. The error
// text carries none of it.
func TestSandboxErrorKeepsTheStartAndEndOfStandardError(t *testing.T) {
	p := fakeProcessor(t)
	res, err := runFake(t, p, "noise", "100000")
	if err == nil {
		t.Fatal("the tool failed but the run did not")
	}
	var te *ToolError
	if !errors.As(err, &te) || te.ExitCode != 4 || te.Signal != 0 || te.Tool != "tool" {
		t.Fatalf("err = %#v, want a *ToolError of tool tool with exit code 4", err)
	}
	for _, want := range []string{"FIRST-LINE", "LAST-LINE", "bytes omitted"} {
		if !strings.Contains(string(te.Stderr), want) {
			t.Errorf("ToolError.Stderr does not contain %q: %.300q", want, te.Stderr)
		}
	}
	if !strings.Contains(string(res.Stderr), "FIRST-LINE") || !strings.Contains(string(res.Stderr), "LAST-LINE") {
		t.Errorf("Result.Stderr does not hold both ends: %.300q", res.Stderr)
	}
	if strings.Contains(err.Error(), "FIRST-LINE") || len(err.Error()) > 100 {
		t.Errorf("the error text carries standard error: %.300q", err)
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
	var te *ToolError
	if !errors.As(err, &te) || te.ExitCode != 5 {
		t.Fatalf("err = %#v, want a *ToolError with exit code 5", err)
	}
	for _, want := range []string{"FIRST-LINE", "LAST-LINE", "bytes omitted"} {
		if !strings.Contains(string(te.Stderr), want) {
			t.Errorf("ToolError.Stderr does not contain %q: %.300q", want, te.Stderr)
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
		_, err := p.run(ctx, nil, "tool", "--tool", "spawn", "marker-cancel", ready, "hold")
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
	// small process that mostly waits. The upper bound on the resident set is
	// the direct run below, because the race detector's shadow memory
	// multiplies a tool built with it.
	if res.Usage.MaxRSS < mib<<20 {
		t.Errorf("MaxRSS = %d MiB, want at least %d", res.Usage.MaxRSS>>20, mib)
	}
	if res.Usage.CPU < busy*time.Millisecond || res.Usage.CPU > (busy+600)*time.Millisecond {
		t.Errorf("CPU = %v, want %dms to %dms", res.Usage.CPU, busy, busy+600)
	}

	self, _ := os.Executable()
	direct := exec.Command(self, "--tool", "alloc", strconv.Itoa(mib), strconv.Itoa(busy))
	if err := direct.Run(); err != nil {
		t.Fatal(err)
	}
	d := usageOf(direct.ProcessState)
	if diff := res.Usage.MaxRSS - d.MaxRSS; diff < -(20<<20) || diff > 20<<20 {
		t.Errorf("MaxRSS through the helper %d MiB, directly %d MiB", res.Usage.MaxRSS>>20, d.MaxRSS>>20)
	}
}

func TestSandboxedUsageExcludesTheCallingProgramsPeak(t *testing.T) {
	p := fakeProcessor(t)
	const held, toolMiB = 600, 2
	buf := make([]byte, held<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	res, err := runFake(t, p, "alloc", strconv.Itoa(toolMiB), "0")
	if err != nil {
		t.Fatal(err)
	}
	if buf[len(buf)-4096] != 1 {
		t.Fatal("buffer lost")
	}
	if res.Usage == nil {
		t.Fatal("no usage")
	}
	t.Logf("MaxRSS %d MiB with %d MiB held by the caller", res.Usage.MaxRSS>>20, held)
	if res.Usage.MaxRSS > 100<<20 {
		t.Errorf("MaxRSS = %d MiB for a %d MiB tool run by a program holding %d MiB", res.Usage.MaxRSS>>20, toolMiB, held)
	}
}

var defaultLimits = rlimitsFor(defaultResources())

func TestProbeRefusesWhenAGuardIsMissing(t *testing.T) {
	defer func() { probeFault = "" }()
	if err := probeSandbox(context.Background(), defaultLimits); err != nil {
		t.Fatalf("probe of an intact sandbox: %v", err)
	}
	faults := map[string]string{"filter": "socket is not denied"}
	for _, l := range defaultLimits.specs() {
		faults["limit:"+l.name] = "limit " + l.name + " reads back"
	}
	for fault, want := range faults {
		t.Run(fault, func(t *testing.T) {
			probeFault = fault
			err := probeSandbox(context.Background(), defaultLimits)
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
		for _, l := range defaultLimits.specs() {
			v := l.value
			if l.name == limitProcesses {
				v = 100 + processHeadroom
			}
			r.Limits[l.name] = [2]uint64{v, v}
		}
		return r
	}
	if err := checkProbe(good(), defaultLimits); err != nil {
		t.Fatalf("intact report: %v", err)
	}
	if err := checkProbe(nil, defaultLimits); err == nil {
		t.Error("nil report accepted")
	}
	for name, mutate := range map[string]func(*probeReport){
		"socket allowed":      func(r *probeReport) { r.SocketErrno = 0 },
		"socket other errno":  func(r *probeReport) { r.SocketErrno = int(syscall.EAFNOSUPPORT) },
		"socketpair allowed":  func(r *probeReport) { r.SocketpairErrno = 0 },
		"limit missing":       func(r *probeReport) { delete(r.Limits, limitCPU) },
		"soft unlimited":      func(r *probeReport) { r.Limits[limitAddress] = [2]uint64{^uint64(0), defaultLimits.Address} },
		"hard unlimited":      func(r *probeReport) { r.Limits[limitFileSize] = [2]uint64{defaultLimits.FileSize, ^uint64(0)} },
		"processes too high":  func(r *probeReport) { r.Limits[limitProcesses] = [2]uint64{1 << 20, 1 << 20} },
		"open files too high": func(r *probeReport) { r.Limits[limitOpenFiles] = [2]uint64{4096, 4096} },
	} {
		r := good()
		mutate(r)
		if err := checkProbe(r, defaultLimits); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r := good()
	r.Limits[limitCPU] = [2]uint64{1, 1}
	if err := checkProbe(r, defaultLimits); err != nil {
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
		_, err := p.composeFrames(ctx, nil, req, func(composedFrame) error { return nil })
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

func TestNewProcessorProbesUnderItsResources(t *testing.T) {
	t.Parallel()
	p := requireProcessorWith(t, Config{Resources: Resources{Memory: 256 << 20, Map: 512 << 20, TimeLimit: 100 * time.Second}})
	if _, err := p.run(context.Background(), nil, "magick", "-version"); err != nil {
		t.Fatal(err)
	}
}

// Unsandboxed starts no helper: the tool keeps the limits of its parent.
func TestUnsandboxedRunsToolsWithoutTheSandbox(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	var base runner
	p, err := newProcessor(context.Background(), Config{Unsandboxed: true, Dir: t.TempDir()}, func(next runner) runner {
		base = next
		return next
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	var out strings.Builder
	if _, err := base(context.Background(), runSpec{Tool: sh, Args: []string{"-c", `while read -r l; do case "$l" in "Max address space"*) echo "$l";; esac; done </proc/self/limits`}, Env: []string{}, Dir: t.TempDir()}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	own, err := os.ReadFile("/proc/self/limits")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines(own) {
		if strings.HasPrefix(l, "Max address space") && strings.TrimSpace(out.String()) != strings.TrimSpace(l) {
			t.Errorf("the tool's address-space limit %q is not its parent's %q", strings.TrimSpace(out.String()), l)
		}
	}
}

// A failure the helper reports itself says so, as one in starting the helper
// does.
func TestSandboxedFailuresNameTheHelper(t *testing.T) {
	t.Parallel()
	p := fakeProcessor(t)
	p.tools["missing"] = filepath.Join(t.TempDir(), "no-such-tool")
	_, err := p.run(context.Background(), nil, "missing")
	if err == nil || !strings.Contains(err.Error(), "sandbox helper: starting the tool") {
		t.Errorf("err = %v, want one that names the sandbox helper and the failed start", err)
	}
}

// A tool in the sandbox finds the input on descriptor 3 and holds no other
// descriptor of the helper's, and opening /dev/fd/3 gives it a read offset of
// its own: two opens each see the whole file though the descriptor sits
// mid-file.
func TestSandboxHandsTheToolTheInputOnDescriptorThree(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	f, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("0123456789"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	script := "read -r a <" + inputFDPath + "; read -r b <" + inputFDPath + "; echo $a $b; for f in /proc/self/fd/*; do echo ${f##*/}; done"
	spec := runSpec{Tool: sh, Args: []string{"-c", script}, Env: []string{}, Dir: t.TempDir(), Input: f}
	var errOut strings.Builder
	if _, err := sandboxRunner(rlimitsFor(Resources{}.withDefaults()))(context.Background(), spec, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(out.String())
	if len(fields) < 3 || fields[0] != "0123456789" || fields[1] != "0123456789" {
		t.Fatalf("the tool printed %q (stderr %q), want the whole file from each open, then its descriptors", out.String(), errOut.String())
	}
	// The listing itself holds one more descriptor, the directory it reads.
	var extra int
	for _, fd := range fields[2:] {
		if n, err := strconv.Atoi(fd); err != nil || n > 3 {
			extra++
		}
	}
	if extra > 1 {
		t.Errorf("the tool holds descriptors %v, want only 0 to 3", fields[2:])
	}
}

// A tool that reaches the sandbox's CPU limit is killed, and the error says it
// ran out of time: the signal and the measured CPU time together are the
// evidence. TimeLimit, which is wall-clock, is not the limit.
func TestToolKilledAtTheCPULimitIsErrTimeLimit(t *testing.T) {
	p := fakeProcessorWith(t, Resources{CPUTime: time.Second, TimeLimit: 300 * time.Second})
	res, err := runFake(t, p, "spin")
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
	var te *ToolError
	if !errors.As(err, &te) || (te.Signal != syscall.SIGKILL && te.Signal != syscall.SIGXCPU) || te.ExitCode != -1 {
		t.Errorf("err = %#v, want a *ToolError killed by SIGKILL or SIGXCPU", err)
	}
	if res.Usage == nil || res.Usage.CPU < time.Second-cpuSlack {
		t.Errorf("usage = %+v, want at least the 1 s limit", res.Usage)
	}
}

// A kill by the same signal that is not at the CPU limit, and an exit, are not
// time limits.
func TestOtherKillsAreNotErrTimeLimit(t *testing.T) {
	p := fakeProcessorWith(t, Resources{CPUTime: time.Second})
	for _, args := range [][]string{{"kill-self", strconv.Itoa(int(syscall.SIGKILL))}, {"exit", "1"}} {
		if _, err := runFake(t, p, args...); err == nil || errors.Is(err, ErrTimeLimit) {
			t.Errorf("%v: err = %v, want an error that is not ErrTimeLimit", args, err)
		}
	}
}

// A compositor that gives up on a frame nobody consumed ran out of time.
func TestCompositorThatTimesOutWaitingIsErrTimeLimit(t *testing.T) {
	p := fakeProcessor(t)
	err := composeWithScript(t, p, "echo 'frame 0 not consumed in 150 ms' >&2\nexit 1\n")
	if !errors.Is(err, ErrTimeLimit) {
		t.Errorf("err = %v, want ErrTimeLimit", err)
	}
	var te *ToolError
	if !errors.As(err, &te) || te.ExitCode != 1 {
		t.Errorf("err = %#v, want a *ToolError with exit code 1", err)
	}
	if err := composeWithScript(t, p, "echo 'something else' >&2\nexit 1\n"); errors.Is(err, ErrTimeLimit) {
		t.Errorf("another compositor failure is ErrTimeLimit: %v", err)
	}
}
