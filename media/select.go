package media

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

type framesKind uint8

const (
	framesFirst framesKind = iota
	framesAll
	framesEven
	framesIndices
	framesTimes
	framesConsecutive
)

// FrameSelection says which frames or images of a Source an Encode call reads.
// Build one with All, Even, Indices, Times or Consecutive; the zero value is
// the first frame or image. A FrameSelection that does not apply to the
// Source's kind is refused before any tool runs.
//
// A time in a selection is read to the millisecond: it is truncated to whole
// milliseconds.
type FrameSelection struct {
	kind  framesKind
	n     int
	start time.Duration
	ints  []int
	times []time.Duration
}

// All selects every frame of an animation and every image of a still. A video
// refuses it.
func All() FrameSelection { return FrameSelection{kind: framesAll} }

// Even selects n frames evenly spaced over the Source, or all of them when it
// has fewer. An animation or still takes frames i*count/n counting from the
// first; a video takes samples at the centres of n equal intervals of its
// duration. n must be at least 1, and a video refuses one above 64.
func Even(n int) FrameSelection { return FrameSelection{kind: framesEven, n: n} }

// Indices selects the frames of an animation, or the images of a still, with
// the given indices, which may repeat and come in any order: each is read
// once, in ascending order. A video refuses it.
func Indices(i ...int) FrameSelection {
	return FrameSelection{kind: framesIndices, ints: slices.Clone(i)}
}

// Times selects, at each of the given times, the frame an animation shows then
// (by its display delays), or the first frame of a video at or after it. Each
// time is truncated to whole milliseconds. The times may repeat and come in
// any order, and a video refuses more than 64. A time must be 0 or inside the
// duration. A still refuses it.
func Times(t ...time.Duration) FrameSelection {
	return FrameSelection{kind: framesTimes, times: slices.Clone(t)}
}

// Consecutive selects n consecutive frames starting at the one shown at start,
// as Times reads a time; fewer when the end is reached. A video reads them in
// one decode and refuses more than 64. A still refuses it.
func Consecutive(start time.Duration, n int) FrameSelection {
	return FrameSelection{kind: framesConsecutive, start: start, n: n}
}

// Select says what an Encode call reads from a Source.
type Select struct {
	// Frames are the frames or images to encode; the zero value is the first.
	Frames FrameSelection
	// Region is the part of each frame to encode; the zero value is the whole
	// frame.
	Region Region
}

var errAllOfVideo = errors.New("a video has no frame count to select all of; use even samples, times or consecutive frames")

// durMS is d in whole milliseconds, truncated; a negative d never reads as 0.
func durMS(d time.Duration) int64 {
	if ms := d.Milliseconds(); ms != 0 || d >= 0 {
		return ms
	}
	return -1
}

// maxVideoSamples is the most samples one call reads from a video.
const maxVideoSamples = maxMaxFrames

// indexList returns the ascending distinct indices f holds, each below n.
func (f FrameSelection) indexList(n int, owner, what string) ([]int, error) {
	if len(f.ints) == 0 {
		return nil, errors.New("no indices given")
	}
	out := slices.Clone(f.ints)
	slices.Sort(out)
	out = slices.Compact(out)
	if out[0] < 0 {
		return nil, fmt.Errorf("index %d is negative", out[0])
	}
	if last := out[len(out)-1]; last >= n {
		return nil, fmt.Errorf("index %d is outside the %s %d %ss", last, owner, n, what)
	}
	return out, nil
}

func evenCount(n int) error {
	if n < 1 {
		return fmt.Errorf("even count %d selects no frame", n)
	}
	return nil
}

// images resolves f against a still of n images.
func (f FrameSelection) images(n int) ([]int, error) {
	switch f.kind {
	case framesFirst:
		return []int{0}, nil
	case framesAll:
		return selectFrames(n, n), nil
	case framesEven:
		if err := evenCount(f.n); err != nil {
			return nil, err
		}
		return selectFrames(min(f.n, n), n), nil
	case framesIndices:
		return f.indexList(n, "still's", "image")
	}
	return nil, errors.New("a still has images, selected by Indices; times and consecutive frames select frames of an animation or video")
}

// animation resolves f against an animation whose frames start at starts, in
// milliseconds, and which lasts total.
func (f FrameSelection) animation(starts []int64, total int64) ([]int, error) {
	n := len(starts)
	at := func(ms int64) (int, error) {
		if ms < 0 {
			return 0, fmt.Errorf("time %d ms is negative", ms)
		}
		if ms > 0 && ms >= total {
			return 0, fmt.Errorf("time %d ms is not inside the animation, which lasts %d ms", ms, total)
		}
		return sort.Search(n, func(i int) bool { return starts[i] > ms }) - 1, nil
	}
	switch f.kind {
	case framesFirst:
		return []int{0}, nil
	case framesAll:
		return selectFrames(n, n), nil
	case framesEven:
		if err := evenCount(f.n); err != nil {
			return nil, err
		}
		return selectFrames(min(f.n, n), n), nil
	case framesIndices:
		return f.indexList(n, "animation's", "frame")
	case framesTimes:
		if len(f.times) == 0 {
			return nil, errors.New("no times given")
		}
		var out []int
		for _, t := range f.times {
			i, err := at(durMS(t))
			if err != nil {
				return nil, err
			}
			out = append(out, i)
		}
		slices.Sort(out)
		return slices.Compact(out), nil
	}
	if f.n < 1 {
		return nil, fmt.Errorf("%d consecutive frames from %d ms selects no frame", f.n, durMS(f.start))
	}
	first, err := at(durMS(f.start))
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, min(f.n, n-first))
	for i := first; i < n && i < first+f.n; i++ {
		out = append(out, i)
	}
	return out, nil
}

// video resolves f against a probed video: the sample times, or the run of
// consecutive frames.
func (f FrameSelection) video(info videoInfo) (times []int64, run *frameRun, err error) {
	count := func(n int, what string) error {
		if n < 1 || n > maxVideoSamples {
			return fmt.Errorf("%d %s is outside 1..%d", n, what, maxVideoSamples)
		}
		return nil
	}
	switch f.kind {
	case framesFirst:
		return []int64{0}, nil, nil
	case framesAll:
		return nil, nil, errAllOfVideo
	case framesEven:
		if err := evenCount(f.n); err != nil {
			return nil, nil, err
		}
		if err := count(f.n, "samples"); err != nil {
			return nil, nil, err
		}
		return sampleTimesMS(info, f.n), nil, nil
	case framesIndices:
		return nil, nil, errors.New("indices select frames of an animation or images of a still; a video is read by even samples, times or consecutive frames")
	case framesTimes:
		if err := count(len(f.times), "times"); err != nil {
			return nil, nil, err
		}
		for _, t := range f.times {
			times = append(times, durMS(t))
		}
		slices.Sort(times)
		times = slices.Compact(times)
		for _, ms := range times {
			if err := checkSeekTime(info, ms); err != nil {
				return nil, nil, err
			}
		}
		return times, nil, nil
	}
	if err := count(f.n, "frames"); err != nil {
		return nil, nil, err
	}
	if err := checkSeekTime(info, durMS(f.start)); err != nil {
		return nil, nil, err
	}
	return nil, &frameRun{startMS: durMS(f.start), count: f.n}, nil
}
