//go:build linux && (amd64 || arm64)

package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// The sandbox's fixed limits. Each sits above the largest value measured for
// any tool run within the Processor's caps (internal/cmd/measure on a 12-CPU
// x86_64 host, with per-process peaks sampled from /proc; see
// docs/measurements.md), so a tool within the caps never meets one; they
// bound a tool that escapes them. The CPU, address space and file size limits
// derive from Resources (rlimitsFor).
const (
	limitCPU       = "cpu"
	limitAddress   = "as"
	limitFileSize  = "fsize"
	limitOpenFiles = "nofile"
	limitProcesses = "nproc"

	// sandboxOpenFiles is a multiple of the most descriptors measured.
	sandboxOpenFiles = 1024
	// processHeadroom is how many tasks beyond the user's current number a
	// run may create, above the most measured in one tool. The limit counts
	// every task of the real user, so it bounds forks relative to the moment
	// the helper starts, and is a bound only where the user has nothing else
	// running: a container with a dedicated uid. It is not enforced for root
	// or CAP_SYS_RESOURCE.
	processHeadroom = 512
)

// inputHelperFD is the descriptor the helper receives the input on: after the
// status and lifeline descriptors.
const inputHelperFD = 5

// rlimitNproc is RLIMIT_NPROC, which package syscall does not name.
const rlimitNproc = 6

type limitSpec struct {
	name     string
	resource int
	value    uint64
}

// backstopCPU is the kernel's RLIMIT_CPU: the budget, which the helper
// enforces itself, plus a tenth of it or one second, whichever is more. The
// kernel checks the limit against tick-sampled CPU time, which runs ahead of
// the run time by a share of it under contention, and the helper only polls
// every cpuPollInterval; the margin keeps the kernel from firing first in
// ordinary runs, and is sized from the gap measured between the two clocks
// (docs/measurements.md). A kernel kill is attributed by its own comparison
// (see helperRun), so the margin is not what makes the attribution right. A
// zero budget stays zero.
func (l rlimits) backstopCPU() uint64 {
	if l.CPU == 0 {
		return 0
	}
	return saturatingAdd(l.CPU, max(1, (l.CPU+9)/10))
}

// specs lists the limits except the process limit's value, which depends on
// the user's current tasks.
func (l rlimits) specs() []limitSpec {
	return []limitSpec{
		{limitCPU, syscall.RLIMIT_CPU, l.backstopCPU()},
		{limitAddress, syscall.RLIMIT_AS, l.Address},
		{limitFileSize, syscall.RLIMIT_FSIZE, l.FileSize},
		{limitOpenFiles, syscall.RLIMIT_NOFILE, sandboxOpenFiles},
		{limitProcesses, rlimitNproc, 0},
	}
}

// helperConfig is what the Processor tells the helper to do.
type helperConfig struct {
	// Probe asks for the sandbox to be applied and reported on instead of a
	// tool to be run.
	Probe bool
	Tool  string
	Args  []string
	Env   []string
	Dir   string
	// Input is the file the tool inherits as descriptor 3. It travels
	// as the helper's descriptor inputHelperFD, not in the JSON.
	Input *os.File `json:"-"`
	// HasInput tells the helper that descriptor inputHelperFD is the input.
	HasInput bool
	// Limits are the resource limits the helper applies to itself.
	Limits rlimits
	// Fault makes the helper skip one guard so that tests can check that the
	// Processor notices: "filter", "limit:<name>" or "cpu-watch", which turns off
	// the helper's own CPU check so that only the kernel's limit ends a run.
	Fault string
}

// helperResult is what the helper reports on its status descriptor.
type helperResult struct {
	// Err is set when the helper itself failed: it could not apply the
	// sandbox, start the tool or clear the tool's descendants.
	Err string `json:",omitempty"`
	// Signal is the signal that killed the tool, or 0.
	Signal int
	// Code is the tool's exit code when Signal is 0.
	Code int
	// Usage is what the tool and its descendants consumed, measured by the
	// helper: the helper's own rusage cannot be used, because a child
	// started with CLONE_VM starts with the parent program's peak resident
	// set.
	Usage *Usage `json:",omitempty"`
	// CPULimit is set when the tool was killed at its CPU limit: by the helper
	// at the budget, or by the kernel at the backstop behind it.
	CPULimit bool         `json:",omitempty"`
	Probe    *probeReport `json:",omitempty"`
}

// probeReport is what the helper observed about its own sandbox.
type probeReport struct {
	// SocketErrno and SocketpairErrno are the errors of an AF_UNIX socket(2)
	// and socketpair(2) call, and IoUringSetupErrno that of io_uring_setup(2),
	// which can create a socket without either; 0 means the call succeeded.
	SocketErrno       int
	SocketpairErrno   int
	IoUringSetupErrno int
	// Limits maps a limit's name to its soft and hard values read back.
	Limits map[string][2]uint64
	// ProcessBase is the number of tasks of the user when the process limit
	// was computed.
	ProcessBase uint64
}

// checkProbe returns an error unless r shows the socket and io_uring calls
// denied and every limit set, no higher than limits (and the fixed ones).
func checkProbe(r *probeReport, limits rlimits) error {
	if r == nil {
		return errors.New("the helper did not report")
	}
	for _, c := range []struct {
		name  string
		errno int
	}{{"socket", r.SocketErrno}, {"socketpair", r.SocketpairErrno}, {"io_uring_setup", r.IoUringSetupErrno}} {
		if syscall.Errno(c.errno) != syscall.EPERM {
			return fmt.Errorf("%s is not denied: errno %d, want %d", c.name, c.errno, int(syscall.EPERM))
		}
	}
	for _, l := range limits.specs() {
		got, ok := r.Limits[l.name]
		if !ok {
			return fmt.Errorf("limit %s not reported", l.name)
		}
		want := l.value
		if l.name == limitProcesses {
			want = r.ProcessBase + processHeadroom
		}
		for _, v := range got {
			if v > want {
				return fmt.Errorf("limit %s reads back as %d and %d, want at most %d", l.name, got[0], got[1], want)
			}
		}
	}
	return nil
}

func sandboxAvailable() error { return nil }

// probeFault is passed to the probe helper as its Fault; tests set it.
var probeFault string

// probeSandbox starts the helper in probe mode and refuses unless the
// sandbox it applied to itself is the one the Processor asks for.
func probeSandbox(ctx context.Context, limits rlimits) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var stderr headTailBuffer
	res, err := runHelper(ctx, helperConfig{Probe: true, Limits: limits, Fault: probeFault}, io.Discard, &stderr)
	if err != nil {
		return fmt.Errorf("sandbox probe: %w: %s", err, bytes.TrimSpace(stderr.bytes()))
	}
	if res.Err != "" {
		return fmt.Errorf("sandbox probe: %s", res.Err)
	}
	if err := checkProbe(res.Probe, limits); err != nil {
		return fmt.Errorf("sandbox probe: %w", err)
	}
	return nil
}

// runHelper starts this executable as the sandbox helper with cfg and waits
// for it. The helper's stdout and stderr go to the given writers. It returns
// the helper's report, whose Usage covers the tool; the error is non-nil
// when the helper did not deliver a report. Cancelling ctx closes the
// helper's lifeline, on which it kills the tool and every descendant.
func runHelper(ctx context.Context, cfg helperConfig, stdout, stderr io.Writer) (helperResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return helperResult{}, err
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return helperResult{}, err
	}
	defer func() { _ = statusR.Close() }()
	lifeR, lifeW, err := os.Pipe()
	if err != nil {
		_ = statusW.Close()
		return helperResult{}, err
	}
	defer func() { _ = lifeW.Close() }()

	cmd := exec.CommandContext(ctx, exe)
	cmd.Args = []string{"winnow-media-sandbox"}
	cmd.Env = []string{helperEnv + "=1"}
	cmd.ExtraFiles = []*os.File{statusW, lifeR}
	if cfg.Input != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, cfg.Input)
		cfg.HasInput = true
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Cancel = func() error { return lifeW.Close() }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Start()
	_ = statusW.Close()
	_ = lifeR.Close()
	if err != nil {
		return helperResult{}, err
	}
	// A write error means the helper is already gone; Wait reports why.
	_ = json.NewEncoder(lifeW).Encode(cfg)
	waitErr := cmd.Wait()
	// The read needs no timer: the helper is reaped, no child of it holds the
	// descriptor (it is close-on-exec) and the report is one write under
	// PIPE_BUF.
	var res helperResult
	if decErr := json.NewDecoder(statusR).Decode(&res); decErr != nil {
		if waitErr != nil {
			return res, waitErr
		}
		return res, fmt.Errorf("the helper reported nothing: %w", decErr)
	}
	return res, nil
}

// sandboxRunner returns the runner that runs each process through the
// sandbox helper under limits. Its Usage is what the helper measured for the
// tool and its descendants.
func sandboxRunner(limits rlimits) runner {
	return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
		res, err := runHelper(ctx, helperConfig{
			Tool: spec.Tool, Args: spec.Args, Env: spec.Env, Dir: spec.Dir, Input: spec.Input, Limits: limits,
		}, stdout, stderr)
		if err != nil {
			err = fmt.Errorf("sandbox helper: %w", err)
		}
		out := res.toolResult()
		if out.Err != "" {
			out.Err = "sandbox helper: " + out.Err
		}
		return out, err
	}
}

func (r helperResult) toolResult() toolResult {
	return toolResult{Err: r.Err, Signal: r.Signal, Code: r.Code, Usage: r.Usage, CPULimit: r.CPULimit}
}

// helperMain is the sandbox helper: it applies the sandbox to itself, runs
// the tool as its child and then removes every process left under it. It
// reads its configuration from descriptor 4, writes its report to
// descriptor 3 and does not return. The tool inherits descriptors 0, 1 and 2,
// and the input, when there is one, as descriptor 3.
func helperMain() {
	// Pdeathsig fires when the thread that started the tool exits, so the
	// thread must live as long as the helper.
	runtime.LockOSThread()
	status, life := os.NewFile(3, "status"), os.NewFile(4, "lifeline")
	var cfg helperConfig
	var res helperResult
	if err := closeOnExecFrom(3); err != nil {
		res.Err = fmt.Sprintf("closing inherited descriptors: %v", err)
	} else if err := json.NewDecoder(life).Decode(&cfg); err != nil {
		res.Err = fmt.Sprintf("reading configuration: %v", err)
	} else {
		res = helperRun(cfg, life)
	}
	_ = json.NewEncoder(status).Encode(res)
	if res.Err != "" {
		os.Exit(1)
	}
	os.Exit(0)
}

// closeOnExecFrom marks every open descriptor from first up close-on-exec, so
// that a process the helper starts does not inherit them. It needs Linux 5.11.
func closeOnExecFrom(first uintptr) error {
	const (
		sysCloseRange        = 436 // the same number on amd64 and arm64
		closeRangeCloexec    = 4
		allDescriptorsFromTo = ^uintptr(0)
	)
	if _, _, e := syscall.RawSyscall(sysCloseRange, first, allDescriptorsFromTo, closeRangeCloexec); e != 0 {
		return fmt.Errorf("the sandbox needs Linux 5.11 or later (close_range): %w", e)
	}
	return nil
}

func helperRun(cfg helperConfig, life *os.File) helperResult {
	if err := setSubreaper(); err != nil {
		return helperResult{Err: err.Error()}
	}
	base, err := applySandbox(cfg.Fault, cfg.Limits)
	if err != nil {
		return helperResult{Err: err.Error()}
	}
	if cfg.Probe {
		return helperResult{Probe: readProbe(base, cfg.Limits)}
	}

	// The parent closing its end of the lifeline, or dying, is the signal to
	// stop: it is how cancelling a context reaches the tool.
	var cancelled atomic.Bool
	go func() {
		_, _ = io.Copy(io.Discard, life)
		cancelled.Store(true)
		killDescendants()
	}()

	cmd := exec.Command(cfg.Tool, cfg.Args...)
	cmd.Env, cmd.Dir = cfg.Env, cfg.Dir
	if cfg.HasInput {
		cmd.ExtraFiles = []*os.File{os.NewFile(inputHelperFD, "input")}
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return helperResult{Err: fmt.Sprintf("starting the tool: %v", err)}
	}
	if cancelled.Load() {
		killDescendants()
	}
	stopWatch := make(chan struct{})
	watched := make(chan struct{})
	var cpuLimit atomic.Bool
	if budget := time.Duration(cfg.Limits.CPU) * time.Second; budget > 0 && cfg.Fault != "cpu-watch" {
		go func() {
			defer close(watched)
			if watchCPU(cmd.Process.Pid, budget, stopWatch) {
				cpuLimit.Store(true)
			}
		}()
	} else {
		close(watched)
	}
	// The watcher names the tool by pid, so it must stop before the tool is
	// reaped: an unreaped tool keeps its pid, a reaped one can lend it to an
	// unrelated process.
	awaitErr := awaitExit(cmd.Process.Pid)
	close(stopWatch)
	<-watched
	// The unreaped tool's clock still reads; a kill once it passed the kernel
	// limit is that limit firing, judged on the kernel's own clock.
	zombieCPU, zombieErr := processCPUTime(cmd.Process.Pid)
	waitErr := cmd.Wait()
	var res helperResult
	res.CPULimit = cpuLimit.Load()
	if awaitErr != nil {
		res.Err = fmt.Sprintf("waiting for the tool: %v", awaitErr)
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) && res.Err == "" {
		res.Err = fmt.Sprintf("waiting for the tool: %v", waitErr)
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		res.Signal = int(ws.Signal())
		if backstop := cfg.Limits.backstopCPU(); ws.Signal() == syscall.SIGKILL && backstop > 0 && zombieErr == nil &&
			zombieCPU >= time.Duration(backstop)*time.Second {
			res.CPULimit = true
		}
	} else {
		res.Code = cmd.ProcessState.ExitCode()
	}
	usage := usageOf(cmd.ProcessState)
	orphans, err := clearDescendants()
	if err != nil && res.Err == "" {
		res.Err = err.Error()
	}
	if usage != nil {
		usage.CPU += orphans.CPU
		usage.MaxRSS = max(usage.MaxRSS, orphans.MaxRSS)
	}
	res.Usage = usage
	return res
}

// awaitExit blocks until process pid has exited, without reaping it (waitid
// with WNOWAIT), and returns the error that stopped it early, if any. EINTR
// is retried.
func awaitExit(pid int) error {
	const (
		pPID    = 1
		wExited = 4
		wNoWait = 0x1000000
	)
	var info [128]byte // siginfo_t
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), wExited|wNoWait, 0, 0)
		switch e {
		case 0:
			return nil
		case syscall.EINTR:
		default:
			return fmt.Errorf("waitid: %w", e)
		}
	}
}

// cpuPollInterval is how often the helper compares the tool's CPU time with
// the budget.
const cpuPollInterval = 50 * time.Millisecond

// watchCPU kills the tool and everything below the helper once the tool's
// process-wide CPU time reaches budget, and reports whether it did. It
// returns, reporting false, when stop closes or the tool's clock can no longer
// be read. The budget is checked on the clock the kernel's own limit uses, and
// sits below that limit, so the kernel fires first only when the process runs
// a whole poll interval past the budget between checks.
func watchCPU(pid int, budget time.Duration, stop <-chan struct{}) bool {
	ticker := time.NewTicker(cpuPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return false
		case <-ticker.C:
		}
		used, err := processCPUTime(pid)
		if err != nil {
			return false
		}
		if used >= budget {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			killDescendants()
			return true
		}
	}
}

// cpuclockProf is the kind in the low bits of a process CPU clock id that the
// kernel checks RLIMIT_CPU against: tick-sampled user plus system time. It can
// run ahead of the precise run time that rusage reports.
const cpuclockProf = 0

// processCPUTime returns the CPU time of every thread of process pid on the
// kernel's RLIMIT_CPU clock, read from the clock clock_getcpuclockid(3)
// returns for it: the encoding is (^pid << 3) | cpuclockProf, process-wide. It
// also reads a process that has exited and is not yet reaped. Reading a clock
// needs no permission beyond naming the process, and clock_gettime is not
// filtered.
func processCPUTime(pid int) (time.Duration, error) {
	clock := int32(^int32(pid)<<3 | cpuclockProf)
	var ts syscall.Timespec
	if _, _, e := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, uintptr(clock), uintptr(unsafe.Pointer(&ts)), 0); e != 0 {
		return 0, e
	}
	return time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec), nil
}

func setSubreaper() error {
	const prSetChildSubreaper = 36
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return fmt.Errorf("becoming child subreaper: %w", e)
	}
	return nil
}

// applySandbox sets no_new_privs, the limits and the seccomp filter on the
// calling process and returns the process base the process limit used. fault
// names a guard to skip, for tests.
func applySandbox(fault string, limits rlimits) (uint64, error) {
	const prSetNoNewPrivs = 38
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
		return 0, fmt.Errorf("setting no_new_privs: %w", e)
	}
	base, err := userTasks(uint32(os.Getuid()))
	if err != nil {
		return 0, fmt.Errorf("counting the user's tasks: %w", err)
	}
	for _, l := range limits.specs() {
		if fault == "limit:"+l.name {
			continue
		}
		value := l.value
		if l.name == limitProcesses {
			value = base + processHeadroom
		}
		var cur syscall.Rlimit
		if err := syscall.Getrlimit(l.resource, &cur); err != nil {
			return 0, fmt.Errorf("reading limit %s: %w", l.name, err)
		}
		// An existing hard limit below ours stays: a limit cannot be raised.
		value = min(value, cur.Max)
		if err := syscall.Setrlimit(l.resource, &syscall.Rlimit{Cur: value, Max: value}); err != nil {
			return 0, fmt.Errorf("setting limit %s: %w", l.name, err)
		}
	}
	if fault != "filter" {
		if err := installFilter(); err != nil {
			return 0, err
		}
	}
	return base, nil
}

func readProbe(base uint64, limits rlimits) *probeReport {
	r := &probeReport{Limits: map[string][2]uint64{}, ProcessBase: base}
	if fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0); err != nil {
		r.SocketErrno = int(err.(syscall.Errno))
	} else {
		_ = syscall.Close(fd)
	}
	if fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0); err != nil {
		r.SocketpairErrno = int(err.(syscall.Errno))
	} else {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
	}
	// A null parameter block: a call that is allowed fails with EFAULT or
	// ENOSYS, or with EPERM when io_uring is disabled by sysctl or a container
	// profile, where it is unusable anyway; one denied by the filter never
	// reaches the kernel.
	if _, _, e := syscall.RawSyscall(sysIoUringSetup, 1, 0, 0); e != 0 {
		r.IoUringSetupErrno = int(e)
	}
	for _, l := range limits.specs() {
		var rl syscall.Rlimit
		if err := syscall.Getrlimit(l.resource, &rl); err == nil {
			r.Limits[l.name] = [2]uint64{rl.Cur, rl.Max}
		}
	}
	return r
}

type sockFilter struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

type sockFprog struct {
	length uint16
	_      [6]byte
	filter *sockFilter
}

// installFilter denies socket, socketpair and the io_uring system calls with
// EPERM and allows every other system call of the native ABI; any other ABI is
// killed, so that neither 32-bit socketcall nor x32 reaches the sockets. Filters apply to the
// calling thread, and the helper may start the tool from any thread, so the
// filter is synchronised to all threads.
func installFilter() error {
	const (
		setModeFilter = 1
		tsync         = 1
	)
	prog := filterProgram()
	fprog := sockFprog{length: uint16(len(prog)), filter: &prog[0]}
	if _, _, e := syscall.Syscall(sysSeccomp, setModeFilter, tsync, uintptr(unsafe.Pointer(&fprog))); e != 0 {
		return fmt.Errorf("installing the seccomp filter: %w", e)
	}
	runtime.KeepAlive(prog)
	return nil
}

const (
	bpfLdAbs = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeq   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJset  = 0x45 // BPF_JMP | BPF_JSET | BPF_K
	bpfRet   = 0x06 // BPF_RET | BPF_K

	retKill  = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	retAllow = 0x7fff0000
	retErrno = 0x00050000
)

// filterProgram is the classic BPF program installFilter loads.
func filterProgram() []sockFilter {
	prog := []sockFilter{
		{code: bpfLdAbs, k: 4}, // seccomp_data.arch
		{code: bpfJeq, jt: 1, k: auditArch},
		{code: bpfRet, k: retKill},
		{code: bpfLdAbs, k: 0}, // seccomp_data.nr
	}
	if rejectX32 {
		prog = append(prog, sockFilter{code: bpfJset, jf: 1, k: x32Bit}, sockFilter{code: bpfRet, k: retKill})
	}
	for i, nr := range deniedSyscalls {
		// A match skips the checks after it and the allow, to the denial.
		prog = append(prog, sockFilter{code: bpfJeq, jt: uint8(len(deniedSyscalls) - i), k: nr})
	}
	return append(prog,
		sockFilter{code: bpfRet, k: retAllow},
		sockFilter{code: bpfRet, k: retErrno | uint32(syscall.EPERM)},
	)
}

// deniedSyscalls are the system calls the filter fails with EPERM. io_uring
// is denied because it can create a socket (IORING_OP_SOCKET, Linux 5.19)
// without socket(2).
var deniedSyscalls = []uint32{sysSocket, sysSocketpair, sysIoUringSetup, sysIoUringEnter, sysIoUringRegister}

// userTasks counts the tasks (threads) owned by uid, the quantity
// RLIMIT_NPROC compares with.
func userTasks(uid uint32) (uint64, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	var n uint64
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
			continue
		}
		f, err := os.Open("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "Threads:"); ok {
				t, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
				n += t
				break
			}
		}
		_ = f.Close()
	}
	return n, nil
}

// descendants returns the pids of every process below the helper.
func descendants() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	children := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// The command name may contain spaces and parentheses; the fields
		// that matter follow its last ")".
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(b[i+1:]))
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	var out []int
	queue := []int{os.Getpid()}
	for len(queue) > 0 {
		next := children[queue[0]]
		queue = queue[1:]
		out = append(out, next...)
		queue = append(queue, next...)
	}
	return out, nil
}

// killDescendants sends SIGKILL to every process below the helper.
func killDescendants() {
	pids, err := descendants()
	if err != nil {
		return
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// clearDescendants kills and reaps every process below the helper until none
// is left. Because the helper is a subreaper, orphans of the tool are among
// them. Killing a process can orphan its children, so this repeats. It
// returns the usage of the orphans it reaped, which the tool's own rusage
// does not cover.
func clearDescendants() (Usage, error) {
	var total Usage
	deadline := time.Now().Add(5 * time.Second)
	for {
		pids, err := descendants()
		if err != nil {
			return total, fmt.Errorf("listing descendants: %w", err)
		}
		if len(pids) == 0 {
			return total, nil
		}
		if time.Now().After(deadline) {
			return total, fmt.Errorf("%d descendant processes survived SIGKILL", len(pids))
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		for {
			var ws syscall.WaitStatus
			var ru syscall.Rusage
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, &ru)
			if pid <= 0 || (err != nil && err != syscall.EINTR) {
				break
			}
			total.CPU += time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
			total.MaxRSS = max(total.MaxRSS, maxRSSBytes(&ru))
		}
		time.Sleep(2 * time.Millisecond)
	}
}
