package media

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
)

// VideoInfo describes the video stream VideoFrames and the other video
// operations sample.
//
// Deprecated: use Info.
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
//
// Deprecated: use Info.
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

// The operations below wrap Open and Encode: each opens the file, makes one
// Encode and closes the Source, writing the file names they always did.

// ProbeVideo reads the properties of the video in an MP4, QuickTime, WebM or
// Matroska file without decoding a frame; its cost is one ffprobe run.
//
// It applies the gate of VideoFrames: the type is taken from the file's
// bytes, and a file VideoFrames would refuse for its type or size, or for
// the caps on streams, duration and dimensions, is refused here with the same
// error. An input with no video stream is an error. Usage covers the tool
// runs of the call, including those of a failed one. A caller with a time
// budget chooses its count of samples (at most 64 per call) from the fields.
//
// Deprecated: use Processor.Open and Source.Info.
func (p *Processor) ProbeVideo(ctx context.Context, path string) (VideoInfo, Usage, error) {
	src, usage, err := p.openKinds(ctx, path, OpenOptions{}, kindVideo)
	if err != nil {
		return VideoInfo{}, usage, fmt.Errorf("probe video: %w", err)
	}
	defer func() { _ = src.Close() }()
	info := src.Info()
	return VideoInfo{
		DurationMS: info.DurationMS, FrameRate: info.FrameRate, Width: info.Width, Height: info.Height,
		Rotation: info.Rotation, HDR: info.HDR, Codec: info.Codec, HasAudio: info.HasAudio, Frames: info.Frames,
	}, usage, nil
}

// ProbeAnimation reads the frame count, canvas size and frame delays of a GIF
// or animated WebP from its headers, without decoding pixels.
//
// It applies the gate of AnimationFrames: a file AnimationFrames would refuse
// for its type, size, frame count or pixel counts is refused here with the
// same error. The canvas is upright by the orientation the header reports,
// which a one-frame file can carry where only a decode sees it;
// AnimationFrames sizes its output from the decoded frame. Usage is as for
// ProbeVideo.
//
// Deprecated: use Processor.Open and Source.Info.
func (p *Processor) ProbeAnimation(ctx context.Context, path string) (AnimationInfo, Usage, error) {
	src, usage, err := p.openKinds(ctx, path, OpenOptions{}, kindAnimation)
	if err != nil {
		return AnimationInfo{}, usage, fmt.Errorf("probe animation: %w", err)
	}
	defer func() { _ = src.Close() }()
	info := src.Info()
	delays := info.DelaysMS
	if info.Kind == Still {
		delays = []int{displayDelayCS(0) * 10}
	}
	return AnimationInfo{Frames: len(delays), Width: info.Width, Height: info.Height, DelaysMS: delays}, usage, nil
}

// Still decodes the first image of a file of any type in Config.Types other
// than GIF and video, and of a WebP that is not animated, and writes it to dir
// as still.<ext> in the encoding's format. The output is upright, converted
// to sRGB when the source carries an ICC profile, flattened onto white for
// JPEG and free of metadata.
//
// The type is taken from the file's bytes, not its name. dir must exist and
// be empty. Every argument is validated, and the input checked against the
// size caps, before pixels are read: a type the operation does not accept
// gives an error wrapping ErrUnsupportedType, and an input over a cap one
// wrapping ErrTooLarge. A tool run that reaches ImageMagick's time limit
// gives an error wrapping ErrTimeLimit. On failure Still removes what it
// wrote, leaving dir empty. Usage covers every tool run of the call,
// including those of a failed one.
//
// Deprecated: use Processor.Open and Source.Encode.
func (p *Processor) Still(ctx context.Context, path string, enc Encoding, dir string) (Image, Usage, error) {
	images, usage, err := p.operate(ctx, "still", kindStill, path, dir, enc, Select{},
		func(_ int, ext string) string { return "still" + ext })
	if err != nil {
		return Image{}, usage, err
	}
	return images[0], usage, nil
}

// AnimationFrames decodes a GIF or WebP file whose type is in Config.Types,
// composites its frames, samples up to maxFrames of them evenly and writes
// each to dir as frame-NNN.<ext>. A sampled frame identical to the one before it after
// resizing is dropped and the files are numbered consecutively from 0;
// Image.Frames[0].Index is the index of the source frame a file shows. Output is as
// for Still.
//
// maxFrames 0 means 16; otherwise it must be 1 to 64. Errors, Usage and the
// state of dir are as for Still; a file with more frames or pixels than the
// caps allow gives an error wrapping ErrTooLarge. A GIF that ends between
// blocks without its trailer is accepted, as ImageMagick does; one truncated
// inside a block, or with a frame outside the logical screen, is refused.
//
// Deprecated: use Processor.Open and Source.Encode with Even.
func (p *Processor) AnimationFrames(ctx context.Context, path string, enc Encoding, maxFrames int, dir string) ([]Image, Usage, error) {
	frames, err := resolveFrames(maxFrames)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("animation frames: %w", err)
	}
	return p.operate(ctx, "animation frames", kindAnimation, path, dir, enc, Select{Frames: Even(frames)}, frameName)
}

// VideoFrames decodes up to maxFrames still images from an MP4, QuickTime,
// WebM or Matroska file whose type is in Config.Types and writes them to dir
// as frame-NNN.<ext>. The samples are the centres of equal intervals of the
// duration of the video stream flagged default (else the first), ignoring
// attached pictures; fewer
// when the stream has fewer frames, targets that coincide are merged, and a
// clip of unknown duration gives one sample at 0. A video that yields no frame
// at any of those times, as when its only frame precedes a duration the
// container reports, is sampled at 0 instead, and fails only if that gives no
// frame either. A stream whose frame count is known and whose decoded pixels
// (frames times width times height as stored) are at most 1e9 is read by one
// decode, in which a select filter keeps the first frame at or after each
// sample time; any other by one decode per sample, started a margin before
// the time. The frame count is the container's or, where it gives none (as in
// Matroska and MPEG-TS), the duration times the declared rate, so a
// variable-rate stream is estimated high and may take the seeks; a stream with
// neither is always read by seeks. An input with no video stream is an
// error.
//
// A container with more than 32 streams, a stream longer than six hours or
// one wider or taller than 16384 pixels gives an error wrapping ErrTooLarge
// before any frame is decoded. HDR (HLG or PQ) video is tone-mapped to SDR,
// output is upright whatever the stream's display rotation, and a sample
// identical to the one before it is dropped, with the files numbered
// consecutively from 0.
// Image.Frames[0].TimeMS is the sample time in milliseconds and its Index is 0
// because a sample has no known frame index. Output is otherwise as for Still.
//
// maxFrames 0 means 16; otherwise it must be 1 to 64. Errors, Usage and the
// state of dir are as for Still.
//
// Deprecated: use Processor.Open and Source.Encode with Even.
func (p *Processor) VideoFrames(ctx context.Context, path string, enc Encoding, maxFrames int, dir string) ([]Image, Usage, error) {
	frames, err := resolveFrames(maxFrames)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("video frames: %w", err)
	}
	return p.operate(ctx, "video frames", kindVideo, path, dir, enc, Select{Frames: Even(frames)}, frameName)
}

// VideoFramesAt decodes the frame at or after each of timesMS from an MP4,
// QuickTime, WebM or Matroska file and writes them to dir as
// frame-NNN.<ext>, read as VideoFrames reads its samples: by one decode or one
// seek per time, as the stream's size decides. VideoFrames is this with evenly
// spaced times. Output, the gate, the caps, errors, Usage and the state of dir
// are as for VideoFrames; Image.Frames[0].TimeMS is the requested time.
//
// timesMS holds 1 to 64 times in milliseconds, in any order, with repeats.
// The frames are written in ascending time and a repeated time is sampled
// once. A time must be 0 or inside the duration ProbeVideo reports: a negative
// time, or a positive one at or after the end, is an error before any frame is
// decoded, as is a positive time when the duration is unknown. As for
// VideoFrames, a sample identical to the one before it is dropped.
//
// Deprecated: use Processor.Open and Source.Encode with Times.
func (p *Processor) VideoFramesAt(ctx context.Context, path string, enc Encoding, timesMS []int64, dir string) ([]Image, Usage, error) {
	if err := checkSampleCount("times", len(timesMS)); err != nil {
		return nil, Usage{}, fmt.Errorf("video frames at: %w", err)
	}
	if first := slices.Min(timesMS); first < 0 {
		return nil, Usage{}, fmt.Errorf("video frames at: time %d ms is negative", first)
	}
	return p.operate(ctx, "video frames at", kindVideo, path, dir, enc, Select{Frames: Times(timesMS...)}, frameName)
}

// VideoFrameRun decodes count consecutive frames of an MP4, QuickTime, WebM
// or Matroska file, starting at the first frame at or after startMS, in one
// decode and writes them to dir as frame-NNN.<ext>. It costs one ffmpeg run
// however many frames it returns: a stream read by seeks starts that run a
// margin before startMS, and a stream read once is decoded from its start.
// Output, the gate, the caps, errors, Usage and the state of dir are as for
// VideoFrames.
//
// count is 1 to 64. startMS must be 0 or inside the duration ProbeVideo
// reports, as for VideoFramesAt. A run that reaches the end of the video
// returns fewer than count frames. Image.Frames[0].TimeMS is the frame's own
// presentation time in milliseconds, so it follows the stream's timing
// rather than a fixed step; as for VideoFrames, a frame identical to the one
// before it is dropped.
//
// Deprecated: use Processor.Open and Source.Encode with Run.
func (p *Processor) VideoFrameRun(ctx context.Context, path string, enc Encoding, startMS int64, count int, dir string) ([]Image, Usage, error) {
	if err := checkSampleCount("count", count); err != nil {
		return nil, Usage{}, fmt.Errorf("video frame run: %w", err)
	}
	if startMS < 0 {
		return nil, Usage{}, fmt.Errorf("video frame run: time %d ms is negative", startMS)
	}
	return p.operate(ctx, "video frame run", kindVideo, path, dir, enc, Select{Frames: Run(startMS, count)}, frameName)
}

// AnimationFramesAt decodes the frames of a GIF or animated WebP with the
// given indices, composites them and writes them to dir as frame-NNN.<ext>,
// through the compositors AnimationFrames uses; AnimationFrames is this with
// evenly spaced indices. Output, the gate, the caps, errors, Usage and the
// state of dir are as for AnimationFrames; Image.Frames[0].Index is the index.
//
// indices holds 1 to 64 indices, in any order, with repeats. The frames are
// written in ascending index and a repeated index is written once. An index
// must be below the Frames count ProbeAnimation reports; a negative one, or
// one at or above the count, is an error before any frame is decoded. As for
// AnimationFrames, a frame identical to the one before it after resizing is
// dropped.
//
// Deprecated: use Processor.Open and Source.Encode with Indices.
func (p *Processor) AnimationFramesAt(ctx context.Context, path string, enc Encoding, indices []int, dir string) ([]Image, Usage, error) {
	if err := checkSampleCount("indices", len(indices)); err != nil {
		return nil, Usage{}, fmt.Errorf("animation frames at: %w", err)
	}
	if first := slices.Min(indices); first < 0 {
		return nil, Usage{}, fmt.Errorf("animation frames at: index %d is negative", first)
	}
	return p.operate(ctx, "animation frames at", kindAnimation, path, dir, enc, Select{Frames: Indices(indices...)}, frameName)
}

// operate opens path as one of kinds, makes one Encode of sel into dir and
// closes the Source. The arguments are validated, and dir checked, before the
// file is read; every error is prefixed with name, and the Usage covers both
// the open and the encode, also when the call fails.
func (p *Processor) operate(ctx context.Context, name string, kinds mediaKind, path, dir string, enc Encoding, sel Select, file func(n int, ext string) string) ([]Image, Usage, error) {
	if _, err := enc.resolve(); err != nil {
		return nil, Usage{}, fmt.Errorf("%s: %w", name, err)
	}
	abs, err := filepath.Abs(dir)
	if err == nil {
		err = requireEmptyDir(abs)
	}
	if err != nil {
		return nil, Usage{}, fmt.Errorf("%s: %w", name, err)
	}
	src, usage, err := p.openKinds(ctx, path, OpenOptions{}, kinds)
	if err != nil {
		return nil, usage, fmt.Errorf("%s: %w", name, err)
	}
	defer func() { _ = src.Close() }()
	images, used, err := src.encode(ctx, abs, sel, []Encoding{enc}, file)
	usage.CPU += used.CPU
	usage.MaxRSS = max(usage.MaxRSS, used.MaxRSS)
	if err != nil {
		return nil, usage, fmt.Errorf("%s: %w", name, err)
	}
	return images, usage, nil
}

// checkSampleCount refuses n unless it is 1 to maxMaxFrames, the number of
// samples of the largest call measured.
func checkSampleCount(what string, n int) error {
	if n < 1 || n > maxMaxFrames {
		return fmt.Errorf("%d %s is outside 1..%d", n, what, maxMaxFrames)
	}
	return nil
}
