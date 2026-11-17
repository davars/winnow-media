//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// The Landlock system calls, whose numbers are the same on amd64 and arm64.
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	landlockCreateRulesetVersion = 1
	landlockRulePathBeneath      = 1

	// oPath is O_PATH, which package syscall does not name.
	oPath = 0x200000
)

// Filesystem access rights, by the Landlock ABI that introduced them.
const (
	fsExecute    = 1 << 0
	fsWriteFile  = 1 << 1
	fsReadFile   = 1 << 2
	fsReadDir    = 1 << 3
	fsRemoveDir  = 1 << 4
	fsRemoveFile = 1 << 5
	fsMakeChar   = 1 << 6
	fsMakeDir    = 1 << 7
	fsMakeReg    = 1 << 8
	fsMakeSock   = 1 << 9
	fsMakeFifo   = 1 << 10
	fsMakeBlock  = 1 << 11
	fsMakeSym    = 1 << 12
	fsRefer      = 1 << 13 // ABI 2
	fsTruncate   = 1 << 14 // ABI 3

	// Rights stop at the ABI 3 set. The later ones, network ports (ABI 4),
	// device ioctls (ABI 5) and signal and abstract-socket scoping (ABI 6),
	// are not handled, so the seccomp filter and the host's own policy decide
	// those accesses, as they did before the confinement.

	// abi1Rights are the rights every Landlock kernel handles.
	abi1Rights = 1<<13 - 1
	// fileRights are the rights a rule on a file, not a directory, may carry.
	fileRights = fsExecute | fsWriteFile | fsReadFile | fsTruncate
)

// The standing allowances of a render run, besides its program and what the
// run is given: where fontconfig and the C library look for fonts and the
// locale, the process and CPU information the runtime reads, and the devices.
var (
	renderReadPaths = []string{
		"/etc/fonts", "/usr/share/fonts", "/usr/local/share/fonts", "/usr/share/fontconfig",
		"/usr/lib/locale", "/etc/localtime", "/etc/ld.so.cache",
		"/proc", "/sys/devices/system/cpu", "/dev/urandom",
	}
	renderWritePaths = []string{"/dev/null"}
	// systemLibraryPaths hold the programs and libraries of a tool that is
	// not in the Nix store.
	systemLibraryPaths = []string{"/usr", "/lib", "/lib64"}
)

// nixStore is where the programs and libraries of the flake's tools are.
const nixStore = "/nix/store"

// renderConfinement returns the confinement of a run of the program tool
// that reads one input and writes only into runDir; extraRead are further
// directories it may read, such as the directory of a policy file. A tool
// that resolves into the Nix store may execute the store, and any other the
// system's library directories and its own.
func renderConfinement(tool, runDir string, extraRead ...string) *confinement {
	c := &confinement{
		Read:  append(append([]string{}, renderReadPaths...), extraRead...),
		Write: append([]string{runDir}, renderWritePaths...),
		Input: true,
	}
	resolved, err := filepath.EvalSymlinks(tool)
	if err != nil {
		resolved = tool
	}
	if resolved == nixStore || strings.HasPrefix(resolved, nixStore+"/") {
		c.Exec = []string{nixStore}
	} else {
		c.Exec = append([]string{filepath.Dir(resolved)}, systemLibraryPaths...)
	}
	return c
}

// landlockABI returns the Landlock ABI version of the kernel, at least 1, or
// an error where Landlock is not built in or not enabled.
func landlockABI() (int, error) {
	v, _, e := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if e != 0 {
		return 0, fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	return int(v), nil
}

// apply confines the calling thread, and so every process it starts after,
// to c. The thread must be one that starts the confined process, which the
// helper's locked thread is. inputFD is the descriptor holding the input, or
// -1 for a run with none. It needs no_new_privs.
func (c *confinement) apply(inputFD int) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	handled := uint64(abi1Rights)
	if abi >= 2 {
		handled |= fsRefer
	}
	if abi >= 3 {
		handled |= fsTruncate
	}
	attr := handled
	rs, _, e := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if e != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	defer func() { _ = syscall.Close(int(rs)) }()

	dirExec := uint64(fsExecute | fsReadFile | fsReadDir)
	dirRead := uint64(fsReadFile | fsReadDir)
	write := uint64(fsReadFile | fsReadDir | fsWriteFile | fsRemoveFile | fsMakeReg | fsRefer | fsTruncate)
	for _, g := range []struct {
		paths  []string
		access uint64
	}{{c.Exec, dirExec}, {c.Read, dirRead}, {c.Write, write}} {
		for _, path := range g.paths {
			if err := addPath(int(rs), path, g.access, handled); err != nil {
				return err
			}
		}
	}
	if c.Input && inputFD >= 0 {
		if err := addRule(int(rs), inputFD, fsReadFile, handled); err != nil {
			return fmt.Errorf("allowing the input: %w", err)
		}
	}
	if _, _, e := syscall.Syscall(sysLandlockRestrictSelf, rs, 0, 0); e != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", e)
	}
	return nil
}

// addPath allows access to the file or directory at path, and everything
// below a directory, unless it does not exist.
func addPath(rs int, path string, access, handled uint64) error {
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("allowing %s: %w", path, err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := addRule(rs, fd, access, handled); err != nil {
		return fmt.Errorf("allowing %s: %w", path, err)
	}
	return nil
}

// addRule adds a rule for the file or directory fd holds. A rule on a file
// takes only the rights that apply to files.
func addRule(rs, fd int, access, handled uint64) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		access &= fileRights
	}
	access &= handled
	if access == 0 {
		return nil
	}
	// struct landlock_path_beneath_attr is packed: u64 allowed_access, s32 parent_fd.
	var attr [12]byte
	binary.NativeEndian.PutUint64(attr[:8], access)
	binary.NativeEndian.PutUint32(attr[8:], uint32(fd))
	if _, _, e := syscall.Syscall6(sysLandlockAddRule, uintptr(rs), landlockRulePathBeneath, uintptr(unsafe.Pointer(&attr[0])), 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

// confineProbe names what the probe helper tries once it is confined, in a
// directory the Processor made: a file the confinement allows reading, one it
// does not, a directory it allows writing in, and a program it does not allow
// executing.
type confineProbe struct {
	Allowed, Denied, Dir, Program string
}

// confineReport is the errno of each attempt of a confineProbe, 0 for success.
type confineReport struct {
	ABI                                                               int
	Read, DeniedRead, DeniedWrite, Write, DeniedExec, DeniedOtherRead int
}

// tryConfined attempts each access of p and reports the errno of each.
func tryConfined(p *confineProbe, abi int) *confineReport {
	r := &confineReport{ABI: abi}
	errno := func(err error) int {
		var e syscall.Errno
		if errors.As(err, &e) {
			return int(e)
		}
		if err != nil {
			return -1
		}
		return 0
	}
	open := func(path string, flags int) error {
		fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC, 0o600)
		if err == nil {
			_ = syscall.Close(fd)
		}
		return err
	}
	r.Read = errno(open(p.Allowed, syscall.O_RDONLY))
	r.DeniedRead = errno(open(p.Denied, syscall.O_RDONLY))
	r.DeniedWrite = errno(open(p.Denied, syscall.O_WRONLY))
	r.DeniedOtherRead = errno(open(filepath.Dir(p.Allowed), syscall.O_RDONLY|syscall.O_DIRECTORY))
	created := filepath.Join(p.Dir, "created")
	r.Write = errno(open(created, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL))
	_ = os.Remove(created)
	// A program the kernel would run but for the confinement: the interpreter
	// its first line names does not exist, so an execve that is allowed fails
	// with ENOENT and returns, and one that is not, with EACCES.
	path, err := syscall.BytePtrFromString(p.Program)
	if err != nil {
		r.DeniedExec = -1
		return r
	}
	argv := []*byte{path, nil}
	_, _, e := syscall.Syscall(syscall.SYS_EXECVE, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&argv[0])), 0)
	r.DeniedExec = int(e)
	return r
}

// checkConfineReport returns an error unless r shows the confinement in
// force: a Landlock kernel, the allowed accesses made, and each other refused
// with EACCES.
func checkConfineReport(r *confineReport) error {
	if r == nil {
		return errors.New("the helper did not report the confinement")
	}
	if r.ABI < 1 {
		return fmt.Errorf("kernel Landlock ABI %d, want at least 1", r.ABI)
	}
	for _, c := range []struct {
		name  string
		errno int
		want  syscall.Errno
	}{
		{"reading an allowed file", r.Read, 0},
		{"creating a file in an allowed directory", r.Write, 0},
		{"reading a file outside the list", r.DeniedRead, syscall.EACCES},
		{"writing a file outside the list", r.DeniedWrite, syscall.EACCES},
		{"listing a directory outside the list", r.DeniedOtherRead, syscall.EACCES},
		{"executing a program outside the list", r.DeniedExec, syscall.EACCES},
	} {
		if c.errno != int(c.want) {
			return fmt.Errorf("%s: errno %d, want %d", c.name, c.errno, int(c.want))
		}
	}
	return nil
}

// probeConfinement starts the probe helper with a test confinement in dir,
// which must exist, and refuses unless the helper reports it in force.
func probeConfinement(ctx context.Context, dir string, limits rlimits) error {
	root, probe, cfg, err := makeConfineProbe(dir)
	if root != "" {
		defer func() { _ = os.RemoveAll(root) }()
	}
	if err != nil {
		return fmt.Errorf("confinement probe: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cfg.Probe, cfg.Limits, cfg.Fault = true, limits, probeFault
	cfg.ConfineProbe = probe
	var stderr headTailBuffer
	res, err := runHelper(ctx, cfg, io.Discard, &stderr)
	if err != nil {
		return fmt.Errorf("confinement probe: %w: %s", err, bytes.TrimSpace(stderr.bytes()))
	}
	if res.Err != "" {
		return fmt.Errorf("confinement probe: %s", res.Err)
	}
	if res.Probe == nil {
		return errors.New("confinement probe: the helper did not report")
	}
	if err := checkConfineReport(res.Probe.Confinement); err != nil {
		return fmt.Errorf("confinement probe: %w: file confinement needs Linux 5.13 or later with the Landlock security module enabled", err)
	}
	return nil
}

// makeConfineProbe makes the files of a confineProbe in dir and returns it
// with the helperConfig that confines to them. The caller removes root.
func makeConfineProbe(dir string) (root string, _ *confineProbe, _ helperConfig, err error) {
	root, err = os.MkdirTemp(dir, "confine-")
	if err != nil {
		return "", nil, helperConfig{}, err
	}
	p := &confineProbe{
		Allowed: filepath.Join(root, "allowed"), Denied: filepath.Join(root, "denied"),
		Dir: filepath.Join(root, "write"), Program: filepath.Join(root, "program"),
	}
	for path, content := range map[string]string{p.Allowed: "x", p.Denied: "x", p.Program: "#!/nonexistent\n"} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			return root, nil, helperConfig{}, err
		}
	}
	if err := os.Mkdir(p.Dir, 0o700); err != nil {
		return root, nil, helperConfig{}, err
	}
	return root, p, helperConfig{Confine: &confinement{Read: []string{p.Allowed}, Write: []string{p.Dir}}}, nil
}
