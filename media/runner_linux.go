//go:build linux

package media

import "syscall"

// directProcAttr puts the tool in its own process group and has the kernel
// kill it if the program dies.
func directProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
