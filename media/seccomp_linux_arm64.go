//go:build linux && arm64

package media

const (
	auditArch          = 0xC00000B7 // AUDIT_ARCH_AARCH64
	sysSeccomp         = 277
	sysSocket          = 198
	sysSocketpair      = 199
	rejectX32     bool = false
)

const x32Bit = 0
