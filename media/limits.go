package media

import (
	"errors"
	"fmt"
)

// ErrTooLarge is returned, wrapped with the cap and the values that
// exceeded it, when an input is larger than the operations accept. Test for
// it with errors.Is.
var ErrTooLarge = errors.New("input too large")

// The caps below are provisional. They are the one place the limits are
// stated; the ImageMagick policy ceilings are derived from them.
const (
	// maxInputBytes is the largest input file, inclusive.
	maxInputBytes = 25 << 20
	// maxDimension is the largest width or height of any frame or stream.
	maxDimension = 16384
	// maxFramePixels is the largest pixel count of one frame.
	maxFramePixels = 100_000_000
	// maxAnimationFrames is the most frames an animation may hold.
	maxAnimationFrames = 2000
	// maxAnimationPixels bounds frames times canvas area, the work of
	// compositing an animation.
	maxAnimationPixels = 1_000_000_000
	// maxStreams is the most streams, of any kind, a video container may hold.
	maxStreams = 32
	// maxDurationMS is the longest video stream, in milliseconds.
	maxDurationMS = 6 * 3600 * 1000
)

// The ffmpeg decode limits.
const (
	// ffmpegThreads bounds the decoder's threads and the filter graph's.
	ffmpegThreads = 4
	// ffmpegMaxAlloc is the largest single allocation ffmpeg may make, in bytes.
	ffmpegMaxAlloc = 1 << 30
)

// The ImageMagick policy ceilings. Width and height sit above maxDimension
// so that the header read reaches the checks in this package, which report
// ErrTooLarge, instead of failing inside the tool.
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
