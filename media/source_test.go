package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// openSourceOn opens path on p with opts, closing the Source when the test
// ends.
func openSourceOn(t *testing.T, p *Processor, path string, opts OpenOptions) *Source {
	t.Helper()
	src, _, err := p.Open(context.Background(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// countCompositor counts the runs of p's frame compositor.
func countCompositor(p *Processor) *atomic.Int64 {
	var n atomic.Int64
	inner := p.compose
	p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
		n.Add(1)
		return inner(ctx, in, req, onFrame)
	}
	return &n
}

// ran returns the recorded runs of the named tool.
func (r *recorder) ran(tool string) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, c := range r.calls {
		if c[0] == tool {
			out = append(out, c)
		}
	}
	return out
}

func uprightOf(t *testing.T, env toolEnv, path string) (w, h int) {
	t.Helper()
	out := strings.TrimSpace(string(magick(t, env, path+"[0]", "-auto-orient", "-format", "%w %h", "info:")))
	if _, err := fmt.Sscanf(out, "%d %d", &w, &h); err != nil {
		t.Fatal(err)
	}
	return w, h
}

// tiffPages is a TIFF of 8-bit grey 32x24 pages, page i having the
// NewSubfileType flags[i].
func tiffPages(flags ...uint32) []byte {
	const w, h = 32, 24
	type entry struct {
		tag, typ uint16
		val      uint32
	}
	const entries = 10
	ifdSize := 2 + 12*entries + 4
	// A transparency mask (NewSubfileType bit 2) is 1-bit with Photometric 4,
	// as the TIFF specification requires; any other page is 8-bit grey.
	pixelBytes := func(flag uint32) int {
		if flag&4 != 0 {
			return w / 8 * h
		}
		return w * h
	}
	out := []byte("II*\x00\x08\x00\x00\x00")
	at := 8
	for i, flag := range flags {
		bits, photometric, size := uint32(8), uint32(1), pixelBytes(flag)
		if flag&4 != 0 {
			bits, photometric = 1, 4
		}
		pixelsAt := uint32(at + ifdSize)
		out = binary.LittleEndian.AppendUint16(out, entries)
		for _, e := range []entry{
			{254, 4, flag},
			{256, 3, w},
			{257, 3, h},
			{258, 3, bits},
			{259, 3, 1},
			{262, 3, photometric},
			{273, 4, pixelsAt},
			{277, 3, 1},
			{278, 3, h},
			{279, 4, uint32(size)},
		} {
			out = binary.LittleEndian.AppendUint16(out, e.tag)
			out = binary.LittleEndian.AppendUint16(out, e.typ)
			out = binary.LittleEndian.AppendUint32(out, 1)
			out = binary.LittleEndian.AppendUint32(out, e.val)
		}
		next := uint32(0)
		if i < len(flags)-1 {
			next = uint32(at + ifdSize + size)
		}
		out = binary.LittleEndian.AppendUint32(out, next)
		for j := range size {
			out = append(out, byte(j+i*40))
		}
		at += ifdSize + size
	}
	return out
}

func TestInfoUprightPerType(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	ctx := context.Background()
	png := Encoding{Format: PNG, MaxEdge: 64}
	t.Run("heic", func(t *testing.T) {
		t.Parallel()
		info := openSourceOn(t, p, heicFixtureFile(t, "gradient-righttop"), OpenOptions{}).Info()
		if !info.Upright || info.Kind != Still || info.Width != 480 || info.Height != 640 {
			t.Errorf("Info = %+v, want a settled 480x640 still", info)
		}
	})
	t.Run("jpeg", func(t *testing.T) {
		t.Parallel()
		info := openSourceOn(t, p, quadrantJPEG(t, env), OpenOptions{}).Info()
		if !info.Upright || info.Width != 480 || info.Height != 640 || info.Orientation != 6 {
			t.Errorf("Info = %+v, want a settled 480x640 with orientation 6", info)
		}
	})
	t.Run("tiff", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "oriented.tif")
		magick(t, env, "-size", "32x24", "gradient:red-blue", "-orient", "RightTop", path)
		info := openSourceOn(t, p, path, OpenOptions{}).Info()
		if !info.Upright || info.Width != 24 || info.Height != 32 || info.Orientation != 6 {
			t.Errorf("Info = %+v, want a settled 24x32 with orientation 6", info)
		}
	})
	for _, file := range []string{"exif-after-idat.png", "ztxt-exif.png", "exif.webp", "xmp.webp"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			path := writeFile(t, "content", mustRead(t, filepath.Join("testdata", "orient", file)))
			uw, uh := uprightOf(t, env, path)
			src := openSourceOn(t, p, path, OpenOptions{})
			before := src.Info()
			if before.Upright || before.Width == uw && before.Height == uh {
				t.Fatalf("Info before = %+v, want an unsettled stored size (upright is %dx%d)", before, uw, uh)
			}
			images, _, err := src.Encode(ctx, t.TempDir(), Select{}, png)
			if err != nil {
				t.Fatal(err)
			}
			after := src.Info()
			if !after.Upright || after.Width != uw || after.Height != uh {
				t.Errorf("Info after = %+v, want a settled %dx%d", after, uw, uh)
			}
			if want := (Region{Width: uw, Height: uh}); images[0].Region != want {
				t.Errorf("Image.Region = %+v, want the decoder's upright frame %+v", images[0].Region, want)
			}
		})
	}
	t.Run("animation", func(t *testing.T) {
		t.Parallel()
		info := openSourceOn(t, p, writeGIF(t, solid, 3), OpenOptions{}).Info()
		if !info.Upright || info.Kind != Animation || info.Animation.Frames != 3 || len(info.Animation.Delays) != 3 || info.Animation.Duration != 150*time.Millisecond {
			t.Errorf("Info = %+v, want a settled animation of 3 frames over 150 ms", info)
		}
	})
	t.Run("video", func(t *testing.T) {
		t.Parallel()
		info := openSourceOn(t, p, sdrClip(t, env, "160x120", 10, 2), OpenOptions{}).Info()
		if !info.Upright || info.Kind != Video || info.Width != 160 || info.Height != 120 || info.Video.Codec != "h264" || info.Video.Duration < 1900*time.Millisecond {
			t.Errorf("Info = %+v, want a settled 160x120 h264 video of 2 s", info)
		}
	})
	t.Run("given orientation", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "plain.png")
		magick(t, env, "-size", "32x24", "gradient:red-blue", path)
		src := openSourceOn(t, p, path, OpenOptions{Orientation: 6})
		if info := src.Info(); !info.Upright || info.Width != 24 || info.Height != 32 {
			t.Errorf("Info = %+v, want a settled 24x32", info)
		}
		if _, _, err := p.Open(ctx, sdrClip(t, env, "160x120", 10, 1), OpenOptions{Orientation: 6}); err == nil {
			t.Error("an orientation was accepted for a video")
		}
	})
}

func TestInfoOrientationGivenSaysWhereTheOrientationCame(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.png")
	magick(t, env, "-size", "32x24", "gradient:red-blue", plain)
	oriented := filepath.Join(dir, "oriented.tif")
	magick(t, env, "-size", "32x24", "gradient:red-blue", "-orient", "RightTop", oriented)
	cases := []struct {
		name        string
		path        string
		opts        OpenOptions
		orientation int
		given       bool
	}{
		{"still without one, option given", plain, OpenOptions{Orientation: 3}, 3, true},
		{"still without one, no option", plain, OpenOptions{}, 0, false},
		{"still with one, option given", oriented, OpenOptions{Orientation: 3}, 6, false},
		{"still with one, no option", oriented, OpenOptions{}, 6, false},
		{"animation, option given", writeGIF(t, solid, 2), OpenOptions{Orientation: 3}, 3, true},
		{"animation, no option", writeGIF(t, solid, 2), OpenOptions{}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			info := openSourceOn(t, p, c.path, c.opts).Info()
			if info.Orientation != c.orientation || info.OrientationGiven != c.given {
				t.Errorf("Orientation = %d, OrientationGiven = %v, want %d, %v",
					info.Orientation, info.OrientationGiven, c.orientation, c.given)
			}
		})
	}
}

func TestInfoIsASnapshot(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, writeGIF(t, solid, 3), OpenOptions{})
	info := src.Info()
	info.Animation.Delays[0] = 12345
	info.Animation.Frames = 99
	if got := src.Info().Animation.Delays[0]; got != 50*time.Millisecond {
		t.Errorf("a caller changed the Source's delays to %v", got)
	}
}

func TestInfoOfAVideoIsASnapshot(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	src := openSourceOn(t, p, sdrClip(t, env, "160x120", 10, 2), OpenOptions{})
	info := src.Info()
	info.Video.Codec = "changed"
	info.Video.Rotation = 90
	if got := src.Info().Video; got.Codec != "h264" || got.Rotation != 0 {
		t.Errorf("a caller changed the Source's video info to %+v", got)
	}
}

func TestTimesAreTruncatedToTheMillisecond(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		d    time.Duration
		want int64
	}{{0, 0}, {time.Millisecond - 1, 0}, {1999 * time.Microsecond, 1}, {2 * time.Second, 2000}, {-time.Microsecond, -1}} {
		if got := durMS(c.d); got != c.want {
			t.Errorf("durMS(%v) = %d, want %d", c.d, got, c.want)
		}
	}
}

func TestVideoSourceProbesOnce(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	src := openSourceOn(t, p, sdrClip(t, env, "160x120", 10, 3), OpenOptions{})
	for _, sel := range []Select{
		{},
		{Frames: Even(3)},
		{Frames: Times(msec(1500))},
		{Frames: Consecutive(msec(500), 3)},
		{Frames: Times(msec(500)), Region: Region{X: 10, Y: 10, Width: 50, Height: 40}},
	} {
		if _, _, err := src.Encode(context.Background(), t.TempDir(), sel, Encoding{Format: PNG, MaxEdge: 64}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(rec.ran("ffprobe")); n != 1 {
		t.Errorf("%d ffprobe runs over an Open and 5 Encodes, want 1", n)
	}
}

func TestVideoTimesTakeTheSeekOrTheSelectPath(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	src := openSourceOn(t, p, sdrClip(t, env, "160x120", 10, 3), OpenOptions{})
	count := func(sel Select) (seeks, runs int) {
		before := len(rec.ran("ffmpeg"))
		if _, _, err := src.Encode(context.Background(), t.TempDir(), sel, Encoding{Format: PNG, MaxEdge: 64}); err != nil {
			t.Fatal(err)
		}
		for _, call := range rec.ran("ffmpeg")[before:] {
			runs++
			if slices.Contains(call, "-ss") {
				seeks++
			}
		}
		return seeks, runs
	}
	if seeks, runs := count(Select{Frames: Times(msec(1500))}); seeks != 1 || runs != 1 {
		t.Errorf("one time: %d seeks of %d ffmpeg runs, want 1 of 1", seeks, runs)
	}
	if seeks, runs := count(Select{Frames: Times(msec(500), msec(1500), msec(2500))}); seeks != 0 || runs != 1 {
		t.Errorf("three times: %d seeks of %d ffmpeg runs, want one select run with no seek", seeks, runs)
	}
	if seeks, runs := count(Select{}); seeks != 1 || runs != 1 {
		t.Errorf("the first frame: %d seeks of %d ffmpeg runs, want 1 of 1", seeks, runs)
	}
}

func TestAnimationSourceKeepsTheFramesOfItsLastCall(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	runs := countCompositor(p)
	src := openSourceOn(t, p, writeGIF(t, solid, 16), OpenOptions{})
	if got := runs.Load(); got != 0 {
		t.Fatalf("Open ran the compositor %d times", got)
	}
	encode := func(sel FrameSelection) []Image {
		t.Helper()
		images, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: sel}, Encoding{Format: PNG, MaxEdge: 32})
		if err != nil {
			t.Fatal(err)
		}
		return images
	}
	indices := func(images []Image) []int {
		var out []int
		for _, img := range images {
			out = append(out, img.Frames[0].Index)
		}
		return out
	}
	if got := indices(encode(Even(16))); len(got) != 16 {
		t.Fatalf("Even(16) shows frames %v", got)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("Even(16) ran the compositor %d times, want 1", got)
	}
	if got := indices(encode(Even(8))); !slices.Equal(got, []int{0, 2, 4, 6, 8, 10, 12, 14}) {
		t.Errorf("Even(8) shows frames %v", got)
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("Even(8) after Even(16) ran the compositor %d more times, want none", got-1)
	}
	encode(Indices(1))
	if got := runs.Load(); got != 2 {
		t.Errorf("frame 1, which the last call did not hold, ran the compositor %d times in all, want 2", got)
	}
	encode(Indices(1, 2))
	if got := runs.Load(); got != 3 {
		t.Errorf("frames 1 and 2 ran the compositor %d times in all, want 3: only frame 2 was missing", got)
	}
	encode(Indices(2))
	if got := runs.Load(); got != 3 {
		t.Errorf("frame 2 again ran the compositor %d more times, want none", got-3)
	}
	// Frame 1 was dropped when the last call did not use it.
	encode(Indices(1))
	if got := runs.Load(); got != 4 {
		t.Errorf("frame 1 again ran the compositor %d times in all, want 4: only the last call's frames are kept", got)
	}
}

// The Source keeps the frames of its most recent call only, so a call that
// needs other frames drops the ones it does not use before it composites its
// own: the scratch never holds two calls' frames.
func TestAnimationSourceDropsUnusedFramesBeforeCompositing(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, writeGIF(t, solid, 6), OpenOptions{})
	var held []int
	inner := p.compose
	p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
		entries, _ := os.ReadDir(src.dir)
		n := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".frame-") {
				n++
			}
		}
		held = append(held, n)
		return inner(ctx, in, req, onFrame)
	}
	for _, sel := range []FrameSelection{Indices(0, 1, 2), Indices(3, 4, 5)} {
		if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: sel}, Encoding{Format: PNG, MaxEdge: 32}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(held, []int{0, 0}) {
		t.Errorf("the scratch held %v composited frames when each compositor run started, want [0 0]", held)
	}
}

// A selection whose composited frames fit in Resources.Disk are kept for the
// next call; one over it is streamed, a batch at a time, and nothing is kept.
func TestAnimationSourceStreamsWhatDoesNotFitInDisk(t *testing.T) {
	t.Parallel()
	const frameBytes = 64 * 48 * 4
	p := requireProcessorWith(t, Config{Resources: Resources{Disk: 2 * frameBytes}})
	runs := countCompositor(p)
	src := openSourceOn(t, p, writeGIF(t, solid, 6), OpenOptions{})
	kept := func() int {
		entries, _ := os.ReadDir(src.dir)
		n := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".frame-") {
				n++
			}
		}
		return n
	}
	encode := func(sel FrameSelection) {
		t.Helper()
		if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: sel}, Encoding{Format: PNG, MaxEdge: 32}); err != nil {
			t.Fatal(err)
		}
	}
	encode(Indices(0, 1)) // exactly the budget
	if got := kept(); got != 2 || len(src.frames) != 2 {
		t.Errorf("a selection at the budget kept %d frames on disk and %d in the cache, want 2 and 2", got, len(src.frames))
	}
	encode(Indices(0, 1))
	if got := runs.Load(); got != 1 {
		t.Errorf("the same selection ran the compositor %d times in all, want 1", got)
	}
	encode(Indices(0, 1, 2)) // one frame over
	if got := kept(); got != 0 || len(src.frames) != 0 {
		t.Errorf("a selection over the budget kept %d frames on disk and %d in the cache, want none", got, len(src.frames))
	}
	if got := runs.Load(); got != 2 {
		t.Errorf("the streamed call ran the compositor %d times in all, want 2", got)
	}
	encode(Indices(0, 1, 2))
	if got := runs.Load(); got != 3 {
		t.Errorf("the same streamed selection ran the compositor %d times in all, want 3: nothing was kept", got)
	}
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Indices(0, 1, 2)}, Encoding{Format: WebP, Animate: true}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one animation over the budget: err = %v, want ErrTooLarge", err)
	}
}

func TestConcurrentEncodesOfOneSource(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	for name, path := range map[string]string{
		"still":     heicFixtureFile(t, "adobe-64x32"),
		"animation": writeGIF(t, solid, 8),
	} {
		t.Run(name, func(t *testing.T) {
			src := openSourceOn(t, p, path, OpenOptions{})
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					sel := Select{Frames: Indices(i % 3)}
					if name == "still" {
						sel = Select{}
					}
					images, _, err := src.Encode(context.Background(), t.TempDir(), sel, Encoding{Format: PNG, MaxEdge: 32})
					if err == nil && len(images) != 1 {
						err = fmt.Errorf("%d images", len(images))
					}
					_ = src.Info()
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestChangedInputIsRefused(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	original, err := os.ReadFile(heicFixtureFile(t, "adobe-64x32"))
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	for name, change := range map[string]func(t *testing.T, path string){
		"rewrite": func(t *testing.T, path string) {
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, later, later.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		},
		"rename over": func(t *testing.T, path string) {
			other := path + ".new"
			if err := os.WriteFile(other, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(other, path); err != nil {
				t.Fatal(err)
			}
		},
		"append": func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if _, err := f.WriteString("x"); err != nil {
				t.Fatal(err)
			}
		},
		"touch": func(t *testing.T, path string) {
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "photo.heic")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			src := openSourceOn(t, p, path, OpenOptions{})
			enc := Encoding{Format: PNG, MaxEdge: 32}
			if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, enc); err != nil {
				t.Fatalf("before the change: %v", err)
			}
			change(t, path)
			dir := t.TempDir()
			_, usage, err := src.Encode(context.Background(), dir, Select{}, enc)
			if !errors.Is(err, ErrInputChanged) {
				t.Fatalf("err = %v, want ErrInputChanged", err)
			}
			if usage != (Usage{}) {
				t.Errorf("the refused call ran tools: %+v", usage)
			}
			if got := dirNames(t, dir); len(got) != 0 {
				t.Errorf("the refused call left %v", got)
			}
		})
	}
}

func TestEncodeNamesFilesAcrossTargetsAndFrames(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, writeGIF(t, solid, 6), OpenOptions{})
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, Select{Frames: Indices(1, 3, 5)},
		Encoding{Format: PNG, MaxEdge: 32}, Encoding{Format: WebP, MaxEdge: 16})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i, img := range images {
		names = append(names, img.Name)
		if want := i / 3; img.Encoding != want {
			t.Errorf("%s: Target %d, want %d", img.Name, img.Encoding, want)
		}
		if want := []int{1, 3, 5}[i%3]; len(img.Frames) != 1 || img.Frames[0].Index != want {
			t.Errorf("%s: Frames %+v, want frame %d", img.Name, img.Frames, want)
		}
	}
	want := []string{"000.png", "001.png", "002.png", "003.webp", "004.webp", "005.webp"}
	if !slices.Equal(names, want) {
		t.Errorf("names %v, want %v", names, want)
	}
	if got := dirNames(t, dir); !slices.Equal(got, want) {
		t.Errorf("directory holds %v, want %v", got, want)
	}
}

func TestAnimationTimesAndRunsSelectTheFrameShownThen(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	// Four frames of 50 ms.
	src := openSourceOn(t, p, writeGIF(t, solid, 4), OpenOptions{})
	frames := func(sel FrameSelection) []Frame {
		t.Helper()
		images, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: sel}, Encoding{Format: PNG, MaxEdge: 32})
		if err != nil {
			t.Fatal(err)
		}
		var out []Frame
		for _, img := range images {
			out = append(out, img.Frames...)
		}
		return out
	}
	if got, want := frames(Times(msec(199), 0, msec(60), msec(110), msec(60))), []Frame{{0, 0}, {1, msec(50)}, {2, msec(100)}, {3, msec(150)}}; !slices.Equal(got, want) {
		t.Errorf("Times = %+v, want %+v", got, want)
	}
	if got, want := frames(Consecutive(msec(60), 2)), []Frame{{1, msec(50)}, {2, msec(100)}}; !slices.Equal(got, want) {
		t.Errorf("Consecutive(60ms, 2) = %+v, want %+v", got, want)
	}
	if got, want := frames(Consecutive(msec(110), 9)), []Frame{{2, msec(100)}, {3, msec(150)}}; !slices.Equal(got, want) {
		t.Errorf("Consecutive(110ms, 9) = %+v, want the frames to the end %+v", got, want)
	}
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Times(msec(200))}, Encoding{}); err == nil {
		t.Error("a time at the end of the animation was accepted")
	}
}

func TestEncodeNamesTheImagesOfAStillAcrossTargets(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, writeFile(t, "pages", tiffPages(0, 0, 0)), OpenOptions{})
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, Select{Frames: Indices(0, 2)},
		Encoding{Format: PNG}, Encoding{Format: JPEG})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var frames []int
	for _, img := range images {
		names = append(names, img.Name)
		frames = append(frames, img.Frames[0].Index)
	}
	if want := []string{"000.png", "001.png", "002.jpg", "003.jpg"}; !slices.Equal(names, want) {
		t.Errorf("names %v, want %v", names, want)
	}
	if !slices.Equal(frames, []int{0, 2, 0, 2}) {
		t.Errorf("frames %v, want 0, 2 for each target", frames)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"000.png", "001.png", "002.jpg", "003.jpg"}) {
		t.Errorf("directory holds %v", got)
	}
}

func TestEncodeAnimateWritesOneFileAndNeedsWebP(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	rec := record(p)
	src := openSourceOn(t, p, writeGIF(t, solid, 6), OpenOptions{})
	dir := t.TempDir()
	before := rec.count()
	if _, _, err := src.Encode(context.Background(), dir, Select{Frames: All()}, Encoding{Format: PNG, Animate: true}); err == nil {
		t.Error("six frames in one PNG were accepted")
	}
	if rec.count() != before || len(dirNames(t, dir)) != 0 {
		t.Errorf("the refusal ran %d tools and left %v", rec.count()-before, dirNames(t, dir))
	}
	images, _, err := src.Encode(context.Background(), dir, Select{Frames: All()}, Encoding{Format: WebP, MaxEdge: 32, Animate: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || images[0].Name != "000.webp" || len(images[0].Frames) != 6 {
		t.Errorf("images = %+v, want one animated 000.webp of 6 frames", images)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{"000.webp"}) {
		t.Errorf("directory holds %v", got)
	}
	// A single frame needs no WebP.
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Indices(2)}, Encoding{Format: PNG, Animate: true}); err != nil {
		t.Errorf("one frame as an animated PNG: %v", err)
	}
}

func TestEncodeLeavesTheDirectoryEmptyOnFailure(t *testing.T) {
	t.Parallel()
	for name, path := range map[string]func(t *testing.T) string{
		"still":     func(t *testing.T) string { return heicFixtureFile(t, "adobe-64x32") },
		"animation": func(t *testing.T) string { return writeGIF(t, solid, 6) },
		"video":     func(t *testing.T) string { return sdrClip(t, requireFFmpeg(t), "160x120", 10, 2) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			src := openSourceOn(t, p, path(t), OpenOptions{})
			rec := record(p)
			rec.failAfter = 2
			dir := t.TempDir()
			sel := Select{Frames: Times(msec(100), msec(700), msec(1300))}
			if name == "animation" {
				sel = Select{Frames: All()}
			}
			_, _, err := src.Encode(context.Background(), dir, sel, Encoding{Format: PNG, MaxEdge: 32}, Encoding{Format: JPEG, MaxEdge: 16})
			if err == nil {
				t.Fatal("the injected failure was not reported")
			}
			if got := dirNames(t, dir); len(got) != 0 {
				t.Errorf("directory holds %v", got)
			}
		})
	}
	p := requireProcessor(t)
	src := openSourceOn(t, p, heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := record(p)
	if _, _, err := src.Encode(context.Background(), dir, Select{}, Encoding{}); err == nil || rec.count() != 0 {
		t.Errorf("a non-empty directory: err = %v after %d tool runs", err, rec.count())
	}
}

func TestEncodeKeepsItsScratchOutOfTheOutputDirectory(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	// A profiled still needs the sRGB profile as a file during the run.
	src := openSourceOn(t, p, heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	dir := t.TempDir()
	seen := false
	rec := record(p)
	inner := rec.inner
	rec.inner = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".") {
					seen = true
				}
			}
		}
		return inner(ctx, in, name, args...)
	}
	if _, _, err := src.Encode(context.Background(), dir, Select{}, Encoding{}); err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Error("a hidden intermediate was in the output directory during a run")
	}
	if _, err := os.Stat(filepath.Join(src.dir, profileName)); err != nil {
		t.Errorf("the profile is not in the Source's scratch: %v", err)
	}
}

func TestSelectsThatDoNotApplyAreRefusedBeforeAnyToolRuns(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	rec := record(p)
	still := openSourceOn(t, p, heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	video := openSourceOn(t, p, sdrClip(t, env, "160x120", 10, 2), OpenOptions{})
	anim := openSourceOn(t, p, writeGIF(t, solid, 4), OpenOptions{})
	forbidCompositor(t, p)
	overMax := make([]time.Duration, MaxVideoSamples+1)
	for i := range overMax {
		overMax[i] = msec(i)
	}
	for _, c := range []struct {
		name string
		src  *Source
		sel  Select
	}{
		{"All on a video", video, Select{Frames: All()}},
		{"Indices on a video", video, Select{Frames: Indices(0)}},
		{"Times on a still", still, Select{Frames: Times(0)}},
		{"Consecutive on a still", still, Select{Frames: Consecutive(0, 2)}},
		{"Indices past a still's images", still, Select{Frames: Indices(1)}},
		{"Indices past an animation", anim, Select{Frames: Indices(4)}},
		{"Times past an animation", anim, Select{Frames: Times(msec(200))}},
		{"Times past a video", video, Select{Frames: Times(msec(2500))}},
		{"a negative time", video, Select{Frames: Times(-time.Millisecond)}},
		{"Even(0)", anim, Select{Frames: Even(0)}},
		{"more times of a video than the maximum", video, Select{Frames: Times(overMax...)}},
		{"more even samples of a video than the maximum", video, Select{Frames: Even(MaxVideoSamples + 1)}},
		{"a run of a video longer than the maximum", video, Select{Frames: Consecutive(0, MaxVideoSamples+1)}},
		{"a run of no frames of a video", video, Select{Frames: Consecutive(0, 0)}},
		{"no times", video, Select{Frames: Times()}},
		{"a negative index", anim, Select{Frames: Indices(1, -1)}},
		{"no indices", anim, Select{Frames: Indices()}},
		{"a region outside a settled frame", still, Select{Region: Region{X: 60, Y: 0, Width: 10, Height: 10}}},
		{"a region outside a video frame", video, Select{Region: Region{X: 155, Y: 0, Width: 10, Height: 10}}},
		{"a region with a side below 1", anim, Select{Region: Region{X: 0, Y: 0, Width: 0, Height: 10}}},
	} {
		before := rec.count()
		dir := t.TempDir()
		if _, usage, err := c.src.Encode(context.Background(), dir, c.sel, Encoding{Format: PNG}); err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", c.name, err)
		} else if usage != (Usage{}) || rec.count() != before || len(dirNames(t, dir)) != 0 {
			t.Errorf("%s: ran tools or left files: %v", c.name, err)
		}
	}
}

func TestOpenTypeReadsWhatNoSniffNames(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	ctx := context.Background()
	for _, typ := range []string{"image/x-tga", "image/vnd.microsoft.icon", "image/g3fax", "image/x-adobe-dng"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			f := typeFixtures[typ]
			path := f.build(t)
			// ImageMagick names a DNG a TIFF; the others it cannot name.
			if src, _, err := p.Open(ctx, path, OpenOptions{}); err == nil {
				defer func() { _ = src.Close() }()
				if src.Info().Type == typ {
					t.Fatal("the file was read as the type without being told it")
				}
			}
			src := openSourceOn(t, p, path, OpenOptions{Type: typ})
			if info := src.Info(); info.Type != typ || info.Kind != Still {
				t.Fatalf("Info = %+v, want a still of type %s", info, typ)
			}
			// A G3 header read reports a page size the decode does not have.
			if _, _, err := src.Encode(ctx, t.TempDir(), Select{}, Encoding{Format: PNG, MaxEdge: 64}); err != nil {
				t.Fatal(err)
			}
			if info := src.Info(); !info.Upright || info.Width != f.w || info.Height != f.h {
				t.Errorf("Info after an Encode = %+v, want a settled %dx%d", info, f.w, f.h)
			}
		})
	}
	t.Run("cur", func(t *testing.T) {
		t.Parallel()
		src := openSourceOn(t, p, gradient(".cur")(t), OpenOptions{Type: "image/vnd.microsoft.icon"})
		if info := src.Info(); info.Width != 32 || info.Height != 24 {
			t.Errorf("Info = %+v, want 32x24", info)
		}
	})
	t.Run("the type replaces the bytes", func(t *testing.T) {
		t.Parallel()
		// A PNG read as a JPEG fails in the JPEG decoder rather than being
		// read as the PNG its bytes are.
		if _, _, err := p.Open(ctx, gradient(".png")(t), OpenOptions{Type: "image/jpeg"}); err == nil {
			t.Error("PNG bytes were opened as a JPEG")
		}
		// A type the bytes name is replaced too.
		if _, _, err := p.Open(ctx, writeGIF(t, solid, 3), OpenOptions{Type: "image/png"}); err == nil {
			t.Error("GIF bytes were opened as a PNG")
		}
	})
}

func TestOpenTypeMustBeConfigured(t *testing.T) {
	t.Parallel()
	p := requireProcessorWith(t, Config{Types: []string{"image/png"}})
	rec := record(p)
	for _, typ := range []string{"image/x-tga", "image/jpeg", "video/mp4", "text/plain"} {
		_, usage, err := p.Open(context.Background(), gradient(".tga")(t), OpenOptions{Type: typ})
		if !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("Type %s: err = %v, want ErrUnsupportedType", typ, err)
		}
		if usage != (Usage{}) || rec.count() != 0 {
			t.Errorf("Type %s: the refusal ran tools", typ)
		}
	}
}

func TestInfoImagesMarkAuxiliaryImages(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := writeFile(t, "pages", tiffPages(0, 1, 0))
	src := openSourceOn(t, p, path, OpenOptions{})
	info := src.Info()
	want := []ImageInfo{{32, 24, false}, {32, 24, true}, {32, 24, false}}
	if info.Kind != Still || len(info.Images) != 3 || !slices.Equal(info.Images, want) {
		t.Fatalf("Info = %+v, want a still of images %v", info, want)
	}
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, Select{Frames: Indices(2, 0)}, Encoding{Format: PNG})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].Frames[0].Index != 0 || images[1].Frames[0].Index != 2 || images[1].Name != "001.png" {
		t.Errorf("images = %+v, want images 0 and 2 in order", images)
	}
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: All()}, Encoding{Format: WebP, Animate: true}); err == nil {
		t.Error("the images of a still were written as one animation")
	}
}

// A TIFF transparency mask is marked auxiliary as a reduced-resolution
// image is: ImageMagick prints MASK for %[tiff:subfiletype] of a directory
// whose NewSubfileType has the mask bit. ImageMagick writes no such
// directory, so the file is built by tiffPages.
func TestInfoImagesMarkATransparencyMaskAuxiliary(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := writeFile(t, "mask", tiffPages(0, 4))
	info := openSourceOn(t, p, path, OpenOptions{}).Info()
	want := []ImageInfo{{32, 24, false}, {32, 24, true}}
	if !slices.Equal(info.Images, want) {
		t.Errorf("Images = %v, want %v", info.Images, want)
	}
}

// The header read of a still lists every image. ImageMagick's list-length
// policy stops a -ping read of a grey TIFF only after every page is listed,
// and does not stop one of an RGB TIFF at all, so the library bounds the
// count itself: a still of more images than the list-length ceiling is
// refused, as an animation of more frames is.
func TestOpenRefusesAStillOfMoreImagesThanTheListLength(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "pages.tif")
	magick(t, env, "-size", "8x8", "xc:red", "-duplicate", "2099", "-compress", "lzw", path)
	src, _, err := p.Open(context.Background(), path, OpenOptions{})
	if err == nil {
		n := len(src.Info().Images)
		_ = src.Close()
		t.Fatalf("opened a still of %d images, over the %s list length", n, policyListLength)
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

// Whatever the file, the header read of a still names a scene range, so that
// a file of very many images is not listed in full.
func TestStillHeaderReadIsBounded(t *testing.T) {
	t.Parallel()
	for name, path := range map[string]string{
		"gate":   gradient(".jpg")(t),
		"sniffs": heicFixtureFile(t, "adobe-64x32"),
	} {
		t.Run(name, func(t *testing.T) {
			p := requireProcessor(t)
			rec := record(p)
			openSourceOn(t, p, path, OpenOptions{})
			calls := rec.ran("magick")
			if len(calls) == 0 {
				t.Fatal("no header read")
			}
			if last := calls[0][len(calls[0])-1]; !strings.HasSuffix(last, "[0-2048]") {
				t.Errorf("the header read names %q, want a scene range ending [0-2048]", last)
			}
		})
	}
}

func TestProcessorCloseClosesItsSources(t *testing.T) {
	t.Parallel()
	p, err := NewProcessor(context.Background(), Config{Dir: t.TempDir()})
	if err != nil {
		t.Skip(err)
	}
	a, _, err := p.Open(context.Background(), heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := p.Open(context.Background(), writeGIF(t, solid, 3), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Source{a, b} {
		if _, err := os.Stat(s.dir); err != nil {
			t.Fatalf("a Source's scratch is missing while open: %v", err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Source's scratch survived Processor.Close: %v", err)
	}
	if _, _, err := a.Encode(context.Background(), t.TempDir(), Select{}, Encoding{}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("a Source of a closed Processor: err = %v, want it to be closed", err)
	}
	if _, _, err := p.Open(context.Background(), writeGIF(t, solid, 3), OpenOptions{}); err == nil {
		t.Error("a closed Processor opened a Source")
	}
}

func TestSourceScratchIsRemovedOnClose(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src, _, err := p.Open(context.Background(), heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, Encoding{}); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(p.tmpDir())
	if err != nil || len(entries) != 0 {
		t.Errorf("tmp holds %v, %v", entries, err)
	}
}

func TestRegionIsReportedInUprightSourcePixels(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, heicFixtureFile(t, "gradient-righttop"), OpenOptions{})
	images, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, Encoding{Format: PNG, MaxEdge: 100})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Region{Width: 480, Height: 640}); images[0].Region != want {
		t.Errorf("Region = %+v, want the whole frame %+v", images[0].Region, want)
	}
	r := Region{X: 100, Y: 200, Width: 50, Height: 60}
	images, _, err = src.Encode(context.Background(), t.TempDir(), Select{Region: r}, Encoding{Format: PNG, MaxEdge: 100})
	if err != nil {
		t.Fatal(err)
	}
	if images[0].Region != r {
		t.Errorf("Region = %+v, want %+v", images[0].Region, r)
	}
}
