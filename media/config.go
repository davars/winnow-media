package media

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Config configures a Processor.
type Config struct {
	// Resources bounds the tools a Processor runs; the zero value uses the
	// defaults documented at Resources.
	Resources Resources
	// Dir is the existing directory under which the Processor makes its
	// private directory (os.MkdirTemp, "media-processor-"). Empty means
	// os.TempDir().
	Dir string
	// Unsandboxed runs tools directly, for hosts the sandbox does not support
	// (it needs Linux 5.11 or later on amd64 or arm64): with no seccomp
	// filter, rlimits or subreaper. The gate, caps, environment, policy and
	// Resources' policy values and tool flags still apply.
	Unsandboxed bool
}

// Resources bounds what a Processor's tools may use. A zero field means the
// library default; a negative field is an error, and there is no upper
// ceiling. The values reach the generated ImageMagick policy and the -limit
// flags of every magick run, ffmpeg's and ffprobe's thread counts, the frame
// compositor's wait for a consumed frame, and the sandbox's resource limits.
type Resources struct {
	// Memory and Map are ImageMagick's memory and map limits in bytes, between
	// which its pixel cache spills (memory, then map, then disk). Defaults
	// 1 GiB and 2 GiB.
	Memory, Map int64
	// Disk is ImageMagick's disk limit in bytes. Default 4 GiB.
	Disk int64
	// Threads is the thread count of ImageMagick, ffmpeg and ffprobe; ffmpeg
	// uses at most two for a source above 4096x2160 whatever Threads is.
	// Default 4.
	Threads int
	// TimeLimit is ImageMagick's per-image time limit, how long a frame
	// compositor waits for a frame to be consumed, and the sandbox's CPU-time
	// limit for a tool. It must be a whole number of seconds, at least one.
	// Default 300 s.
	TimeLimit time.Duration
}

const (
	defaultMemory    = 1 << 30
	defaultMap       = 2 << 30
	defaultDisk      = 4 << 30
	defaultThreads   = 4
	defaultTimeLimit = 300 * time.Second
)

func defaultResources() Resources { return Resources{}.withDefaults() }

func (r Resources) withDefaults() Resources {
	if r.Memory == 0 {
		r.Memory = defaultMemory
	}
	if r.Map == 0 {
		r.Map = defaultMap
	}
	if r.Disk == 0 {
		r.Disk = defaultDisk
	}
	if r.Threads == 0 {
		r.Threads = defaultThreads
	}
	if r.TimeLimit == 0 {
		r.TimeLimit = defaultTimeLimit
	}
	return r
}

// resolve returns r with the defaults filled in, or an error for a negative
// field or a TimeLimit that is not a whole number of seconds of at least one.
func (r Resources) resolve() (Resources, error) {
	for _, f := range []struct {
		name string
		v    int64
	}{{"Memory", r.Memory}, {"Map", r.Map}, {"Disk", r.Disk}, {"Threads", int64(r.Threads)}} {
		if f.v < 0 {
			return Resources{}, fmt.Errorf("resources: %s is negative: %d", f.name, f.v)
		}
	}
	if r.Threads > math.MaxInt32 {
		return Resources{}, fmt.Errorf("resources: Threads %d is above %d", r.Threads, math.MaxInt32)
	}
	if r.TimeLimit != 0 && (r.TimeLimit < time.Second || r.TimeLimit%time.Second != 0) {
		return Resources{}, fmt.Errorf("resources: TimeLimit %v is not a whole number of seconds of at least one", r.TimeLimit)
	}
	return r.withDefaults(), nil
}

// magickSize spells a byte count as ImageMagick reads it: in the largest
// binary unit that divides it exactly, else as bare bytes.
func magickSize(n int64) string {
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

func (r Resources) seconds() string { return strconv.FormatInt(int64(r.TimeLimit/time.Second), 10) }

// limitArgs returns the -limit flags that bound one magick process: the one
// implementation behind Processor runs and MemoryProfile.LimitArgs. r must be
// resolved.
func (r Resources) limitArgs() []string {
	return []string{
		"-limit", "memory", magickSize(r.Memory),
		"-limit", "map", magickSize(r.Map),
		"-limit", "area", policyArea,
		"-limit", "list-length", policyListLength,
		"-limit", "time", r.seconds(),
	}
}

// rlimits are the sandbox limits derived from Resources, in the units of
// setrlimit.
type rlimits struct {
	CPU, Address, FileSize uint64
}

// addressHeadroom is what a tool's address space may use beyond ImageMagick's
// memory and map: its code, libraries, thread stacks and the per-thread arenas
// that grow with a host's CPU count. At the default Resources the limit is
// 8 GiB, a multiple of the worst peak measured (docs/measurements.md).
const addressHeadroom = 5 << 30

// rlimitInfinity is RLIM_INFINITY, where the derived limits saturate.
const rlimitInfinity = math.MaxUint64

func saturatingAdd(a, b uint64) uint64 {
	if a > rlimitInfinity-b {
		return rlimitInfinity
	}
	return a + b
}

// rlimitsFor derives the sandbox limits from resolved resources. The file
// size is the disk limit plus a quarter, so ImageMagick's own ceiling stops
// it first and the limit catches only a tool that ignores the policy. A sum
// too large for a limit saturates at no limit.
func rlimitsFor(r Resources) rlimits {
	disk := uint64(r.Disk)
	return rlimits{
		CPU:      uint64(r.TimeLimit / time.Second),
		Address:  saturatingAdd(saturatingAdd(uint64(r.Memory), uint64(r.Map)), addressHeadroom),
		FileSize: saturatingAdd(disk, disk/4),
	}
}
