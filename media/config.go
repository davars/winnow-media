package media

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Config configures a Processor. The zero Config works, and accepts every
// type and the largest sizes the library can handle: a deployment for hostile
// input sets Types and Caps.
type Config struct {
	// Types are the media types the Processor accepts, each one that Types()
	// lists; naming any other is an error, as is an empty non-nil slice. Nil
	// means every type Types() lists, a set that later versions may extend.
	// image/heic, image/heif and image/avif are all named or none, since one
	// decoder reads all three.
	Types []string
	// Caps turn away inputs that are too large. The zero value is the
	// library's ceilings.
	Caps Caps
	// Resources bounds each tool run. The zero value is the defaults.
	Resources Resources
	// Dir is an existing directory for scratch: the tools' temporary files
	// and what Sources keep between calls (the package documentation's Sizing
	// section says how much). The Processor works in a subdirectory of it
	// named media-processor-*, which Close removes; a process that ends
	// without Close leaves it behind. Empty means os.TempDir().
	Dir string
	// Unsandboxed runs the tools without the sandbox, which is the only way
	// to run them on a host the sandbox does not support. It is not for
	// hostile input: the package documentation says what still holds.
	Unsandboxed bool
}

// Caps are the limits that turn an input away with ErrTooLarge. A zero field
// is the library's ceiling for it; a negative field, or one above its ceiling,
// is an error from NewProcessor. The ceilings are what the library can handle,
// not what any host can bear.
//
// Whatever Caps says, these are refused too: a frame or video wider or taller
// than 16384 pixels, a still of more than 2048 images, a video container of
// more than 32 streams and a video longer than six hours.
type Caps struct {
	// MaxInputBytes is the largest input file, in bytes, of any kind. No
	// ceiling.
	MaxInputBytes int64
	// MaxFramePixels is the most pixels one frame may have, for every kind:
	// the canvas and each frame of an animation, the stored frame of a video,
	// and each image of a still as stored (the first at Open, another when it
	// is encoded). A JPEG is the exception: it is judged at Encode by the
	// size it is decoded at, which is below the stored size when every
	// Encoding of the call is small enough and the call has no Region. Open
	// can therefore admit a JPEG that an Encode refuses. Ceiling: 16384 x
	// 16384.
	MaxFramePixels int64
	// MaxReturnedPixels bounds the frames one Encode selects from an
	// animation, times its canvas area. Animations only. No ceiling.
	MaxReturnedPixels int64
	// MaxDecodedPixels bounds the frames an animation holds, times its canvas
	// area: a call may decode every frame, whatever it returns. Animations
	// only. No ceiling.
	MaxDecodedPixels int64
	// MaxAnimationFrames is the most frames an animation may hold. Animations
	// only: it does not count the images of a still or the frames of a video.
	// Ceiling: 2000.
	MaxAnimationFrames int
}

// caps is Caps resolved: no field is zero.
type caps struct {
	inputBytes, framePixels, returnedPixels, decodedPixels int64
	frames                                                 int
}

// ceilingCaps are the library ceilings, which a zero Caps resolves to.
var ceilingCaps = caps{
	inputBytes:     math.MaxInt64,
	framePixels:    maxDimension * maxDimension,
	returnedPixels: math.MaxInt64,
	decodedPixels:  math.MaxInt64,
	frames:         maxAnimationFrames,
}

// resolve returns c with zero fields at their ceilings, or an error for a
// negative field or one above its ceiling.
func (c Caps) resolve() (caps, error) {
	out := ceilingCaps
	for _, f := range []struct {
		name    string
		v       int64
		ceiling int64
		dst     *int64
	}{
		{"MaxInputBytes", c.MaxInputBytes, ceilingCaps.inputBytes, &out.inputBytes},
		{"MaxFramePixels", c.MaxFramePixels, ceilingCaps.framePixels, &out.framePixels},
		{"MaxReturnedPixels", c.MaxReturnedPixels, ceilingCaps.returnedPixels, &out.returnedPixels},
		{"MaxDecodedPixels", c.MaxDecodedPixels, ceilingCaps.decodedPixels, &out.decodedPixels},
		{"MaxAnimationFrames", int64(c.MaxAnimationFrames), int64(ceilingCaps.frames), nil},
	} {
		switch {
		case f.v < 0:
			return caps{}, fmt.Errorf("caps: %s is negative: %d", f.name, f.v)
		case f.v > f.ceiling:
			return caps{}, fmt.Errorf("caps: %s %d is above the ceiling %d", f.name, f.v, f.ceiling)
		case f.v == 0:
		case f.dst != nil:
			*f.dst = f.v
		default:
			out.frames = int(f.v)
		}
	}
	return out, nil
}

// Resources bounds each tool run of a Processor, not the Processor: calls on
// different Sources run at once, and each may use this much. The package
// documentation's Sizing section says what that asks of the host. A zero field
// means the default; a negative field is an error from NewProcessor, and
// there is no ceiling.
type Resources struct {
	// Memory and Map bound ImageMagick's pixel cache, in bytes: it uses
	// memory up to Memory, then memory-mapped files up to Map, then disk.
	// They do not bound a tool's other memory. Defaults 1 GiB and 2 GiB.
	Memory, Map int64
	// Disk bounds, in bytes, ImageMagick's pixel cache on disk, what a Source
	// keeps between calls, and the frames one Encode call holds on disk at
	// once: a video's sampled frames, uncompressed at the size the largest
	// Encoding needs, and an animation's composited frames. A video call over
	// it is refused with ErrTooLarge before any frame is decoded; an animation
	// over it is written a batch at a time. Default 4 GiB.
	Disk int64
	// Threads is the thread count of ImageMagick, ffmpeg and ffprobe; ffmpeg
	// uses at most two for a source above 4096x2160 whatever Threads is.
	// Default 4.
	Threads int
	// TimeLimit is the wall-clock time ImageMagick may work on one image, and
	// how long a frame compositor waits for a frame it wrote to be consumed.
	// It is not a deadline for a call, which may process many images. It must
	// be a whole number of seconds, at least one. Default 300 s.
	TimeLimit time.Duration
	// CPUTime is the CPU time one tool run may use, summed over its threads.
	// The sandbox enforces it, so it has no effect with Config.Unsandboxed.
	// It must be a whole number of seconds, at least one. Zero means
	// TimeLimit times Threads, each after its default: 1200 s at the
	// defaults.
	CPUTime time.Duration
}

const (
	defaultMemory    = 1 << 30
	defaultMap       = 2 << 30
	defaultDisk      = 4 << 30
	defaultThreads   = 4
	defaultTimeLimit = 300 * time.Second
)

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
	if r.CPUTime == 0 {
		// Whole seconds, saturating where a Duration would overflow.
		const maxSeconds = math.MaxInt64 / int64(time.Second)
		secs, threads := int64(r.TimeLimit/time.Second), int64(r.Threads)
		if secs > maxSeconds/threads {
			secs = maxSeconds
		} else {
			secs *= threads
		}
		r.CPUTime = time.Duration(secs) * time.Second
	}
	return r
}

// resolve returns r with the defaults filled in, or an error for a negative
// field or a TimeLimit or CPUTime that is not a whole number of seconds of at
// least one.
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
	if r.CPUTime != 0 && (r.CPUTime < time.Second || r.CPUTime%time.Second != 0) {
		return Resources{}, fmt.Errorf("resources: CPUTime %v is not a whole number of seconds of at least one", r.CPUTime)
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

// limitArgs returns the -limit flags that bound one magick process, for every
// Processor run. r must be resolved.
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
		CPU:      uint64(r.CPUTime / time.Second),
		Address:  saturatingAdd(saturatingAdd(uint64(r.Memory), uint64(r.Map)), addressHeadroom),
		FileSize: saturatingAdd(disk, disk/4),
	}
}
