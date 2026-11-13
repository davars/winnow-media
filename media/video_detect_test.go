package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ffClip writes a one-second 160x120 clip at 10 fps with the given ffmpeg
// output arguments, and returns it staged under a name with no extension.
func ffClip(t *testing.T, codec string, out ...string) string {
	t.Helper()
	args := append(testsrc("160x120", 10, 1), "-c:v", codec)
	return writeClip(t, requireFFmpeg(t), ".bin", append(args, out...)...)
}

// quickTimeWithoutFtyp returns a QuickTime file whose ftyp box is overwritten
// by a free box of the same length, so every chunk offset stays valid.
func quickTimeWithoutFtyp(t *testing.T) string {
	t.Helper()
	path := ffClip(t, "libx264", "-pix_fmt", "yuv420p", "-f", "mov")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[4:8]) != "ftyp" {
		t.Fatalf("clip starts with %q, want an ftyp box", data[4:8])
	}
	copy(data[4:8], "free")
	return writeFile(t, "input", data)
}

// mp4Brand returns an MP4 whose major brand is brand.
func mp4Brand(t *testing.T, brand string) string {
	t.Helper()
	return ffClip(t, "libx264", "-pix_fmt", "yuv420p", "-brand", brand, "-f", "mp4")
}

// detectedFixtures are videos whose bytes sniff names no type for.
var detectedFixtures = []struct {
	name  string
	typ   string
	build func(t *testing.T) string
}{
	{"avi", typeAVI, func(t *testing.T) string { return ffClip(t, "mpeg4", "-f", "avi") }},
	{"asf", typeASF, func(t *testing.T) string { return ffClip(t, "wmv2", "-f", "asf") }},
	{"mpeg-ps", typeMPEG, func(t *testing.T) string { return ffClip(t, "mpeg2video", "-f", "mpeg") }},
	{"mpeg2-es", typeMPEG, func(t *testing.T) string { return ffClip(t, "mpeg2video", "-r", "25", "-f", "mpeg2video") }},
	{"mpeg1-es", typeMPEG, func(t *testing.T) string { return ffClip(t, "mpeg1video", "-r", "25", "-f", "mpeg1video") }},
	{"mpeg-ts", typeMPEGTS, func(t *testing.T) string { return ffClip(t, "mpeg2video", "-f", "mpegts") }},
	{"flv", typeFLV, func(t *testing.T) string { return ffClip(t, "flv1", "-f", "flv") }},
	{"quicktime-no-ftyp", typeMOV, quickTimeWithoutFtyp},
	{"brand-3gs6", typeMP4, func(t *testing.T) string { return mp4Brand(t, "3gs6") }},
	{"brand-M4VH", typeMP4, func(t *testing.T) string { return mp4Brand(t, "M4VH") }},
}

func TestSniffNamesNoTypeForDetectedFixtures(t *testing.T) {
	t.Parallel()
	for _, f := range detectedFixtures {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			if typ, err := sniffPath(f.build(t)); err == nil {
				t.Errorf("sniff names %s; the fixture would not reach the probe", typ)
			}
		})
	}
}

func TestOpenNamesAContainerTheBytesDoNot(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	for _, f := range detectedFixtures {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			src, _, err := p.Open(context.Background(), f.build(t), OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = src.Close() }()
			info := src.Info()
			if info.Kind != Video || info.Type != f.typ {
				t.Errorf("Kind %v Type %q, want Video %q", info.Kind, info.Type, f.typ)
			}
			if info.Width != 160 || info.Height != 120 || (info.Video.Duration > 1300*time.Millisecond || (info.Video.Duration < 800*time.Millisecond && !strings.HasSuffix(f.name, "-es"))) || info.Video.Codec == "" {
				t.Errorf("Info = %+v, want 160x120, about a second (an elementary stream has no duration), with a codec", info)
			}
			imgs, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Even(1)}, Encoding{Format: PNG, MaxEdge: 4096, MaxArea: 20_000_000})
			if err != nil || len(imgs) != 1 || imgs[0].Width != 160 || imgs[0].Height != 120 {
				t.Fatalf("Encode: %v, %v", imgs, err)
			}
		})
	}
}

// toolRuns records the tool runs of a Processor.
type toolRuns struct {
	mu   sync.Mutex
	runs [][]string
}

func (r *toolRuns) wrap(next runner) runner {
	return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
		r.mu.Lock()
		r.runs = append(r.runs, append([]string{filepath.Base(spec.Tool)}, spec.Args...))
		r.mu.Unlock()
		return next(ctx, spec, stdout, stderr)
	}
}

// of returns the runs of the tool.
func (r *toolRuns) of(tool string) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, run := range r.runs {
		if run[0] == tool {
			out = append(out, run)
		}
	}
	return out
}

func recordingProcessor(t *testing.T, types []string) (*Processor, *toolRuns) {
	t.Helper()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	var runs toolRuns
	p, err := newProcessor(context.Background(), Config{Types: types, Dir: t.TempDir()}, runs.wrap)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	runs.mu.Lock()
	runs.runs = nil
	runs.mu.Unlock()
	return p, &runs
}

func TestDetectionProbeIsRestrictedToTheConfiguredContainers(t *testing.T) {
	t.Parallel()
	p, runs := recordingProcessor(t, []string{typeAVI, typeMP4, "image/png"})
	src, _, err := p.Open(context.Background(), ffClip(t, "mpeg4", "-f", "avi"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
	probes := runs.of("ffprobe")
	if len(probes) != 1 {
		t.Fatalf("%d probes, want the one that names the container", len(probes))
	}
	argv := strings.Join(probes[0], " ")
	for _, want := range []string{"-protocol_whitelist file", "-format_whitelist avi,mov "} {
		if !strings.Contains(argv, want) {
			t.Errorf("probe argv %q lacks %q", argv, want)
		}
	}
	if !slices.Contains(probes[0], "/dev/fd/3") {
		t.Errorf("probe argv %q does not read the input descriptor", argv)
	}
}

func TestDetectedContainerOutsideTypesIsRefusedBeforeAnyDecode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		types     []string
		wantProbe bool
	}{
		{"another detected type configured", []string{typeMP4, "image/png"}, true},
		{"no detected type configured", []string{"image/png"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, runs := recordingProcessor(t, tc.types)
			path := ffClip(t, "mpeg4", "-f", "avi")
			if _, _, err := p.Open(context.Background(), path, OpenOptions{}); !isUnsupported(err) {
				t.Errorf("Open: err = %v, want ErrUnsupportedType", err)
			}
			if n := len(runs.of("ffmpeg")); n != 0 {
				t.Errorf("%d ffmpeg runs before the refusal", n)
			}
			if got := len(runs.of("ffprobe")) > 0; got != tc.wantProbe {
				t.Errorf("probed = %v, want %v", got, tc.wantProbe)
			}
		})
	}
}

func TestDetectedMOVContainerIsNamedOnlyAsAConfiguredType(t *testing.T) {
	t.Parallel()
	// The mov demuxer reads both; a file with no ftyp is QuickTime, one with
	// an unknown brand MP4, and neither is named a type that is not configured.
	for _, c := range []struct {
		name  string
		types []string
		path  string
		want  string // "" for a refusal
	}{
		{"no ftyp, only MP4", []string{typeMP4}, quickTimeWithoutFtyp(t), ""},
		{"no ftyp, only QuickTime", []string{typeMOV}, quickTimeWithoutFtyp(t), typeMOV},
		{"brand, only QuickTime", []string{typeMOV}, mp4Brand(t, "3gs6"), ""},
		{"brand, only MP4", []string{typeMP4}, mp4Brand(t, "3gs6"), typeMP4},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessorWith(t, Config{Types: c.types})
			src, _, err := p.Open(context.Background(), c.path, OpenOptions{})
			if c.want == "" {
				if !isUnsupported(err) {
					t.Errorf("err = %v, want ErrUnsupportedType", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = src.Close() }()
			if got := src.Info().Type; got != c.want {
				t.Errorf("Type = %q, want %q", got, c.want)
			}
		})
	}
}

func TestImagesAreNotProbedAsVideos(t *testing.T) {
	t.Parallel()
	p, runs := recordingProcessor(t, nil)
	path := gradient(".jpg")(t)
	src, _, err := p.Open(context.Background(), path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
	if n := len(runs.of("ffprobe")); n != 0 {
		t.Errorf("%d probes of a JPEG", n)
	}
}

func TestStillBrandsAreNeverProbedAsVideos(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		major      string
		compatible []string
	}{
		{"crx ", []string{"isom"}},
		{"jp2 ", []string{"isom"}},
		{"mif1", []string{"isom"}},
		{"avci", []string{"isom"}},
		{"avis", []string{"avis", "avif", "msf1", "iso8", "mif1", "miaf", "MA1B"}},
		{"MiHB", []string{"mif1", "heic", "miaf"}},
		{"jpeg", []string{"mif1", "miaf"}},
		{"vvic", []string{"mif1", "msf1"}},
	} {
		brand := c.major
		t.Run(brand, func(t *testing.T) {
			t.Parallel()
			p, runs := recordingProcessor(t, nil)
			box := ftypBox(brand, c.compatible...)
			data := append(box, binary.BigEndian.AppendUint32(nil, 8)...)
			data = append(data, "free"...)
			if _, _, err := p.Open(context.Background(), writeFile(t, "input", data), OpenOptions{}); !isUnsupported(err) {
				t.Errorf("Open: err = %v, want ErrUnsupportedType", err)
			}
			if n := len(runs.of("ffprobe")); n != 0 {
				t.Errorf("%d probes", n)
			}
		})
	}
}

func TestDetectionRefusesWhatNoConfiguredDemuxerReads(t *testing.T) {
	t.Parallel()
	p, runs := recordingProcessor(t, []string{typeAVI, typeMP4})
	// A well-formed FLV is a video, but not of a configured container.
	for name, path := range map[string]string{
		"flv":     ffClip(t, "flv1", "-f", "flv"),
		"garbage": writeFile(t, "input", []byte(strings.Repeat("not a video ", 400))),
	} {
		if _, _, err := p.Open(context.Background(), path, OpenOptions{}); !isUnsupported(err) {
			t.Errorf("%s: err = %v, want ErrUnsupportedType", name, err)
		}
	}
	if n := len(runs.of("ffmpeg")); n != 0 {
		t.Errorf("%d ffmpeg runs", n)
	}
}

func TestElementaryMPEGStreamsDecodeAndPinMpegvideo(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	for _, codec := range []string{"mpeg1video", "mpeg2video"} {
		t.Run(codec, func(t *testing.T) {
			t.Parallel()
			path := ffClip(t, codec, "-r", "25", "-f", codec)
			if w, h := decodeFixture(t, p, typeMPEG, path, ""); w != 160 || h != 120 {
				t.Errorf("decoded %dx%d", w, h)
			}
			src, _, err := p.Open(context.Background(), path, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = src.Close() }()
			if src.dec != (decoder{"ffmpeg", "mpegvideo"}) {
				t.Errorf("decoder = %v, want ffmpeg mpegvideo", src.dec)
			}
		})
	}
}

func TestHEIFBehindAFreeBoxIsNotProbedAsVideo(t *testing.T) {
	t.Parallel()
	p, runs := recordingProcessor(t, nil)
	heic, err := os.ReadFile(heicFixtureFile(t, "adobe-64x32"))
	if err != nil {
		t.Fatal(err)
	}
	data := append([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}, heic...)
	src, _, err := p.Open(context.Background(), writeFile(t, "input", data), OpenOptions{})
	if err == nil {
		defer func() { _ = src.Close() }()
		if src.Info().Kind == Video {
			t.Errorf("Info = %+v, want no video", src.Info())
		}
	}
	if n := len(runs.of("ffprobe")); n != 0 {
		t.Errorf("%d probes of a HEIF", n)
	}
}

// aviHead opens like an AVI, so the gate probes it first.
var aviHead = append([]byte("RIFF\x00\x00\x00\x00AVI "), make([]byte, 64)...)

// stubbedProcessor is a Processor whose tool runs go to fn, and the names of
// the tools it was asked to run.
func stubbedProcessor(t *testing.T, types []string, fn func(name string) (runResult, error)) (*Processor, func() []string) {
	t.Helper()
	p := requireProcessorWith(t, Config{Types: types})
	var mu sync.Mutex
	var names []string
	p.run = func(_ context.Context, in *os.File, name string, _ ...string) (runResult, error) {
		mu.Lock()
		names = append(names, name)
		mu.Unlock()
		return fn(name)
	}
	return p, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(names)
	}
}

var errProbeRefusal error = &ToolError{Tool: "ffprobe", ExitCode: 183, Stderr: []byte("Invalid data found when processing input")}

func probeOutput(width, height, streams int) []byte {
	var parts []string
	for i := range streams {
		parts = append(parts, fmt.Sprintf(`{"index":%d,"codec_name":"mpeg4","codec_type":"video","width":%d,"height":%d,"r_frame_rate":"10/1"}`, i, width, height))
	}
	return []byte(`{"streams":[` + strings.Join(parts, ",") + `],"format":{"format_name":"avi","duration":"1.0"}}`)
}

func TestProbeCapsRefuseWithErrTooLargeBeforeAnyDecode(t *testing.T) {
	t.Parallel()
	for name, out := range map[string][]byte{
		"dimension": probeOutput(maxDimension+1, 100, 1),
		"streams":   probeOutput(100, 100, maxStreams+1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, calls := stubbedProcessor(t, nil, func(tool string) (runResult, error) {
				if tool == "ffprobe" {
					return runResult{Stdout: out}, nil
				}
				return runResult{}, errors.New("unexpected tool " + tool)
			})
			if _, _, err := p.Open(context.Background(), writeFile(t, "input", aviHead), OpenOptions{}); !errors.Is(err, ErrTooLarge) {
				t.Errorf("err = %v, want ErrTooLarge", err)
			}
			if got := calls(); !slices.Equal(got, []string{"ffprobe"}) {
				t.Errorf("tool runs %v, want one probe", got)
			}
		})
	}
}

// MaxFramePixels covers a video's stored size, inclusively, whether a probe
// named the type or the bytes did.
func TestVideoFramePixelsCapIsAppliedAtOpen(t *testing.T) {
	t.Parallel()
	for name, head := range map[string][]byte{
		"named by a probe":   aviHead,
		"named by the bytes": ftypBox("isom", "isom", "mp42"),
	} {
		for _, c := range []struct {
			cap     int64
			refused bool
		}{{20_000, false}, {19_999, true}} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				p := requireProcessorWith(t, Config{Caps: Caps{MaxFramePixels: c.cap}})
				p.run = func(_ context.Context, _ *os.File, tool string, _ ...string) (runResult, error) {
					if tool != "ffprobe" {
						return runResult{}, errors.New("unexpected tool " + tool)
					}
					return runResult{Stdout: probeOutput(200, 100, 1)}, nil
				}
				src, _, err := p.Open(context.Background(), writeFile(t, "input", head), OpenOptions{})
				if src != nil {
					_ = src.Close()
				}
				if got := errors.Is(err, ErrTooLarge); got != c.refused || (err != nil && !got) {
					t.Errorf("200x100 against a cap of %d: err = %v, want ErrTooLarge = %v", c.cap, err, c.refused)
				}
			})
		}
	}
}

func TestProbeFailuresThatSayNothingOfTheFileAreNotRefusals(t *testing.T) {
	t.Parallel()
	for name, probeErr := range map[string]error{
		"time limit":                      fmt.Errorf("ffprobe: %w: %w", ErrTimeLimit, &ToolError{Tool: "ffprobe", ExitCode: 1, Stderr: []byte("Invalid data found")}),
		"too large":                       fmt.Errorf("ffprobe: %w: %w", tooLarge("a cap"), &ToolError{Tool: "ffprobe", ExitCode: 1, Stderr: []byte("Invalid data found")}),
		"killed":                          &ToolError{Tool: "ffprobe", ExitCode: -1, Signal: syscall.SIGKILL},
		"killed after printing a refusal": &ToolError{Tool: "ffprobe", ExitCode: -1, Signal: syscall.SIGKILL, Stderr: []byte("Invalid data found")},
		"fork failed":                     errors.New("ffprobe: fork/exec: resource temporarily unavailable"),
		"memory":                          &ToolError{Tool: "ffprobe", ExitCode: 1, Stderr: []byte("Cannot allocate memory")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, _ := stubbedProcessor(t, nil, func(tool string) (runResult, error) {
				if tool == "ffprobe" {
					return runResult{}, probeErr
				}
				return runResult{}, noDelegate
			})
			_, _, err := p.Open(context.Background(), writeFile(t, "input", aviHead), OpenOptions{})
			if err == nil || isUnsupported(err) {
				t.Errorf("err = %v, want an error that is not ErrUnsupportedType", err)
			}
			for _, sentinel := range []error{ErrTooLarge, ErrTimeLimit} {
				if errors.Is(probeErr, sentinel) && !errors.Is(err, sentinel) {
					t.Errorf("err = %v, want it to wrap %v", err, sentinel)
				}
			}
		})
	}
}

func TestSignalKilledProbeOfAnAVIIsNotUnsupported(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	p, err := newProcessor(context.Background(), Config{Dir: t.TempDir()}, func(next runner) runner {
		return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
			if filepath.Base(spec.Tool) == "ffprobe" {
				return toolResult{Signal: 9}, nil
			}
			return next(ctx, spec, stdout, stderr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	_, _, err = p.Open(context.Background(), ffClip(t, "mpeg4", "-f", "avi"), OpenOptions{})
	if err == nil || isUnsupported(err) {
		t.Errorf("err = %v, want an error that is not ErrUnsupportedType", err)
	}
}

func TestUnknownISOBrandIsProbedOnceAndNeverReadAsAnImage(t *testing.T) {
	t.Parallel()
	p, calls := stubbedProcessor(t, nil, func(tool string) (runResult, error) { return runResult{}, errProbeRefusal })
	audio := writeFile(t, "input", ftypBox("M4A ", "M4A ", "mp42"))
	if _, _, err := p.Open(context.Background(), audio, OpenOptions{}); !isUnsupported(err) {
		t.Errorf("err = %v, want ErrUnsupportedType", err)
	}
	if got := calls(); !slices.Equal(got, []string{"ffprobe"}) {
		t.Errorf("tool runs %v, want one probe and no header read", got)
	}
}

func TestRefusedProbeIsFollowedByTheHeaderRead(t *testing.T) {
	t.Parallel()
	p, calls := stubbedProcessor(t, nil, func(tool string) (runResult, error) {
		if tool == "ffprobe" {
			return runResult{}, errProbeRefusal
		}
		return runResult{}, noDelegate
	})
	if _, _, err := p.Open(context.Background(), writeFile(t, "input", aviHead), OpenOptions{}); !isUnsupported(err) {
		t.Errorf("err = %v, want ErrUnsupportedType", err)
	}
	if got := calls(); !slices.Equal(got, []string{"ffprobe", "magick"}) {
		t.Errorf("tool runs %v, want the probe, then the header read", got)
	}
}

func TestVideoLookingHeadOverDangerousContentIsRefused(t *testing.T) {
	t.Parallel()
	secret := writeFile(t, "secret.txt", []byte("secret"))
	for name, body := range map[string]string{
		"svg": `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="8" height="8"><image width="8" height="8" xlink:href="file://` + secret + `"/></svg>`,
		"mvg": "push graphic-context\nviewbox 0 0 64 64\nimage over 0,0 0,0 'text:" + secret + "'\npop graphic-context\n",
	} {
		for _, head := range []string{"RIFF\x00\x00\x00\x00AVI ", "\x00\x00\x01\xba", "FLV\x01"} {
			t.Run(name+"/"+strings.TrimSpace(head[:4]), func(t *testing.T) {
				t.Parallel()
				p, runs := recordingProcessor(t, nil)
				path := writeFile(t, "input", []byte(head+body))
				if _, _, err := p.Open(context.Background(), path, OpenOptions{}); !isUnsupported(err) {
					t.Errorf("err = %v, want ErrUnsupportedType", err)
				}
				if n := len(runs.of("ffmpeg")); n != 0 {
					t.Errorf("%d ffmpeg runs", n)
				}
				for _, run := range runs.of("magick") {
					if !slices.Contains(run, "-ping") || !slices.Contains(run, gateHeaderFormat) {
						t.Errorf("magick run %q is not the gate's header read", run)
					}
				}
			})
		}
	}
}

var noDelegate error = &ToolError{Tool: "magick", ExitCode: 1, Stderr: []byte("no decode delegate for this image format")}
