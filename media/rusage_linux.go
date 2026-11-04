//go:build !darwin

package media

import "syscall"

// maxRSSBytes converts Rusage.Maxrss, which is in kilobytes on this platform.
func maxRSSBytes(ru *syscall.Rusage) int64 { return int64(ru.Maxrss) * 1024 }
