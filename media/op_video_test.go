package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/davars/winnow-media/tool"
)

func requireFFmpeg(t *testing.T) Env {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := tool.Resolve(name); err != nil {
			t.Skip(name + " not available")
		}
	}
	return requireMagick(t)
}

// writeClip runs ffmpeg to write a clip with the given extension and returns
// it staged without one.
func writeClip(t *testing.T, env Env, ext string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clip"+ext)
	full := append([]string{"-nostdin", "-v", "error", "-y"}, args...)
	if _, err := env.Run(context.Background(), "ffmpeg", append(full, path)...); err != nil {
		t.Fatalf("ffmpeg %q: %v", args, err)
	}
	return stage(t, path)
}

func testsrc(size string, rate, seconds int) []string {
	return []string{"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=%d:duration=%d", size, rate, seconds)}
}

func sdrClip(t *testing.T, env Env, size string, rate, seconds int, extra ...string) string {
	args := append(testsrc(size, rate, seconds), "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "10")
	return writeClip(t, env, ".mp4", append(args, extra...)...)
}

func hlgArgs(input []string) []string {
	return append(input,
		"-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1",
		"-x265-params", "keyint=10:log-level=error:colorprim=bt2020:transfer=arib-std-b67:colormatrix=bt2020nc",
		"-color_primaries", "bt2020", "-color_trc", "arib-std-b67", "-colorspace", "bt2020nc")
}

func hlgClip(t *testing.T, env Env, input []string) string {
	return writeClip(t, env, ".mp4", hlgArgs(input)...)
}

func rotatedClip(t *testing.T, env Env) string {
	t.Helper()
	src := sdrClip(t, env, "160x120", 10, 4)
	out := filepath.Join(t.TempDir(), "rot.mp4")
	if _, err := env.Run(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-display_rotation", "90", "-f", "mov", "-i", src, "-c", "copy", out); err != nil {
		t.Fatal(err)
	}
	return stage(t, out)
}

func pixelRGB(t *testing.T, env Env, path string) [3]int {
	t.Helper()
	out := strings.TrimSpace(string(magick(t, env, path, "-resize", "1x1!", "-format", "%[fx:round(255*r)] %[fx:round(255*g)] %[fx:round(255*b)]", "info:")))
	var rgb [3]int
	for i, f := range strings.Fields(out) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("parsing %q: %v", out, err)
		}
		rgb[i] = n
	}
	return rgb
}

func videoInto(t *testing.T, p *Processor, path string, enc Encoding, maxFrames int) ([]Image, string) {
	t.Helper()
	out := t.TempDir()
	images, _, err := p.VideoFrames(context.Background(), path, enc, maxFrames, out)
	if err != nil {
		t.Fatal(err)
	}
	return images, out
}

func TestVideoFramesRefusals(t *testing.T) {
	p := requireProcessor(t)
	var calls int
	p.run = func(context.Context, string, ...string) (Result, error) {
		calls++
		return Result{}, errors.New("tool run")
	}
	mp4 := writeFile(t, "input", ftypBox("isom", "isom", "mp42"))
	gif := writeFile(t, "input", []byte("GIF89a"))
	audio := writeFile(t, "input", ftypBox("M4A ", "M4A ", "mp42"))
	garbage := writeFile(t, "input", []byte("x"))
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name            string
		path            string
		enc             Encoding
		maxFrames       int
		dir             string
		unsupportedType bool
	}{
		{"image type", gif, Encoding{}, 0, t.TempDir(), true},
		{"audio type", audio, Encoding{}, 0, t.TempDir(), true},
		{"unrecognized content", garbage, Encoding{}, 0, t.TempDir(), true},
		{"max frames negative", mp4, Encoding{}, -1, t.TempDir(), false},
		{"max frames too large", mp4, Encoding{}, 65, t.TempDir(), false},
		{"unknown format", mp4, Encoding{Format: "gif"}, 0, t.TempDir(), false},
		{"quality too large", mp4, Encoding{Quality: 101}, 0, t.TempDir(), false},
		{"max edge too small", mp4, Encoding{MaxEdge: 15}, 0, t.TempDir(), false},
		{"max area too large", mp4, Encoding{MaxArea: 100_000_001}, 0, t.TempDir(), false},
		{"dir not empty", mp4, Encoding{}, 0, nonEmpty, false},
		{"dir missing", mp4, Encoding{}, 0, filepath.Join(t.TempDir(), "absent"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := p.VideoFrames(context.Background(), c.path, c.enc, c.maxFrames, c.dir)
			if err == nil {
				t.Fatal("VideoFrames succeeded")
			}
			if got := errors.Is(err, ErrUnsupportedType); got != c.unsupportedType {
				t.Errorf("errors.Is(err, ErrUnsupportedType) = %v, want %v (err %v)", got, c.unsupportedType, err)
			}
		})
	}
	if calls != 0 {
		t.Errorf("%d tool runs before refusal", calls)
	}
	if got := dirNames(t, nonEmpty); !slices.Equal(got, []string{"leftover"}) {
		t.Errorf("refused directory now holds %v", got)
	}
}

func TestVideoSpecPins(t *testing.T) {
	pins := map[string]string{}
	for _, in := range videoSpec.inputs {
		pins[in.mediaType] = in.decoder.tool + ":" + in.decoder.name
	}
	want := map[string]string{
		"video/mp4": "ffmpeg:mov", "video/quicktime": "ffmpeg:mov",
		"video/webm": "ffmpeg:matroska", "video/x-matroska": "ffmpeg:matroska",
	}
	if len(pins) != len(want) {
		t.Errorf("pins = %v", pins)
	}
	for k, v := range want {
		if pins[k] != v {
			t.Errorf("pin for %s = %q, want %q", k, pins[k], v)
		}
	}
	if !slices.Equal(videoSpec.tools, []string{"ffprobe", "ffmpeg", "magick"}) {
		t.Errorf("tools = %v", videoSpec.tools)
	}
}

func TestSampleTimes(t *testing.T) {
	cases := []struct {
		name string
		v    videoInfo
		max  int
		want []int64
	}{
		{"even centres", videoInfo{durationMS: 16000, frames: 480}, 16, func() []int64 {
			var w []int64
			for i := range 16 {
				w = append(w, int64(i)*1000+500)
			}
			return w
		}()},
		{"unknown duration", videoInfo{}, 16, []int64{0}},
		{"single frame", videoInfo{durationMS: 100, frames: 1}, 16, []int64{0}},
		{"fewer frames than samples", videoInfo{durationMS: 400, frames: 4}, 16, []int64{50, 150, 250}},
		{"one sample", videoInfo{durationMS: 10000, frames: 300}, 1, []int64{5000}},
	}
	for _, c := range cases {
		if got := sampleTimesMS(c.v, c.max); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestVideoFramesSamplesEvenlyAndHonoursMaxFrames(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := sdrClip(t, env, "160x120", 10, 16)

	images, out := videoInto(t, p, src, Encoding{}, 0)
	if len(images) != 16 {
		t.Fatalf("%d images, want 16", len(images))
	}
	var names []string
	for i, f := range images {
		if want := int64(i)*1000 + 500; f.TimeMS != want {
			t.Errorf("image %d at %d ms, want %d", i, f.TimeMS, want)
		}
		if f.Name != fmt.Sprintf("frame-%03d.jpg", i) || f.Frame != 0 || f.Width != 160 || f.Height != 120 {
			t.Errorf("image %d = %+v", i, f)
		}
		names = append(names, f.Name)
	}
	if got := dirNames(t, out); !slices.Equal(got, names) {
		t.Errorf("output dir holds %v, want only %v", got, names)
	}

	images, _ = videoInto(t, p, src, Encoding{Format: WebP}, 5)
	if len(images) != 5 || images[0].Name != "frame-000.webp" {
		t.Fatalf("max frames 5: %+v", images)
	}
	if images[0].TimeMS != 1600 || images[4].TimeMS != 14400 {
		t.Errorf("max frames 5 times: %d .. %d", images[0].TimeMS, images[4].TimeMS)
	}
}

func TestVideoFramesShortClips(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)

	t.Run("one frame mp4", func(t *testing.T) {
		src := sdrClip(t, env, "160x120", 10, 1, "-frames:v", "1")
		images, out := videoInto(t, p, src, Encoding{}, 0)
		if len(images) != 1 || images[0].TimeMS != 0 || len(dirNames(t, out)) != 1 {
			t.Errorf("images = %+v", images)
		}
	})
	t.Run("one frame webm", func(t *testing.T) {
		src := writeClip(t, env, ".webm", append(testsrc("160x120", 10, 1), "-frames:v", "1", "-c:v", "libvpx")...)
		images, _ := videoInto(t, p, src, Encoding{}, 0)
		if len(images) != 1 {
			t.Errorf("images = %+v", images)
		}
	})
	t.Run("fewer frames than max_frames", func(t *testing.T) {
		src := sdrClip(t, env, "160x120", 10, 1, "-frames:v", "4")
		images, _ := videoInto(t, p, src, Encoding{}, 0)
		if len(images) < 1 || len(images) > 4 {
			t.Errorf("images = %+v", images)
		}
	})
	t.Run("matroska", func(t *testing.T) {
		src := writeClip(t, env, ".mkv", append(testsrc("160x120", 10, 6), "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")...)
		images, _ := videoInto(t, p, src, Encoding{}, 4)
		if len(images) != 4 {
			t.Errorf("images = %+v", images)
		}
	})
}

func TestVideoFramesStaticClipDedupes(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := writeClip(t, env, ".mp4", "-f", "lavfi", "-i", "color=c=0x3070c0:size=160x120:rate=10:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	images, out := videoInto(t, p, src, Encoding{}, 0)
	if len(images) != 1 || images[0].Name != "frame-000.jpg" {
		t.Fatalf("images = %+v", images)
	}
	if got := dirNames(t, out); !slices.Equal(got, []string{"frame-000.jpg"}) {
		t.Errorf("output dir holds %v", got)
	}
}

func TestVideoFramesRotatedClipGivesPortrait(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := rotatedClip(t, env)

	images, _ := videoInto(t, p, src, Encoding{}, 3)
	for _, f := range images {
		if f.Width != 120 || f.Height != 160 {
			t.Errorf("%s is %dx%d, want 120x160", f.Name, f.Width, f.Height)
		}
	}
	images, _ = videoInto(t, p, src, Encoding{MaxEdge: 80}, 3)
	for _, f := range images {
		if f.Width != 60 || f.Height != 80 {
			t.Errorf("bounded: %s is %dx%d, want 60x80", f.Name, f.Width, f.Height)
		}
	}
}

func TestVideoFramesSizing(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := sdrClip(t, env, "320x240", 10, 4)
	cases := []struct {
		name string
		enc  Encoding
		w, h int
	}{
		{"no upscale", Encoding{MaxEdge: 4000, MaxArea: 90_000_000}, 320, 240},
		{"edge", Encoding{MaxEdge: 160}, 160, 120},
		{"area", Encoding{MaxArea: 20000}, 163, 122},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			images, _ := videoInto(t, p, src, c.enc, 2)
			for _, f := range images {
				if f.Width != c.w || f.Height != c.h {
					t.Errorf("%s is %dx%d, want %dx%d", f.Name, f.Width, f.Height, c.w, c.h)
				}
			}
		})
	}
}

func TestVideoFramesHDRIsConvertedToSDR(t *testing.T) {
	env := requireFFmpeg(t)
	green := hlgClip(t, env, []string{"-f", "lavfi", "-i", "color=c=0x00c000:size=320x240:rate=10:duration=2"})
	p := requireProcessor(t)
	rec := record(p)
	images, out := videoInto(t, p, green, Encoding{}, 0)
	if len(images) == 0 {
		t.Fatal("no images")
	}
	var sawZscale bool
	for _, call := range rec.calls {
		if call[0] == "ffmpeg" && strings.Contains(strings.Join(call, " "), "zscale") {
			sawZscale = true
		}
	}
	if !sawZscale {
		t.Error("HLG input was not converted")
	}
	for _, f := range images {
		if rgb := pixelRGB(t, env, filepath.Join(out, f.Name)); rgb[1] < rgb[0]+60 || rgb[1] < rgb[2]+60 {
			t.Errorf("%s: mean colour %v is not strongly green", f.Name, rgb)
		}
	}
}

func TestVideoFramesSDRHasNoColourFilter(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	videoInto(t, p, sdrClip(t, env, "160x120", 10, 4), Encoding{MaxEdge: 80}, 0)
	for _, call := range rec.calls {
		if strings.Contains(strings.Join(call, " "), "zscale") {
			t.Errorf("SDR input got a colour filter: %q", call)
		}
	}
}

func TestVideoFramesStripsMetadata(t *testing.T) {
	requireExiftool(t)
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := sdrClip(t, env, "160x120", 10, 4, "-metadata", "artist=secret-artist", "-metadata", "location=+40.7128-074.0060/", "-metadata", "comment=secret-comment")
	if got := exiftool(t, env, "-a", "-s", src); !strings.Contains(got, "secret-artist") {
		t.Fatalf("fixture lacks metadata:\n%s", got)
	}
	for _, format := range []Format{JPEG, WebP, PNG} {
		images, out := videoInto(t, p, src, Encoding{Format: format}, 2)
		for _, f := range images {
			path := filepath.Join(out, f.Name)
			if got := metadataGroups(t, env, path); strings.TrimSpace(got) != "" {
				t.Errorf("%s carries metadata:\n%s", f.Name, got)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"secret-artist", "secret-comment", "40.7128"} {
				if strings.Contains(string(data), secret) {
					t.Errorf("%s contains %q", f.Name, secret)
				}
			}
		}
	}
}

func TestVideoFramesCleansUpOnFailure(t *testing.T) {
	env := requireFFmpeg(t)
	src := sdrClip(t, env, "160x120", 10, 4)
	// Calls: probe, 4 samples, signatures, encode, identify.
	for _, failAfter := range []int{1, 3, 6, 7, 8} {
		p := requireProcessor(t)
		rec := record(p)
		rec.failAfter = failAfter
		out := t.TempDir()
		_, _, err := p.VideoFrames(context.Background(), src, Encoding{}, 4, out)
		if err == nil {
			t.Fatalf("failAfter=%d: VideoFrames succeeded", failAfter)
		}
		if got := dirNames(t, out); len(got) != 0 {
			t.Errorf("failAfter=%d: output dir holds %v", failAfter, got)
		}
	}
}

func TestVideoFramesCostIsIndependentOfDuration(t *testing.T) {
	env := requireFFmpeg(t)
	cpu := func(seconds int) float64 {
		src := sdrClip(t, env, "160x120", 10, seconds)
		p := requireProcessor(t)
		_, usage, err := p.VideoFrames(context.Background(), src, Encoding{}, 0, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return usage.CPU.Seconds()
	}
	short, long := cpu(30), cpu(300)
	t.Logf("30 s: %.3f s CPU; 300 s: %.3f s CPU", short, long)
	if long > 2.5*short {
		t.Errorf("CPU for 300 s = %.3f s, more than 2.5x the %.3f s for 30 s", long, short)
	}
}

func TestVideoFramesGoldenArgv(t *testing.T) {
	env := requireFFmpeg(t)
	cases := []struct {
		name  string
		input func() string
		enc   Encoding
	}{
		{"video-frames-sdr-h264", func() string { return sdrClip(t, env, "320x240", 10, 4) }, Encoding{MaxEdge: 160}},
		{"video-frames-hlg-hevc", func() string {
			return hlgClip(t, env, testsrc("320x240", 10, 4))
		}, Encoding{Format: WebP}},
		{"video-frames-rotated", func() string { return rotatedClip(t, env) }, Encoding{MaxEdge: 80}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			rec := record(p)
			input := c.input()
			out := t.TempDir()
			if _, _, err := p.VideoFrames(context.Background(), input, c.enc, 3, out); err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.name, goldenArgv(rec.calls, input, out))
		})
	}
}

func TestParseVideoProbeStreamSelection(t *testing.T) {
	stream := func(index int, dispositions string) string {
		return fmt.Sprintf(`{"index":%d,"codec_type":"video","width":%d,"height":10,"disposition":{%s}}`,
			index, 100+index, dispositions)
	}
	cases := []struct {
		name    string
		streams []string
		want    int
		wantErr bool
	}{
		{"default beats first", []string{stream(0, `"default":0`), stream(1, `"default":1`)}, 1, false},
		{"first default wins", []string{stream(0, `"default":0`), stream(1, `"default":1`), stream(2, `"default":1`)}, 1, false},
		{"no default falls back to first", []string{stream(0, `"default":0`), stream(1, `"default":0`)}, 0, false},
		{"attached picture is skipped even when default", []string{stream(0, `"default":1,"attached_pic":1`), stream(1, `"default":0`)}, 1, false},
		{"default skips audio", []string{`{"index":0,"codec_type":"audio","disposition":{"default":1}}`, stream(1, `"default":0`)}, 1, false},
		{"only attached pictures", []string{stream(0, `"default":1,"attached_pic":1`)}, 0, true},
	}
	for _, c := range cases {
		out := fmt.Sprintf(`{"streams":[%s],"format":{"duration":"1.0"}}`, strings.Join(c.streams, ","))
		v, err := parseVideoProbe([]byte(out))
		if (err != nil) != c.wantErr || (err == nil && v.stream != c.want) {
			t.Errorf("%s: stream %d, err %v; want stream %d, error %v", c.name, v.stream, err, c.want, c.wantErr)
		}
		if err == nil && v.streams != len(c.streams) {
			t.Errorf("%s: %d streams counted, want %d", c.name, v.streams, len(c.streams))
		}
	}
}

func TestVideoFramesSamplesTheDefaultStream(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := writeClip(t, env, ".mp4",
		"-f", "lavfi", "-i", "color=c=red:size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "color=c=blue:size=320x240:rate=10:duration=2",
		"-map", "0", "-map", "1", "-disposition:v:0", "0", "-disposition:v:1", "default",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	images, out := videoInto(t, p, src, Encoding{}, 2)
	if len(images) == 0 {
		t.Fatal("no images")
	}
	for _, f := range images {
		if f.Width != 320 || f.Height != 240 {
			t.Errorf("%s is %dx%d, want the default stream's 320x240", f.Name, f.Width, f.Height)
		}
		if rgb := pixelRGB(t, env, filepath.Join(out, f.Name)); rgb[2] < rgb[0]+100 {
			t.Errorf("%s: mean colour %v is not blue", f.Name, rgb)
		}
	}
}

func TestCheckVideoInfo(t *testing.T) {
	ok := videoInfo{streams: maxStreams, durationMS: maxDurationMS, width: maxDimension, height: maxDimension}
	if err := checkVideoInfo(ok); err != nil {
		t.Fatalf("limits are inclusive: %v", err)
	}
	cases := map[string]videoInfo{
		"streams":  {streams: maxStreams + 1},
		"duration": {streams: 1, durationMS: maxDurationMS + 1},
		"width":    {streams: 1, width: maxDimension + 1, height: 1},
	}
	for name, v := range cases {
		if err := checkVideoInfo(v); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: err = %v, want ErrTooLarge", name, err)
		}
	}
}

func TestVideoFramesRefusesOversizedProbeBeforeSampling(t *testing.T) {
	mp4 := writeFile(t, "input", ftypBox("isom", "isom", "mp42"))
	manyStreams := `{"index":0,"codec_type":"video","width":16,"height":16,"duration":"1.0"}`
	for i := 1; i <= maxStreams; i++ {
		manyStreams += fmt.Sprintf(`,{"index":%d,"codec_type":"audio"}`, i)
	}
	cases := map[string]string{
		"stream count":    `{"streams":[` + manyStreams + `],"format":{}}`,
		"stream duration": `{"streams":[{"index":0,"codec_type":"video","width":16,"height":16,"duration":"21600.001"}],"format":{}}`,
		"format duration": `{"streams":[{"index":0,"codec_type":"video","width":16,"height":16}],"format":{"duration":"99999"}}`,
		"width":           `{"streams":[{"index":0,"codec_type":"video","width":16385,"height":16,"duration":"1"}],"format":{}}`,
	}
	for name, probe := range cases {
		t.Run(name, func(t *testing.T) {
			p := requireProcessor(t)
			var tools []string
			p.run = func(_ context.Context, tool string, _ ...string) (Result, error) {
				tools = append(tools, tool)
				return Result{Stdout: []byte(probe)}, nil
			}
			_, _, err := p.VideoFrames(context.Background(), mp4, Encoding{}, 0, t.TempDir())
			if !errors.Is(err, ErrTooLarge) {
				t.Errorf("err = %v, want ErrTooLarge", err)
			}
			if !slices.Equal(tools, []string{"ffprobe"}) {
				t.Errorf("tools run = %v, want only the probe", tools)
			}
		})
	}
}

func TestVideoToolRunsShareTheInputArguments(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	videoInto(t, p, sdrClip(t, env, "160x120", 10, 4), Encoding{}, 3)
	seen := map[string]int{}
	for _, call := range rec.calls {
		tool := call[0]
		if tool != "ffprobe" && tool != "ffmpeg" {
			continue
		}
		seen[tool]++
		input := strings.Join(inputArgs(decoder{tool, "mov"}, "X"), " ")
		input = strings.TrimSuffix(input, "X")
		if !strings.Contains(strings.Join(call[1:], " "), input) {
			t.Errorf("%s call lacks %q: %q", tool, input, call)
		}
		if tool == "ffmpeg" {
			joined := " " + strings.Join(call[1:], " ") + " "
			for _, want := range []string{
				" -max_alloc " + strconv.Itoa(ffmpegMaxAlloc) + " ",
				" -filter_threads " + strconv.Itoa(ffmpegThreads) + " ",
				" -threads " + strconv.Itoa(ffmpegThreads) + " -protocol_whitelist ",
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("ffmpeg call lacks %q: %q", want, call)
				}
			}
		}
	}
	if seen["ffprobe"] != 1 || seen["ffmpeg"] < 2 {
		t.Errorf("calls seen = %v", seen)
	}
}
