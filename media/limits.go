package media

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// ErrTooLarge is returned, wrapped with the cap and the values that
// exceeded it, when an input is larger than the operations accept. Test for
// it with errors.Is.
var ErrTooLarge = errors.New("input too large")

// The caps below are the one place the limits are stated; the ImageMagick
// policy ceilings are derived from them. Each was set from measurements of the
// largest inputs it admits, made with internal/cmd/measure on a 12-CPU x86_64
// host (see the README).
const (
	// maxInputBytes is the largest input file, inclusive. It is a requirement
	// of the callers, not a measurement.
	maxInputBytes = 25 << 20
	// maxDimension is the largest width or height of any frame or stream.
	// WebP cannot exceed 16383, and a 16000x16000 video decodes within the
	// policy.
	maxDimension = 16384
	// maxFramePixels is the largest pixel count of one frame. A one-colour GIF
	// frame of 81 MP completes under the policy and one of 90 MP does not, and
	// two 64 MP frames complete.
	maxFramePixels = 52_000_000
	// maxAnimationFrames is the most frames an animation may hold. 2000
	// frames complete in under 20 s whatever the canvas, and the count is
	// below the policy's list length.
	maxAnimationFrames = 2000
	// maxAnimationPixels bounds frames times canvas area. For a GIF it
	// bounds the compositor, which holds every decoded frame at one byte per
	// pixel, and the work of decoding. For a WebP it bounds ImageMagick,
	// which holds every composited frame at 16 bytes per pixel beside the
	// decoded ones: a GIF of full-canvas frames read by ImageMagick
	// completed at 185 MP and failed at 194 MP.
	maxAnimationPixels = 160_000_000
	// maxStreams is the most streams, of any kind, a video container may
	// hold. It is a structural bound, not a measured one.
	maxStreams = 32
	// maxDurationMS is the longest video stream, in milliseconds. Sampling
	// seeks, so a six-hour video costs what a short one does.
	maxDurationMS = 6 * 3600 * 1000
)

// ffmpegThreads bounds the decoder's threads and the filter graph's;
// MEASURE_FFMPEG_THREADS overrides it on the measurement branch.
var ffmpegThreads = func() int {
	if n, err := strconv.Atoi(os.Getenv("MEASURE_FFMPEG_THREADS")); err == nil && n > 0 {
		return n
	}
	return 4
}()

// The ffmpeg decode limits.
const (
	// ffmpegMaxAlloc is the largest single allocation ffmpeg may make, in
	// bytes. A 4K HDR frame needs 32 MiB, one float plane of the tone-mapping
	// filters; 1 GiB is four times what an 8K frame needs.
	ffmpegMaxAlloc = 1 << 30
)

// The ImageMagick policy ceilings. Width and height sit above maxDimension
// so that the header read reaches the checks in this package, which report
// ErrTooLarge, instead of failing inside the tool. Memory, map and disk
// together hold the pixel cache of an animation at maxAnimationPixels; time
// is at least four times the longest ImageMagick operation measured to
// complete (36 s); a Raspberry Pi 5 measured at worst about 4 times a 12-CPU
// x86_64 host (CPU 4.1 times for 4K HEVC decode, wall time 4.0 times for two
// 8000x8000 GIF frames).
const (
	policyMemory     = "1GiB"
	policyMap        = "2GiB"
	policyDisk       = "4GiB"
	policyArea       = "512MP"
	policyDimension  = "65536"
	policyListLength = "2048"
	policyTime       = "300"
	policyThreads    = "4"
)

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
// before any pixel is decoded.
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
	if animated {
		if total := int64(len(scenes)) * int64(cw) * int64(ch); total > maxAnimationPixels {
			return tooLarge("animation has %d frames of %dx%d, %d pixels in all, the limit is %d",
				len(scenes), cw, ch, total, int64(maxAnimationPixels))
		}
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
