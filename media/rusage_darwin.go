//go:build darwin

package media

import "syscall"

// maxRSSBytes converts Rusage.Maxrss, which is already in bytes on darwin.
func maxRSSBytes(ru *syscall.Rusage) int64 { return int64(ru.Maxrss) }
