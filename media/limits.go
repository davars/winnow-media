package media

import (
	"fmt"
	"strconv"
	"time"
)

// The bomb ceilings below hold whatever Caps a Processor is given; each was
// set from measurements of the largest inputs it admits, made with
// internal/cmd/measure on a 12-CPU x86_64 host (see docs/measurements.md).
const (
	// maxDimension is the largest width or height of any frame or stream. For
	// a still it applies to the frame actually decoded, after any
	// decode-stage reduction.
	// WebP cannot exceed 16383, and a 16000x16000 video decodes within the
	// policy.
	maxDimension = 16384
	// maxAnimationFrames is the most frames an animation may hold. It is a
	// structural bound, not a time bound: the header read holds memory per
	// frame (docs/measurements.md), and the frame count stays below the
	// policy's list length.
	maxAnimationFrames = 2000
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
	policyAreaMP     = 512
	policyDimension  = "65536"
	policyListLength = "2048"
)

// policyArea is the area ceiling as ImageMagick spells it.
var policyArea = strconv.Itoa(policyAreaMP) + "MP"

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
func checkSceneHeaders(scenes []sceneHeader, animated bool, c caps) error {
	if animated && len(scenes) > c.frames {
		return tooLarge("animation has %d frames, the limit is %d", len(scenes), c.frames)
	}
	var cw, ch int
	for i, s := range scenes {
		w, h := s.canvas()
		for _, d := range [][2]int{{s.width, s.height}, {w, h}} {
			if d[0] > maxDimension || d[1] > maxDimension {
				return tooLarge("frame %d is %dx%d, the limit is %d on each side", i, d[0], d[1], maxDimension)
			}
			if px := int64(d[0]) * int64(d[1]); px > c.framePixels {
				return tooLarge("frame %d has %d pixels, the limit is %d", i, px, c.framePixels)
			}
		}
		cw, ch = max(cw, w), max(ch, h)
	}
	// The compositor allocates the largest width by the largest height, which
	// no single scene need reach; each side is already within maxDimension.
	if px := int64(cw) * int64(ch); px > c.framePixels {
		return tooLarge("composite canvas has %d pixels, the limit is %d", px, c.framePixels)
	}
	if animated {
		if total := int64(len(scenes)) * int64(cw) * int64(ch); total > c.decodedPixels {
			return tooLarge("animation has %d frames of %dx%d, %d pixels to decode, the limit is %d",
				len(scenes), cw, ch, total, c.decodedPixels)
		}
	}
	return nil
}

// checkStillSize refuses a still whose decoded frame, of size decoded, has a
// side over maxDimension or more than c.framePixels pixels. stored is the
// size the file declares; it differs from decoded when the decode reduces it,
// and the error says both.
func checkStillSize(stored, decoded [2]int, c caps) error {
	for _, side := range decoded {
		if side > maxDimension {
			return tooLarge("still decodes at %dx%d (stored %dx%d), the limit is %d on each side",
				decoded[0], decoded[1], stored[0], stored[1], maxDimension)
		}
	}
	if px := int64(decoded[0]) * int64(decoded[1]); px > c.framePixels {
		return tooLarge("still decodes at %dx%d, %d pixels (stored %dx%d), the limit is %d",
			decoded[0], decoded[1], px, stored[0], stored[1], c.framePixels)
	}
	return nil
}

// checkReturnedPixels refuses a call that would return count frames of a
// cw x ch canvas totalling more than c.returnedPixels.
func checkReturnedPixels(count, cw, ch int, c caps) error {
	area := int64(cw) * int64(ch)
	if total := int64(count) * area; total > c.returnedPixels {
		return tooLarge("%d frames of %dx%d are %d pixels to return, the limit is %d; at most %d frames fit",
			count, cw, ch, total, c.returnedPixels, max(0, c.returnedPixels/area))
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
