package media

import (
	"strings"
	"time"
)

// Frame is one frame of an encoded output and the frame of the ORIGINAL media
// it came from. The slice index of a Frame is its position in the encoded
// sequence.
type Frame struct {
	// Index is the frame's index in the original media: 0 for a still and
	// for a video, whose samples have no frame index.
	Index int
	// Time is the time the frame is shown in the original, to the
	// millisecond: the sum of the display delays of the frames before it in
	// an animation (a stored delay below 20 ms counts as 100 ms, as browsers
	// display it), or the time a video was sampled at; 0 for a still.
	Time time.Duration
}

// selectFrames returns n evenly spaced indices of sourceFrames frames:
// selected[i] = i*sourceFrames/n, integer division, so frame 0 always appears.
// The last frame appears only when sourceFrames <= n: even spacing over the
// whole sequence is the rule, and pinning both endpoints would make the
// interior spacing uneven.
func selectFrames(n, sourceFrames int) []int {
	if n < 1 || sourceFrames < 1 {
		return []int{0}
	}
	selected := make([]int, 0, n)
	for i := range n {
		selected = append(selected, i*sourceFrames/n)
	}
	return selected
}

// timestamps returns, for each original frame index, the elapsed
// milliseconds before it in the ORIGINAL media — the sum runs over every
// preceding frame, not only the sampled ones.
//
// A stored delay below 2 centiseconds is rendered as 10 by the long-standing
// GIF display convention, and real GIFs do store 0 on every frame; an
// unclamped sum makes every timestamp 0, which locates no moment in the
// media. It is a display convention only: frames are identified by source
// frame index, not by timestamp, so a wrong clamp cannot change a mapping.
func timestamps(delaysCS []int) []int64 {
	out := make([]int64, len(delaysCS))
	var elapsedCS int64
	for i, delay := range delaysCS {
		out[i] = elapsedCS * 10
		elapsedCS += int64(displayDelayCS(delay))
	}
	return out
}

// displayDelayCS is the delay a frame is displayed for: a stored delay below
// 2 centiseconds is shown as 10.
func displayDelayCS(delay int) int {
	if delay < 2 {
		return 10
	}
	return delay
}

func profileListHasICC(profiles string) bool {
	for _, profile := range strings.FieldsFunc(profiles, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		if strings.EqualFold(profile, "icc") || strings.EqualFold(profile, "icm") {
			return true
		}
	}
	return false
}

func frameDelays(frames []frameInfo) []int {
	delays := make([]int, len(frames))
	for i, frame := range frames {
		delays[i] = frame.delayCS
	}
	return delays
}
