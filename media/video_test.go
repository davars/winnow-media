package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const stubVideoProbe = `{"streams":[{"index":0,"codec_type":"video","width":64,"height":48,` +
	`"duration":"10","nb_frames":"250","r_frame_rate":"25/1","start_time":"%s"}],` +
	`"format":{"duration":"10","start_time":"0"}}`

// videoStub is an toolEnv that answers the probe and records every ffmpeg and
// ffprobe run; the ffmpeg runs write nothing.
type videoStub struct {
	mu     sync.Mutex
	probe  string
	probes [][]string
	runs   [][]string
}

// processor returns a Processor whose ffprobe and ffmpeg runs the stub
// answers; magick and the rest run for real. The Processor is unsandboxed so
// that the stub sees the argv of every run.
func (s *videoStub) processor(t *testing.T) *Processor {
	t.Helper()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	p, err := newProcessor(context.Background(), Config{Unsandboxed: true, Dir: t.TempDir()}, func(next runner) runner {
		return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
			s.mu.Lock()
			switch filepath.Base(spec.Tool) {
			case "ffprobe":
				s.probes = append(s.probes, spec.Args)
				s.mu.Unlock()
				_, err := stdout.Write([]byte(s.probe))
				return toolResult{}, err
			case "ffmpeg":
				s.runs = append(s.runs, spec.Args)
				s.mu.Unlock()
				return toolResult{}, nil
			}
			s.mu.Unlock()
			return next(ctx, spec, stdout, stderr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// open opens an MP4 on a Processor the stub answers for.
func (s *videoStub) open(t *testing.T) (*Source, error) {
	t.Helper()
	path := writeFile(t, "input", ftypBox("isom", "isom", "mp42"))
	src, _, err := s.processor(t).Open(context.Background(), path, OpenOptions{})
	if err == nil {
		t.Cleanup(func() { _ = src.Close() })
	}
	return src, err
}

// encode makes the Encode of sel that the stub's silent ffmpeg cannot satisfy;
// the argv of the runs is the subject.
func (s *videoStub) encode(t *testing.T, sel Select, targets ...Encoding) error {
	t.Helper()
	src, err := s.open(t)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = src.Encode(context.Background(), t.TempDir(), sel, targets...)
	return err
}

// forceSeek makes every sampling in the test use one seek per time.
func forceSeek(t *testing.T) { forceSelectLimit(t, 0) }

// forceSelectLimit sets the decoded-pixel limit under which a stream is read
// once, until the test ends.
func forceSelectLimit(t *testing.T, limit int64) {
	t.Helper()
	old := selectLimit
	selectLimit = limit
	t.Cleanup(func() { selectLimit = old })
}

func newVideoStub() *videoStub { return &videoStub{probe: fmt.Sprintf(stubVideoProbe, "0")} }

// Every ffmpeg run opens its input under the bounds the package applies to a
// hostile file, whichever entry point started it.
func TestEveryFFmpegRunCarriesTheInputBounds(t *testing.T) {
	webp := Encoding{Format: WebP, MaxEdge: 100, MaxArea: 10000}
	entries := map[string]func(*videoStub, *testing.T) error{
		"two targets": func(s *videoStub, t *testing.T) error {
			small, large := webp, webp
			small.MaxEdge, small.MaxArea, large.MaxEdge, large.MaxArea = 64, 64*64, 200, 200*200
			return s.encode(t, Select{Frames: Even(5)}, small, large)
		},
		"Even":        func(s *videoStub, t *testing.T) error { return s.encode(t, Select{Frames: Even(5)}, webp) },
		"Consecutive": func(s *videoStub, t *testing.T) error { return s.encode(t, Select{Frames: Consecutive(0, 3)}, webp) },
	}
	for _, path := range []struct {
		name  string
		limit int64
	}{{"seek", 0}, {"select", selectMaxPixels}} {
		for name, run := range entries {
			t.Run(path.name+"/"+name, func(t *testing.T) {
				forceSelectLimit(t, path.limit)
				testBounds(t, run)
			})
		}
	}
}

func testBounds(t *testing.T, run func(*videoStub, *testing.T) error) {
	stub := newVideoStub()
	// No run writes a frame, so the call ends in an error; the argv
	// of the runs is the subject.
	_ = run(stub, t)
	if len(stub.runs) == 0 {
		t.Fatal("no ffmpeg run")
	}
	for _, args := range stub.runs {
		joined := " " + strings.Join(args, " ") + " "
		for _, want := range []string{
			" -protocol_whitelist file ",
			" -max_alloc " + strconv.Itoa(ffmpegMaxAlloc) + " ",
			" -nostdin ",
			" -threads ",
			" -filter_threads ",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("ffmpeg run lacks %q: %q", strings.TrimSpace(want), args)
			}
		}
	}
	for _, args := range stub.probes {
		joined := " " + strings.Join(args, " ") + " "
		for _, want := range []string{
			" -protocol_whitelist file ",
			" -max_alloc " + strconv.Itoa(ffmpegMaxAlloc) + " ",
			" -threads ",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("ffprobe run lacks %q: %q", strings.TrimSpace(want), args)
			}
		}
	}
}

// The cap is a property of the input: an oversized stream is refused by Open
// before any ffmpeg run.
func TestVideoOpenRefusesOversizedBeforeSampling(t *testing.T) {
	t.Parallel()
	for _, size := range [][2]int{{16385, 100}, {100, 16385}} {
		stub := newVideoStub()
		stub.probe = fmt.Sprintf(`{"streams":[{"index":0,"codec_type":"video","width":%d,"height":%d,"duration":"10"}],"format":{"duration":"10"}}`, size[0], size[1])
		_, err := stub.open(t)
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("%v: err = %v, want ErrTooLarge", size, err)
		}
		if len(stub.runs) != 0 {
			t.Errorf("%v: %d ffmpeg runs before the refusal", size, len(stub.runs))
		}
	}
}

// A Source stages the samples of a call in its own scratch directory, not in
// the output directory.
func TestVideoSourceStagesSamplesInItsScratch(t *testing.T) {
	stub := newVideoStub()
	src, err := stub.open(t)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	_, _, _ = src.Encode(context.Background(), out, Select{Frames: Even(3)}, Encoding{Format: WebP, MaxEdge: 200})
	if len(stub.runs) == 0 {
		t.Fatal("no ffmpeg run")
	}
	for _, args := range stub.runs {
		sample := args[len(args)-1]
		if !strings.HasPrefix(sample, src.dir+string(filepath.Separator)) {
			t.Errorf("scratch sample %s is not under %s", sample, src.dir)
		}
	}
}

// -ss counts from the container's start, so a stream that starts later is
// sought by its own offset.
func TestVideoSeeksCountFromTheContainerStart(t *testing.T) {
	forceSeek(t)
	stub := newVideoStub()
	stub.probe = fmt.Sprintf(stubVideoProbe, "1.5")
	_ = stub.encode(t, Select{Frames: Even(15)}, Encoding{Format: WebP, MaxEdge: 200})
	if len(stub.runs) == 0 {
		t.Fatal("no ffmpeg run")
	}
	first := stub.runs[0]
	i := slices.Index(first, "-ss")
	// 10 s over 15 samples centres the first at 333 ms.
	if i < 0 || first[i+1] != "1.193000" {
		t.Errorf("first seek = %v, want 1.193000 (333 ms plus the 1.5 s offset, less the 640 ms margin of 16 frames at 25 fps)", first)
	}
}

func TestReducedSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		w, h, tw, th, margin int
		rw, rh               int
		ok                   bool
	}{
		{3840, 2160, 256, 144, 2, 512, 288, true},
		{3840, 2160, 1024, 576, 2, 2048, 1152, true},
		{3840, 2160, 1024, 576, 1, 1024, 576, true},
		{1280, 720, 200, 112, 2, 400, 225, true},
		{402, 208, 200, 103, 2, 400, 207, true},
		{640, 360, 640, 360, 2, 640, 360, false},
		{2160, 3840, 144, 256, 2, 288, 512, true},
	}
	for _, c := range cases {
		rw, rh, ok := reducedSize(c.w, c.h, c.tw, c.th, c.margin)
		if rw != c.rw || rh != c.rh || ok != c.ok {
			t.Errorf("reducedSize(%d,%d,%d,%d,%d) = %d,%d,%v, want %d,%d,%v", c.w, c.h, c.tw, c.th, c.margin, rw, rh, ok, c.rw, c.rh, c.ok)
		}
	}
}

func TestVideoPlanSourceIsLiteralOnlyForAReducedFrame(t *testing.T) {
	t.Parallel()
	info := videoInfo{width: 402, height: 208}
	small := encoding{maxEdge: 64, maxArea: 64 * 64}
	if got := (videoPlan{info: info}).source(small); got != (encodeSource{}) {
		t.Errorf("unreduced: %+v, want none", got)
	}
	// Reduced to 200x103, the small size computed from the frame is 64x32; from
	// the source it is 64x33.
	got := (videoPlan{info: info, reduced: true, fw: 200, fh: 103}).source(small)
	if got.width != 64 || got.height != 33 {
		t.Errorf("reduced: %+v, want 64x33", got)
	}
	args := encodeArgs([]encodeFrame{{input: "pam:/a"}}, got, small, encodeOutput{kind: outputStill, path: "/o.webp"})
	if !slices.Contains(args, "64x33!") {
		t.Errorf("args lack the literal size: %q", args)
	}
}

func sceneSizes(t *testing.T, env toolEnv, path string) []string {
	t.Helper()
	out := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%wx%h,%T,%z\n", path)))
	return strings.Split(out, "\n")
}

type runCounter struct {
	mu   sync.Mutex
	runs map[string][][]string
}

// withRunCounts routes the tool runs of p through a counter of the argv of each
// tool's runs.
func withRunCounts(p *Processor) *runCounter {
	c := &runCounter{runs: map[string][][]string{}}
	inner := p.run
	p.run = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		c.mu.Lock()
		c.runs[name] = append(c.runs[name], args)
		c.mu.Unlock()
		return inner(ctx, in, name, args...)
	}
	return c
}

// videoSampleFrames is the number of samples the tests take of a video.
const videoSampleFrames = 15

func TestWebPPairSeeksEverySample(t *testing.T) {
	forceSeek(t)
	env := requireFFmpeg(t)
	// 80 frames at 25 fps, so the run count shows one seek per sample.
	src := sdrClip(t, env, "320x240", 25, 80)
	p := requireProcessor(t)
	c := withRunCounts(p)
	out := t.TempDir()
	small, large := filepath.Join(out, "small.webp"), filepath.Join(out, "large.webp")
	if err := encodeWebPPair(p, context.Background(), src, small, large, 64, 200, 80); err != nil {
		t.Fatal(err)
	}
	if n := len(c.runs["ffmpeg"]); n != videoSampleFrames {
		t.Errorf("ffmpeg runs = %d, want %d seeks", n, videoSampleFrames)
	}
	for _, args := range c.runs["ffmpeg"] {
		joined := strings.Join(args, " ")
		if !slices.Contains(args, "-ss") || !slices.Contains(args, "-copyts") || strings.Contains(joined, "prev_t") || strings.Contains(joined, "concat") {
			t.Errorf("run does not seek to one sample: %v", args)
		}
	}
	for path, want := range map[string]string{small: "64x48", large: "200x150"} {
		scenes := sceneSizes(t, env, path)
		if len(scenes) != videoSampleFrames {
			t.Errorf("%s has %d frames, want %d", filepath.Base(path), len(scenes), videoSampleFrames)
		}
		if got := scenes[0]; got != want+",50,8" {
			t.Errorf("%s first frame = %s, want %s with a 50 cs delay at 8 bits", filepath.Base(path), got, want)
		}
	}
	if names := dirNames(t, out); !slices.Equal(names, []string{"large.webp", "small.webp"}) {
		t.Errorf("directory holds %v", names)
	}
}

func TestWebPPairDropsRepeatedFrames(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	src := writeClip(t, env, ".mp4", "-f", "lavfi", "-i", "color=c=red:size=160x120:rate=10:duration=4",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	out := t.TempDir()
	small, large := filepath.Join(out, "small.webp"), filepath.Join(out, "large.webp")
	if err := encodeWebPPair(requireProcessor(t), context.Background(), src, small, large, 64, 100, 80); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{small, large} {
		if n := len(sceneSizes(t, env, path)); n != 1 {
			t.Errorf("%s has %d frames, want the one that is left", filepath.Base(path), n)
		}
	}
}

// Both outputs come from one reduced decode, and the smaller one is sized
// from the source, not from the frame reduced for the larger.
func TestWebPPairSizeAHDRSmallFromTheSource(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	src := hlgClip(t, env, testsrc("402x208", 10, 3))
	out := t.TempDir()
	small, large := filepath.Join(out, "small.webp"), filepath.Join(out, "large.webp")
	if err := encodeWebPPair(requireProcessor(t), context.Background(), src, small, large, 64, 200, 80); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{small: "64x33", large: "200x103"} {
		if got := sceneSizes(t, env, path)[0]; !strings.HasPrefix(got, want+",") {
			t.Errorf("%s = %s, want %s", filepath.Base(path), got, want)
		}
	}
}

// A sample keeps the decoder's depth; the encode writes 8 bits whatever it
// was.
func TestVideoSamplesKeepTheDecodersDepth(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	ten := writeClip(t, env, ".mp4", append(testsrc("160x120", 10, 2), "-c:v", "libx264", "-preset", "ultrafast",
		"-profile:v", "high10", "-pix_fmt", "yuv420p10le")...)
	for name, c := range map[string]struct {
		path  string
		depth string
	}{
		"8-bit":  {sdrClip(t, env, "160x120", 10, 2), "8"},
		"10-bit": {ten, "16"},
	} {
		t.Run(name, func(t *testing.T) {
			dec := decoder{"ffmpeg", "mov"}
			plan, err := env.planVideo(context.Background(), dec, c.path, encoding{maxEdge: 100, maxArea: 10000})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			samples, err := env.sampleVideo(context.Background(), dec, c.path, dir, plan, []int64{500})
			if err != nil || len(samples) != 1 {
				t.Fatalf("samples = %v, %v", samples, err)
			}
			if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%m %z", samples[0].path))); got != "PAM "+c.depth {
				t.Errorf("sample = %q, want PAM at %s bits", got, c.depth)
			}
			if _, err := os.Stat(samples[0].path); err != nil {
				t.Fatal(err)
			}
			out := t.TempDir()
			images, _, err := openSourceOn(t, p, c.path, OpenOptions{}).Encode(context.Background(), out, Select{Frames: Even(2)},
				Encoding{Format: PNG, Quality: 90, MaxEdge: 100, MaxArea: 10000})
			if err != nil || len(images) == 0 {
				t.Fatalf("images = %v, %v", images, err)
			}
			if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%z", filepath.Join(out, images[0].Name)))); got != "8" {
				t.Errorf("output depth = %s, want 8", got)
			}
		})
	}
}

// vfScale returns the size of the scale in the -vf of an ffmpeg run, or ""
// when it has none.
func vfScale(args []string) string {
	i := slices.Index(args, "-vf")
	if i < 0 {
		return ""
	}
	m := regexp.MustCompile(`scale=(\d+):(\d+):`).FindStringSubmatch(args[i+1])
	if m == nil {
		return ""
	}
	return m[1] + "x" + m[2]
}

// An SDR frame is decoded at twice the target, not in full, and the output is
// the size of the upright source fitted to the target.
func TestSDRVideoIsDecodedAtTheReducedSize(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	src := sdrClip(t, env, "3840x2160", 5, 1)
	p := requireProcessor(t)
	rec := record(p)
	enc := encoding{format: WebP, ext: ".webp", quality: 80, maxEdge: 256, maxArea: 256 * 256}
	out := t.TempDir()
	images, _, err := openSourceOn(t, p, src, OpenOptions{}).Encode(context.Background(), out, Select{Frames: Even(2)},
		Encoding{Format: WebP, Quality: 80, MaxEdge: 256, MaxArea: 256 * 256})
	if err != nil || len(images) == 0 {
		t.Fatalf("images = %v, %v", images, err)
	}
	tw, th, _ := fitSize(3840, 2160, 256, 256*256)
	for _, im := range images {
		if im.Width != tw || im.Height != th {
			t.Errorf("%s is %dx%d, want %dx%d", im.Name, im.Width, im.Height, tw, th)
		}
	}
	if len(rec.ran("ffmpeg")) == 0 {
		t.Fatal("no ffmpeg run")
	}
	for _, args := range rec.ran("ffmpeg") {
		if got := vfScale(args); got != "512x288" {
			t.Errorf("ffmpeg scale = %q, want 512x288: %v", got, args)
		}
	}
	plan, err := env.planVideo(context.Background(), decoder{"ffmpeg", "mov"}, src, enc)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := env.sampleVideo(context.Background(), decoder{"ffmpeg", "mov"}, src, t.TempDir(), plan, []int64{300})
	if err != nil || len(samples) != 1 {
		t.Fatalf("samples = %v, %v", samples, err)
	}
	if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%wx%h", samples[0].path))); got != "512x288" {
		t.Errorf("sample is %s, want 512x288", got)
	}
	// A target at or above the source is not upscaled.
	big := encoding{format: WebP, ext: ".webp", quality: 80, maxEdge: 4000, maxArea: 4000 * 4000}
	if plan, err = env.planVideo(context.Background(), decoder{"ffmpeg", "mov"}, src, big); err != nil || plan.reduced || plan.filter != "" {
		t.Errorf("plan for a target above the source = %+v, %v", plan, err)
	}
}

// Encoding at two sizes decodes for the larger output, so that output is never
// upscaled from a frame reduced for the smaller one.
func TestWebPPairDecodeForTheLargerOutput(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	for name, c := range map[string]struct {
		clip func() string
		want func(string) bool
	}{
		"sdr": {
			func() string { return sdrClip(t, env, "1280x720", 10, 2) },
			func(s string) bool {
				var w, h int
				_, err := fmt.Sscanf(s, "%dx%d", &w, &h)
				return err == nil && w >= 400 && h >= 224
			},
		},
		"hdr": {
			func() string { return hlgClip(t, env, testsrc("402x208", 10, 2)) },
			func(s string) bool { return s == "200x103" },
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := requireProcessor(t)
			counter := withRunCounts(p)
			out := t.TempDir()
			if err := encodeWebPPair(p, context.Background(), c.clip(), filepath.Join(out, "s.webp"), filepath.Join(out, "l.webp"), 64, 200, 80); err != nil {
				t.Fatal(err)
			}
			for _, args := range counter.runs["ffmpeg"] {
				if got := vfScale(args); !c.want(got) {
					t.Errorf("decode scale = %q, want the larger output's size: %v", got, args)
				}
			}
		})
	}
}

// offsetClip writes a lossless clip whose frame N has luma 16+5N, starting
// 2.5 s into a container whose first stream, audio, starts earlier, so that
// the container's start, the stream's and zero all differ.
func offsetClip(t *testing.T, env toolEnv) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "off.mkv")
	if _, err := env.run(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-itsoffset", "2.5", "-f", "lavfi", "-i", "nullsrc=size=64x48:rate=10:duration=4,format=gray,geq=lum='16+N*5'",
		"-itsoffset", "1", "-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono", "-t", "6.5",
		"-c:v", "libx264", "-qp", "0", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", path); err != nil {
		t.Fatal(err)
	}
	return stage(t, path)
}

// frameNumber reads which frame of offsetClip an image shows.
func frameNumber(t *testing.T, env toolEnv, path string) int {
	t.Helper()
	return int(math.Round(float64(pixelRGB(t, env, path)[0]) / 5.82))
}

func TestVideoSeeksAreRelativeToTheStreamStart(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := offsetClip(t, env)
	info, _, err := p.infoOf(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	enc := Encoding{Format: PNG}
	t.Run("at", func(t *testing.T) {
		out := t.TempDir()
		images, _, err := p.encodeTimes(context.Background(), src, enc, []int64{1000, 2000}, out)
		if err != nil || len(images) != 2 {
			t.Fatalf("images = %v, %v (duration %d ms)", images, err, info.Video.Duration.Milliseconds())
		}
		for i, want := range []int{10, 20} {
			if got := frameNumber(t, env, filepath.Join(out, images[i].Name)); got < want-1 || got > want+1 {
				t.Errorf("frame at %d ms is %d, want %d", images[i].Frames[0].Time.Milliseconds(), got, want)
			}
		}
	})
	t.Run("run", func(t *testing.T) {
		out := t.TempDir()
		images, _, err := p.encodeRun(context.Background(), src, enc, 1000, 3, out)
		if err != nil || len(images) != 3 {
			t.Fatalf("images = %v, %v", images, err)
		}
		for i, want := range []int{10, 11, 12} {
			if got := frameNumber(t, env, filepath.Join(out, images[i].Name)); got < want-1 || got > want+1 {
				t.Errorf("run frame %d is %d, want %d", i, got, want)
			}
			if wantMS := int64(1000 + 100*i); images[i].Frames[0].Time.Milliseconds() < wantMS-50 || images[i].Frames[0].Time.Milliseconds() > wantMS+50 {
				t.Errorf("run frame %d Time = %d ms, want about %d: the stream's offset must not shift it", i, images[i].Frames[0].Time.Milliseconds(), wantMS)
			}
		}
	})
}

func TestParseVideoProbeStartOffset(t *testing.T) {
	t.Parallel()
	probe := func(stream, format string) string {
		return `{"streams":[{"index":0,"codec_type":"video","width":10,"height":10,"start_time":"` + stream +
			`"}],"format":{"start_time":"` + format + `"}}`
	}
	cases := []struct {
		stream, format string
		want           int64
	}{
		{"11.500000", "10.000000", 1500}, // MPEG-TS: both start at the PCR origin
		{"2.500000", "0.872000", 1628},
		{"0.000000", "0.000000", 0},
		{"N/A", "0.000000", 0},
		{"1.0", "N/A", 0},
		{"0.000000", "1.000000", -1000},
	}
	for _, c := range cases {
		v, err := parseVideoProbe([]byte(probe(c.stream, c.format)))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.streamStartUS - v.containerStartUS; got != c.want*1000 {
			t.Errorf("stream %s format %s: offset %d us, want %d ms", c.stream, c.format, got, c.want)
		}
	}
}

// A seek past the last frame writes nothing, and no sample is kept for it.
func TestSampleVideoSkipsASeekThatWritesNothing(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	src := sdrClip(t, env, "160x120", 10, 2)
	dec := decoder{"ffmpeg", "mov"}
	plan, err := env.planVideo(context.Background(), dec, src, encoding{maxEdge: 100, maxArea: 10000})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	samples, err := env.sampleVideo(context.Background(), dec, src, dir, plan, []int64{500, 600_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].timeMS != 500 {
		t.Fatalf("samples = %+v, want only the one at 500 ms", samples)
	}
	for _, s := range samples {
		if _, err := os.Stat(s.path); err != nil {
			t.Errorf("sample %s was kept but not written", s.path)
		}
	}
}

// With no type named, ffmpeg's detection may not choose a format that opens
// other files the input lists.
func TestDetectedFormatsRefuseListFormats(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	real := sdrClip(t, env, "64x48", 10, 1)
	dir := t.TempDir()
	segment := filepath.Join(dir, "seg.mp4")
	copyFile(t, real, segment)
	lists := map[string]string{
		"list.ffconcat": "ffconcat version 1.0\nfile " + segment + "\n",
		"list.m3u8":     "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n" + segment + "\n#EXT-X-ENDLIST\n",
	}
	for name, body := range lists {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			p := requireProcessor(t)
			c := withRunCounts(p)
			out := t.TempDir()
			err := encodeWebPPair(p, context.Background(), path, filepath.Join(out, "s.webp"), filepath.Join(out, "l.webp"), 64, 200, 80)
			if err == nil {
				t.Fatal("a list format was opened")
			}
			if len(c.runs["ffmpeg"]) != 0 {
				t.Errorf("ffmpeg ran on a refused input")
			}
		})
	}
}

// Containers that are neither MP4 nor Matroska are still read when no type is
// named.
func TestWebPPairReadsOtherContainers(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	for ext, codec := range map[string]string{".avi": "mpeg4", ".ts": "mpeg2video", ".flv": "flv", ".wmv": "wmv2", ".mpg": "mpeg2video"} {
		t.Run(ext, func(t *testing.T) {
			src := writeClip(t, env, ext, append(testsrc("160x120", 10, 2), "-c:v", codec)...)
			out := t.TempDir()
			if err := encodeWebPPair(requireProcessor(t), context.Background(), src, filepath.Join(out, "s.webp"), filepath.Join(out, "l.webp"), 64, 100, 80); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Raw MPEG video streams and 3GPP files are read whether or not their brand
// is one the sniff table names.
func TestWebPPairReadsRawMPEGAndThreeGPP(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	cases := map[string][]string{
		"mpeg1video": {"-c:v", "mpeg1video", "-r", "25", "-f", "mpeg1video"},
		"mpeg2video": {"-c:v", "mpeg2video", "-f", "mpeg2video"},
		"3gp":        {"-c:v", "h263", "-s", "176x144", "-f", "3gp"},
		"3g2":        {"-c:v", "h263", "-s", "176x144", "-f", "3g2"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input")
			full := append(testsrc("176x144", 10, 2), args...)
			if _, err := env.run(context.Background(), "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y"}, append(full, path)...)...); err != nil {
				t.Fatal(err)
			}
			out := t.TempDir()
			small, large := filepath.Join(out, "s.webp"), filepath.Join(out, "l.webp")
			if err := encodeWebPPair(requireProcessor(t), context.Background(), path, small, large, 64, 100, 80); err != nil {
				t.Fatal(err)
			}
			if got := sceneSizes(t, env, large)[0]; !strings.HasPrefix(got, "100x") {
				t.Errorf("large = %s", got)
			}
		})
	}
}

// A reduction is sized from the upright frame: a rotated 160x120 clip at edge
// 40 is decoded at 60x80, not at the squashed 107x80 its stored size gives.
func TestRotatedVideoIsReducedInItsUprightAspect(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	src := rotatedClip(t, env)
	p := requireProcessor(t)
	rec := record(p)
	enc := encoding{format: WebP, ext: ".webp", quality: 80, maxEdge: 40, maxArea: 40 * 40}
	dec := decoder{"ffmpeg", "mov"}
	images, _, err := openSourceOn(t, p, src, OpenOptions{}).Encode(context.Background(), t.TempDir(), Select{Frames: Even(2)},
		Encoding{Format: WebP, Quality: 80, MaxEdge: 40, MaxArea: 40 * 40})
	if err != nil || len(images) == 0 {
		t.Fatalf("images = %v, %v", images, err)
	}
	for _, im := range images {
		if im.Width != 30 || im.Height != 40 {
			t.Errorf("%s is %dx%d, want 30x40", im.Name, im.Width, im.Height)
		}
	}
	for _, args := range rec.ran("ffmpeg") {
		if got := vfScale(args); got != "60x80" {
			t.Errorf("ffmpeg scale = %q, want the portrait 60x80: %v", got, args)
		}
	}
	plan, err := env.planVideo(context.Background(), dec, src, enc)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := env.sampleVideo(context.Background(), dec, src, t.TempDir(), plan, []int64{500})
	if err != nil || len(samples) != 1 {
		t.Fatalf("samples = %v, %v", samples, err)
	}
	if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%wx%h", samples[0].path))); got != "60x80" {
		t.Errorf("sample is %s, want 60x80", got)
	}
}

// When the stream has no duration of its own, the container's, which runs
// from the container's start, is shortened by the stream's offset. The result
// is still an upper bound: Matroska's container duration also covers the audio
// that outlasts this clip's video, so the samples near its end are skipped.
func TestVideoWithoutStreamDurationUsesTheContainersFromTheStreamStart(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := offsetClip(t, env)
	info, _, err := p.infoOf(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if info.Video.Duration != 5004*time.Millisecond {
		t.Errorf("duration = %v, want the container's 6632 ms less the stream's 1628 ms", info.Video.Duration)
	}
	images, _, err := p.encodeEven(context.Background(), src, Encoding{Format: PNG}, 15, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(images) < 12 {
		t.Errorf("%d frames, want at least 12 of the 15", len(images))
	}
}

func TestParseVideoProbeDurationFallbackIsRelativeToTheStream(t *testing.T) {
	t.Parallel()
	probe := func(streamDuration string) string {
		return `{"streams":[{"index":0,"codec_type":"video","width":10,"height":10,"start_time":"2.5","duration":"` +
			streamDuration + `"}],"format":{"start_time":"0.872","duration":"6.632"}}`
	}
	cases := []struct {
		streamDuration string
		want           int64
	}{
		{"N/A", 5004},
		{"3.9", 3900},
	}
	for _, c := range cases {
		v, err := parseVideoProbe([]byte(probe(c.streamDuration)))
		if err != nil {
			t.Fatal(err)
		}
		if v.durationMS != c.want {
			t.Errorf("stream duration %s: %d ms, want %d", c.streamDuration, v.durationMS, c.want)
		}
	}
	short := `{"streams":[{"index":0,"codec_type":"video","width":10,"height":10,"start_time":"9","duration":"N/A"}],"format":{"start_time":"0","duration":"3"}}`
	if v, err := parseVideoProbe([]byte(short)); err != nil || v.durationMS != 1 {
		t.Errorf("offset past the container's end: %+v, %v, want 1 ms", v, err)
	}
}

// The matroska and mov demuxers, and so WebM, MP4 and QuickTime, are opened
// as the type their bytes name.
func TestDetectedFormatsOpenMatroskaWebMAndMov(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	for ext, codec := range map[string]string{".mkv": "libx264", ".webm": "libvpx", ".mp4": "libx264", ".mov": "libx264"} {
		t.Run(ext, func(t *testing.T) {
			src := writeClip(t, env, ext, append(testsrc("160x120", 10, 1), "-c:v", codec)...)
			mt, err := sniffPath(src)
			if err != nil || mt == "" {
				t.Fatalf("sniff = %q, %v", mt, err)
			}
			if _, err := runVideoProbe(context.Background(), env, typeTable[mt].decoderFor(""), src, probeEntries); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// tenBitClip writes a 10-bit SDR H.264 clip.
func tenBitClip(t *testing.T, env toolEnv) string {
	t.Helper()
	return writeClip(t, env, ".mp4", append(testsrc("160x120", 10, 3), "-c:v", "libx264", "-preset", "ultrafast",
		"-profile:v", "high10", "-pix_fmt", "yuv420p10le", "-g", "10")...)
}

// closedGOPHLG writes an HLG clip with closed GOPs. A seek into an open-GOP
// stream cannot decode the pictures that lead its keyframe, so it returns the
// frame after the one asked for; the frames of a select run are then the
// correct ones and differ from the seek's.
func closedGOPHLG(t *testing.T, env toolEnv) string {
	t.Helper()
	args := hlgArgs(testsrc("320x240", 10, 3))
	for i, a := range args {
		if strings.HasPrefix(a, "keyint=10:") {
			args[i] = "open-gop=0:" + a
		}
	}
	return writeClip(t, env, ".mp4", args...)
}

// vfrClip writes a clip whose frames are unevenly spaced.
func vfrClip(t *testing.T, env toolEnv) string {
	t.Helper()
	return writeClip(t, env, ".mkv", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=30:duration=4",
		"-vf", `select=lt(mod(n\,10)\,7)`, "-fps_mode", "vfr",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "10")
}

type sampled struct {
	images []Image
	files  map[string][]byte
}

func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range dirNames(t, dir) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = b
	}
	return files
}

var errNotProcessable = errors.New("the Processor does not read this container")

// Reading a short stream once gives the frames, and so the bytes, that a seek
// to each time gives, through every entry point.
func TestSelectAndSeekGiveTheSameFrames(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	clips := map[string]func(t *testing.T) string{
		"sdr 8-bit":     func(t *testing.T) string { return sdrClip(t, env, "320x240", 10, 4) },
		"sdr 10-bit":    func(t *testing.T) string { return tenBitClip(t, env) },
		"hlg":           func(t *testing.T) string { return closedGOPHLG(t, env) },
		"rotated":       func(t *testing.T) string { return rotatedClip(t, env) },
		"late start":    func(t *testing.T) string { return offsetClip(t, env) },
		"variable rate": func(t *testing.T) string { return vfrClip(t, env) },
		"reduced 720p":  func(t *testing.T) string { return sdrClip(t, env, "1280x720", 10, 3) },
		"timescale 600": func(t *testing.T) string {
			return writeClip(t, env, ".mov", append(testsrc("320x240", 30, 2), "-c:v", "libx264", "-preset", "ultrafast",
				"-pix_fmt", "yuv420p", "-g", "15", "-video_track_timescale", "600")...)
		},
		"open GOP": func(t *testing.T) string {
			return writeClip(t, env, ".mp4", append(testsrc("320x240", 10, 6), "-c:v", "libx265", "-preset", "ultrafast",
				"-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-x265-params", "keyint=10:open-gop=1:bframes=3:log-level=error")...)
		},
		"fractional offset": func(t *testing.T) string { return fractionalOffsetTS(t, env) },
		"low-rate open GOP": func(t *testing.T) string {
			return writeClip(t, env, ".mp4", append(testsrc("160x120", 2, 12), "-c:v", "libx265", "-preset", "ultrafast",
				"-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-x265-params",
				"keyint=6:min-keyint=6:open-gop=1:bframes=8:b-adapt=0:scenecut=0:rc-lookahead=10:log-level=error")...)
		},
		"static content": func(t *testing.T) string {
			return writeClip(t, env, ".mp4", "-f", "lavfi", "-i", "color=c=blue:size=160x120:rate=10:duration=3", "-c:v", "libx264", "-pix_fmt", "yuv420p")
		},
	}
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 100}
	// processable reports whether the Processor accepts the clip's type; the
	// others, such as MPEG-TS, are read only through the toolEnv entries.
	processable := func(src string) bool {
		mt, err := sniffPath(src)
		if err != nil {
			return false
		}
		return typeTable[mt].video
	}
	entries := map[string]func(t *testing.T, src string) (sampled, error){
		"Even": func(t *testing.T, src string) (sampled, error) {
			dir := t.TempDir()
			if !processable(src) {
				return sampled{}, errNotProcessable
			}
			images, _, err := p.encodeEven(context.Background(), src, enc, 15, dir)
			return sampled{images, readDir(t, dir)}, err
		},
		"Times": func(t *testing.T, src string) (sampled, error) {
			dir := t.TempDir()
			times := []int64{0, 67, 167, 300, 1033, 1200, 1250, 1900}
			if !processable(src) {
				return sampled{}, errNotProcessable
			}
			images, _, err := p.encodeTimes(context.Background(), src, enc, times, dir)
			return sampled{images, readDir(t, dir)}, err
		},
		"Consecutive": func(t *testing.T, src string) (sampled, error) {
			dir := t.TempDir()
			if !processable(src) {
				return sampled{}, errNotProcessable
			}
			images, _, err := p.encodeRun(context.Background(), src, enc, 500, 4, dir)
			return sampled{images, readDir(t, dir)}, err
		},
		"Source.Encode": func(t *testing.T, src string) (sampled, error) {
			dir := t.TempDir()
			source, _, err := p.Open(context.Background(), src, OpenOptions{})
			if err != nil {
				return sampled{}, err
			}
			defer func() { _ = source.Close() }()
			_, _, err = source.Encode(context.Background(), dir, Select{Frames: Even(videoSampleFrames)}, openTargets...)
			return sampled{nil, readDir(t, dir)}, err
		},
		"encodeWebPPair": func(t *testing.T, src string) (sampled, error) {
			dir := t.TempDir()
			err := encodeWebPPair(requireProcessor(t), context.Background(), src, filepath.Join(dir, "s.webp"), filepath.Join(dir, "l.webp"), 48, 100, 80)
			return sampled{nil, readDir(t, dir)}, err
		},
	}
	// The limit is a package variable, so the seek runs and the select runs
	// are two phases, each running its cases in parallel.
	paths := map[string]string{}
	keep := t.TempDir()
	t.Run("clips", func(t *testing.T) {
		var mu sync.Mutex
		for clip, build := range clips {
			t.Run(clip, func(t *testing.T) {
				t.Parallel()
				// The clip outlives the subtest that built it.
				built := build(t)
				path := filepath.Join(keep, strings.ReplaceAll(clip, " ", "_")+filepath.Ext(built))
				if err := os.WriteFile(path, mustReadFile(t, built), 0o600); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				paths[clip] = path
				mu.Unlock()
			})
		}
	})
	type cell struct{ clip, entry string }
	results := map[string]map[cell]sampled{}
	for _, phase := range []struct {
		name  string
		limit int64
	}{{"seek", 0}, {"select", selectMaxPixels}} {
		got := map[cell]sampled{}
		results[phase.name] = got
		t.Run(phase.name, func(t *testing.T) {
			forceSelectLimit(t, phase.limit)
			var mu sync.Mutex
			for clip, src := range paths {
				for entry, run := range entries {
					t.Run(clip+"/"+entry, func(t *testing.T) {
						t.Parallel()
						s, err := run(t, src)
						if errors.Is(err, errNotProcessable) {
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						mu.Lock()
						got[cell{clip, entry}] = s
						mu.Unlock()
					})
				}
			}
		})
	}
	for c, seek := range results["seek"] {
		once := results["select"][c]
		t.Run(c.clip+"/"+c.entry, func(t *testing.T) {
			if len(seek.files) == 0 {
				t.Fatal("seek wrote nothing")
			}
			if !sameImages(seek.images, once.images) {
				t.Errorf("images differ:\nseek   %+v\nselect %+v", seek.images, once.images)
			}
			for name, want := range seek.files {
				if got, ok := once.files[name]; !ok || !bytes.Equal(got, want) {
					t.Errorf("%s differs between seek and select", name)
				}
			}
			if len(seek.files) != len(once.files) {
				t.Errorf("seek wrote %d files, select %d", len(seek.files), len(once.files))
			}
		})
	}
}

func isSelectRun(args []string) bool {
	i := slices.Index(args, "-vf")
	return i >= 0 && strings.HasPrefix(args[i+1], "select=") && !slices.Contains(args, "-ss")
}

// A stream is read once if and only if its decoded pixels, frames times width
// times height, are within the limit, whoever asks.
func TestSelectIsChosenByDecodedPixelsOnly(t *testing.T) {
	probe := func(frames string) string {
		return `{"streams":[{"index":0,"codec_type":"video","width":100,"height":50,"duration":"10",` + frames +
			`"r_frame_rate":"0/0"}],"format":{"duration":"10"}}`
	}
	const pixels = 100 * 50 * 25
	cases := []struct {
		name    string
		frames  string
		limit   int64
		select_ bool
	}{
		{"at the limit", `"nb_frames":"25",`, pixels, true},
		{"one pixel over", `"nb_frames":"25",`, pixels - 1, false},
		{"far under", `"nb_frames":"25",`, pixels * 100, true},
		{"unknown frame count", ``, pixels * 100, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forceSelectLimit(t, c.limit)
			webp := Encoding{Format: WebP, MaxEdge: 50, MaxArea: 2500}
			entries := map[string]func(*videoStub){
				"two targets": func(s *videoStub) {
					small, large := webp, webp
					small.MaxEdge, small.MaxArea, large.MaxEdge, large.MaxArea = 32, 32*32, 64, 64*64
					_ = s.encode(t, Select{Frames: Even(5)}, small, large)
				},
				"Even":        func(s *videoStub) { _ = s.encode(t, Select{Frames: Even(5)}, webp) },
				"Consecutive": func(s *videoStub) { _ = s.encode(t, Select{Frames: Consecutive(0, 3)}, webp) },
			}
			for name, run := range entries {
				stub := &videoStub{probe: probe(c.frames)}
				run(stub)
				if len(stub.runs) == 0 {
					t.Fatalf("%s: no ffmpeg run", name)
				}
				for _, args := range stub.runs {
					if isSelectRun(args) != c.select_ {
						t.Errorf("%s: select = %v, want %v: %v", name, isSelectRun(args), c.select_, args)
					}
					if !c.select_ && !slices.Contains(args, "-ss") {
						t.Errorf("%s: a seek run has no -ss: %v", name, args)
					}
				}
			}
		})
	}
}

// The first time of a select run is counted from the container's start, as a
// seek's is.
func TestSelectCountsFromTheContainerStart(t *testing.T) {
	forceSelectLimit(t, selectMaxPixels)
	stub := newVideoStub()
	stub.probe = fmt.Sprintf(stubVideoProbe, "1.5")
	_ = stub.encode(t, Select{Frames: Even(15)}, Encoding{Format: WebP, MaxEdge: 200})
	// The stub writes no frame, so the sample at 0 that follows is a second
	// select run.
	if len(stub.runs) == 0 || !isSelectRun(stub.runs[0]) {
		t.Fatalf("runs = %v, want a select run first", stub.runs)
	}
	// 10 s over 15 samples centres the first at 333 ms, 1833 ms on the
	// container's timeline.
	if expr := stub.runs[0][slices.Index(stub.runs[0], "-vf")+1]; !strings.Contains(expr, "gte(t\\,1.832999)*lt(prev_t\\,1.832999)") {
		t.Errorf("select expression lacks the offset first time: %s", expr)
	}
}

// fractionalOffsetTS writes an MPEG-TS clip whose video starts 9.733 ms after
// its audio: a fraction of a millisecond above one half, which a stream offset
// held in whole milliseconds rounds up to 10.
func fractionalOffsetTS(t *testing.T, env toolEnv) string {
	t.Helper()
	return writeClip(t, env, ".ts",
		"-itsoffset", "1.48", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration=3",
		"-itsoffset", "1.4916", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono", "-t", "4.5",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "12", "-bf", "2",
		"-c:a", "aac", "-muxdelay", "0", "-muxpreload", "0")
}

// The frame at time 0 is the first frame of the stream, whichever way it is
// read, even when the stream starts a fraction of a millisecond after the
// container.
func TestTimeZeroIsTheFirstFrameOnBothPaths(t *testing.T) {
	env := requireFFmpeg(t)
	src := fractionalOffsetTS(t, env)
	dec := decoder{"ffmpeg", "mpegts"}
	for name, limit := range map[string]int64{"seek": 0, "select": selectMaxPixels} {
		t.Run(name, func(t *testing.T) {
			forceSelectLimit(t, limit)
			plan, err := env.planVideo(context.Background(), dec, src, encoding{maxEdge: 100, maxArea: 10000})
			if err != nil {
				t.Fatal(err)
			}
			if offset := plan.info.streamStartUS - plan.info.containerStartUS; offset < 9000 || offset > 10000 || offset%1000 == 0 {
				t.Fatalf("stream offset = %d us, want a fraction of a millisecond near 9733", offset)
			}
			samples, err := env.sampleVideo(context.Background(), dec, src, t.TempDir(), plan, []int64{0})
			if err != nil || len(samples) != 1 {
				t.Fatalf("samples = %v, %v", samples, err)
			}
			if want := ".sample-1480000.pam"; !strings.HasSuffix(samples[0].path, want) {
				t.Errorf("sample at 0 is %s, want the frame at 1.48 s (%s)", filepath.Base(samples[0].path), want)
			}
		})
	}
}

// planVideo probes the video and plans its samples for the largest encoding
// decode.
func (e toolEnv) planVideo(ctx context.Context, dec decoder, inputPath string, decode encoding) (videoPlan, error) {
	info, err := e.probeChecked(ctx, dec, inputPath)
	if err != nil {
		return videoPlan{}, err
	}
	return newVideoPlan(info, region{}, decode, e.threads()), nil
}

// encodeWebPPair opens a video and encodes it at two sizes of animated
// WebP, writing the outputs to smallPath and largePath. The outputs are
// written beside them, in a directory that is removed.
func encodeWebPPair(p *Processor, ctx context.Context, inputPath, smallPath, largePath string, sizeSmall, sizeLarge, quality int) error {
	src, _, err := p.Open(ctx, inputPath, OpenOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dir, err := os.MkdirTemp(filepath.Dir(smallPath), ".video-pair-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	webp := func(edge int) Encoding {
		return Encoding{Format: WebP, Quality: quality, MaxEdge: edge, MaxArea: edge * edge, Animate: true}
	}
	images, _, err := src.Encode(ctx, dir, Select{Frames: Even(videoSampleFrames)}, webp(sizeSmall), webp(sizeLarge))
	if err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, images[0].Name), smallPath); err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, images[1].Name), largePath)
}
