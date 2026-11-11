//go:build unix && !linux

package media

import "syscall"

// directProcAttr puts the tool in its own process group.
func directProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
