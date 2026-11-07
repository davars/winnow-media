package media

import (
	"context"
	"fmt"
	"slices"
)

// VideoInfo describes the video stream VideoFrames and the other video
// operations sample.
type VideoInfo struct {
	// DurationMS is the duration of the stream, else of the container, in
	// milliseconds; 0 when neither is known.
	DurationMS int64
	// FrameRate is the stream's base frame rate in frames per second; 0 when
	// unknown.
	FrameRate float64
	// Width and Height are the size of the decoded frames, after the
	// display rotation.
	Width, Height int
	// Rotation is the stream's display rotation: 0, 90, 180 or 270 degrees.
	Rotation int
	// HDR is set when the stream's transfer characteristic is HLG or PQ.
	HDR bool
	// Codec is the stream's codec name as ffprobe reports it, such as h264.
	Codec string
	// HasAudio is set when the container holds an audio stream.
	HasAudio bool
	// Frames is the frame count the container declares for the stream; 0
	// when it declares none.
	Frames int
}

// AnimationInfo describes a GIF or animated WebP.
type AnimationInfo struct {
	// Frames is the number of frames; AnimationFramesAt takes indices below
	// it.
	Frames int
	// Width and Height are the size of the canvas after any display
	// orientation.
	Width, Height int
	// DelaysMS is the display delay of each frame in milliseconds, as the
	// header stores it, except that a delay below 20 ms is 100 ms, as
	// browsers display it. It has Frames entries.
	DelaysMS []int
}

// ProbeVideo reads the properties of the video in an MP4, QuickTime, WebM or
// Matroska file without decoding a frame; its cost is one ffprobe run.
//
// It applies the gate of VideoFrames: the type is taken from the file's
// bytes, and a file VideoFrames would refuse for its type or size, or for
// the caps on streams, duration and dimensions, is refused here with the same
// error. An input with no video stream is an error. Usage covers the tool
// runs of the call, including those of a failed one.
func (p *Processor) ProbeVideo(ctx context.Context, path string) (VideoInfo, Usage, error) {
	in, err := p.admitInput(videoSpec, path)
	if err != nil {
		return VideoInfo{}, Usage{}, fmt.Errorf("probe video: %w", err)
	}
	var m meter
	v, err := runVideoProbe(ctx, p.env(&m), in.decoder, in.path, probeEntriesWithCodec)
	if err == nil {
		err = checkVideoInfo(v)
	}
	if err != nil {
		return VideoInfo{}, m.usage, fmt.Errorf("probe video: %w", err)
	}
	w, h := v.upright()
	return VideoInfo{
		DurationMS: v.durationMS,
		FrameRate:  v.rate,
		Width:      w,
		Height:     h,
		Rotation:   v.rotation,
		HDR:        isHDRTransfer(v.transfer),
		Codec:      v.codec,
		HasAudio:   v.hasAudio,
		Frames:     v.declared,
	}, m.usage, nil
}

// ProbeAnimation reads the frame count, canvas size and frame delays of a GIF
// or animated WebP from its headers, without decoding pixels.
//
// It applies the gate of AnimationFrames: a file AnimationFrames would refuse
// for its type, size, frame count or pixel counts is refused here with the
// same error. Usage is as for ProbeVideo.
func (p *Processor) ProbeAnimation(ctx context.Context, path string) (AnimationInfo, Usage, error) {
	in, err := p.admitInput(animationSpec, path)
	if err != nil {
		return AnimationInfo{}, Usage{}, fmt.Errorf("probe animation: %w", err)
	}
	var m meter
	hdr, err := p.env(&m).readSource(ctx, in.request(Hints{}, nil, false))
	if err != nil {
		return AnimationInfo{}, m.usage, fmt.Errorf("probe animation: %w", err)
	}
	layout := hdr.layout
	delays := make([]int, len(layout.infos))
	for i, f := range layout.infos {
		delays[i] = displayDelayCS(f.delayCS) * 10
	}
	return AnimationInfo{Frames: len(delays), Width: layout.uw, Height: layout.uh, DelaysMS: delays}, m.usage, nil
}

// VideoFramesAt decodes the frame at or after each of timesMS from an MP4,
// QuickTime, WebM or Matroska file and writes them to dir as
// frame-NNN.<ext>, each taken by its own seek. Output, the gate, the caps,
// errors, Usage and the state of dir are as for VideoFrames; Image.TimeMS is
// the requested time.
//
// timesMS holds 1 to 64 times in milliseconds, in any order, with repeats.
// The frames are written in ascending time and a repeated time is sampled
// once. A time must be 0 or inside the duration ProbeVideo reports: a negative
// time, or a positive one at or after the end, is an error before any frame is
// decoded, as is a positive time when the duration is unknown. As for
// VideoFrames, a sample identical to the one before it is dropped.
func (p *Processor) VideoFramesAt(ctx context.Context, path string, enc Encoding, timesMS []int64, dir string) ([]Image, Usage, error) {
	ec, err := enc.resolve()
	if err != nil {
		return nil, Usage{}, fmt.Errorf("video frames at: %w", err)
	}
	if err := checkSampleCount("times", len(timesMS)); err != nil {
		return nil, Usage{}, fmt.Errorf("video frames at: %w", err)
	}
	times := slices.Clone(timesMS)
	slices.Sort(times)
	times = slices.Compact(times)
	if times[0] < 0 {
		return nil, Usage{}, fmt.Errorf("video frames at: time %d ms is negative", times[0])
	}
	in, err := p.admit(videoSpec, path, dir)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("video frames at: %w", err)
	}
	var m meter
	env := p.env(&m)
	images, err := inDir(in.dir, func() ([]Image, error) {
		return env.videoFramesPicked(ctx, in.decoder, in.path, ec, func(info videoInfo) ([]int64, error) {
			for _, t := range times {
				if err := checkSeekTime(info, t); err != nil {
					return nil, err
				}
			}
			return times, nil
		}, in.dir)
	})
	if err != nil {
		return nil, m.usage, fmt.Errorf("video frames at: %w", err)
	}
	return images, m.usage, nil
}

// VideoFrameRun decodes count consecutive frames of an MP4, QuickTime, WebM
// or Matroska file, starting at the first frame at or after startMS, in one
// decode and writes them to dir as frame-NNN.<ext>. It costs one ffmpeg run
// however many frames it returns. Output, the gate, the caps, errors, Usage and the
// state of dir are as for VideoFrames.
//
// count is 1 to 64. startMS must be 0 or inside the duration ProbeVideo
// reports, as for VideoFramesAt. A run that reaches the end of the video
// returns fewer than count frames. Image.TimeMS is the frame's own
// presentation time in milliseconds, so it follows the stream's timing
// rather than a fixed step; as for VideoFrames, a frame identical to the one
// before it is dropped.
func (p *Processor) VideoFrameRun(ctx context.Context, path string, enc Encoding, startMS int64, count int, dir string) ([]Image, Usage, error) {
	ec, err := enc.resolve()
	if err != nil {
		return nil, Usage{}, fmt.Errorf("video frame run: %w", err)
	}
	if err := checkSampleCount("count", count); err != nil {
		return nil, Usage{}, fmt.Errorf("video frame run: %w", err)
	}
	if startMS < 0 {
		return nil, Usage{}, fmt.Errorf("video frame run: time %d ms is negative", startMS)
	}
	in, err := p.admit(videoSpec, path, dir)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("video frame run: %w", err)
	}
	var m meter
	env := p.env(&m)
	images, err := inDir(in.dir, func() ([]Image, error) {
		return env.videoFrameRun(ctx, in.decoder, in.path, ec, startMS, count, in.dir)
	})
	if err != nil {
		return nil, m.usage, fmt.Errorf("video frame run: %w", err)
	}
	return images, m.usage, nil
}

// AnimationFramesAt decodes the frames of a GIF or animated WebP with the
// given indices, composites them and writes them to dir as frame-NNN.<ext>.
// Output, the gate, the caps, errors, Usage and the state of dir are as for
// AnimationFrames; Image.Frame is the index.
//
// indices holds 1 to 64 indices, in any order, with repeats. The frames are
// written in ascending index and a repeated index is written once. An index
// must be below the Frames count ProbeAnimation reports; a negative one, or
// one at or above the count, is an error before any frame is decoded. As for
// AnimationFrames, a frame identical to the one before it after resizing is
// dropped.
func (p *Processor) AnimationFramesAt(ctx context.Context, path string, enc Encoding, indices []int, dir string) ([]Image, Usage, error) {
	ec, err := enc.resolve()
	if err != nil {
		return nil, Usage{}, fmt.Errorf("animation frames at: %w", err)
	}
	if err := checkSampleCount("indices", len(indices)); err != nil {
		return nil, Usage{}, fmt.Errorf("animation frames at: %w", err)
	}
	selected := slices.Clone(indices)
	slices.Sort(selected)
	selected = slices.Compact(selected)
	if selected[0] < 0 {
		return nil, Usage{}, fmt.Errorf("animation frames at: index %d is negative", selected[0])
	}
	in, err := p.admit(animationSpec, path, dir)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("animation frames at: %w", err)
	}
	var m meter
	env := p.env(&m)
	images, err := inDir(in.dir, func() ([]Image, error) {
		return env.encodeAnimationFrames(ctx, in, ec, func(sourceFrames int) ([]int, error) {
			if last := selected[len(selected)-1]; last >= sourceFrames {
				return nil, fmt.Errorf("index %d is outside the animation's %d frames", last, sourceFrames)
			}
			return selected, nil
		})
	})
	if err != nil {
		return nil, m.usage, fmt.Errorf("animation frames at: %w", err)
	}
	return images, m.usage, nil
}

// checkSampleCount refuses n unless it is 1 to maxMaxFrames, the number of
// samples of the largest call measured.
func checkSampleCount(what string, n int) error {
	if n < 1 || n > maxMaxFrames {
		return fmt.Errorf("%d %s is outside 1..%d", n, what, maxMaxFrames)
	}
	return nil
}
