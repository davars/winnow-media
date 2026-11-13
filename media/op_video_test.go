package media

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func requireFFmpeg(t *testing.T) toolEnv { return requireTools(t, "magick", "ffmpeg", "ffprobe") }

// writeClip runs ffmpeg to write a clip with the given extension and returns
// it staged without one.
func writeClip(t *testing.T, env toolEnv, ext string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clip"+ext)
	full := append([]string{"-nostdin", "-v", "error", "-y"}, args...)
	if _, err := env.run(context.Background(), "ffmpeg", append(full, path)...); err != nil {
		t.Fatalf("ffmpeg %q: %v", args, err)
	}
	return stage(t, path)
}

func testsrc(size string, rate, seconds int) []string {
	return []string{"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=%d:duration=%d", size, rate, seconds)}
}

func sdrClip(t *testing.T, env toolEnv, size string, rate, seconds int, extra ...string) string {
	args := append(testsrc(size, rate, seconds), "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "10")
	return writeClip(t, env, ".mp4", append(args, extra...)...)
}

func hlgArgs(input []string) []string {
	return append(input,
		"-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1",
		"-x265-params", "keyint=10:log-level=error:colorprim=bt2020:transfer=arib-std-b67:colormatrix=bt2020nc",
		"-color_primaries", "bt2020", "-color_trc", "arib-std-b67", "-colorspace", "bt2020nc")
}

func hlgClip(t *testing.T, env toolEnv, input []string) string {
	return writeClip(t, env, ".mp4", hlgArgs(input)...)
}

func rotatedClip(t *testing.T, env toolEnv) string {
	t.Helper()
	src := sdrClip(t, env, "160x120", 10, 4)
	out := filepath.Join(t.TempDir(), "rot.mp4")
	if _, err := env.run(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-display_rotation", "90", "-f", "mov", "-i", src, "-c", "copy", out); err != nil {
		t.Fatal(err)
	}
	return stage(t, out)
}

func pixelRGB(t *testing.T, env toolEnv, path string) [3]int {
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
	images, _, err := p.encodeEven(context.Background(), path, enc, maxFrames, out)
	if err != nil {
		t.Fatal(err)
	}
	return images, out
}

// Open refuses a container of another kind after one probe and runs nothing
// else; Encode refuses a bad Encoding, Select or directory of a video before
// any ffmpeg run.
func TestVideoRefusals(t *testing.T) {
	t.Parallel()
	t.Run("audio container", func(t *testing.T) {
		p := requireProcessor(t)
		var probes, others int
		p.run = func(_ context.Context, in *os.File, name string, _ ...string) (runResult, error) {
			if name == "ffprobe" {
				probes++
				return runResult{}, errors.New("ffprobe: exit status 183: Invalid data found when processing input")
			}
			others++
			return runResult{}, errors.New("tool run")
		}
		audio := writeFile(t, "input", ftypBox("M4A ", "M4A ", "mp42"))
		if _, _, err := p.Open(context.Background(), audio, OpenOptions{}); !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("err = %v, want ErrUnsupportedType", err)
		}
		if probes != 1 || others != 0 {
			t.Errorf("%d probes and %d other runs, want one probe", probes, others)
		}
	})

	stub := newVideoStub()
	src, err := stub.open(t)
	if err != nil {
		t.Fatal(err)
	}
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		sel  Select
		enc  Encoding
		dir  string
	}{
		{"even count too large", Select{Frames: Even(65)}, Encoding{}, t.TempDir()},
		{"even count negative", Select{Frames: Even(-1)}, Encoding{}, t.TempDir()},
		{"a negative time", Select{Frames: Times(-time.Millisecond)}, Encoding{}, t.TempDir()},
		{"a run of no frames", Select{Frames: Consecutive(0, 0)}, Encoding{}, t.TempDir()},
		{"unknown format", Select{Frames: Even(3)}, Encoding{Format: "gif"}, t.TempDir()},
		{"quality too large", Select{Frames: Even(3)}, Encoding{Quality: 101}, t.TempDir()},
		{"max edge too small", Select{Frames: Even(3)}, Encoding{MaxEdge: 15}, t.TempDir()},
		{"max area too large", Select{Frames: Even(3)}, Encoding{MaxArea: 100_000_001}, t.TempDir()},
		{"dir not empty", Select{Frames: Even(3)}, Encoding{}, nonEmpty},
		{"dir missing", Select{Frames: Even(3)}, Encoding{}, filepath.Join(t.TempDir(), "absent")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := src.Encode(context.Background(), c.dir, c.sel, c.enc); err == nil {
				t.Fatal("Encode succeeded")
			}
			if n := len(stub.runs); n != 0 {
				t.Errorf("%d ffmpeg runs before refusal", n)
			}
		})
	}
	if got := dirNames(t, nonEmpty); !slices.Equal(got, []string{"leftover"}) {
		t.Errorf("refused directory now holds %v", got)
	}
}

func TestVideoTypePins(t *testing.T) {
	t.Parallel()
	pins := map[string]string{}
	for typ, e := range typeTable {
		if e.video {
			dec := e.decoderFor("")
			pins[typ] = dec.tool + ":" + dec.name
		}
	}
	want := map[string]string{
		"video/mp4": "ffmpeg:mov", "video/quicktime": "ffmpeg:mov",
		"video/webm": "ffmpeg:matroska", "video/x-matroska": "ffmpeg:matroska",
		"video/x-msvideo": "ffmpeg:avi", "video/x-ms-asf": "ffmpeg:asf", "video/mpeg": "ffmpeg:mpeg",
		"video/mp2t": "ffmpeg:mpegts", "video/x-flv": "ffmpeg:flv",
	}
	if !maps.Equal(pins, want) {
		t.Errorf("pins = %v, want %v", pins, want)
	}
}

func TestSampleTimes(t *testing.T) {
	t.Parallel()
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

func TestVideoEncodeSamplesEvenlyAndHonoursMaxFrames(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := sdrClip(t, env, "160x120", 10, 16)

	images, out := videoInto(t, p, src, Encoding{}, defaultFrames)
	if len(images) != 16 {
		t.Fatalf("%d images, want 16", len(images))
	}
	var names []string
	for i, f := range images {
		if want := int64(i)*1000 + 500; f.Frames[0].Time.Milliseconds() != want {
			t.Errorf("image %d at %d ms, want %d", i, f.Frames[0].Time.Milliseconds(), want)
		}
		if f.Name != fmt.Sprintf("frame-%03d.jpg", i) || f.Frames[0].Index != 0 || f.Width != 160 || f.Height != 120 {
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
	if images[0].Frames[0].Time.Milliseconds() != 1600 || images[4].Frames[0].Time.Milliseconds() != 14400 {
		t.Errorf("max frames 5 times: %d .. %d", images[0].Frames[0].Time.Milliseconds(), images[4].Frames[0].Time.Milliseconds())
	}
}

func TestVideoEncodeShortClips(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)

	t.Run("one frame mp4", func(t *testing.T) {
		src := sdrClip(t, env, "160x120", 10, 1, "-frames:v", "1")
		images, out := videoInto(t, p, src, Encoding{}, defaultFrames)
		if len(images) != 1 || images[0].Frames[0].Time.Milliseconds() != 0 || len(dirNames(t, out)) != 1 {
			t.Errorf("images = %+v", images)
		}
	})
	t.Run("one frame webm", func(t *testing.T) {
		src := writeClip(t, env, ".webm", append(testsrc("160x120", 10, 1), "-frames:v", "1", "-c:v", "libvpx")...)
		images, _ := videoInto(t, p, src, Encoding{}, defaultFrames)
		if len(images) != 1 {
			t.Errorf("images = %+v", images)
		}
	})
	t.Run("fewer frames than max_frames", func(t *testing.T) {
		src := sdrClip(t, env, "160x120", 10, 1, "-frames:v", "4")
		images, _ := videoInto(t, p, src, Encoding{}, defaultFrames)
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

func TestVideoEncodeStaticClipDedupes(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := writeClip(t, env, ".mp4", "-f", "lavfi", "-i", "color=c=0x3070c0:size=160x120:rate=10:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	images, out := videoInto(t, p, src, Encoding{}, defaultFrames)
	if len(images) != 1 || images[0].Name != "frame-000.jpg" {
		t.Fatalf("images = %+v", images)
	}
	if got := dirNames(t, out); !slices.Equal(got, []string{"frame-000.jpg"}) {
		t.Errorf("output dir holds %v", got)
	}
}

func TestVideoEncodeRotatedClipGivesPortrait(t *testing.T) {
	t.Parallel()
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

func TestVideoEncodeSizing(t *testing.T) {
	t.Parallel()
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

func TestVideoEncodeHDRIsConvertedToSDR(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	green := hlgClip(t, env, []string{"-f", "lavfi", "-i", "color=c=0x00c000:size=320x240:rate=10:duration=2"})
	p := requireProcessor(t)
	rec := record(p)
	images, out := videoInto(t, p, green, Encoding{}, defaultFrames)
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

func TestIsHDRTransfer(t *testing.T) {
	t.Parallel()
	for trc, want := range map[string]bool{
		"arib-std-b67": true,
		"smpte2084":    true,
		"bt709":        false,
		"iec61966-2-1": false,
		"":             false,
	} {
		if got := isHDRTransfer(trc); got != want {
			t.Errorf("isHDRTransfer(%q) = %v, want %v", trc, got, want)
		}
	}
}

// A PQ clip is HDR, as an HLG one is: it is reported so and is tone mapped.
func TestPQVideoIsHDR(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := writeClip(t, env, ".mp4", append(testsrc("160x120", 10, 2),
		"-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1",
		"-x265-params", "keyint=10:log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc")...)
	p := requireProcessor(t)
	info, _, err := p.infoOf(context.Background(), clip)
	if err != nil || !info.Video.HDR {
		t.Fatalf("PQ clip: %+v, %v", info, err)
	}
	rec := record(p)
	if images, _ := videoInto(t, p, clip, Encoding{}, 1); len(images) == 0 {
		t.Fatal("no images")
	}
	var sawZscale bool
	for _, call := range rec.calls {
		if call[0] == "ffmpeg" && strings.Contains(strings.Join(call, " "), "zscale") {
			sawZscale = true
		}
	}
	if !sawZscale {
		t.Error("PQ input was not converted")
	}
}

func TestVideoEncodeHDRScalesBeforeToneMapping(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	videoInto(t, p, hlgClip(t, env, testsrc("320x240", 10, 2)), Encoding{MaxEdge: 160}, 1)
	var filters []string
	for _, call := range rec.calls {
		if call[0] != "ffmpeg" {
			continue
		}
		if i := slices.Index(call, "-vf"); i >= 0 {
			filters = append(filters, call[i+1])
		}
	}
	if len(filters) == 0 {
		t.Fatal("no ffmpeg call carried a filter chain")
	}
	for _, f := range filters {
		scale, tone := strings.Index(f, "scale="), strings.Index(f, "zscale=t=linear")
		if scale < 0 || tone < 0 || scale > tone {
			t.Errorf("tone mapping is not preceded by the scale: %q", f)
		}
		if !strings.HasSuffix(f, "format=yuv444p") {
			t.Errorf("chain does not end in 4:4:4: %q", f)
		}
	}
}

func TestVideoEncodeHDROddTargetSize(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	clip := hlgClip(t, env, testsrc("320x240", 10, 2))
	images, _ := videoInto(t, p, clip, Encoding{MaxEdge: 165}, 1)
	if len(images) == 0 {
		t.Fatal("no images")
	}
	for _, f := range images {
		if f.Width != 165 || f.Height != 123 {
			t.Errorf("%s is %dx%d, want 165x123", f.Name, f.Width, f.Height)
		}
	}
}

func TestVideoEncodeSDRHasNoColourFilter(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	videoInto(t, p, sdrClip(t, env, "160x120", 10, 4), Encoding{MaxEdge: 80}, defaultFrames)
	for _, call := range rec.calls {
		if strings.Contains(strings.Join(call, " "), "zscale") {
			t.Errorf("SDR input got a colour filter: %q", call)
		}
	}
}

func TestVideoEncodeStripsMetadata(t *testing.T) {
	t.Parallel()
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

func TestVideoEncodeCleansUpOnFailure(t *testing.T) {
	forceSeek(t)
	env := requireFFmpeg(t)
	src := sdrClip(t, env, "160x120", 10, 4)
	// Calls: probe, 4 samples, 1 encode of the 4.
	for _, failAfter := range []int{1, 3, 5, 6} {
		p := requireProcessor(t)
		rec := record(p)
		rec.failAfter = failAfter
		out := t.TempDir()
		_, _, err := p.encodeEven(context.Background(), src, Encoding{}, 4, out)
		if err == nil {
			t.Fatalf("failAfter=%d: the encode succeeded", failAfter)
		}
		if got := dirNames(t, out); len(got) != 0 {
			t.Errorf("failAfter=%d: output dir holds %v", failAfter, got)
		}
	}
}

func TestVideoEncodeCostIsIndependentOfDuration(t *testing.T) {
	forceSeek(t)
	env := requireFFmpeg(t)
	cpu := func(seconds int) float64 {
		src := sdrClip(t, env, "160x120", 10, seconds)
		p := requireProcessor(t)
		_, usage, err := p.encodeEven(context.Background(), src, Encoding{}, 16, t.TempDir())
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

func TestVideoEncodeGoldenArgv(t *testing.T) {
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
	for _, path := range []struct {
		name  string
		limit int64
	}{{"seek", 0}, {"select", selectMaxPixels}} {
		for _, c := range cases {
			t.Run(c.name+"-"+path.name, func(t *testing.T) {
				forceSelectLimit(t, path.limit)
				p := requireProcessor(t)
				rec := record(p)
				input := c.input()
				out := t.TempDir()
				if _, _, err := p.encodeEven(context.Background(), input, c.enc, 3, out); err != nil {
					t.Fatal(err)
				}
				checkGolden(t, c.name+"-"+path.name, goldenArgv(rec.calls, out))
			})
		}
	}
}

func TestParseVideoProbeStreamSelection(t *testing.T) {
	t.Parallel()
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

// A probe report that is not JSON, names no video stream, or gives the stream
// no size is an error, not a video of unknown properties.
func TestParseVideoProbeRefusesUnusableOutput(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, out, want string }{
		{"empty", "", "parsing probe output"},
		{"not JSON", "streams: none", "parsing probe output"},
		{"no streams", `{"streams":[],"format":{}}`, "no video stream"},
		{"audio alone", `{"streams":[{"index":0,"codec_type":"audio"}],"format":{}}`, "no video stream"},
		{"zero width", `{"streams":[{"index":0,"codec_type":"video","width":0,"height":10}],"format":{}}`, "reports size 0x10"},
		{"zero height", `{"streams":[{"index":0,"codec_type":"video","width":10,"height":0}],"format":{}}`, "reports size 10x0"},
		{"no size", `{"streams":[{"index":0,"codec_type":"video"}],"format":{}}`, "reports size 0x0"},
	} {
		if _, err := parseVideoProbe([]byte(c.out)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to say %q", c.name, err, c.want)
		}
	}
}

// The display rotation is reported as 0, 90, 180 or 270, whichever way the
// container writes it.
func TestParseVideoProbeNormalisesRotation(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		written float64
		want    int
	}{{0, 0}, {90, 90}, {180, 180}, {270, 270}, {-90, 270}, {-180, 180}, {-270, 90}, {450, 90}, {89.6, 90}} {
		out := fmt.Sprintf(`{"streams":[{"index":0,"codec_type":"video","width":10,"height":10,"side_data_list":[{"rotation":%v}]}],"format":{}}`, c.written)
		v, err := parseVideoProbe([]byte(out))
		if err != nil || v.rotation != c.want {
			t.Errorf("rotation %v: got %d, %v; want %d", c.written, v.rotation, err, c.want)
		}
	}
}

func TestVideoEncodeSamplesTheDefaultStream(t *testing.T) {
	t.Parallel()
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

func TestCheckSeekTime(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		durationMS int64
		ms         int64
		want       string
	}{
		{"start of a video", 4000, 0, ""},
		{"start of a video of unknown duration", 0, 0, ""},
		{"last millisecond", 4000, 3999, ""},
		{"at the duration", 4000, 4000, "is not inside the video"},
		{"past the duration", 4000, 4001, "is not inside the video"},
		{"any time of a video of unknown duration", 0, 1, "is not inside the video"},
		{"negative", 4000, -1, "is negative"},
	} {
		err := checkSeekTime(videoInfo{durationMS: c.durationMS}, c.ms)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: checkSeekTime(%d ms of %d) = %v, want %q", c.name, c.ms, c.durationMS, err, c.want)
		}
	}
}

func TestCheckVideoInfo(t *testing.T) {
	t.Parallel()
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

func TestVideoEncodeRefusesOversizedProbeBeforeSampling(t *testing.T) {
	t.Parallel()
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
		for _, entry := range videoEntries() {
			t.Run(name+"/"+entry.name, func(t *testing.T) {
				p := requireProcessor(t)
				var tools []string
				p.run = func(_ context.Context, in *os.File, tool string, _ ...string) (runResult, error) {
					tools = append(tools, tool)
					return runResult{Stdout: []byte(probe)}, nil
				}
				err := entry.call(p, mp4, Encoding{}, 0, t.TempDir())
				if !errors.Is(err, ErrTooLarge) {
					t.Errorf("err = %v, want ErrTooLarge", err)
				}
				if !slices.Equal(tools, []string{"ffprobe"}) {
					t.Errorf("tools run = %v, want only the probe", tools)
				}
			})
		}
	}
}

func TestVideoToolRunsShareTheInputArguments(t *testing.T) {
	forceSeek(t)
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
				" -filter_threads " + strconv.Itoa(defaultThreads) + " ",
				" -threads " + strconv.Itoa(defaultThreads) + " -protocol_whitelist ",
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

func TestVideoEncodeAtMostOneBatchPerMagickRun(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	// Two 160x120 samples per run.
	old := encodeBatchPixels
	encodeBatchPixels = 2 * 160 * 120
	t.Cleanup(func() { encodeBatchPixels = old })
	rec := record(p)
	images, _ := videoInto(t, p, sdrClip(t, env, "160x120", 10, 4), Encoding{}, 5)
	if len(images) < 3 {
		t.Fatalf("%d images, want several", len(images))
	}
	inputs, runs := 0, 0
	for _, call := range rec.calls {
		if call[0] != "magick" {
			continue
		}
		n := 0
		for _, a := range call[1:] {
			if strings.HasPrefix(a, "pam:") {
				n++
			}
		}
		if n > 2 {
			t.Errorf("magick run names %d input frames, the batch is 2: %q", n, call)
		}
		inputs += n
		if n > 0 {
			runs++
		}
	}
	if inputs < len(images) || runs < 2 {
		t.Errorf("%d inputs in %d runs for %d images", inputs, runs, len(images))
	}
}

func TestVideoEncodeDedupesAcrossBatches(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	old := encodeBatchPixels
	encodeBatchPixels = 160 * 120
	t.Cleanup(func() { encodeBatchPixels = old })
	src := writeClip(t, env, ".mp4", "-f", "lavfi", "-i", "color=c=0x3070c0:size=160x120:rate=10:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	images, out := videoInto(t, p, src, Encoding{}, 4)
	if len(images) != 1 {
		t.Fatalf("images = %+v", images)
	}
	if got := dirNames(t, out); !slices.Equal(got, []string{"frame-000.jpg"}) {
		t.Errorf("output dir holds %v", got)
	}
}

func TestVideoEncodeWritesIntoDirectoryWithPercent(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	out := filepath.Join(t.TempDir(), "100%d-%03d%%")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	images, _, err := p.encodeEven(context.Background(), sdrClip(t, env, "160x120", 10, 4), Encoding{}, 3, out)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, im := range images {
		want = append(want, im.Name)
	}
	if got := dirNames(t, out); len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("output dir holds %v, want %v", got, want)
	}
}

func TestVideoEncodeUseFewerThreadsAboveFourK(t *testing.T) {
	t.Parallel()
	mp4 := writeFile(t, "input", ftypBox("isom", "isom", "mp42"))
	cases := []struct {
		name     string
		w, h     int
		transfer string
		rotation int
		want     string
	}{
		{"UHD", 3840, 2160, "bt709", 0, "4"},
		{"DCI 4K", 4096, 2160, "bt709", 0, "4"},
		{"one column past DCI 4K", 4097, 2160, "bt709", 0, "2"},
		{"8K SDR", 7680, 4320, "bt709", 0, "2"},
		{"8K HLG", 7680, 4320, "arib-std-b67", 0, "2"},
		{"rotated 4097x2160", 4097, 2160, "bt709", 90, "2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			probe := fmt.Sprintf(`{"streams":[{"index":0,"codec_type":"video","width":%d,"height":%d,`+
				`"duration":"10","color_transfer":%q,"side_data_list":[{"rotation":%d}]}],"format":{}}`,
				c.w, c.h, c.transfer, c.rotation)
			rec := record(p)
			rec.inner = func(_ context.Context, _ *os.File, tool string, _ ...string) (runResult, error) {
				if tool == "ffprobe" {
					return runResult{Stdout: []byte(probe)}, nil
				}
				return runResult{}, nil
			}
			// The fake writes no samples, so the call ends in an error after
			// the ffmpeg runs have been recorded.
			_, _, _ = p.encodeEven(context.Background(), mp4, Encoding{}, 3, t.TempDir())
			ran := 0
			for _, call := range rec.calls {
				if call[0] != "ffmpeg" {
					continue
				}
				ran++
				joined := " " + strings.Join(call[1:], " ") + " "
				for _, want := range []string{
					" -filter_threads " + c.want + " ",
					" -threads " + c.want + " ",
				} {
					if !strings.Contains(joined, want) {
						t.Errorf("ffmpeg call lacks %q: %q", want, call)
					}
				}
			}
			if ran == 0 {
				t.Fatal("no ffmpeg call recorded")
			}
		})
	}
}

// Resources.Threads reaches the probe and every ffmpeg decode of a Processor,
// with the large-source rule as a ceiling.
func TestVideoEncodeUseTheConfiguredThreads(t *testing.T) {
	t.Parallel()
	mp4 := writeFile(t, "input", ftypBox("isom", "isom", "mp42"))
	for _, c := range []struct {
		name    string
		threads int
		w, h    int
		want    string
	}{
		{"below the large-source size", 3, 1920, 1080, "3"},
		{"above it, more threads than the rule allows", 3, 4097, 2160, "2"},
		{"above it, fewer threads than the rule allows", 1, 4097, 2160, "1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessorWith(t, Config{Resources: Resources{Threads: c.threads}, Unsandboxed: true})
			probe := fmt.Sprintf(`{"streams":[{"index":0,"codec_type":"video","width":%d,"height":%d,"duration":"10"}],"format":{}}`, c.w, c.h)
			rec := record(p)
			rec.inner = func(_ context.Context, _ *os.File, tool string, _ ...string) (runResult, error) {
				if tool == "ffprobe" {
					return runResult{Stdout: []byte(probe)}, nil
				}
				return runResult{}, nil
			}
			_, _, _ = p.encodeEven(context.Background(), mp4, Encoding{}, 3, t.TempDir())
			probed, decoded := 0, 0
			for _, call := range rec.calls {
				joined := " " + strings.Join(call[1:], " ") + " "
				switch call[0] {
				case "ffprobe":
					probed++
					if want := " -threads " + strconv.Itoa(c.threads) + " "; !strings.Contains(joined, want) {
						t.Errorf("ffprobe call lacks %q: %q", want, call)
					}
				case "ffmpeg":
					decoded++
					for _, want := range []string{" -filter_threads " + c.want + " ", " -threads " + c.want + " "} {
						if !strings.Contains(joined, want) {
							t.Errorf("ffmpeg call lacks %q: %q", want, call)
						}
					}
				}
			}
			if probed == 0 || decoded == 0 {
				t.Fatalf("recorded %d probes and %d decodes", probed, decoded)
			}
		})
	}
}
