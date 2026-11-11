//go:build unix && !linux

package media

import "syscall"

// platformID returns the device, inode and status-change time (in nanoseconds)
// of a file's os.FileInfo.Sys value.
func platformID(sys any) (dev, ino uint64, ctimeNS int64) {
	if st, ok := sys.(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino), st.Ctimespec.Nano() //nolint:unconvert // field widths differ by platform
	}
	return 0, 0, 0
}
