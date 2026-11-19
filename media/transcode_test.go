package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// av1WebM and hevcMP4 are the two transcodes the argv tests pin: AV1 with
// Opus in a reproducible WebM, and HEVC with AAC in an MP4 with its index
// first.
var (
	av1WebM = Transcode{
		Video: SVTAV1{Preset: 5, CRF: 18}, Audio: Opus{Bitrate: 256_000}, Container: WebM{BitExact: true},
	}
	hevcMP4 = Transcode{
		Video: X265{Preset: "medium", CRF: 18}, Audio: AAC{Bitrate: 256_000}, Container: MP4{FastStart: true},
	}
)

// intPtr returns a pointer to i, for Transcode.VideoStream.
func intPtr(i int) *int { return &i }

// avClip writes a 2-second 320x240 H.264 clip with an AAC tone in an MP4.
func avClip(t *testing.T, env toolEnv) string {
	t.Helper()
	return writeClip(t, env, ".mp4",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest")
}

// probeStreams returns ffprobe's codec_type, codec_name and codec_tag_string
// of each stream of path, and its width and height.
func probeStreams(t *testing.T, env toolEnv, path string) []map[string]any {
	t.Helper()
	res, err := env.run(context.Background(), "ffprobe", "-v", "error", "-of", "json",
		"-show_entries", "stream=codec_type,codec_name,codec_tag_string,width,height,pix_fmt", path)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Streams []map[string]any `json:"streams"`
	}
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		t.Fatal(err)
	}
	return out.Streams
}

// openVideo opens path through p and closes the Source when the test ends.
func openVideo(t *testing.T, p *Processor, path string) *Source {
	t.Helper()
	src, _, err := p.Open(context.Background(), path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// The two pinned transcodes produce these arguments, the first stream chosen
// by the caller and the audio by default. The encoder arguments are
// reproduced exactly; what precedes them is the input prefix of every ffmpeg
// run.
func TestTranscodeArgs(t *testing.T) {
	t.Parallel()
	probe := videoInfo{stream: 0, width: 720, height: 388, all: []probedStream{
		{index: 0, kind: "video", width: 720, height: 388, isDefault: true},
		{index: 1, kind: "audio", isDefault: true},
	}}
	prefix := []string{
		"-hide_banner", "-nostdin", "-v", "error", "-y", "-max_alloc", "1073741824", "-filter_threads", "32",
		"-threads", "32", "-protocol_whitelist", "file", "-format_whitelist", "mov", "-enable_drefs", "0",
		"-f", "mov", "-i", "/dev/fd/3",
	}
	for _, c := range []struct {
		name string
		t    Transcode
		want []string
	}{
		{"av1-webm", av1WebM, []string{
			"-map", "0:0", "-map", "0:1",
			"-c:v", "libsvtav1", "-preset", "5", "-crf", "18", "-b:v", "0", "-pix_fmt", "yuv420p",
			"-c:a", "libopus", "-b:a", "256k",
			"-fflags", "+bitexact", "-max_interleave_delta", "0", "-f", "webm", "/out/000.webm",
		}},
		{"hevc-mp4", hevcMP4, []string{
			"-map", "0:0", "-map", "0:1",
			"-c:v", "libx265", "-preset", "medium", "-crf", "18", "-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-x265-params", "log-level=info",
			"-c:a", "aac", "-b:a", "256k",
			"-movflags", "+faststart", "-f", "mp4", "/out/000.mp4",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := c.t
			tr.VideoStream = intPtr(0)
			plan, err := planTranscode(tr, probe, ceilingCaps)
			if err != nil {
				t.Fatal(err)
			}
			got := transcodeArgs(decoder{"ffmpeg", "mov"}, inputFDPath, plan, 32, "/out/"+numbered(0, plan.ext))
			if want := append(slices.Clone(prefix), c.want...); !slices.Equal(got, want) {
				t.Errorf("args =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

func TestTranscodeContainerArgs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		c    Container
		want []string
	}{
		{WebM{}, []string{"-f", "webm"}},
		{WebM{BitExact: true}, []string{"-fflags", "+bitexact", "-max_interleave_delta", "0", "-f", "webm"}},
		{MP4{}, []string{"-f", "mp4"}},
		{MP4{FastStart: true, BitExact: true}, []string{
			"-movflags", "+faststart", "-fflags", "+bitexact", "-max_interleave_delta", "0", "-f", "mp4",
		}},
	} {
		if got := c.c.containerArgs(); !slices.Equal(got, c.want) {
			t.Errorf("%+v: args %q, want %q", c.c, got, c.want)
		}
	}
}

func TestTranscodeStreamChoice(t *testing.T) {
	t.Parallel()
	probe := videoInfo{stream: 1, width: 320, height: 240, all: []probedStream{
		{index: 0, kind: "video", width: 64, height: 64, attachedPic: true},
		{index: 1, kind: "video", width: 320, height: 240, isDefault: true},
		{index: 2, kind: "audio"},
		{index: 3, kind: "audio", isDefault: true},
		{index: 4, kind: "video", width: 160, height: 120},
		{index: 5, kind: "subtitle"},
	}}
	maps := func(plan transcodePlan) []string {
		var out []string
		for i, a := range plan.output {
			if a == "-map" || a == "-an" {
				out = append(out, a, plan.output[i+1])
			}
		}
		return out
	}
	for _, c := range []struct {
		name  string
		video *int
		audio AudioEncoder
		ids   []int
		want  []string
	}{
		{"defaults", nil, AAC{Bitrate: 128_000}, nil, []string{"-map", "0:1", "-map", "0:3"}},
		{"no audio encoder", nil, nil, nil, []string{"-map", "0:1"}},
		{"named", intPtr(4), AAC{Bitrate: 128_000}, []int{2, 3}, []string{"-map", "0:4", "-map", "0:2", "-map", "0:3"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := hevcMP4
			tr.VideoStream, tr.Audio, tr.AudioStreams = c.video, c.audio, c.ids
			plan, err := planTranscode(tr, probe, ceilingCaps)
			if err != nil {
				t.Fatal(err)
			}
			if got := maps(plan); !slices.Equal(got, c.want) {
				t.Errorf("maps = %q, want %q", got, c.want)
			}
			if c.audio == nil && slices.Contains(plan.output, "-c:a") {
				t.Errorf("args %q encode audio with no audio encoder", plan.output)
			}
		})
	}

	// With no audio stream, the default choice writes none.
	silent := videoInfo{stream: 0, width: 320, height: 240, all: []probedStream{{index: 0, kind: "video", width: 320, height: 240}}}
	plan, err := planTranscode(hevcMP4, silent, ceilingCaps)
	if err != nil {
		t.Fatal(err)
	}
	if got := maps(plan); !slices.Equal(got, []string{"-map", "0:0"}) || slices.Contains(plan.output, "-c:a") {
		t.Errorf("args = %q, want the video stream alone and no audio encoder", plan.output)
	}
}

func TestTranscodeRefusals(t *testing.T) {
	t.Parallel()
	probe := videoInfo{stream: 0, width: 320, height: 240, all: []probedStream{
		{index: 0, kind: "video", width: 320, height: 240},
		{index: 1, kind: "audio"},
		{index: 2, kind: "video", width: 64, height: 64, attachedPic: true},
		{index: 3, kind: "video", width: 20000, height: 100},
		{index: 4, kind: "video", width: 4000, height: 4000},
		{index: 5, kind: "video", width: 320, height: 240, durationMS: maxDurationMS + 1},
		{index: 6, kind: "video", width: 320, height: 240, durationMS: maxDurationMS},
	}}
	with := func(f func(*Transcode)) Transcode {
		tr := hevcMP4
		f(&tr)
		return tr
	}
	invalidCases := []struct {
		name string
		t    Transcode
	}{
		{"no video encoder", with(func(t *Transcode) { t.Video = nil })},
		{"no container", with(func(t *Transcode) { t.Container = nil })},
		{"svt preset low", with(func(t *Transcode) { t.Video, t.Container = SVTAV1{Preset: -1, CRF: 30}, MP4{} })},
		{"svt preset high", with(func(t *Transcode) { t.Video, t.Container = SVTAV1{Preset: 14, CRF: 30}, MP4{} })},
		{"svt crf zero", with(func(t *Transcode) { t.Video, t.Container = SVTAV1{Preset: 8}, MP4{} })},
		{"svt crf high", with(func(t *Transcode) { t.Video, t.Container = SVTAV1{Preset: 8, CRF: 64}, MP4{} })},
		{"x265 no preset", with(func(t *Transcode) { t.Video = X265{CRF: 18} })},
		{"x265 unknown preset", with(func(t *Transcode) { t.Video = X265{Preset: "medium:pools=1", CRF: 18} })},
		{"x265 crf negative", with(func(t *Transcode) { t.Video = X265{Preset: "fast", CRF: -1} })},
		{"x265 crf high", with(func(t *Transcode) { t.Video = X265{Preset: "fast", CRF: 52} })},
		{"opus low", with(func(t *Transcode) { t.Audio, t.Container = Opus{Bitrate: 5_999}, WebM{} })},
		{"opus high", with(func(t *Transcode) { t.Audio, t.Container = Opus{Bitrate: 510_001}, WebM{} })},
		{"aac zero", with(func(t *Transcode) { t.Audio = AAC{} })},
		{"aac high", with(func(t *Transcode) { t.Audio = AAC{Bitrate: 512_001} })},
		{"hevc in webm", with(func(t *Transcode) { t.Audio, t.Container = nil, WebM{} })},
		{"aac in webm", with(func(t *Transcode) { t.Video, t.Container = SVTAV1{Preset: 8, CRF: 30}, WebM{} })},
		{"opus in mp4", with(func(t *Transcode) { t.Audio = Opus{Bitrate: 64_000} })},
		{"audio streams without an encoder", with(func(t *Transcode) { t.Audio, t.AudioStreams = nil, []int{1} })},
		{"video stream absent", with(func(t *Transcode) { t.VideoStream = intPtr(9) })},
		{"video stream negative", with(func(t *Transcode) { t.VideoStream = intPtr(-1) })},
		{"video stream is audio", with(func(t *Transcode) { t.VideoStream = intPtr(1) })},
		{"video stream is a picture", with(func(t *Transcode) { t.VideoStream = intPtr(2) })},
		{"audio stream is video", with(func(t *Transcode) { t.AudioStreams = []int{0} })},
		{"audio stream absent", with(func(t *Transcode) { t.AudioStreams = []int{7} })},
		{"audio stream twice", with(func(t *Transcode) { t.AudioStreams = []int{1, 1} })},
	}
	for _, c := range invalidCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := planTranscode(c.t, probe, ceilingCaps); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	tooLargeCases := []struct {
		name   string
		stream int
		caps   caps
	}{
		{"over the fixed side", 3, ceilingCaps},
		{"over MaxFramePixels", 4, caps{framePixels: 4000 * 3999}},
		{"over the longest duration", 5, ceilingCaps},
	}
	for _, c := range tooLargeCases {
		t.Run(c.name, func(t *testing.T) {
			tr := with(func(t *Transcode) { t.VideoStream = intPtr(c.stream) })
			if _, err := planTranscode(tr, probe, c.caps); !errors.Is(err, ErrTooLarge) {
				t.Errorf("err = %v, want ErrTooLarge", err)
			}
		})
	}
}

// A video stream the caller names is held to the duration Open holds the
// default stream to, from the probe's own duration for it, else the
// container's.
func TestTranscodeProbeReadsEachStreamsDuration(t *testing.T) {
	t.Parallel()
	v, err := parseVideoProbe([]byte(`{"streams":[
		{"index":0,"codec_type":"video","width":320,"height":240,"duration":"10.000"},
		{"index":1,"codec_type":"video","width":320,"height":240,"duration":"30000.5"},
		{"index":2,"codec_type":"video","width":320,"height":240,"duration":"N/A"},
		{"index":3,"codec_type":"audio"}],
		"format":{"duration":"20.0"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{10_000, 30_000_500, 20_000, 20_000} {
		if got := v.all[i].durationMS; got != want {
			t.Errorf("stream %d: durationMS = %d, want %d", i, got, want)
		}
	}
}

func TestTranscodeLimitsResolve(t *testing.T) {
	t.Parallel()
	res := defaultResources()
	got, err := TranscodeLimits{}.resolve(res)
	if err != nil {
		t.Fatal(err)
	}
	if want := (transcodeLimits{rlimits: rlimitsFor(res), wall: res.TimeLimit, threads: res.Threads}); got != want {
		t.Errorf("zero limits = %+v, want the Processor's %+v", got, want)
	}
	got, err = TranscodeLimits{
		CPUTime: 20 * time.Hour, WallTime: 90 * time.Minute, Threads: 32, AddressSpace: 32 << 30, OutputBytes: 10 << 30,
	}.resolve(res)
	if err != nil {
		t.Fatal(err)
	}
	want := transcodeLimits{
		rlimits: rlimits{CPU: 20 * 3600, Address: 32 << 30, FileSize: 10 << 30}, wall: 90 * time.Minute, threads: 32,
	}
	if got != want {
		t.Errorf("limits = %+v, want %+v", got, want)
	}
	for _, l := range []TranscodeLimits{
		{CPUTime: -time.Second},
		{WallTime: -1},
		{Threads: -1},
		{AddressSpace: -1},
		{AddressSpace: 2<<30 - 1},
		{OutputBytes: -1},
		{CPUTime: 1500 * time.Millisecond},
		{Threads: 1 << 40},
	} {
		if _, err := l.resolve(res); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v: err = %v, want ErrInvalidRequest", l, err)
		}
	}
}

// Each pinned transcode, run on a clip, gives the argv in its golden file and
// a file of the streams it names.
func TestTranscodeGoldenArgv(t *testing.T) {
	input := goldenClip(t, func(env toolEnv) string { return avClip(t, env) })
	for _, c := range []struct {
		name   string
		t      Transcode
		codecs []string
		tag    string
	}{
		{"transcode-av1-webm", av1WebM, []string{"video av1", "audio opus"}, ""},
		{"transcode-hevc-mp4", hevcMP4, []string{"video hevc", "audio aac"}, "hvc1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, rec, g := goldenProcessor(t, c.name, Config{})
			src := openVideo(t, p, input)
			out := t.TempDir()
			g.out = out
			tr := c.t
			tr.VideoStream = intPtr(0)
			got, usage, err := src.Transcode(context.Background(), out, tr)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.name, goldenArgv(rec.calls[len(rec.calls)-1:], out))
			if testing.Short() {
				return
			}
			env := requireFFmpeg(t)
			path := filepath.Join(out, got.Name)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Bytes != info.Size() || got.Bytes == 0 || got.Wall <= 0 {
				t.Errorf("Transcoded = %+v, the file has %d bytes", got, info.Size())
			}
			if usage.CPU <= 0 || usage.MaxRSS <= 0 {
				t.Errorf("usage = %+v, want CPU and MaxRSS", usage)
			}
			streams := probeStreams(t, env, path)
			var codecs []string
			for _, s := range streams {
				codecs = append(codecs, s["codec_type"].(string)+" "+s["codec_name"].(string))
			}
			if !slices.Equal(codecs, c.codecs) {
				t.Errorf("streams = %q, want %q", codecs, c.codecs)
			}
			v := streams[0]
			if v["width"] != 320.0 || v["height"] != 240.0 || v["pix_fmt"] != "yuv420p" {
				t.Errorf("video stream = %v, want 320x240 yuv420p", v)
			}
			if c.tag != "" && v["codec_tag_string"] != c.tag {
				t.Errorf("video tag = %v, want %s", v["codec_tag_string"], c.tag)
			}
		})
	}
}

// What the call adds to the encoder arguments (the input prefix, the forced
// demuxer and muxer, the limits and the sandbox) changes no byte of the file:
// each pinned transcode equals plain ffmpeg's output with the same encoder
// arguments on the same host.
func TestTranscodeEqualsPlainFFmpeg(t *testing.T) {
	env := requireFFmpeg(t)
	input := avClip(t, env)
	p := requireProcessor(t)
	src := openVideo(t, p, input)
	for _, c := range []struct {
		name  string
		t     Transcode
		plain []string
	}{
		{"av1-webm", av1WebM, []string{
			"-c:v", "libsvtav1", "-preset", "5", "-crf", "18", "-b:v", "0", "-pix_fmt", "yuv420p",
			"-c:a", "libopus", "-b:a", "256k", "-fflags", "+bitexact", "-max_interleave_delta", "0", "out.webm",
		}},
		{"hevc-mp4", hevcMP4, []string{
			"-c:v", "libx265", "-preset", "medium", "-crf", "18", "-pix_fmt", "yuv420p", "-tag:v", "hvc1",
			"-c:a", "aac", "-b:a", "256k", "-movflags", "+faststart", "out.mp4",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := t.TempDir()
			got, _, err := src.Transcode(context.Background(), out, c.t)
			if err != nil {
				t.Fatal(err)
			}
			call, err := os.ReadFile(filepath.Join(out, got.Name))
			if err != nil {
				t.Fatal(err)
			}
			plainDir := t.TempDir()
			plainOut := filepath.Join(plainDir, c.plain[len(c.plain)-1])
			args := append([]string{"-nostdin", "-y", "-i", input, "-map", "0:v:0", "-map", "0:a:0?"}, c.plain[:len(c.plain)-1]...)
			if _, err := env.run(context.Background(), "ffmpeg", append(args, plainOut)...); err != nil {
				t.Fatal(err)
			}
			plain, err := os.ReadFile(plainOut)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(call, plain) {
				t.Errorf("the call wrote %d bytes, plain ffmpeg %d; they differ", len(call), len(plain))
			}
		})
	}
}

// An MP4 written with FastStart has its index before its media, and one
// written without it after.
func TestTranscodeMP4FastStart(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := openVideo(t, p, avClip(t, env))
	for _, fast := range []bool{true, false} {
		out := t.TempDir()
		tr := Transcode{Video: SVTAV1{Preset: 12, CRF: 40}, Audio: AAC{Bitrate: 64_000}, Container: MP4{FastStart: fast}}
		got, _, err := src.Transcode(context.Background(), out, tr)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(out, got.Name))
		if err != nil {
			t.Fatal(err)
		}
		moov, mdat := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat"))
		if moov < 0 || mdat < 0 || (moov < mdat) != fast {
			t.Errorf("FastStart %v: moov at %d, mdat at %d", fast, moov, mdat)
		}
		if codecs := probeStreams(t, env, filepath.Join(out, got.Name)); codecs[0]["codec_name"] != "av1" {
			t.Errorf("FastStart %v: video stream %v, want av1", fast, codecs[0])
		}
	}
}

// BitExact makes two transcodes of one input the same file; without it a
// WebM differs by its random identifiers.
func TestTranscodeBitExact(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := openVideo(t, p, avClip(t, env))
	run := func(exact bool) []byte {
		out := t.TempDir()
		tr := Transcode{Video: SVTAV1{Preset: 12, CRF: 40}, Audio: Opus{Bitrate: 64_000}, Container: WebM{BitExact: exact}}
		got, _, err := src.Transcode(context.Background(), out, tr)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(out, got.Name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if !bytes.Equal(run(true), run(true)) {
		t.Error("two BitExact transcodes differ")
	}
	if bytes.Equal(run(false), run(false)) {
		t.Error("two WebM transcodes without BitExact are the same; the test no longer shows what BitExact changes")
	}
}

// The streams written are the caller's, or by default those with the default
// disposition.
func TestTranscodeWritesTheChosenStreams(t *testing.T) {
	env := requireFFmpeg(t)
	input := writeClip(t, env, ".mkv",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10:duration=1",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1:sample_rate=48000",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=1:sample_rate=44100",
		"-map", "0", "-map", "1", "-map", "2", "-map", "3",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "flac", "-ac:a:1", "2",
		"-disposition:v:0", "0", "-disposition:v:1", "default", "-disposition:a:0", "0", "-disposition:a:1", "default")
	p := requireProcessor(t)
	src := openVideo(t, p, input)
	base := Transcode{Video: SVTAV1{Preset: 12, CRF: 40}, Audio: Opus{Bitrate: 64_000}, Container: WebM{}}
	describe := func(t *testing.T, path string) []string {
		res, err := env.run(context.Background(), "ffprobe", "-v", "error", "-of", "csv=p=0",
			"-show_entries", "stream=codec_type,width,channels", path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(res.Stdout))
	}
	for _, c := range []struct {
		name  string
		video *int
		audio []int
		want  []string
	}{
		{"defaults", nil, nil, []string{"video,160", "audio,2"}},
		{"named", intPtr(0), []int{2, 3}, []string{"video,320", "audio,1", "audio,2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := base
			tr.VideoStream, tr.AudioStreams = c.video, c.audio
			out := t.TempDir()
			got, _, err := src.Transcode(context.Background(), out, tr)
			if err != nil {
				t.Fatal(err)
			}
			if streams := describe(t, filepath.Join(out, got.Name)); !slices.Equal(streams, c.want) {
				t.Errorf("streams = %q, want %q", streams, c.want)
			}
		})
	}
}

func TestTranscodeRefusesWithoutRunning(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	video := openVideo(t, p, avClip(t, env))
	still := openVideo(t, p, writeClip(t, env, ".png", "-f", "lavfi", "-i", "color=c=red:size=16x16", "-frames:v", "1"))
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before := rec.count()
	for _, c := range []struct {
		name string
		src  *Source
		dir  string
		t    Transcode
	}{
		{"a still", still, t.TempDir(), hevcMP4},
		{"a directory that is not empty", video, nonEmpty, hevcMP4},
		{"a missing directory", video, filepath.Join(t.TempDir(), "missing"), hevcMP4},
		{"an incomplete request", video, t.TempDir(), Transcode{Video: X265{Preset: "medium", CRF: 18}}},
		{"a negative limit", video, t.TempDir(), Transcode{Video: X265{Preset: "medium", CRF: 18}, Container: MP4{}, Limits: TranscodeLimits{Threads: -1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := c.src.Transcode(context.Background(), c.dir, c.t); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("%d tool runs for refused calls, want none", n)
	}
	if err := video.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := video.Transcode(context.Background(), t.TempDir(), hevcMP4); !errors.Is(err, ErrClosed) {
		t.Errorf("after Close: err = %v, want ErrClosed", err)
	}
}

// The wall time limit, a cancelled context and a changed input each end the
// call with their own error and leave the directory empty.
func TestTranscodeFailuresLeaveTheDirectoryEmpty(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	long := writeClip(t, env, ".mp4", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=60",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	slow := Transcode{Video: SVTAV1{Preset: 2, CRF: 18}, Container: WebM{}}
	empty := func(t *testing.T, dir string) {
		t.Helper()
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("the call left %d entries in its directory", len(entries))
		}
	}

	t.Run("wall time", func(t *testing.T) {
		src := openVideo(t, p, long)
		out := t.TempDir()
		tr := slow
		tr.Limits.WallTime = time.Second
		_, _, err := src.Transcode(context.Background(), out, tr)
		if !errors.Is(err, ErrTimeLimit) || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want ErrTimeLimit and not the context's error", err)
		}
		empty(t, out)
	})

	t.Run("cancelled", func(t *testing.T) {
		src := openVideo(t, p, long)
		out := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, err := src.Transcode(ctx, out, slow)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTimeLimit) {
			t.Errorf("err = %v, want the context's error and not ErrTimeLimit", err)
		}
		empty(t, out)
	})

	t.Run("input changed", func(t *testing.T) {
		path := avClip(t, env)
		src := openVideo(t, p, path)
		if err := os.Chtimes(path, time.Now(), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		out := t.TempDir()
		if _, _, err := src.Transcode(context.Background(), out, hevcMP4); !errors.Is(err, ErrInputChanged) {
			t.Errorf("err = %v, want ErrInputChanged", err)
		}
		empty(t, out)
	})
}

// Above 4096x2160 the decoder gets two threads, whatever the call's count,
// and the filters keep it.
func TestTranscodeDecoderThreadsAboveFourK(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		w, h int
		want string
	}{{4096, 2160, "32"}, {4096, 2161, "2"}} {
		probe := videoInfo{stream: 0, width: c.w, height: c.h, all: []probedStream{{index: 0, kind: "video", width: c.w, height: c.h}}}
		plan, err := planTranscode(hevcMP4, probe, ceilingCaps)
		if err != nil {
			t.Fatal(err)
		}
		args := transcodeArgs(decoder{"ffmpeg", "mov"}, inputFDPath, plan, 32, "/out/000.mp4")
		if i := slices.Index(args, "-threads"); i < 0 || args[i+1] != c.want {
			t.Errorf("%dx%d: args %q, want -threads %s", c.w, c.h, args, c.want)
		}
		if i := slices.Index(args, "-filter_threads"); i < 0 || args[i+1] != "32" {
			t.Errorf("%dx%d: args %q, want -filter_threads 32", c.w, c.h, args)
		}
	}
}

// A rotated video is written upright.
func TestTranscodeAppliesTheRotation(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := openVideo(t, p, rotatedClip(t, env))
	out := t.TempDir()
	got, _, err := src.Transcode(context.Background(), out, Transcode{Video: SVTAV1{Preset: 12, CRF: 40}, Container: WebM{}})
	if err != nil {
		t.Fatal(err)
	}
	v := probeStreams(t, env, filepath.Join(out, got.Name))[0]
	if info := src.Info(); v["width"] != float64(info.Width) || v["height"] != float64(info.Height) || info.Width != 120 {
		t.Errorf("output %vx%v, want the upright %dx%d", v["width"], v["height"], info.Width, info.Height)
	}
}

// The per-call thread count reaches the decoder and the filters.
func TestTranscodeUsesTheCallsThreads(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	src := openVideo(t, p, avClip(t, env))
	tr := Transcode{Video: SVTAV1{Preset: 12, CRF: 40}, Container: WebM{}, Limits: TranscodeLimits{Threads: 7}}
	if _, _, err := src.Transcode(context.Background(), t.TempDir(), tr); err != nil {
		t.Fatal(err)
	}
	argv := rec.calls[len(rec.calls)-1]
	for _, flag := range []string{"-threads", "-filter_threads"} {
		i := slices.Index(argv, flag)
		if i < 0 || argv[i+1] != "7" {
			t.Errorf("%s in %q, want 7", flag, argv)
		}
	}
}

// Config.Caps reaches Transcode: a video stream the caller names is judged
// against the Processor's caps, not the library's ceilings, though Open
// admitted the video by its default stream.
func TestTranscodeHonorsConfigCaps(t *testing.T) {
	env := requireFFmpeg(t)
	input := writeClip(t, env, ".mp4",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=1",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10:duration=1",
		"-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	p := requireProcessorWith(t, Config{Caps: Caps{MaxFramePixels: 64 * 64}})
	src := openVideo(t, p, input)
	rec := record(p)
	tr := Transcode{Video: SVTAV1{Preset: 12, CRF: 40}, Container: WebM{}, VideoStream: intPtr(1)}
	if _, _, err := src.Transcode(context.Background(), t.TempDir(), tr); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a stream over Config.Caps: err = %v, want ErrTooLarge", err)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("%d tool runs for the refused call, want none", n)
	}
	tr.VideoStream = intPtr(0)
	if _, _, err := src.Transcode(context.Background(), t.TempDir(), tr); err != nil {
		t.Errorf("the stream within Config.Caps: %v", err)
	}
}
