package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color/palette"
	"image/gif"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

var openTargets = []Encoding{
	{Format: WebP, Quality: 80, MaxEdge: 48, MaxArea: 48 * 48},
	{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100},
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Frames of a video are its sample times, final once decoded, and the
// artifacts map each kept frame to its time.
func TestVideoSourceFramesAreSampleTimes(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := sdrClip(t, env, "320x240", 10, 4)
	info, err := probeVideo(context.Background(), env, sniffedVideoDecoder(clip), clip)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ hint, want int }{{0, 15}, {4, 4}, {64, 64}} {
		src := openSource(t, env, clip, Hints{Frames: c.hint})
		times := sampleTimesMS(info, c.want)
		var want []Frame
		for _, ms := range times {
			want = append(want, Frame{SourceTimeMS: ms})
		}
		if got := src.Frames(); !slices.Equal(got, want) {
			t.Errorf("Frames{%d} = %v, want %v", c.hint, got, want)
		}
		arts, err := src.Encode(context.Background(), 0, openTargets[1])
		if err != nil {
			t.Fatal(err)
		}
		if got := src.Frames(); !slices.Equal(got, want) {
			t.Errorf("Frames{%d} after encode = %v, want %v", c.hint, got, want)
		}
		if got := arts[0].Frames; !slices.Equal(got, want) {
			t.Errorf("Artifact.Frames{%d} = %v, want %v", c.hint, got, want)
		}
	}

	// maxFrames chooses among the samples, evenly from the first.
	src := openSource(t, env, clip, Hints{Frames: 8})
	arts, err := src.Encode(context.Background(), 2, openTargets[1])
	if err != nil {
		t.Fatal(err)
	}
	all := src.Frames()
	if got := arts[0].Frames; len(got) != 2 || got[0] != all[0] || !slices.Contains(all, got[1]) {
		t.Errorf("Artifact.Frames = %v, want 2 of %v, the first among them", got, all)
	}
}

// A run of identical samples is merged into one scene, the artifact says
// which samples its scenes show, and the written file holds exactly those
// scenes.
func TestVideoMapsTheScenesTheWriterMerged(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := writeClip(t, env, ".mp4",
		"-f", "lavfi", "-i", "color=c=red:size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=2",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	src := openSource(t, env, clip, Hints{})
	arts, err := src.Encode(context.Background(), 0, openTargets[1])
	if err != nil {
		t.Fatal(err)
	}
	frames := arts[0].Frames
	if len(frames) < 2 || len(frames) >= len(src.Frames()) {
		t.Fatalf("kept %d of %d samples, want the red run collapsed to one", len(frames), len(src.Frames()))
	}
	if frames[0].SourceTimeMS != src.Frames()[0].SourceTimeMS || frames[1].SourceTimeMS < 2000 {
		t.Errorf("kept times %v: want the first red sample, then the first testsrc sample from 2 s", frames)
	}
	if scenes := sceneSizes(t, env, arts[0].Path); len(scenes) != len(frames) {
		t.Errorf("file holds %d scenes, mapping describes %d", len(scenes), len(frames))
	}
}

// Samples that are not reduced and are already within the target are not
// resampled, as a still is not; a reduced one is.
func TestVideoSamplesWithinTheTargetAreNotResized(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	small := sdrClip(t, env, "64x48", 10, 2)
	for _, c := range []struct {
		name       string
		clip       string
		resizes    bool
		maxEdge    int
		wantWidth  int
		wantHeight int
	}{
		{"fits", small, false, 100, 64, 48},
		{"larger than the target", small, true, 32, 32, 24},
	} {
		t.Run(c.name, func(t *testing.T) {
			counted := withRunCounts(env)
			src := openSource(t, counted.Env, c.clip, Hints{Frames: 3})
			arts, err := src.Encode(context.Background(), 0, Encoding{Format: WebP, Quality: 80, MaxEdge: c.maxEdge, MaxArea: c.maxEdge * c.maxEdge})
			if err != nil {
				t.Fatal(err)
			}
			if arts[0].Width != c.wantWidth || arts[0].Height != c.wantHeight {
				t.Errorf("size %dx%d, want %dx%d", arts[0].Width, arts[0].Height, c.wantWidth, c.wantHeight)
			}
			encodes := 0
			for _, args := range counted.runs["magick"] {
				if !slices.Contains(args, "-strip") {
					continue
				}
				encodes++
				if resized := slices.Contains(args, "-resize"); resized != c.resizes {
					t.Errorf("run resizes = %v, want %v: %v", resized, c.resizes, args)
				}
			}
			if encodes == 0 {
				t.Fatal("no encode run")
			}
		})
	}
}

// A second encode at a larger target decodes again unless the first decode
// already covers it, so its frames are what a source opened for that target
// alone would give. A decode even a little smaller than the margin requires
// is not reused.
func TestVideoSourceDecodesAgainForALargerTarget(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := sdrClip(t, env, "1280x720", 10, 3)
	large := Encoding{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100}
	once := openSource(t, env, clip, Hints{Frames: 4})
	alone, err := once.Encode(context.Background(), 0, large)
	if err != nil {
		t.Fatal(err)
	}
	// Decodes for edges 48, 60 and 90 are 96, 120 and 180 wide against the
	// 200 the large target needs.
	for _, edge := range []int{48, 60, 90, 99} {
		t.Run(strconv.Itoa(edge), func(t *testing.T) {
			small := Encoding{Format: WebP, Quality: 80, MaxEdge: edge, MaxArea: edge * edge}
			counted := withRunCounts(env)
			both := openSource(t, counted.Env, clip, Hints{Frames: 4})
			if _, err := both.Encode(context.Background(), 0, small); err != nil {
				t.Fatal(err)
			}
			got, err := both.Encode(context.Background(), 0, large)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, got[0].Path), readFile(t, alone[0].Path)) {
				t.Error("the large encode of a source first encoded small differs from one encoded large alone")
			}

			// A smaller target after a larger one reuses the samples.
			before := len(counted.runs["ffmpeg"])
			if _, err := both.Encode(context.Background(), 0, small); err != nil {
				t.Fatal(err)
			}
			if n := len(counted.runs["ffmpeg"]); n != before {
				t.Errorf("a smaller encode ran ffmpeg %d more times", n-before)
			}
		})
	}
}

// The decode serves the target with the largest output, whatever its edge:
// that target is encoded as it is alone, and the other has the size it has
// alone. Its bytes may differ, as it is resampled from a larger decode.
func TestVideoSourceDecodesForTheLargestOutput(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := sdrClip(t, env, "1280x720", 10, 3)
	small := Encoding{Format: WebP, Quality: 80, MaxEdge: 1280, MaxArea: 256 * 144}
	large := Encoding{Format: WebP, Quality: 80, MaxEdge: 640, MaxArea: 640 * 360}
	alone := func(enc Encoding) Artifact {
		arts, err := openSource(t, env, clip, Hints{Frames: 4}).Encode(context.Background(), 0, enc)
		if err != nil {
			t.Fatal(err)
		}
		return arts[0]
	}
	wantSmall, wantLarge := alone(small), alone(large)
	for name, order := range map[string][]Encoding{"small first": {small, large}, "large first": {large, small}} {
		t.Run(name, func(t *testing.T) {
			arts, err := openSource(t, env, clip, Hints{Frames: 4}).Encode(context.Background(), 0, order...)
			if err != nil {
				t.Fatal(err)
			}
			gotSmall, gotLarge := arts[0], arts[1]
			if name == "large first" {
				gotSmall, gotLarge = arts[1], arts[0]
			}
			if !bytes.Equal(readFile(t, gotLarge.Path), readFile(t, wantLarge.Path)) {
				t.Error("the larger target beside another differs from alone")
			}
			if gotSmall.Width != wantSmall.Width || gotSmall.Height != wantSmall.Height {
				t.Errorf("the smaller target is %dx%d, alone %dx%d", gotSmall.Width, gotSmall.Height, wantSmall.Width, wantSmall.Height)
			}
		})
	}
}

// A failed decode leaves the source as Open made it, so Frames is intact and
// the encode can be retried.
func TestVideoSourceSurvivesAFailedDecode(t *testing.T) {
	real := requireFFmpeg(t)
	clip := sdrClip(t, real, "320x240", 10, 4)
	for name, c := range map[string]struct {
		limit  int64
		failAt int32
	}{
		"seek, the third sample": {0, 3},
		"select, the one run":    {selectMaxPixels, 1},
	} {
		t.Run(name, func(t *testing.T) {
			forceSelectLimit(t, c.limit)
			env := stagedEnv(real, t.TempDir())
			var calls, failAt atomic.Int32
			failAt.Store(c.failAt)
			inner := env.Run
			env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
				if name == "ffmpeg" && calls.Add(1) == failAt.Load() {
					return Result{}, errors.New("injected ffmpeg failure")
				}
				return inner(ctx, name, args...)
			}
			src := openSource(t, env, clip, Hints{Frames: 6})
			before := src.Frames()
			if len(before) != 6 {
				t.Fatalf("Open chose %d times", len(before))
			}
			if _, err := src.Encode(context.Background(), 0, openTargets[1]); err == nil {
				t.Fatal("encode succeeded")
			}
			if got := src.Frames(); !slices.Equal(got, before) {
				t.Errorf("Frames after the failure = %v, want %v", got, before)
			}
			failAt.Store(-1)
			arts, err := src.Encode(context.Background(), 0, openTargets[1])
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if !slices.Equal(arts[0].Frames, before) {
				t.Errorf("retry holds %v, want %v", arts[0].Frames, before)
			}
		})
	}
}

// Leading bytes that look like a video container do not take a file from the
// image route: sniff names a GIF before anything else is looked at, and a
// file the probe refuses is read as an image.
func TestOpenKeepsImagesThatLookLikeVideo(t *testing.T) {
	t.Parallel()
	env := stagedEnv(requireFFmpeg(t), t.TempDir())
	img := image.NewPaletted(image.Rect(0, 0, 32, 32), palette.Plan9)
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 256)
	}
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, &gif.Options{NumColors: 256}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	// 0x47 at 188 and 376 are two transport stream sync bytes more than a
	// GIF's 'G' at 0.
	b[188], b[376] = 0x47, 0x47
	counted := withRunCounts(env)
	src := openSource(t, counted.Env, writeFile(t, "gif", b), Hints{})
	if src.video != nil || src.route != routeGIF {
		t.Errorf("GIF opened as route %d, video %v", src.route, src.video != nil)
	}
	if n := len(counted.runs["ffprobe"]); n != 0 {
		t.Errorf("a sniffed GIF was probed as a video %d times", n)
	}

	// A WBMP of one column and 7425 rows opens with 00 00 01 BA: type 0,
	// no extension header, width 1 and the height 0xBA 0x01.
	wbmp := append([]byte{0, 0, 1, 0xBA, 1}, make([]byte, 7425)...)
	src = openSource(t, env, writeFile(t, "x.wbmp", wbmp), Hints{})
	if src.video != nil || src.route != routeStill {
		t.Errorf("WBMP opened as route %d, video %v", src.route, src.video != nil)
	}
}

// A QuickTime file that does not open with ftyp is a video, whatever its
// name: it is read through the mov demuxer and never by ImageMagick.
func TestOpenReadsQuickTimeWithoutFtyp(t *testing.T) {
	t.Parallel()
	real := requireFFmpeg(t)
	clip := writeClip(t, real, ".mov", append(testsrc("64x48", 10, 2), "-c:v", "mpeg4")...)
	b := readFile(t, clip)
	if string(b[4:8]) != "ftyp" {
		t.Fatalf("clip opens with %q", b[4:8])
	}
	// Turning ftyp into a free atom of the same size keeps every offset.
	copy(b[4:8], "free")
	for _, name := range []string{"noext", "clip.mov"} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, name, b)
			if _, err := sniffFile(path); !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("sniff = %v, want it to name no type", err)
			}
			counted := withRunCounts(stagedEnv(real, t.TempDir()))
			src := openSource(t, counted.Env, path, Hints{Frames: 3})
			if src.video == nil {
				t.Fatal("opened as an image")
			}
			if src.video.dec != (decoder{"ffmpeg", "mov"}) {
				t.Errorf("decoder = %+v, want mov", src.video.dec)
			}
			if _, err := src.Encode(context.Background(), 0, openTargets[1]); err != nil {
				t.Fatal(err)
			}
			for _, args := range counted.runs["magick"] {
				if slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, name) }) {
					t.Errorf("magick read the video: %v", args)
				}
			}
		})
	}
}

func TestOpenVideoRefusesHintsThatDoNotApply(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := sdrClip(t, env, "64x48", 10, 1)
	for name, h := range map[string]Hints{
		"still":       {Still: true},
		"orientation": {Orientation: 6},
		"negative":    {Frames: -1},
		"too many":    {Frames: maxMaxFrames + 1},
	} {
		if _, err := env.Open(context.Background(), clip, h); err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

func TestOpenVideoRefusesOversizedInputBeforeSampling(t *testing.T) {
	t.Parallel()
	stub := newVideoStub()
	stub.probe = strings.Replace(stub.probe, `"width":64,"height":48`, `"width":20000,"height":48`, 1)
	e := stub.env()
	e.Scratch = testScratch
	path := writeFile(t, "v.mp4", append(ftypBox("isom"), make([]byte, 64)...))
	if _, err := e.Open(context.Background(), path, Hints{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if len(stub.runs) != 0 {
		t.Errorf("ffmpeg ran %d times before the refusal", len(stub.runs))
	}
}

// The Processor's operations and the Env's Open sample alike.
func TestOpenVideoMatchesTheProcessorsFrames(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	clip := sdrClip(t, env, "320x240", 10, 4)
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 100}
	want, _, err := p.VideoFrames(context.Background(), clip, enc, 6, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := openSource(t, env, clip, Hints{Frames: 6})
	dir := t.TempDir()
	got, err := src.EncodeFrames(context.Background(), enc, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("images differ:\nOpen       %+v\nProcessor  %+v", got, want)
	}
	for _, img := range want {
		if img.Frame != 0 || img.TimeMS < 0 {
			t.Errorf("image %+v: want Frame 0 and the sample time", img)
		}
	}
}

// Open reads every container video decoding supports, with no type
// named: the bytes pin MP4, MOV, WebM and Matroska, and ffmpeg detects the
// rest among detectedFormats.
func TestOpenReadsEveryVideoContainer(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := func(args ...string) string {
		path := filepath.Join(t.TempDir(), "input")
		full := append(testsrc("176x144", 10, 2), args...)
		if _, err := env.Run(context.Background(), "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y"}, append(full, path)...)...); err != nil {
			t.Fatal(err)
		}
		return path
	}
	withPrefix := func(path string) string {
		b := append([]byte("junk\n"), readFile(t, path)...)
		return writeFile(t, "prefixed", b)
	}
	ts := clip("-c:v", "mpeg2video", "-f", "mpegts")
	cases := map[string]string{
		"avi":                 clip("-c:v", "mpeg4", "-f", "avi"),
		"mpegts":              ts,
		"asf":                 clip("-c:v", "wmv2", "-f", "asf"),
		"mpeg program stream": clip("-c:v", "mpeg2video", "-f", "mpeg"),
		"raw mpeg1video":      clip("-c:v", "mpeg1video", "-r", "25", "-f", "mpeg1video"),
		"raw mpeg2video":      clip("-c:v", "mpeg2video", "-f", "mpeg2video"),
		"flv":                 clip("-c:v", "flv", "-f", "flv"),
		"3gp":                 clip("-c:v", "h263", "-s", "176x144", "-f", "3gp"),
		"a transport stream the bytes do not lead": withPrefix(ts),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			counted := withRunCounts(env)
			src := openSource(t, counted.Env, path, Hints{Frames: 4})
			arts, err := src.Encode(context.Background(), 0, openTargets[1])
			if err != nil {
				t.Fatal(err)
			}
			// ImageMagick must not be handed a container it could pass to a
			// delegate; only a file nothing names is tried as an image first.
			for _, args := range counted.runs["magick"] {
				if slices.Contains(args, path) && !strings.HasPrefix(name, "a transport stream the bytes") {
					t.Errorf("magick read the video: %v", args)
				}
			}
			if len(arts[0].Frames) == 0 || arts[0].Width != 100 {
				t.Errorf("artifact %dx%d with frames %v", arts[0].Width, arts[0].Height, arts[0].Frames)
			}
		})
	}
}

func TestOpenRefusesWhatIsNeitherImageNorVideo(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	for name, body := range map[string][]byte{
		"text":  []byte("not media at all\n"),
		"empty": {},
		"zeros": make([]byte, 8192),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := env.Open(context.Background(), writeFile(t, "x", body), Hints{}); !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("err = %v, want ErrUnsupportedType", err)
			}
		})
	}
	// A missing file is not a type problem.
	if _, err := env.Open(context.Background(), filepath.Join(t.TempDir(), "absent"), Hints{}); err == nil || errors.Is(err, ErrUnsupportedType) {
		t.Errorf("missing file: err = %v", err)
	}
}

// The Processor's type set is the sniffed one: a container only the Env reads
// is refused.
func TestProcessorVideoOperationsRefuseAnAVI(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "input")
	args := append(testsrc("64x48", 10, 1), "-c:v", "mpeg4", "-f", "avi", path)
	if _, err := env.Run(context.Background(), "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y"}, args...)...); err != nil {
		t.Fatal(err)
	}
	enc := Encoding{Format: WebP, MaxEdge: 64}
	dir := t.TempDir()
	if _, _, err := p.VideoFrames(context.Background(), path, enc, 2, dir); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("VideoFrames: err = %v, want ErrUnsupportedType", err)
	}
	if _, _, err := p.ProbeVideo(context.Background(), path); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("ProbeVideo: err = %v, want ErrUnsupportedType", err)
	}
	assertEmptyDir(t, dir)
}

// A hint that does not apply to a video is an error once the probe has found
// one, whatever the container leads with: the file is never handed to
// ImageMagick, whose delegate would decode the video.
func TestOpenRefusesAVideoHintWithoutReadingItAsAnImage(t *testing.T) {
	t.Parallel()
	real := requireFFmpeg(t)
	clip := func(args ...string) []byte {
		path := filepath.Join(t.TempDir(), "src")
		full := append([]string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=64x48:rate=25", "-t", "1"}, args...)
		if _, err := real.Run(context.Background(), "ffmpeg", append(full, path)...); err != nil {
			t.Fatal(err)
		}
		return readFile(t, path)
	}
	ps := clip("-c:v", "mpeg1video", "-f", "mpeg")
	avi := clip("-c:v", "mpeg4", "-f", "avi")
	for _, c := range []struct {
		name string
		data []byte
		h    Hints
	}{
		{"input", ps, Hints{Still: true}},
		{"clip.mpg", ps, Hints{Still: true}},
		{"clip.mpg", ps, Hints{Frames: 100}},
		{"clip.mpg", ps, Hints{Orientation: 6}},
		{"input", avi, Hints{Still: true}},
		{"clip.avi", avi, Hints{Still: true}},
	} {
		t.Run(fmt.Sprintf("%s/%+v", c.name, c.h), func(t *testing.T) {
			counted := withRunCounts(stagedEnv(real, t.TempDir()))
			src, err := counted.Open(context.Background(), writeFile(t, c.name, c.data), c.h)
			if err == nil {
				src.Close()
				t.Fatalf("Open succeeded as route %d", src.route)
			}
			if errors.Is(err, ErrUnsupportedType) {
				t.Errorf("err = %v, want the hint refused, not the file", err)
			}
			if n := len(counted.runs["magick"]); n != 0 {
				t.Errorf("ImageMagick ran %d times on a video", n)
			}
			if n := len(counted.runs["ffprobe"]); n != 1 {
				t.Errorf("ffprobe ran %d times, want 1", n)
			}
		})
	}
}

// A decode for a larger target that fails after a successful one leaves a
// source that still encodes the smaller target from its own samples.
func TestVideoSourceStillEncodesAfterAFailedRedecode(t *testing.T) {
	real := requireFFmpeg(t)
	clip := sdrClip(t, real, "640x480", 10, 4)
	for name, limit := range map[string]int64{"seek": 0, "select": selectMaxPixels} {
		t.Run(name, func(t *testing.T) {
			forceSelectLimit(t, limit)
			env := stagedEnv(real, t.TempDir())
			var fail atomic.Bool
			inner := env.Run
			env.Run = func(ctx context.Context, tool string, args ...string) (Result, error) {
				if tool == "ffmpeg" && fail.Load() {
					return Result{}, errors.New("injected ffmpeg failure")
				}
				return inner(ctx, tool, args...)
			}
			src := openSource(t, env, clip, Hints{Frames: 4})
			want, err := src.Encode(context.Background(), 0, openTargets[0])
			if err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			larger := Encoding{Format: WebP, Quality: 80, MaxEdge: 400, MaxArea: 400 * 400}
			if _, err := src.Encode(context.Background(), 0, larger); err == nil {
				t.Fatal("the larger encode succeeded")
			}
			fail.Store(false)
			got, err := src.Encode(context.Background(), 0, openTargets[0])
			if err != nil {
				t.Fatalf("the smaller encode after the failure: %v", err)
			}
			if !bytes.Equal(readFile(t, got[0].Path), readFile(t, want[0].Path)) || !slices.Equal(got[0].Frames, want[0].Frames) {
				t.Errorf("after the failure: frames %v, want %v", got[0].Frames, want[0].Frames)
			}
		})
	}
}

// A video whose only frame is at 0 and whose container outlasts it still
// yields that frame, as one sample at time 0, whether or not the frame count
// is known and on either sampling path.
func TestVideoWithOneFrameBeforeALongerContainerYieldsASampleAtZero(t *testing.T) {
	env := requireFFmpeg(t)
	for _, ext := range []string{".mkv", ".mp4"} {
		clip := writeClip(t, env, ext, append(testsrc("160x120", 10, 1),
			"-f", "lavfi", "-i", "sine=duration=3", "-vf", "trim=end_frame=1",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac")...)
		for name, limit := range map[string]int64{"seek": 0, "select": selectMaxPixels} {
			t.Run(ext[1:]+" "+name, func(t *testing.T) {
				forceSelectLimit(t, limit)
				src := openSource(t, env, clip, Hints{})
				arts, err := src.Encode(context.Background(), 0, openTargets[1])
				if err != nil {
					t.Fatal(err)
				}
				want := []Frame{{SourceTimeMS: 0}}
				if got := src.Frames(); !slices.Equal(got, want) {
					t.Errorf("Frames = %v, want %v", got, want)
				}
				if got := arts[0].Frames; !slices.Equal(got, want) {
					t.Errorf("Artifact.Frames = %v, want %v", got, want)
				}
			})
		}
	}
}
