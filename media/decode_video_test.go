package media

import (
	"bytes"
	"context"
	"errors"
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
	{Format: WebP, Quality: 80, MaxEdge: 48, MaxArea: 48 * 48, Animate: true},
	{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100, Animate: true},
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// encodeVideo makes one Encode of sel into a fresh directory on src.
func encodeVideo(t *testing.T, src *Source, sel Select, targets ...Encoding) ([]Image, string, error) {
	t.Helper()
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, sel, targets...)
	return images, dir, err
}

// The frames of an encoded video are its sample times.
func TestVideoImageFramesAreSampleTimes(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	clip := sdrClip(t, env, "320x240", 10, 4)
	info, err := probeVideo(context.Background(), env, decoder{"ffmpeg", "mov"}, clip)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{4, 15, 64} {
		src := openSourceOn(t, p, clip, OpenOptions{})
		var want []Frame
		for _, ms := range sampleTimesMS(info, n) {
			want = append(want, Frame{Time: msec(ms)})
		}
		images, _, err := encodeVideo(t, src, Select{Frames: Even(n)}, openTargets[1])
		if err != nil {
			t.Fatal(err)
		}
		if got := images[0].Frames; !slices.Equal(got, want) {
			t.Errorf("Even(%d): Image.Frames = %v, want %v", n, got, want)
		}
	}
}

// A run of identical samples is merged into one scene, the image says which
// samples its scenes show, and the written file holds exactly those scenes.
func TestVideoMapsTheScenesTheWriterMerged(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := writeClip(t, env, ".mp4",
		"-f", "lavfi", "-i", "color=c=red:size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=2",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	src := openSourceOn(t, requireProcessor(t), clip, OpenOptions{})
	const samples = 15
	images, dir, err := encodeVideo(t, src, Select{Frames: Even(samples)}, openTargets[1])
	if err != nil {
		t.Fatal(err)
	}
	frames := images[0].Frames
	if len(frames) < 2 || len(frames) >= samples {
		t.Fatalf("kept %d of %d samples, want the red run collapsed to one", len(frames), samples)
	}
	if frames[0].Time.Milliseconds() >= 2000 || frames[1].Time.Milliseconds() < 2000 {
		t.Errorf("kept times %v: want the first red sample, then the first testsrc sample from 2 s", frames)
	}
	if scenes := sceneSizes(t, env, filepath.Join(dir, images[0].Name)); len(scenes) != len(frames) {
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
			p := requireProcessor(t)
			src := openSourceOn(t, p, small, OpenOptions{})
			rec := record(p)
			images, _, err := encodeVideo(t, src, Select{Frames: Even(3)}, Encoding{Format: WebP, Quality: 80, MaxEdge: c.maxEdge, MaxArea: c.maxEdge * c.maxEdge, Animate: true})
			if err != nil {
				t.Fatal(err)
			}
			if images[0].Width != c.wantWidth || images[0].Height != c.wantHeight {
				t.Errorf("size %dx%d, want %dx%d", images[0].Width, images[0].Height, c.wantWidth, c.wantHeight)
			}
			encodes := 0
			for _, args := range rec.ran("magick") {
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

// A second encode of a Source at another target is what a Source encoding that
// target alone gives: each Encode samples afresh.
func TestVideoEncodesOfOneSourceAgreeWithEncodesAlone(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	clip := sdrClip(t, env, "1280x720", 10, 3)
	large := Encoding{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100, Animate: true}
	alone, aloneDir, err := encodeVideo(t, openSourceOn(t, p, clip, OpenOptions{}), Select{Frames: Even(4)}, large)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range []int{48, 60, 90, 99} {
		t.Run(strconv.Itoa(edge), func(t *testing.T) {
			small := Encoding{Format: WebP, Quality: 80, MaxEdge: edge, MaxArea: edge * edge, Animate: true}
			both := openSourceOn(t, p, clip, OpenOptions{})
			if _, _, err := encodeVideo(t, both, Select{Frames: Even(4)}, small); err != nil {
				t.Fatal(err)
			}
			got, gotDir, err := encodeVideo(t, both, Select{Frames: Even(4)}, large)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, filepath.Join(gotDir, got[0].Name)), readFile(t, filepath.Join(aloneDir, alone[0].Name))) {
				t.Error("the large encode of a source first encoded small differs from one encoded large alone")
			}
		})
	}
}

// The decode serves the target with the largest output, whatever its edge:
// that target is encoded as it is alone, and the other has the size it has
// alone. Its bytes may differ, as it is resampled from a larger decode.
func TestVideoDecodesForTheLargestOutput(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	clip := sdrClip(t, env, "1280x720", 10, 3)
	small := Encoding{Format: WebP, Quality: 80, MaxEdge: 1280, MaxArea: 256 * 144, Animate: true}
	large := Encoding{Format: WebP, Quality: 80, MaxEdge: 640, MaxArea: 640 * 360, Animate: true}
	alone := func(enc Encoding) (Image, string) {
		images, dir, err := encodeVideo(t, openSourceOn(t, p, clip, OpenOptions{}), Select{Frames: Even(4)}, enc)
		if err != nil {
			t.Fatal(err)
		}
		return images[0], dir
	}
	wantSmall, _ := alone(small)
	wantLarge, wantLargeDir := alone(large)
	for name, order := range map[string][]Encoding{"small first": {small, large}, "large first": {large, small}} {
		t.Run(name, func(t *testing.T) {
			images, dir, err := encodeVideo(t, openSourceOn(t, p, clip, OpenOptions{}), Select{Frames: Even(4)}, order...)
			if err != nil {
				t.Fatal(err)
			}
			gotSmall, gotLarge := images[0], images[1]
			if name == "large first" {
				gotSmall, gotLarge = images[1], images[0]
			}
			if !bytes.Equal(readFile(t, filepath.Join(dir, gotLarge.Name)), readFile(t, filepath.Join(wantLargeDir, wantLarge.Name))) {
				t.Error("the larger target beside another differs from alone")
			}
			if gotSmall.Width != wantSmall.Width || gotSmall.Height != wantSmall.Height {
				t.Errorf("the smaller target is %dx%d, alone %dx%d", gotSmall.Width, gotSmall.Height, wantSmall.Width, wantSmall.Height)
			}
		})
	}
}

// A failed decode leaves the Source able to encode again, with the same
// frames.
func TestVideoSourceSurvivesAFailedDecode(t *testing.T) {
	env := requireFFmpeg(t)
	clip := sdrClip(t, env, "320x240", 10, 4)
	for name, c := range map[string]struct {
		limit  int64
		failAt int32
	}{
		"seek, the third sample": {0, 3},
		"select, the one run":    {selectMaxPixels, 1},
	} {
		t.Run(name, func(t *testing.T) {
			forceSelectLimit(t, c.limit)
			p := requireProcessor(t)
			var calls, failAt atomic.Int32
			failAt.Store(c.failAt)
			inner := p.run
			p.run = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
				if name == "ffmpeg" && calls.Add(1) == failAt.Load() {
					return runResult{}, errors.New("injected ffmpeg failure")
				}
				return inner(ctx, in, name, args...)
			}
			src := openSourceOn(t, p, clip, OpenOptions{})
			dir := t.TempDir()
			if _, _, err := src.Encode(context.Background(), dir, Select{Frames: Even(6)}, openTargets[1]); err == nil {
				t.Fatal("encode succeeded")
			}
			assertEmptyDir(t, dir)
			failAt.Store(-1)
			images, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Even(6)}, openTargets[1])
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if len(images[0].Frames) != 6 {
				t.Errorf("retry holds %v, want 6 frames", images[0].Frames)
			}
		})
	}
}

// Leading bytes that look like a video container do not take a file from the
// image route: sniff names a GIF before anything else is looked at, and a
// file the probe refuses is read as an image.
func TestOpenKeepsImagesThatLookLikeVideo(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
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
	rec := record(p)
	src := openSourceOn(t, p, writeFile(t, "gif", b), OpenOptions{})
	if info := src.Info(); info.Kind == Video || info.Type != typeGIF {
		t.Errorf("GIF opened as %s of kind %v", info.Type, info.Kind)
	}
	if n := len(rec.ran("ffprobe")); n != 0 {
		t.Errorf("a sniffed GIF was probed as a video %d times", n)
	}

	// A WBMP of one column and 7425 rows opens with 00 00 01 BA: type 0,
	// no extension header, width 1 and the height 0xBA 0x01. It is no type of
	// the Processor, and a refusal that says so, not one of a video.
	wbmp := append([]byte{0, 0, 1, 0xBA, 1}, make([]byte, 7425)...)
	_, _, err := p.Open(context.Background(), writeFile(t, "x.wbmp", wbmp), OpenOptions{})
	if !errors.Is(err, ErrUnsupportedType) || !strings.Contains(err.Error(), "reading headers") {
		t.Errorf("WBMP: err = %v, want ErrUnsupportedType after the image read", err)
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
			if _, err := sniffPath(path); !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("sniff = %v, want it to name no type", err)
			}
			p := requireProcessor(t)
			rec := record(p)
			src := openSourceOn(t, p, path, OpenOptions{})
			if src.Info().Kind != Video {
				t.Fatal("opened as an image")
			}
			if src.dec != (decoder{"ffmpeg", "mov"}) {
				t.Errorf("decoder = %+v, want mov", src.dec)
			}
			if _, _, err := encodeVideo(t, src, Select{Frames: Even(3)}, openTargets[1]); err != nil {
				t.Fatal(err)
			}
			for _, args := range rec.ran("magick") {
				if slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, name) }) {
					t.Errorf("magick read the video: %v", args)
				}
			}
		})
	}
}

// Open reads every container video decoding supports, with no type
// named: the bytes pin MP4, MOV, WebM and Matroska, and ffmpeg detects the
// rest among the demuxers of the detected types, each then pinned.
func TestOpenReadsEveryVideoContainer(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	clip := func(args ...string) string {
		path := filepath.Join(t.TempDir(), "input")
		full := append(testsrc("176x144", 10, 2), args...)
		if _, err := env.run(context.Background(), "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y"}, append(full, path)...)...); err != nil {
			t.Fatal(err)
		}
		return path
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
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			p := requireProcessor(t)
			rec := record(p)
			src := openSourceOn(t, p, path, OpenOptions{})
			images, _, err := encodeVideo(t, src, Select{Frames: Even(4)}, openTargets[1])
			if err != nil {
				t.Fatal(err)
			}
			// ImageMagick must not be handed a container it could pass to a
			// delegate. No tool is given the path: the input is src.ref.
			for _, args := range rec.ran("magick") {
				if slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, src.ref) }) {
					t.Errorf("magick read the video: %v", args)
				}
			}
			if len(images[0].Frames) == 0 || images[0].Width != 100 {
				t.Errorf("image %dx%d with frames %v", images[0].Width, images[0].Height, images[0].Frames)
			}
		})
	}
}

func TestOpenRefusesWhatIsNeitherImageNorVideo(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	for name, body := range map[string][]byte{
		"text":  []byte("not media at all\n"),
		"empty": {},
		"zeros": make([]byte, 8192),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := p.Open(context.Background(), writeFile(t, "x", body), OpenOptions{}); !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("err = %v, want ErrUnsupportedType", err)
			}
		})
	}
	// A missing file is not a type problem.
	if _, _, err := p.Open(context.Background(), filepath.Join(t.TempDir(), "absent"), OpenOptions{}); err == nil || errors.Is(err, ErrUnsupportedType) {
		t.Errorf("missing file: err = %v", err)
	}
}

// An AVI is read only when Config.Types names it.
func TestProcessorVideoOperationsRefuseAnAVIOutsideTypes(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessorWith(t, Config{Types: []string{typeMP4}})
	path := filepath.Join(t.TempDir(), "input")
	args := append(testsrc("64x48", 10, 1), "-c:v", "mpeg4", "-f", "avi", path)
	if _, err := env.run(context.Background(), "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y"}, args...)...); err != nil {
		t.Fatal(err)
	}
	enc := Encoding{Format: WebP, MaxEdge: 64}
	dir := t.TempDir()
	if _, _, err := p.encodeEven(context.Background(), path, enc, 2, dir); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("encode: err = %v, want ErrUnsupportedType", err)
	}
	if _, _, err := p.infoOf(context.Background(), path); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("info: err = %v, want ErrUnsupportedType", err)
	}
	assertEmptyDir(t, dir)
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
				src := openSourceOn(t, requireProcessor(t), clip, OpenOptions{})
				images, _, err := encodeVideo(t, src, Select{Frames: Even(15)}, openTargets[1])
				if err != nil {
					t.Fatal(err)
				}
				want := []Frame{{Time: 0}}
				if got := images[0].Frames; !slices.Equal(got, want) {
					t.Errorf("Image.Frames = %v, want %v", got, want)
				}
			})
		}
	}
}
