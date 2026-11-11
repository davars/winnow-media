//go:build linux

package media

import "syscall"

// platformID returns the device, inode and status-change time (in nanoseconds)
// of a file's os.FileInfo.Sys value.
func platformID(sys any) (dev, ino uint64, ctimeNS int64) {
	if st, ok := sys.(*syscall.Stat_t); ok {
		return uint64(st.Dev), st.Ino, st.Ctim.Nano() //nolint:unconvert // Dev's width differs by architecture
	}
	return 0, 0, 0
}
