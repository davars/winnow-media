//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestProbeConfinement(t *testing.T) {
	skipShort(t)
	dir := t.TempDir()
	abi, err := probeConfinement(context.Background(), dir, defaultLimits)
	if err != nil {
		t.Fatalf("probe of an intact confinement: %v", err)
	}
	if abi < 1 {
		t.Errorf("the probe reported Landlock ABI %d, want at least 1", abi)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the probe left %d entries behind", len(entries))
	}
}

// A probe that skips the confinement must be noticed.
func TestProbeConfinementRefusesWhenItIsSkipped(t *testing.T) {
	skipShort(t)
	probeFault = "confine"
	defer func() { probeFault = "" }()
	_, err := probeConfinement(context.Background(), t.TempDir(), defaultLimits)
	if err == nil || !strings.Contains(err.Error(), "outside the list") || !strings.Contains(err.Error(), "Landlock") {
		t.Errorf("err = %v, want one naming the access that was not refused and Landlock", err)
	}
	if errors.Is(err, errConfinementUnavailable) {
		t.Errorf("a confinement that is not enforced was reported as unavailable: %v", err)
	}
}

// A kernel with no Landlock is reported as such, not as a failed probe.
func TestProbeConfinementReportsUnavailable(t *testing.T) {
	probeFault = "landlock-unavailable"
	defer func() { probeFault = "" }()
	dir := t.TempDir()
	abi, err := probeConfinement(context.Background(), dir, defaultLimits)
	if !errors.Is(err, errConfinementUnavailable) || abi != 0 {
		t.Errorf("abi, err = %d, %v, want 0 and errConfinementUnavailable", abi, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the probe left %d entries behind", len(entries))
	}
}

// A Landlock version query that fails other than as unavailable is a failed
// probe, never errConfinementUnavailable, so NewProcessor cannot fall back to
// unconfined render runs on it. The fault makes the query fail with EINVAL.
func TestProbeConfinementRefusesAnUnexpectedQueryError(t *testing.T) {
	probeFault = "landlock-query-error"
	defer func() { probeFault = "" }()
	abi, err := probeConfinement(context.Background(), t.TempDir(), defaultLimits)
	if err == nil || errors.Is(err, errConfinementUnavailable) || abi != 0 {
		t.Errorf("abi, err = %d, %v; want 0 and a failed probe that is not errConfinementUnavailable", abi, err)
	}
}

func TestLandlockUnavailable(t *testing.T) {
	for _, e := range []syscall.Errno{syscall.ENOSYS, syscall.EOPNOTSUPP, syscall.EPERM} {
		if !landlockUnavailable(fmt.Errorf("landlock_create_ruleset: %w", e)) {
			t.Errorf("%v was not taken as unavailable", e)
		}
	}
	for _, e := range []syscall.Errno{syscall.EINVAL, syscall.EACCES, syscall.ENOMEM} {
		if landlockUnavailable(e) {
			t.Errorf("%v was taken as unavailable", e)
		}
	}
}

func TestCheckConfineReport(t *testing.T) {
	good := confineReport{ABI: 3, DeniedRead: int(syscall.EACCES), DeniedWrite: int(syscall.EACCES), DeniedOtherRead: int(syscall.EACCES), DeniedExec: int(syscall.EACCES)}
	if err := checkConfineReport(&good); err != nil {
		t.Fatalf("intact report: %v", err)
	}
	if err := checkConfineReport(nil); err == nil {
		t.Error("a missing report passed")
	}
	for name, mutate := range map[string]func(*confineReport){
		"no ABI":            func(r *confineReport) { r.ABI = 0 },
		"read refused":      func(r *confineReport) { r.Read = int(syscall.EACCES) },
		"write refused":     func(r *confineReport) { r.Write = int(syscall.EACCES) },
		"read allowed":      func(r *confineReport) { r.DeniedRead = 0 },
		"write allowed":     func(r *confineReport) { r.DeniedWrite = 0 },
		"listing allowed":   func(r *confineReport) { r.DeniedOtherRead = 0 },
		"exec allowed":      func(r *confineReport) { r.DeniedExec = int(syscall.ENOENT) },
		"exec other errno":  func(r *confineReport) { r.DeniedExec = int(syscall.EPERM) },
		"read other errno":  func(r *confineReport) { r.DeniedRead = int(syscall.ENOENT) },
		"write other errno": func(r *confineReport) { r.DeniedWrite = int(syscall.EROFS) },
	} {
		r := good
		mutate(&r)
		if err := checkConfineReport(&r); err == nil {
			t.Errorf("%s: the report passed", name)
		}
	}
}

func TestRenderConfinementChoosesTheLibraryRoots(t *testing.T) {
	store := renderConfinement(nixStore+"/abc-tool/bin/tool", "/run")
	if len(store.Exec) != 1 || store.Exec[0] != nixStore {
		t.Errorf("a tool in the Nix store: Exec = %v, want the store", store.Exec)
	}
	other := renderConfinement("/opt/tools/tool", "/run", "/policy")
	want := []string{"/opt/tools", "/usr", "/lib", "/lib64"}
	if fmt.Sprint(other.Exec) != fmt.Sprint(want) {
		t.Errorf("another tool: Exec = %v, want %v", other.Exec, want)
	}
	if !other.Input || other.Write[0] != "/run" || other.Read[len(other.Read)-1] != "/policy" {
		t.Errorf("confinement = %+v, want the run directory writable, the policy directory readable and the input", other)
	}
}

// confinedRun runs the fake tool under c with in as its input and returns what
// it printed.
func confinedRun(t *testing.T, c *confinement, in *os.File, args ...string) string {
	t.Helper()
	p := fakeProcessor(t)
	self := p.tools["tool"]
	if _, err := os.Stat(nixStore); err == nil && c != nil {
		// The test binary may load the C library from the store.
		c.Exec = append(c.Exec, nixStore)
	}
	var out, errOut strings.Builder
	spec := runSpec{Tool: self, Args: append([]string{"--tool"}, args...), Env: []string{}, Dir: p.dir, Input: in, Confine: c}
	res, err := p.start(context.Background(), spec, &out, &errOut)
	if err != nil || res.Err != "" || res.Code != 0 || res.Signal != 0 {
		t.Fatalf("confined run of %v: %+v, %v (stderr %q)", args, res, err, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

func errnoLine(t *testing.T, out string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimPrefix(out, "errno "))
	if err != nil {
		t.Fatalf("output %q is not an errno line", out)
	}
	return n
}

func TestConfinedToolReachesOnlyItsAllowances(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	other := filepath.Join(dir, "other")
	for _, d := range []string{run, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(dir, "input.txt")
	sibling := filepath.Join(dir, "sibling.txt")
	secret := filepath.Join(other, "secret")
	for path, content := range map[string]string{input: "the input", sibling: "x", secret: "x"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(t.TempDir(), "home-file")
	if err := os.WriteFile(home, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Dir(home))
	program := filepath.Join(other, "program")
	if err := os.WriteFile(program, []byte("#!/nonexistent\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	conf := func() *confinement { return renderConfinement(self, run) }
	eacces := int(syscall.EACCES)

	if got := confinedRun(t, conf(), in, "read-input"); got != "read the input 0" {
		t.Errorf("reading the input descriptor: %q", got)
	}
	for _, c := range []struct {
		name string
		args []string
		want int
	}{
		{"the input's inode by its name", []string{"open", input, "r"}, 0},
		{"a sibling of the input", []string{"open", sibling, "r"}, eacces},
		{"the input's directory", []string{"open", dir, "d"}, eacces},
		{"a file of another directory", []string{"open", secret, "r"}, eacces},
		{"a file of the home directory", []string{"open", os.Getenv("HOME") + "/home-file", "r"}, eacces},
		{"a file in the run directory", []string{"open", filepath.Join(run, "out.png"), "c"}, 0},
		{"a file made outside the run directory", []string{"open", filepath.Join(other, "new"), "c"}, eacces},
		{"a file written outside the run directory", []string{"open", secret, "w"}, eacces},
		{"the null device", []string{"open", "/dev/null", "w"}, 0},
		{"a process's list of descriptors", []string{"open", "/proc/self/fd", "d"}, 0},
		{"a program outside the list", []string{"exec", program}, eacces},
		{"the program's environment of the calling process", []string{"open", "/proc/" + strconv.Itoa(os.Getpid()) + "/environ", "r"}, eacces},
		{"a file, from a process the tool started", []string{"child", "open", secret, "r"}, eacces},
		{"the input, from a process the tool started", []string{"child", "open", "/dev/fd/3", "r"}, 0},
	} {
		if got := errnoLine(t, confinedRun(t, conf(), in, c.args...)); got != c.want {
			t.Errorf("%s: errno %d, want %d", c.name, got, c.want)
		}
	}
	// A file put at the input's path after it was admitted is another inode.
	if err := os.WriteFile(sibling, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sibling, input); err != nil {
		t.Fatal(err)
	}
	if got := errnoLine(t, confinedRun(t, conf(), in, "open", input, "r")); got != eacces {
		t.Errorf("a file put at the input's path: errno %d, want %d", got, eacces)
	}
	if got := confinedRun(t, conf(), in, "read-input"); got != "read the input 0" {
		t.Errorf("reading the admitted input after its path was replaced: %q", got)
	}
	// The confinement is that of one run.
	if got := errnoLine(t, confinedRun(t, nil, in, "open", secret, "r")); got != 0 {
		t.Errorf("an unconfined run after confined ones: errno %d, want 0", got)
	}
}

// The helper that starts a confined tool is not inside a domain the tool may
// reach: a tool that can read or write its parent's /proc entries can forge
// the helper's report through its status descriptor, or, where Yama does not
// restrict ptrace, write the helper's memory and run as one of its
// unconfined threads.
func TestConfinedToolCannotReachItsHelper(t *testing.T) {
	run := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, entry, mode string
	}{
		{"the helper's environment", "environ", "r"},
		{"the helper's memory map", "maps", "r"},
		{"the helper's status descriptor", "fd/3", "w"},
		{"the helper's memory", "mem", "w"},
	} {
		got := errnoLine(t, confinedRun(t, renderConfinement(self, run), nil, "open-parent", c.entry, c.mode))
		if got == 0 {
			t.Errorf("%s: opened, want refused", c.name)
		} else {
			t.Logf("%s: errno %d", c.name, got)
		}
	}
}

// TestLandlockRightValues pins the rights the confinement handles to the
// kernel's ABI 1 set and the file-capable subset.
func TestLandlockRightValues(t *testing.T) {
	if abi1Rights != 1<<13-1 {
		t.Errorf("abi1Rights = %#x, want the thirteen ABI 1 bits %#x", abi1Rights, 1<<13-1)
	}
	if want := 1<<0 | 1<<1 | 1<<2 | 1<<14; fileRights != want {
		t.Errorf("fileRights = %#x, want %#x", fileRights, want)
	}
}
