package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// Transcode says how Source.Transcode writes a video: the encoders, the
// container, the streams and the limits of the run. Video and Container are
// required. The video is written as 8-bit 4:2:0 (yuv420p), upright: a
// display rotation is applied to the pixels. An HDR source is converted to
// 8 bits without tone mapping. The container's metadata and chapters are
// copied from the input.
type Transcode struct {
	// Video is the video encoder: an SVTAV1 or an X265.
	Video VideoEncoder
	// Audio is the encoder of every audio stream written: an Opus or an AAC.
	// Nil writes no audio, whatever the input holds.
	Audio AudioEncoder
	// Container is the output's container: a WebM, which holds AV1 and Opus,
	// or an MP4, which holds AV1, HEVC and AAC.
	Container Container
	// VideoStream, when not nil, is the container index of the video stream
	// to write: a video stream that is not an attached picture, within
	// Config.Caps. Nil is the stream Source.Info describes, the one with the
	// default disposition, else the first.
	VideoStream *int
	// AudioStreams are the container indexes of the audio streams to write,
	// in that order, each encoded with Audio, which must then be set. Empty
	// is the audio stream with the default disposition, else the first, else
	// none when the input has no audio stream.
	AudioStreams []int
	// Limits bound the run.
	Limits TranscodeLimits
}

// TranscodeLimits bound the one tool run of a Transcode call. A negative field
// is refused with ErrInvalidRequest. A zero field is the bound every tool run
// of the Processor has, from its Resources, except WallTime. CPUTime,
// AddressSpace and OutputBytes are enforced by the sandbox, so have no effect
// with Config.Unsandboxed.
type TranscodeLimits struct {
	// CPUTime is the CPU time the run may use, summed over its threads, as
	// Resources.CPUTime is for any run. It must be a whole number of seconds.
	// Zero means Resources.CPUTime. Over it the call fails with ErrTimeLimit.
	CPUTime time.Duration
	// WallTime is the wall-clock time the run may take. Zero means
	// Resources.TimeLimit, which no other ffmpeg run is held to. Over it the
	// call fails with ErrTimeLimit.
	WallTime time.Duration
	// Threads is the thread count of the decoder and of the filters; the
	// decoder uses at most two for a stream above 4096x2160. Zero means
	// Resources.Threads. It does not bound the encoders, which size their
	// threads from the host's processors.
	Threads int
	// AddressSpace is the address space the run may map, in bytes. Zero
	// means the limit of any run: Resources.Memory plus Resources.Map plus
	// 5 GiB. A run that needs more fails with a *ToolError from ffmpeg.
	AddressSpace int64
	// OutputBytes is the largest file the run may write, the output. Zero
	// means the limit of any run: Resources.Disk and a quarter. Over it the
	// call fails with ErrTooLarge.
	OutputBytes int64
}

// VideoEncoder is a video encoder of a Transcode: an SVTAV1 or an X265.
type VideoEncoder interface {
	videoArgs() ([]string, error)
	videoCodec() string
}

// AudioEncoder is an audio encoder of a Transcode: an Opus or an AAC.
type AudioEncoder interface {
	audioArgs() ([]string, error)
	audioCodec() string
}

// Container is the container of a Transcode: a WebM or an MP4.
type Container interface {
	containerArgs() []string
	extension() string
	holds(codec string) bool
}

// SVTAV1 encodes AV1 with SVT-AV1 at a constant quality, with no bitrate
// target.
type SVTAV1 struct {
	// Preset is the speed preset, 0 (slowest) to 13 (fastest).
	Preset int
	// CRF is the constant rate factor, 1 (best) to 63.
	CRF int
}

func (e SVTAV1) videoCodec() string { return "av1" }

func (e SVTAV1) videoArgs() ([]string, error) {
	if e.Preset < 0 || e.Preset > 13 {
		return nil, invalid("SVTAV1.Preset %d is not in 0 to 13", e.Preset)
	}
	if e.CRF < 1 || e.CRF > 63 {
		return nil, invalid("SVTAV1.CRF %d is not in 1 to 63", e.CRF)
	}
	return []string{
		"-c:v", "libsvtav1", "-preset", strconv.Itoa(e.Preset), "-crf", strconv.Itoa(e.CRF), "-b:v", "0",
		"-pix_fmt", "yuv420p",
	}, nil
}

// X265 encodes HEVC with x265 at a constant quality. In an MP4 the stream is
// tagged hvc1, the form that carries its parameter sets in the sample
// description, which Apple's players require. The encoder's own log level is
// fixed at info, which is what ffmpeg's default level gives it, so that the
// stream is the one plain ffmpeg writes.
type X265 struct {
	// Preset is the speed preset: ultrafast, superfast, veryfast, faster,
	// fast, medium, slow, slower, veryslow or placebo.
	Preset string
	// CRF is the constant rate factor, 0 (best) to 51.
	CRF int
}

// x265Presets are the presets X265.Preset may name.
var x265Presets = []string{
	"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow", "placebo",
}

func (e X265) videoCodec() string { return "hevc" }

func (e X265) videoArgs() ([]string, error) {
	if !slices.Contains(x265Presets, e.Preset) {
		return nil, invalid("X265.Preset %q is not one of %v", e.Preset, x265Presets)
	}
	if e.CRF < 0 || e.CRF > 51 {
		return nil, invalid("X265.CRF %d is not in 0 to 51", e.CRF)
	}
	return []string{
		"-c:v", "libx265", "-preset", e.Preset, "-crf", strconv.Itoa(e.CRF), "-pix_fmt", "yuv420p",
		"-tag:v", "hvc1", "-x265-params", "log-level=info",
	}, nil
}

// Opus encodes audio with libopus.
type Opus struct {
	// Bitrate is the target bitrate of each stream, in bits per second, 6000
	// to 510000.
	Bitrate int
}

func (e Opus) audioCodec() string { return "opus" }

func (e Opus) audioArgs() ([]string, error) {
	if e.Bitrate < 6_000 || e.Bitrate > 510_000 {
		return nil, invalid("Opus.Bitrate %d is not in 6000 to 510000", e.Bitrate)
	}
	return []string{"-c:a", "libopus", "-b:a", bitrate(e.Bitrate)}, nil
}

// AAC encodes audio with ffmpeg's own AAC-LC encoder.
type AAC struct {
	// Bitrate is the target bitrate of each stream, in bits per second, 8000
	// to 512000.
	Bitrate int
}

func (e AAC) audioCodec() string { return "aac" }

func (e AAC) audioArgs() ([]string, error) {
	if e.Bitrate < 8_000 || e.Bitrate > 512_000 {
		return nil, invalid("AAC.Bitrate %d is not in 8000 to 512000", e.Bitrate)
	}
	return []string{"-c:a", "aac", "-b:a", bitrate(e.Bitrate)}, nil
}

// bitrate spells bits per second as ffmpeg reads them, in kilobits when exact.
func bitrate(bps int) string {
	if bps%1000 == 0 {
		return strconv.Itoa(bps/1000) + "k"
	}
	return strconv.Itoa(bps)
}

// WebM writes a WebM file, which holds AV1 video and Opus audio.
type WebM struct {
	// BitExact makes the same streams give the same file: it writes no
	// random identifiers and no muxer version, and interleaves the streams
	// strictly by time, holding the others while a slow encoder has not yet
	// produced, which costs the memory of their packets meanwhile.
	BitExact bool
}

func (c WebM) extension() string { return ".webm" }

func (c WebM) holds(codec string) bool { return codec == "av1" || codec == "opus" }

func (c WebM) containerArgs() []string {
	var args []string
	if c.BitExact {
		args = append(args, bitExactArgs...)
	}
	return append(args, "-f", "webm")
}

// MP4 writes an MP4 file, which holds AV1 or HEVC video and AAC audio.
type MP4 struct {
	// FastStart puts the index before the media, so that playback can start
	// before the whole file has arrived. The file is written once more to
	// move it.
	FastStart bool
	// BitExact makes the same streams give the same file, as WebM.BitExact
	// does: no muxer version, and the streams interleaved strictly by time.
	BitExact bool
}

func (c MP4) extension() string { return ".mp4" }

func (c MP4) holds(codec string) bool { return codec == "av1" || codec == "hevc" || codec == "aac" }

func (c MP4) containerArgs() []string {
	var args []string
	if c.FastStart {
		args = append(args, "-movflags", "+faststart")
	}
	if c.BitExact {
		args = append(args, bitExactArgs...)
	}
	return append(args, "-f", "mp4")
}

// bitExactArgs make a muxer's output depend on the streams alone. By default
// the muxer writes the packets it holds once they span more than 10 s
// without a packet of a slower stream, so where the first packet of a slow
// encoder lands depends on how fast the host ran it; 0 turns that off.
var bitExactArgs = []string{"-fflags", "+bitexact", "-max_interleave_delta", "0"}

// Transcoded describes the file Source.Transcode wrote.
type Transcoded struct {
	// Name is the file's name in the call's directory: 000 and the
	// container's extension, .webm or .mp4.
	Name string
	// Bytes is the file's size.
	Bytes int64
	// Wall is the wall-clock time of the tool run.
	Wall time.Duration
}

// Transcode encodes the video, and the audio t asks for, into one file in dir
// with the encoders and container t names, in one ffmpeg run under t.Limits.
// The Source must be a Video. dir, which may be relative, must exist and be
// empty. The input is read as Open admitted it: through its descriptor, with
// the demuxer of its type and no reference to another file followed. Under
// the sandbox, where the Processor confines render runs (see
// Processor.Confinement), the run is confined in the same way, with dir as
// the one directory it may write.
//
// The Usage covers the run, also when it fails. Calls on one Source run one
// at a time. A call that fails or is cancelled leaves dir as it found it.
//
// Errors wrap ErrInvalidRequest for a Source that is not a Video or a t that
// is incomplete, out of range or names an encoder its container does not
// hold or a stream that is not of the right kind; ErrTooLarge for a chosen
// video stream over Config.Caps or an output over Limits.OutputBytes;
// ErrTimeLimit at Limits.CPUTime or Limits.WallTime; ErrInputChanged,
// ErrClosed or ctx's error; or they are a *ToolError. t is checked before
// any tool runs.
func (s *Source) Transcode(ctx context.Context, dir string, t Transcode) (Transcoded, Usage, error) {
	out, usage, err := s.transcode(ctx, dir, t)
	if err != nil {
		return Transcoded{}, usage, fmt.Errorf("transcode: %w", err)
	}
	return out, usage, nil
}

// transcodeLimits are TranscodeLimits resolved against a Processor.
type transcodeLimits struct {
	rlimits rlimits
	wall    time.Duration
	threads int
}

// resolve returns l with its zero fields taken from res, which is resolved.
func (l TranscodeLimits) resolve(res Resources) (transcodeLimits, error) {
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"CPUTime", int64(l.CPUTime)},
		{"WallTime", int64(l.WallTime)},
		{"Threads", int64(l.Threads)},
		{"AddressSpace", l.AddressSpace},
		{"OutputBytes", l.OutputBytes},
	} {
		if f.v < 0 {
			return transcodeLimits{}, invalid("Limits.%s is negative: %d", f.name, f.v)
		}
	}
	if l.CPUTime != 0 && l.CPUTime%time.Second != 0 {
		return transcodeLimits{}, invalid("Limits.CPUTime %v is not a whole number of seconds", l.CPUTime)
	}
	if l.Threads > math.MaxInt32 {
		return transcodeLimits{}, invalid("Limits.Threads %d is above %d", l.Threads, math.MaxInt32)
	}
	out := transcodeLimits{rlimits: rlimitsFor(res), wall: res.TimeLimit, threads: res.Threads}
	if l.CPUTime != 0 {
		out.rlimits.CPU = uint64(l.CPUTime / time.Second)
	}
	if l.WallTime != 0 {
		out.wall = l.WallTime
	}
	if l.Threads != 0 {
		out.threads = l.Threads
	}
	if l.AddressSpace != 0 {
		out.rlimits.Address = uint64(l.AddressSpace)
	}
	if l.OutputBytes != 0 {
		out.rlimits.FileSize = uint64(l.OutputBytes)
	}
	return out, nil
}

// transcodePlan is a Transcode checked against a probed video: the streams,
// and the arguments that follow the input.
type transcodePlan struct {
	video  probedStream
	audio  []int
	output []string
	ext    string
}

// planTranscode checks t against the probe v and the caps c.
func planTranscode(t Transcode, v videoInfo, c caps) (transcodePlan, error) {
	if t.Video == nil {
		return transcodePlan{}, invalid("no video encoder")
	}
	if t.Container == nil {
		return transcodePlan{}, invalid("no container")
	}
	vargs, err := t.Video.videoArgs()
	if err != nil {
		return transcodePlan{}, err
	}
	if !t.Container.holds(t.Video.videoCodec()) {
		return transcodePlan{}, invalid("%T does not hold %s video", t.Container, t.Video.videoCodec())
	}
	var aargs []string
	if t.Audio != nil {
		if aargs, err = t.Audio.audioArgs(); err != nil {
			return transcodePlan{}, err
		}
		if !t.Container.holds(t.Audio.audioCodec()) {
			return transcodePlan{}, invalid("%T does not hold %s audio", t.Container, t.Audio.audioCodec())
		}
	} else if len(t.AudioStreams) > 0 {
		return transcodePlan{}, invalid("audio streams are named but there is no audio encoder")
	}

	plan := transcodePlan{ext: t.Container.extension()}
	index := v.stream
	if t.VideoStream != nil {
		index = *t.VideoStream
	}
	st, ok := v.streamAt(index)
	if !ok || st.kind != "video" || st.attachedPic {
		return transcodePlan{}, invalid("stream %d is not a video stream", index)
	}
	if st.width < 1 || st.height < 1 {
		return transcodePlan{}, invalid("video stream %d reports size %dx%d", index, st.width, st.height)
	}
	if st.width > maxDimension || st.height > maxDimension {
		return transcodePlan{}, tooLarge("video stream %d is %dx%d, the limit is %d on each side", index, st.width, st.height, maxDimension)
	}
	if px := int64(st.width) * int64(st.height); px > c.framePixels {
		return transcodePlan{}, tooLarge("video stream %d is %dx%d, %d pixels, the limit is %d", index, st.width, st.height, px, c.framePixels)
	}
	plan.video = st

	if t.Audio != nil {
		if len(t.AudioStreams) == 0 {
			if a, ok := v.defaultAudio(); ok {
				plan.audio = []int{a}
			}
		}
		for i, a := range t.AudioStreams {
			if st, ok := v.streamAt(a); !ok || st.kind != "audio" {
				return transcodePlan{}, invalid("stream %d is not an audio stream", a)
			}
			if slices.Contains(t.AudioStreams[:i], a) {
				return transcodePlan{}, invalid("audio stream %d is named twice", a)
			}
			plan.audio = append(plan.audio, a)
		}
	}

	plan.output = []string{"-map", fmt.Sprintf("0:%d", plan.video.index)}
	for _, a := range plan.audio {
		plan.output = append(plan.output, "-map", fmt.Sprintf("0:%d", a))
	}
	plan.output = append(plan.output, vargs...)
	if len(plan.audio) > 0 {
		plan.output = append(plan.output, aargs...)
	}
	plan.output = append(plan.output, t.Container.containerArgs()...)
	return plan, nil
}

// streamAt returns the stream with container index i.
func (v videoInfo) streamAt(i int) (probedStream, bool) {
	for _, s := range v.all {
		if s.index == i {
			return s, true
		}
	}
	return probedStream{}, false
}

// defaultAudio returns the index of the audio stream with the default
// disposition, else of the first audio stream; false when there is none.
func (v videoInfo) defaultAudio() (int, bool) {
	first := -1
	for _, s := range v.all {
		if s.kind != "audio" {
			continue
		}
		if s.isDefault {
			return s.index, true
		}
		if first < 0 {
			first = s.index
		}
	}
	return first, first >= 0
}

// transcodeArgs returns the ffmpeg arguments of a transcode by plan of the
// input at inputPath, read with dec, to output, with threads for the decoder
// and filters. Like other runs it logs at errors, so that a failed run's
// standard error is the failure and not a summary of the input; X265 pins its
// own level because x265 writes the level it ran at into the stream's header.
func transcodeArgs(dec decoder, inputPath string, plan transcodePlan, threads int, output string) []string {
	args := ffmpegArgs(threads)
	args = append(args, "-threads", strconv.Itoa(ffmpegThreadsFor(threads, plan.video.width, plan.video.height)))
	args = append(args, inputArgs(dec, inputPath)...)
	args = append(args, plan.output...)
	return append(args, output)
}

// transcodeReadPaths are what a confined transcode run may read beyond a
// render run: the NUMA topology, from which an encoder's thread pools are
// sized.
var transcodeReadPaths = []string{"/sys/devices/system/node"}

// transcodeEnv is added to a transcode run's environment. SVT-AV1 logs to
// standard error by its own level, not ffmpeg's; errors alone keep a failed
// run's standard error to the failure.
var transcodeEnv = []string{"SVT_LOG=1"}

func (s *Source) transcode(ctx context.Context, dir string, t Transcode) (Transcoded, Usage, error) {
	var m meter
	limits, err := t.Limits.resolve(s.p.res)
	if err != nil {
		return Transcoded{}, m.usage, err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return Transcoded{}, m.usage, err
	}
	if err := s.lock(ctx); err != nil {
		return Transcoded{}, m.usage, err
	}
	defer s.unlock()
	if s.closed {
		return Transcoded{}, m.usage, fmt.Errorf("source: %w", ErrClosed)
	}
	if s.kind != Video {
		return Transcoded{}, m.usage, invalid("a %s cannot be transcoded, only a video", s.kind)
	}
	plan, err := planTranscode(t, s.probe, s.p.caps)
	if err != nil {
		return Transcoded{}, m.usage, err
	}
	if err := requireEmptyDir(dir); err != nil {
		return Transcoded{}, m.usage, err
	}
	if err := s.checkIdentity(); err != nil {
		return Transcoded{}, m.usage, err
	}
	name := numbered(0, plan.ext)
	output := filepath.Join(dir, name)
	env := s.p.env(&m, s.file).pinnedTo(s.file, s.id, "since it was opened")

	runCtx, cancel := context.WithTimeout(ctx, limits.wall)
	defer cancel()
	runCtx = withRunLimits(withRenderRun(runCtx, renderRun{dir: dir, read: transcodeReadPaths, env: transcodeEnv}), limits.rlimits)
	start := time.Now()
	_, err = env.run(runCtx, "ffmpeg", transcodeArgs(s.dec, s.ref, plan, limits.threads, output)...)
	wall := time.Since(start)
	if err != nil {
		err = transcodeError(ctx, runCtx, err, output, limits, !s.p.unsandboxed)
		clearDir(dir)
		return Transcoded{}, m.usage, err
	}
	info, err := os.Stat(output)
	if err != nil {
		clearDir(dir)
		return Transcoded{}, m.usage, fmt.Errorf("the output was not written: %w", withoutPath(err))
	}
	return Transcoded{Name: name, Bytes: info.Size(), Wall: wall}, m.usage, nil
}

// transcodeError attributes the failure err of a run in runCtx, derived from
// the caller's ctx, that wrote output under limits: the run's own wall limit,
// or, when the sandbox set the limits, an output that reached the file size
// limit, which the kernel enforces by a signal or by failing the write.
func transcodeError(ctx, runCtx context.Context, err error, output string, limits transcodeLimits, sandboxed bool) error {
	if ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: the run reached its wall time limit of %v", ErrTimeLimit, limits.wall)
	}
	var te *ToolError
	if !sandboxed || !errors.As(err, &te) {
		return err
	}
	size := int64(-1)
	if info, serr := os.Stat(output); serr == nil {
		size = info.Size()
	}
	if te.Signal == syscall.SIGXFSZ || (limits.rlimits.FileSize > 0 && size >= 0 && uint64(size) >= limits.rlimits.FileSize) {
		return fmt.Errorf("%w: the output reached the limit of %d bytes: %w", ErrTooLarge, limits.rlimits.FileSize, err)
	}
	return err
}
