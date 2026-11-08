package media

import (
	"errors"
	"fmt"
	"time"
)

// ErrTooLarge is returned, wrapped with the cap and the values that
// exceeded it, when an input is larger than the operations accept. Test for
// it with errors.Is.
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
// host (see the README).
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
	// structural bound, not a time bound: the header read holds about 62 KiB
	// per frame (10000 frames of a GIF peaked at 624 MiB in ImageMagick), and
	// the frame count stays below the policy's list length.
	maxAnimationFrames = 2000
	// maxReturnedPixels bounds the frames a call returns times the canvas
	// area, and maxDecodedPixels bounds all frames times the canvas area.
	// With maxAnimationFrames they keep an animation within a 30 s budget per
	// call on a Raspberry Pi 5: the worst admitted GIF, 225 frames of
	// 2000x2000 with 16 returned (64 MP returned, 900 MP decoded), took 27.8 s
	// there, and 16 frames of 2000x2000 alone took 20.2 s. A returned
	// megapixel costs about 0.28 s and a decoded one about 0.01 s. Every frame
	// up to the last selected is decoded because frames composite onto earlier
	// ones, and a one-colour GIF under maxInputBytes can declare tens of
	// gigapixels, so decoding needs its own bound.
	maxReturnedPixels = 64_000_000
	maxDecodedPixels  = 900_000_000
	// maxStreams is the most streams, of any kind, a video container may
	// hold. It is a structural bound, not a measured one.
	maxStreams = 32
	// maxDurationMS is the longest video stream, in milliseconds. Sampling
	// seeks, so a six-hour video costs what a short one does.
	maxDurationMS = 6 * 3600 * 1000
)

// The ffmpeg decode limits.
const (
	// ffmpegThreads bounds the decoder's threads and the filter graph's.
	ffmpegThreads = 4
	// ffmpegLargeSourcePixels is the largest source, in pixels, sampled with
	// ffmpegThreads: 4096x2160.
	ffmpegLargeSourcePixels = 4096 * 2160
	// ffmpegLargeSourceThreads replaces ffmpegThreads above that size. The
	// HEVC decoder holds about one frame per thread plus references, so its
	// memory is threads times frame size: an 8K 10-bit HLG source peaked at
	// 1811 MiB with 4 threads and 1280 MiB with 2, for 28% more time.
	ffmpegLargeSourceThreads = 2
	// ffmpegMaxAlloc is the largest single allocation ffmpeg may make, in
	// bytes. A 4K HDR frame needs 32 MiB, one float plane of the tone-mapping
	// filters; 1 GiB is four times what an 8K frame needs.
	ffmpegMaxAlloc = 1 << 30
)

// ffmpegThreadsFor returns the thread count for sampling a width x height
// source. The stored size is used because rotation leaves the product alone.
func ffmpegThreadsFor(width, height int) int {
	if width*height > ffmpegLargeSourcePixels {
		return ffmpegLargeSourceThreads
	}
	return ffmpegThreads
}

// policyTimeSeconds is the ImageMagick policy's time ceiling, the longest
// ImageMagick works on any one image: the clock starts when an image is
// created, clones inherit it, and it is checked when pixels are next
// accessed, so it bounds neither a process nor a run of several images. A
// compositor waits this long for ImageMagick to consume a frame it wrote.
const policyTimeSeconds = 300

// consumeTimeout is how long a compositor waits for a written frame to be
// consumed before it fails.
const consumeTimeout = policyTimeSeconds * time.Second

// composeTimeout is the timeout the operations give a compositor; tests lower
// it.
var composeTimeout = consumeTimeout

// The ImageMagick policy ceilings. Width and height sit above maxDimension
// so that the header read reaches the checks in this package, which report
// ErrTooLarge, instead of failing inside the tool. Memory, map and disk
// together hold the pixel cache of the frames a call returns, at
// maxReturnedPixels, because the compositor decodes the rest outside the
// tool; time is at least four times the longest ImageMagick operation
// measured to complete (36 s); a Raspberry Pi 5 measured at worst about 4
// times a 12-CPU x86_64 host (CPU 4.1 times for 4K HEVC decode, wall time 4.0
// times for two 8000x8000 GIF frames).
const (
	policyDisk       = "4GiB"
	policyArea       = "512MP"
	policyDimension  = "65536"
	policyListLength = "2048"
	policyThreads    = "4"
)

// MemoryProfile names the ImageMagick memory limits a run is held to. The
// zero value is HighMemory. NewProcessor and the Env methods return an error
// for an undefined value, and LimitArgs panics.
type MemoryProfile int

const (
	// HighMemory suits a host with plenty of RAM: 1 GiB of memory and 2 GiB
	// of map.
	HighMemory MemoryProfile = iota
	// LowMemory suits a memory-limited container: 512 MiB of memory and
	// 1 GiB of map.
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

// profileSettings is what a MemoryProfile sets, as ImageMagick spells it.
type profileSettings struct{ memory, mapLimit string }

var memoryProfiles = map[MemoryProfile]profileSettings{
	HighMemory: {memory: "1GiB", mapLimit: "2GiB"},
	LowMemory:  {memory: "512MiB", mapLimit: "1GiB"},
}

func (p MemoryProfile) settings() (profileSettings, error) {
	s, ok := memoryProfiles[p]
	if !ok {
		return profileSettings{}, fmt.Errorf("unknown memory profile %d", int(p))
	}
	return s, nil
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
