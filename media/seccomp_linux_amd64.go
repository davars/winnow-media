//go:build linux && amd64

package media

const (
	auditArch          = 0xC000003E // AUDIT_ARCH_X86_64
	sysSeccomp         = 317
	sysSocket          = 41
	sysSocketpair      = 53
	rejectX32     bool = true
)

// x32Bit marks a system call number of the x32 ABI, which shares the
// architecture value with amd64.
const x32Bit = 0x40000000
