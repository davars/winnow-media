//go:build linux && arm64

package media

import "golang.org/x/sys/unix"

const (
	auditArch      = unix.AUDIT_ARCH_AARCH64
	rejectX32 bool = false
)

const x32Bit = 0
