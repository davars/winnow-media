package media

import (
	"errors"
	"fmt"
	"time"
)

// ErrTooLarge is returned, wrapped with the cap and the values that
// exceeded it, when an input is larger than the operations accept, and when a
// header read reaches ImageMagick's list length ceiling (2048 scenes). Test
// for it with errors.Is.
var ErrTooLarge = errors.New("input too large")

// ErrTimeLimit is returned, wrapped with what the tool reported, when a
// magick run (an encode, a header read, RunMagickWithCoderFallback) reached
// ImageMagick's time limit. ImageMagick often exits 0 when it
// does, with the work unfinished, so such a run's output is never used. The
// error is recognised from ImageMagick's message on stderr: through an Env it
// needs the Runner to fill Result.Stderr, and without it only the size check
// of the encodes catches an unfinished one. A Processor always fills it. Test
// for it with errors.Is.
var ErrTimeLimit = errors.New("time limit exceeded")

// The caps below are the one place the limits are stated; the ImageMagick
// policy ceilings are derived from them. Each was set from measurements of the
// largest inputs it admits, made with internal/cmd/measure on a 12-CPU x86_64
// host (see docs/measurements.md).
const (
	// maxInputBytes is the largest input file, inclusive. It is a requirement
	// of the callers, not a measurement.
	maxInputBytes = 25 << 20
	// maxDimension is the largest width or height of any frame or stream. For
	// a still it applies to the frame actually decoded, after any
	// decode-stage reduction.
	// WebP cannot exceed 16383, and a 16000x16000 video decodes within the
	// policy.
	maxDimension = 16384
	// maxFramePixels is the largest pixel count of one frame, of a still
	// after any decode-stage reduction. The largest phone formats sit just
	// below it: 48 MP iPhone (8064x6048, 48.8 MP) and 50 MP sensors (at most
	// 8192x6144, 50.3 MP). Every format admitted at this size completes
	// within the 1536Mi deployment limit.
	maxFramePixels = 52_000_000
	// maxAnimationFrames is the most frames an animation may hold. It is a
	// structural bound, not a time bound: the header read holds memory per
	// frame (docs/measurements.md), and the frame count stays below the
	// policy's list length.
	maxAnimationFrames = 2000
	// maxReturnedPixels bounds the frames a call returns times the canvas
	// area, and maxDecodedPixels bounds all frames times the canvas area.
	// With maxAnimationFrames they keep an animation within a 30 s budget per
	// call on a Raspberry Pi 5 (docs/measurements.md). Every frame up to the
	// last selected is decoded because frames composite onto earlier ones, and
	// a one-colour GIF under maxInputBytes can declare tens of gigapixels, so
	// decoding needs its own bound.
	maxReturnedPixels = 64_000_000
	maxDecodedPixels  = 900_000_000
	// maxStreams is the most streams, of any kind, a video container may
	// hold. It is a structural bound, not a measured one.
	maxStreams = 32
	// maxDurationMS is the longest video stream, in milliseconds. A stream
	// too long to read once is sampled by seeks, which cost what they do on a
	// short one.
	maxDurationMS = 6 * 3600 * 1000
)

// The ffmpeg decode limits.
const (
	// ffmpegLargeSourcePixels is the most pixels (not the largest side) a
	// source may have, 4096x2160, and be sampled with Resources.Threads.
	ffmpegLargeSourcePixels = 4096 * 2160
	// ffmpegLargeSourceThreads caps the thread count for a source with more
	// pixels than that. The HEVC decoder holds about one frame per thread plus
	// references, so its memory is threads times frame size; the measurement
	// is in docs/measurements.md.
	ffmpegLargeSourceThreads = 2
	// ffmpegMaxAlloc is the largest single allocation ffmpeg may make, in
	// bytes: four times what an 8K frame needs (docs/measurements.md).
	ffmpegMaxAlloc = 1 << 30
)

// ffmpegThreadsFor returns the thread count for sampling a width x height
// source, given the configured threads. The stored size is used because
// rotation leaves the product alone.
func ffmpegThreadsFor(threads, width, height int) int {
	if width*height > ffmpegLargeSourcePixels {
		return min(threads, ffmpegLargeSourceThreads)
	}
	return threads
}

// composeTimeout, when not zero, replaces the time limit as how long a
// compositor waits for a written frame to be consumed; tests lower it.
var composeTimeout time.Duration

// The ImageMagick policy ceilings. Width and height sit above maxDimension
// so that the header read reaches the checks in this package, which report
// ErrTooLarge, instead of failing inside the tool. Memory, map and disk
// together hold the pixel cache of the frames a call returns, at
// maxReturnedPixels, because the compositor decodes the rest outside the
// tool. The default time limit, the longest ImageMagick works on any one
// image, is a multiple of the longest operation measured to complete, with
// margin for a slower host (docs/measurements.md): the clock starts when an
// image is created, clones inherit it, and it is checked when pixels are next
// accessed, so it bounds neither a process nor a run of several images.
const (
	policyArea       = "512MP"
	policyDimension  = "65536"
	policyListLength = "2048"
)

// MemoryProfile names the ImageMagick memory limits the Env methods hold a
// run to: the memory and map limits, between which the pixel cache spills
// (memory, then map, then disk). Callers that build their own magick runs
// start them with profile.LimitArgs(). The zero value is HighMemory. The Env
// methods return an error for an undefined value, and LimitArgs panics. A
// Processor takes Resources instead.
type MemoryProfile int

const (
	// HighMemory suits a host with plenty of RAM: 1 GiB of memory and 2 GiB
	// of map.
	HighMemory MemoryProfile = iota
	// LowMemory suits a memory-limited container: 512 MiB of memory and
	// 1 GiB of map. Its measurement is in docs/measurements.md.
	LowMemory
)

func (p MemoryProfile) String() string {
	switch p {
	case HighMemory:
		return "HighMemory"
	case LowMemory:
		return "LowMemory"
	}
	return fmt.Sprintf("MemoryProfile(%d)", int(p))
}

// resources is the Resources a profile stands for, with defaults filled in.
func (p MemoryProfile) resources() (Resources, error) {
	switch p {
	case HighMemory:
		return defaultResources(), nil
	case LowMemory:
		return Resources{Memory: 512 << 20, Map: 1 << 30}.withDefaults(), nil
	}
	return Resources{}, fmt.Errorf("unknown memory profile %d", int(p))
}

func tooLarge(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTooLarge, fmt.Sprintf(format, args...))
}

// canvas returns the size frames of the scene are composited at.
func (s sceneHeader) canvas() (int, int) {
	if s.canvasW < 1 || s.canvasH < 1 {
		return s.width, s.height
	}
	return s.canvasW, s.canvasH
}

// checkSceneHeaders refuses headers that declare more than the caps allow,
// before any pixel is decoded. Frames of an animation are never reduced, so
// their header sizes are the decoded sizes; a still is capped by
// checkStillSize on the size its decode produces.
func checkSceneHeaders(scenes []sceneHeader, animated bool) error {
	if animated && len(scenes) > maxAnimationFrames {
		return tooLarge("animation has %d frames, the limit is %d", len(scenes), maxAnimationFrames)
	}
	var cw, ch int
	for i, s := range scenes {
		w, h := s.canvas()
		for _, d := range [][2]int{{s.width, s.height}, {w, h}} {
			if d[0] > maxDimension || d[1] > maxDimension {
				return tooLarge("frame %d is %dx%d, the limit is %d on each side", i, d[0], d[1], maxDimension)
			}
			if px := int64(d[0]) * int64(d[1]); px > maxFramePixels {
				return tooLarge("frame %d has %d pixels, the limit is %d", i, px, int64(maxFramePixels))
			}
		}
		cw, ch = max(cw, w), max(ch, h)
	}
	// The compositor allocates the largest width by the largest height, which
	// no single scene need reach; each side is already within maxDimension.
	if px := int64(cw) * int64(ch); px > maxFramePixels {
		return tooLarge("composite canvas has %d pixels, the limit is %d", px, int64(maxFramePixels))
	}
	if animated {
		if total := int64(len(scenes)) * int64(cw) * int64(ch); total > maxDecodedPixels {
			return tooLarge("animation has %d frames of %dx%d, %d pixels to decode, the limit is %d",
				len(scenes), cw, ch, total, int64(maxDecodedPixels))
		}
	}
	return nil
}

// checkStillSize refuses a still whose decoded frame, of size decoded, has a
// side over maxDimension or more than maxFramePixels pixels. stored is the
// size the file declares; it differs from decoded when the decode reduces it,
// and the error says both.
func checkStillSize(stored, decoded [2]int) error {
	for _, side := range decoded {
		if side > maxDimension {
			return tooLarge("still decodes at %dx%d (stored %dx%d), the limit is %d on each side",
				decoded[0], decoded[1], stored[0], stored[1], maxDimension)
		}
	}
	if px := int64(decoded[0]) * int64(decoded[1]); px > maxFramePixels {
		return tooLarge("still decodes at %dx%d, %d pixels (stored %dx%d), the limit is %d",
			decoded[0], decoded[1], px, stored[0], stored[1], int64(maxFramePixels))
	}
	return nil
}

// checkReturnedPixels refuses a call that would return count frames of a
// cw x ch canvas totalling more than maxReturnedPixels.
func checkReturnedPixels(count, cw, ch int) error {
	area := int64(cw) * int64(ch)
	if total := int64(count) * area; total > maxReturnedPixels {
		return tooLarge("%d frames of %dx%d are %d pixels to return, the limit is %d; at most %d frames fit",
			count, cw, ch, total, int64(maxReturnedPixels), max(0, int64(maxReturnedPixels)/area))
	}
	return nil
}

// checkVideoInfo refuses a probed video that has more streams, a longer
// duration or a larger stored size than the caps allow.
func checkVideoInfo(v videoInfo) error {
	if v.streams > maxStreams {
		return tooLarge("container has %d streams, the limit is %d", v.streams, maxStreams)
	}
	if v.durationMS > maxDurationMS {
		return tooLarge("video lasts %d ms, the limit is %d", v.durationMS, int64(maxDurationMS))
	}
	return checkVideoSize(v.width, v.height)
}

// checkVideoSize refuses a stream whose stored size exceeds maxDimension.
func checkVideoSize(width, height int) error {
	if width > maxDimension || height > maxDimension {
		return tooLarge("video is %dx%d, the limit is %d on each side", width, height, maxDimension)
	}
	return nil
}
