//go:build linux && amd64

package media

import "golang.org/x/sys/unix"

const (
	auditArch      = unix.AUDIT_ARCH_X86_64
	rejectX32 bool = true
)

// x32Bit marks a system call number of the x32 ABI, which shares the
// architecture value with amd64; x/sys has no name for it.
const x32Bit = 0x40000000
