package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/davars/winnow-media/tool"
)

var update = flag.Bool("update", false, "rewrite the golden argv files")

// recorder wraps a Processor's runner, recording every invocation and
// optionally failing one after it has run for real.
type recorder struct {
	inner toolRun
	mu    sync.Mutex
	calls [][]string
	// failAfter fails the n-th call (1-based) after running it; 0 disables.
	failAfter int
}

func (r *recorder) run(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	n := len(r.calls)
	r.mu.Unlock()
	res, err := r.inner(ctx, in, name, args...)
	if err == nil && n == r.failAfter {
		err = errors.New("injected failure")
	}
	return res, err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// record routes the processor's tool runs through a new recorder.
func record(p *Processor) *recorder {
	r := &recorder{inner: p.run}
	p.run = r.run
	return r
}

func requireExiftool(t *testing.T) {
	t.Helper()
	if _, err := tool.Resolve("exiftool"); err != nil {
		t.Skip("exiftool not available")
	}
}

// stage moves path to a file named "input" in a fresh directory, so the
// operation cannot rely on an extension.
func stage(t *testing.T, path string) string {
	t.Helper()
	dir := t.TempDir()
	staged := filepath.Join(dir, "input")
	if err := os.Rename(path, staged); err != nil {
		t.Fatal(err)
	}
	return staged
}

// writeHEIC returns a staged copy of a 640x480 gradient tagged RightTop,
// which reads back as 480x640.
func writeHEIC(t *testing.T) string {
	t.Helper()
	return heicFixture(t, "gradient-righttop")
}

func exiftool(t *testing.T, env toolEnv, args ...string) string {
	t.Helper()
	res, err := env.run(context.Background(), "exiftool", args...)
	if err != nil {
		t.Fatalf("exiftool %q: %v", args, err)
	}
	return string(res.Stdout)
}

// addMetadata writes EXIF, GPS and XMP fields into path.
func addMetadata(t *testing.T, env toolEnv, path string) {
	t.Helper()
	exiftool(t, env, "-overwrite_original", "-Artist=someone", "-GPSLatitude=1", "-GPSLatitudeRef=N",
		"-XMP-dc:Creator=bar", "-f", "-m", "-ext", "*", path)
}

func metadataGroups(t *testing.T, env toolEnv, path string) string {
	t.Helper()
	return exiftool(t, env, "-G1", "-a", "-s", "-EXIF:all", "-GPS:all", "-XMP:all", "-IPTC:all", "-ICC_Profile:all", path)
}

func solid(i int) string {
	return fmt.Sprintf("xc:rgb(%d,%d,%d)", i*13%256, i*71%256, i*37%256)
}

// writeGIF writes frames solid 64x48 frames of 50 ms each, staged without an
// extension, at most 256 of them (each frame's palette index is a byte).
// colour names each frame's colour as "xc:rgb(r,g,b)".
func writeGIF(t *testing.T, colour func(i int) string, frames int) string {
	t.Helper()
	rect := image.Rect(0, 0, 64, 48)
	palette := make(color.Palette, frames)
	for i := range palette {
		var r, g, b int
		if _, err := fmt.Sscanf(colour(i), "xc:rgb(%d,%d,%d)", &r, &g, &b); err != nil {
			t.Fatalf("colour %q: %v", colour(i), err)
		}
		palette[i] = color.RGBA{uint8(r), uint8(g), uint8(b), 255}
	}
	out := &gif.GIF{Config: image.Config{ColorModel: palette, Width: rect.Dx(), Height: rect.Dy()}}
	for i := range frames {
		img := image.NewPaletted(rect, palette)
		for j := range img.Pix {
			img.Pix[j] = uint8(i)
		}
		out.Image = append(out.Image, img)
		out.Delay = append(out.Delay, 5)
		out.Disposal = append(out.Disposal, 0)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, out); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "x.gif")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return stage(t, path)
}

func stillInto(t *testing.T, p *Processor, path string, enc Encoding) (Image, string) {
	t.Helper()
	out := t.TempDir()
	img, _, err := p.encodeFirst(context.Background(), path, enc, out)
	if err != nil {
		t.Fatal(err)
	}
	return img, out
}

func framesInto(t *testing.T, p *Processor, path string, enc Encoding, maxFrames int) ([]Image, string) {
	t.Helper()
	out := t.TempDir()
	images, _, err := p.encodeEven(context.Background(), path, enc, maxFrames, out)
	if err != nil {
		t.Fatal(err)
	}
	return images, out
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestEmbeddedProfile(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256(srgbProfile)
	const want = "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("embedded profile sha256 = %s, want %s", got, want)
	}
}

func TestFitSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		w, h, edge, area int
		tw, th           int
		resize           bool
	}{
		{480, 640, 4000, 100_000_000, 480, 640, false},
		{480, 640, 320, 100_000_000, 240, 320, true},
		{480, 640, 16384, 100000, 273, 365, true},
		{480, 640, 400, 100000, 273, 365, true},
		{480, 640, 640, 307200, 480, 640, false},
		{6000, 4000, 2576, 3750000, 2371, 1581, true},
		{1, 4000, 100, 100000, 1, 100, true},
		// A side that floors to 1 leaves the other over the area until the
		// area bound clamps it.
		{1, 4000, 16384, 100, 1, 100, true},
		{4000, 1, 16384, 100, 100, 1, true},
		// A rung is an edge bound and the area of its square. ImageMagick's
		// -thumbnail rounds 1536.5 up to 1537; the size is the floor.
		{4000, 3001, 2048, 2048 * 2048, 2048, 1536, true},
		{3001, 4000, 2048, 2048 * 2048, 1536, 2048, true},
		{4000, 3000, 2048, 2048 * 2048, 2048, 1536, true},
		{2048, 1537, 2048, 2048 * 2048, 2048, 1537, false},
		{800, 601, 512, 512 * 512, 512, 384, true},
	}
	for _, c := range cases {
		tw, th, resize := fitSize(c.w, c.h, c.edge, c.area)
		if tw != c.tw || th != c.th || resize != c.resize {
			t.Errorf("fitSize(%d,%d,%d,%d) = %d,%d,%v want %d,%d,%v", c.w, c.h, c.edge, c.area, tw, th, resize, c.tw, c.th, c.resize)
		}
		if tw*th > c.area || max(tw, th) > c.edge {
			t.Errorf("fitSize(%d,%d,%d,%d) = %dx%d exceeds a bound", c.w, c.h, c.edge, c.area, tw, th)
		}
	}
}

// A header report that cannot be read is an error, not a shorter list of
// scenes.
func TestParseSceneHeadersRefusesAnUnreadableReport(t *testing.T) {
	t.Parallel()
	const good = "5||40|30|40|30|WEBP|Undefined|DirectClass sRGB|False|END"
	if scenes, err := parseSceneHeaders([]byte(good + "\n" + good + "\n")); err != nil || len(scenes) != 2 || scenes[0].delayCS != 5 || scenes[1].width != 40 {
		t.Fatalf("a good report gave %+v, %v", scenes, err)
	}
	for _, c := range []struct{ name, report, want string }{
		{"empty", "", "identify returned no scenes"},
		{"too few fields", "5||40|30|40|30|WEBP|Undefined|DirectClass sRGB|False\n", "missing fields"},
		{"too many fields", good + "|\n", "missing fields"},
		{"no END", strings.TrimSuffix(good, "END") + "ENDS\n", "missing fields"},
		{"a delay that is no number", "x" + strings.TrimPrefix(good, "5") + "\n", "parsing"},
		{"a width", "5||x|30|40|30|WEBP|Undefined|DirectClass sRGB|False|END\n", "parsing"},
		{"a height", "5||40|x|40|30|WEBP|Undefined|DirectClass sRGB|False|END\n", "parsing"},
		{"a canvas width", "5||40|30|x|30|WEBP|Undefined|DirectClass sRGB|False|END\n", "parsing"},
		{"a canvas height", "5||40|30|40|x|WEBP|Undefined|DirectClass sRGB|False|END\n", "parsing"},
		{"a bad second scene", good + "\n" + "5||40|30|40|30|WEBP\n", "missing fields"},
	} {
		if scenes, err := parseSceneHeaders([]byte(c.report)); err == nil || !strings.Contains(err.Error(), c.want) || scenes != nil {
			t.Errorf("%s: %+v, %v; want an error saying %q and no scenes", c.name, scenes, err, c.want)
		}
	}
}

// identifyScenes reads what identify reports of an encoded file, and fails on
// a report it cannot read.
func TestIdentifyScenesRefusesAnUnreadableReport(t *testing.T) {
	t.Parallel()
	read := func(report string) (int, int, int, error) {
		env := toolEnv{run: func(context.Context, string, ...string) (runResult, error) {
			return runResult{Stdout: []byte(report)}, nil
		}}
		delays, w, h, err := env.identifyScenes(context.Background(), "/a.webp")
		return len(delays), w, h, err
	}
	if scenes, w, h, err := read("1 8 6 0\n2 8 6 5\n"); err != nil || scenes != 2 || w != 8 || h != 6 {
		t.Fatalf("a good report gave %d scenes %dx%d, %v", scenes, w, h, err)
	}
	for _, c := range []struct{ name, report, want string }{
		{"empty", "", "identify returned no output"},
		{"too few fields", "1 8 6\n", "missing fields"},
		{"a width", "1 x 6 0\n", "parsing width"},
		{"a height", "1 8 x 0\n", "parsing height"},
		{"a delay", "1 8 6 x\n", "parsing delay"},
		{"a bad second scene", "1 8 6 0\n2 8 6\n", "missing fields"},
		{"a delay of a later one", "1 8 6 0\n2 8 6 x\n", "parsing delay"},
	} {
		if scenes, w, h, err := read(c.report); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %d scenes %dx%d, %v; want an error saying %q", c.name, scenes, w, h, err, c.want)
		}
	}
}

// An encode whose report cannot be read, or reports other frames than it was
// given, fails and leaves no frame in the output directory.
func TestAnimationFramesRefuseAnUnreadableEncodeReport(t *testing.T) {
	t.Parallel()
	src := writeGIF(t, solid, 6)
	for _, c := range []struct {
		name, report, want string
	}{
		{"empty", "", "missing fields"},
		{"no size", "0123abcd\n", "missing fields"},
		{"unsigned", "8|6\n", "missing fields"},
		{"a fourth field", "0123abcd|8|6|1\n", "missing fields"},
		{"a width that is no number", "0123abcd|x|6\n", "unreadable size"},
		{"a height that is no number", "0123abcd|8|x\n", "unreadable size"},
		{"fewer frames than were given", "0123abcd|8|6\n", "reported 1 frames"},
		{"a bad second line", "0123abcd|8|6\n0123abcd|8\n", "missing fields"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			inner := p.run
			p.run = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
				res, err := inner(ctx, in, name, args...)
				if err == nil && name == "magick" && slices.Contains(args, "info:") && !slices.Contains(args, "identify") {
					res.Stdout = []byte(c.report)
				}
				return res, err
			}
			out := t.TempDir()
			_, _, err := p.encodeEven(context.Background(), src, Encoding{}, 6, out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to say %q", err, c.want)
			}
			if got := dirNames(t, out); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}
}

// Each bound of an Encoding is inclusive, and a value one past it is refused.
func TestEncodingBoundsAreInclusive(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		enc  Encoding
		ok   bool
	}{
		{"quality 1", Encoding{Quality: 1}, true},
		{"quality 100", Encoding{Quality: 100}, true},
		{"quality 101", Encoding{Quality: 101}, false},
		{"max edge 16", Encoding{MaxEdge: 16}, true},
		{"max edge 15", Encoding{MaxEdge: 15}, false},
		{"max edge 16384", Encoding{MaxEdge: 16384}, true},
		{"max edge 16385", Encoding{MaxEdge: 16385}, false},
		{"max area 256", Encoding{MaxArea: 256}, true},
		{"max area 255", Encoding{MaxArea: 255}, false},
		{"max area 100000000", Encoding{MaxArea: 100_000_000}, true},
		{"max area 100000001", Encoding{MaxArea: 100_000_001}, false},
	} {
		if _, err := c.enc.resolve(); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want accepted = %v", c.name, err, c.ok)
		}
	}
}

func TestDisplayDelayCS(t *testing.T) {
	t.Parallel()
	for delay, want := range map[int]int{-1: 10, 0: 10, 1: 10, 2: 2, 3: 3, 100: 100} {
		if got := displayDelayCS(delay); got != want {
			t.Errorf("displayDelayCS(%d) = %d, want %d", delay, got, want)
		}
	}
}

// uprightSize swaps the sides for exactly the four orientations that turn the
// image a quarter.
func TestUprightSizeSwapsForTheQuarterTurns(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		swap bool
	}{
		{"", false},
		{"Undefined", false},
		{"TopLeft", false},
		{"TopRight", false},
		{"BottomRight", false},
		{"BottomLeft", false},
		{"LeftTop", true},
		{"RightTop", true},
		{"RightBottom", true},
		{"LeftBottom", true},
	} {
		w, h := uprightSize(30, 20, c.name)
		if want := [2]int{30, 20}; c.swap && (w != 20 || h != 30) || !c.swap && [2]int{w, h} != want {
			t.Errorf("uprightSize(30, 20, %q) = %dx%d", c.name, w, h)
		}
	}
}

// A directory or a FIFO is refused as a source before a tool runs, and the
// call returns instead of waiting for a writer to open the FIFO.
func TestOperationsRefuseASourceThatIsNotARegularFile(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	rec := record(p)
	defer func() { p.run = rec.inner }()
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("cannot make a FIFO:", err)
	}
	for name, path := range map[string]string{"directory": t.TempDir(), "FIFO": fifo} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, _, err := p.encodeFirst(context.Background(), path, Encoding{}, t.TempDir())
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
					t.Errorf("err = %v, want a refusal of a file that is not regular", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the call did not return")
			}
			if n := rec.count(); n != 0 {
				t.Errorf("%d tool runs", n)
			}
		})
	}
}

// Encode refuses a bad Encoding, Select or directory before any tool runs, and
// Open refuses a missing file.
func TestEncodeRefusesBeforeRunningTools(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	still := openSourceOn(t, p, writeHEIC(t), OpenOptions{})
	anim := openSourceOn(t, p, writeGIF(t, solid, 2), OpenOptions{})
	rec := record(p)
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "absent")
	cases := []struct {
		name string
		src  *Source
		sel  Select
		enc  Encoding
		dir  string
	}{
		{"quality too large", still, Select{}, Encoding{Quality: 101}, t.TempDir()},
		{"quality negative", still, Select{}, Encoding{Quality: -1}, t.TempDir()},
		{"max edge too small", still, Select{}, Encoding{MaxEdge: 15}, t.TempDir()},
		{"max edge too large", still, Select{}, Encoding{MaxEdge: 16385}, t.TempDir()},
		{"max area too small", still, Select{}, Encoding{MaxArea: 255}, t.TempDir()},
		{"max area too large", still, Select{}, Encoding{MaxArea: 100_000_001}, t.TempDir()},
		{"unknown format", still, Select{}, Encoding{Format: "gif"}, t.TempDir()},
		{"dir not empty", still, Select{}, Encoding{}, nonEmpty},
		{"dir missing", still, Select{}, Encoding{}, absent},
		{"frames: quality too large", anim, Select{Frames: Even(2)}, Encoding{Quality: 101}, t.TempDir()},
		{"frames: too few", anim, Select{Frames: Even(-1)}, Encoding{}, t.TempDir()},
		{"frames: dir not empty", anim, Select{Frames: Even(2)}, Encoding{}, nonEmpty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := rec.count()
			if _, _, err := c.src.Encode(context.Background(), c.dir, c.sel, c.enc); err == nil {
				t.Fatal("Encode succeeded")
			} else if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
			if n := rec.count() - before; n != 0 {
				t.Errorf("%d tool runs before refusal", n)
			}
		})
	}
	if got := dirNames(t, nonEmpty); !slices.Equal(got, []string{"leftover"}) {
		t.Errorf("refused directory now holds %v", got)
	}
	if _, _, err := p.Open(context.Background(), filepath.Join(t.TempDir(), "absent"), OpenOptions{}); err == nil || errors.Is(err, ErrUnsupportedType) {
		t.Errorf("missing source: err = %v, want a refusal that is not of the type", err)
	}
}

func TestStillOutput(t *testing.T) {
	t.Parallel()
	requireExiftool(t)
	env := requireMagick(t)
	p := requireProcessor(t)
	src := writeHEIC(t)
	addMetadata(t, env, src)
	if got := metadataGroups(t, env, src); !strings.Contains(got, "Artist") || !strings.Contains(got, "GPSLatitude") || !strings.Contains(got, "Creator") {
		t.Fatalf("fixture lacks metadata:\n%s", got)
	}

	for _, format := range []Format{"", JPEG, WebP, PNG} {
		t.Run("format="+string(format), func(t *testing.T) {
			wantType, wantName := "image/jpeg", "still.jpg"
			switch format {
			case WebP:
				wantType, wantName = "image/webp", "still.webp"
			case PNG:
				wantType, wantName = "image/png", "still.png"
			}
			img, out := stillInto(t, p, src, Encoding{Format: format, MaxEdge: 400})
			if img.Name != wantName || img.Width != 300 || img.Height != 400 || img.Frames[0].Index != 0 || img.Frames[0].Time.Milliseconds() != 0 {
				t.Errorf("image = %+v, want %s 300x400 frame 0 at 0ms", img, wantName)
			}
			if got := dirNames(t, out); !slices.Equal(got, []string{wantName}) {
				t.Errorf("output dir holds %v", got)
			}
			path := filepath.Join(out, wantName)
			if got := metadataGroups(t, env, path); strings.TrimSpace(got) != "" {
				t.Errorf("output carries metadata:\n%s", got)
			}
			if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%w %h", path))); got != "300 400" {
				t.Errorf("identify = %q", got)
			}
			if got := exiftool(t, env, "-s3", "-MIMEType", path); strings.TrimSpace(got) != wantType {
				t.Errorf("media type is %q, want %q", strings.TrimSpace(got), wantType)
			}
		})
	}
}

func TestStillSizing(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := writeHEIC(t) // 480x640 upright
	cases := []struct {
		name         string
		edge, area   int
		wantW, wantH int
	}{
		{"both bounds exceeded", 400, 100000, 273, 365},
		{"only edge exceeded", 320, 100_000_000, 240, 320},
		{"only area exceeded", 16384, 100000, 273, 365},
		{"neither exceeded", 4000, 100_000_000, 480, 640},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, _ := stillInto(t, p, src, Encoding{MaxEdge: c.edge, MaxArea: c.area})
			if img.Width != c.wantW || img.Height != c.wantH {
				t.Errorf("got %dx%d, want %dx%d", img.Width, img.Height, c.wantW, c.wantH)
			}
		})
	}
}

func TestStillConvertsProfiledInputToSRGB(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	dir := t.TempDir()
	srgb := writeSRGBProfile(t)
	heic := heicFixtureFile(t, "adobe-256x96")
	if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%[profiles]", heic))); !strings.Contains(got, "icc") {
		t.Skipf("magick wrote no ICC profile into the HEIC (profiles %q)", got)
	}
	reference := filepath.Join(dir, "reference.png")
	magick(t, env, heic, "-auto-orient", "-profile", srgb, reference)
	naive := filepath.Join(dir, "naive.png")
	magick(t, env, heic, "-auto-orient", naive)

	img, out := stillInto(t, p, stage(t, heic), Encoding{Format: PNG})
	if got := dirNames(t, out); !slices.Equal(got, []string{"still.png"}) {
		t.Errorf("output dir holds %v", got)
	}
	got := filepath.Join(out, img.Name)
	if profiles := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%[profiles]", got))); profiles != "" {
		t.Errorf("output embeds profiles %q", profiles)
	}
	converted := meanAbsoluteDifference(t, env, got, reference)
	unconverted := meanAbsoluteDifference(t, env, got, naive)
	if converted > 0.004 || unconverted < 5*converted {
		t.Errorf("difference from sRGB reference %.6f, from unconverted %.6f", converted, unconverted)
	}
}

// ImageMagick's JXL coder reports no profile under -ping, so the header read
// of a JXL must be completed by a full read of the image to be encoded. A JXL
// of several frames is read as several images, and the first must still be
// converted to sRGB.
func TestStillConvertsTheFirstFrameOfAProfiledMultiFrameJXL(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	dir := t.TempDir()
	srgb := writeSRGBProfile(t)
	adobe := writeAdobeProfile(t, dir)
	path := filepath.Join(dir, "wide.jxl")
	magick(t, env, "-delay", "10", "-size", "64x48", "gradient:#0020ff-#ffb000", "-profile", srgb, "-profile", adobe,
		"(", "+clone", "-negate", ")", path)
	if n := strings.Count(string(magick(t, env, "identify", "-ping", "-format", "%m\n", path)), "JXL"); n < 2 {
		t.Skipf("magick wrote a JXL of %d images", n)
	}
	if got := string(magick(t, env, "identify", "-format", "%[profiles]", path+"[0]")); !strings.Contains(got, "icc") {
		t.Skipf("magick wrote no ICC profile into the JXL (profiles %q)", got)
	}
	rec := record(p)
	stillInto(t, p, stage(t, path), Encoding{Format: PNG})
	var converted bool
	for _, call := range rec.ran("magick") {
		if slices.Contains(call, "-profile") {
			converted = true
		}
	}
	if !converted {
		t.Error("the first frame of a profiled multi-frame JXL was encoded with no conversion to sRGB")
	}
}

// The profile of a later frame of a multi-frame JXL, which ImageMagick reports
// only from a full read, is completed before that frame is encoded.
func TestSourceCompletesTheProfileOfALaterFrameOfAMultiFrameJXL(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "wide.jxl")
	magick(t, env, "-delay", "10", "-size", "64x48", "gradient:#0020ff-#ffb000", "(", "+clone", "-negate", ")", path)
	if n := strings.Count(string(magick(t, env, "identify", "-ping", "-format", "%m\n", path)), "JXL"); n < 2 {
		t.Skipf("magick wrote a JXL of %d images", n)
	}
	src := openSourceOn(t, p, path, OpenOptions{})
	rec := record(p)
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Indices(1)}, Encoding{Format: PNG}); err != nil {
		t.Fatal(err)
	}
	var full bool
	for _, call := range rec.ran("magick") {
		if slices.Contains(call, "identify") && !slices.Contains(call, "-ping") && strings.HasSuffix(call[len(call)-1], "[1]") {
			full = true
		}
	}
	if !full {
		t.Errorf("no full read of the second frame before its encode: %q", rec.calls)
	}
}

func TestFramesSelection(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	src := writeGIF(t, solid, 20)
	images, out := framesInto(t, p, src, Encoding{}, defaultFrames)
	if len(images) != 16 {
		t.Fatalf("%d images, want 16", len(images))
	}
	var names []string
	for i, f := range images {
		wantIdx := i * 20 / 16
		wantMS := int64(wantIdx) * 50
		if f.Frames[0].Index != wantIdx || f.Frames[0].Time.Milliseconds() != wantMS {
			t.Errorf("image %d: source frame %d at %d ms, want frame %d at %d ms", i, f.Frames[0].Index, f.Frames[0].Time.Milliseconds(), wantIdx, wantMS)
		}
		if f.Name != fmt.Sprintf("frame-%03d.jpg", i) || f.Width != 64 || f.Height != 48 {
			t.Errorf("image %d = %+v", i, f)
		}
		names = append(names, f.Name)
	}
	if got := dirNames(t, out); !slices.Equal(got, names) {
		t.Errorf("output dir holds %v, want %v", got, names)
	}
	// The written frame is the selected source frame, not its neighbour.
	for _, i := range []int{3, 4} {
		want := strings.NewReplacer("xc:", "").Replace(solid(i * 20 / 16))
		got := strings.TrimSpace(string(magick(t, env, filepath.Join(out, images[i].Name), "-format", "rgb(%[fx:round(255*p{1,1}.r)],%[fx:round(255*p{1,1}.g)],%[fx:round(255*p{1,1}.b)])", "info:")))
		if rgbDistance(t, got, want) > 8 {
			t.Errorf("file %d pixel %s, want about %s", i, got, want)
		}
	}
}

func rgbDistance(t *testing.T, a, b string) int {
	t.Helper()
	parse := func(s string) [3]int {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "rgb("), ")")
		var out [3]int
		for i, p := range strings.Split(s, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				t.Fatalf("parsing %q: %v", s, err)
			}
			out[i] = n
		}
		return out
	}
	pa, pb := parse(a), parse(b)
	d := 0
	for i := range pa {
		d = max(d, max(pa[i]-pb[i], pb[i]-pa[i]))
	}
	return d
}

func TestFramesDropsConsecutiveDuplicates(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := writeGIF(t, func(int) string { return "xc:rgb(10,120,200)" }, 4)
	images, out := framesInto(t, p, src, Encoding{}, defaultFrames)
	if len(images) != 1 || images[0].Name != "frame-000.jpg" || images[0].Frames[0].Index != 0 {
		t.Fatalf("images = %+v", images)
	}
	if got := dirNames(t, out); !slices.Equal(got, []string{"frame-000.jpg"}) {
		t.Errorf("output dir holds %v", got)
	}
}

func TestFramesRenumbersAfterDedup(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	// Frames 0-1 identical, 2-3 identical: two kept frames, numbered 0 and 1.
	src := writeGIF(t, func(i int) string { return solid(i / 2) }, 4)
	images, out := framesInto(t, p, src, Encoding{}, defaultFrames)
	var got []int
	for _, f := range images {
		got = append(got, f.Frames[0].Index)
	}
	if !slices.Equal(got, []int{0, 2}) {
		t.Errorf("source frames %v", got)
	}
	if names := dirNames(t, out); !slices.Equal(names, []string{"frame-000.jpg", "frame-001.jpg"}) {
		t.Errorf("output dir holds %v", names)
	}
}

func TestFramesFlattensTransparencyOnWhite(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "t.gif")
	magick(t, env, "-size", "40x40", "-dispose", "background", "-delay", "5",
		"xc:none", "-fill", "red", "-draw", "circle 20,20 20,6",
		"(", "-size", "40x40", "xc:none", "-fill", "blue", "-draw", "circle 20,20 20,12", ")",
		"-loop", "0", path)
	images, out := framesInto(t, p, stage(t, path), Encoding{}, defaultFrames)
	if len(images) != 2 {
		t.Fatalf("%d images, want 2", len(images))
	}
	for _, f := range images {
		got := strings.TrimSpace(string(magick(t, env, filepath.Join(out, f.Name), "-format", "rgb(%[fx:round(255*p{1,1}.r)],%[fx:round(255*p{1,1}.g)],%[fx:round(255*p{1,1}.b)])", "info:")))
		if d := rgbDistance(t, got, "rgb(255,255,255)"); d > 3 {
			t.Errorf("%s corner pixel %s, want white", f.Name, got)
		}
	}
}

func TestFramesStripsMetadataAndAcceptsWebP(t *testing.T) {
	t.Parallel()
	requireExiftool(t)
	env := requireMagick(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "m.webp")
	magick(t, env, "-size", "300x200", "gradient:red-blue", path)
	addMetadata(t, env, path)
	if got := metadataGroups(t, env, path); !strings.Contains(got, "Artist") {
		t.Fatalf("fixture lacks metadata:\n%s", got)
	}
	images, out := framesInto(t, p, stage(t, path), Encoding{Format: PNG, MaxEdge: 100}, defaultFrames)
	if len(images) != 1 || images[0].Width != 100 || images[0].Height != 66 {
		t.Fatalf("images = %+v", images)
	}
	if got := metadataGroups(t, env, filepath.Join(out, images[0].Name)); strings.TrimSpace(got) != "" {
		t.Errorf("output carries metadata:\n%s", got)
	}
}

func TestFramesSizing(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := writeGIF(t, solid, 3) // 64x48
	cases := []struct {
		name         string
		enc          Encoding
		wantW, wantH int
	}{
		{"edge", Encoding{MaxEdge: 32}, 32, 24},
		{"area", Encoding{MaxArea: 768}, 32, 24},
		{"both", Encoding{MaxEdge: 48, MaxArea: 768}, 32, 24},
		{"no upscale", Encoding{MaxEdge: 1000}, 64, 48},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			images, _ := framesInto(t, p, src, c.enc, defaultFrames)
			for _, f := range images {
				if f.Width != c.wantW || f.Height != c.wantH {
					t.Errorf("%s is %dx%d, want %dx%d", f.Name, f.Width, f.Height, c.wantW, c.wantH)
				}
			}
		})
	}
}

func TestFramesMaxFrames(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := writeGIF(t, solid, 20)
	images, _ := framesInto(t, p, src, Encoding{}, 4)
	var idx []int
	for _, f := range images {
		idx = append(idx, f.Frames[0].Index)
	}
	if !slices.Equal(idx, []int{0, 5, 10, 15}) {
		t.Errorf("source frames %v", idx)
	}
}

func TestOperationsCleanUpOnFailure(t *testing.T) {
	t.Parallel()
	wide := func() string { return heicFixture(t, "adobe-64x64") }
	cases := []struct {
		name      string
		still     bool
		input     func() string
		failAfter int
	}{
		{"still after encode", true, func() string { return writeHEIC(t) }, 2},
		{"still with profile after encode", true, wide, 2},
		{"frames after header read", false, func() string { return writeGIF(t, solid, 6) }, 1},
		{"frames after encode", false, func() string { return writeGIF(t, solid, 6) }, 2},
	}
	for _, c := range cases {
		opNames := animationFrameOpNames
		if c.still {
			opNames = []string{"still"}
		}
		for _, opName := range opNames {
			t.Run(c.name+"/"+opName, func(t *testing.T) {
				p := requireProcessor(t)
				ops := animationFrameOps(p)
				ops["still"] = stillOp(p)
				rec := record(p)
				rec.failAfter = c.failAfter
				out := t.TempDir()
				if _, err := ops[opName](c.input(), out); err == nil {
					t.Fatal("operation succeeded")
				}
				if got := dirNames(t, out); len(got) != 0 {
					t.Errorf("output dir holds %v", got)
				}
			})
		}
	}
}

// TestGIFFramesCleanUpOnFailureBetweenBatches fails an encode while the
// compositor is still waiting to write the next batch.
func TestGIFFramesCleanUpOnFailureBetweenBatches(t *testing.T) {
	old := encodeBatchPixels
	encodeBatchPixels = 2 * 64 * 48
	t.Cleanup(func() { encodeBatchPixels = old })
	for _, failAfter := range []int{2, 3, 4} {
		p := requireStreamingProcessor(t)
		rec := record(p)
		rec.failAfter = failAfter
		out := t.TempDir()
		_, _, err := p.encodeEven(context.Background(), writeGIF(t, solid, 12), Encoding{}, 6, out)
		if err == nil {
			t.Fatalf("failAfter=%d: operation succeeded", failAfter)
		}
		if got := dirNames(t, out); len(got) != 0 {
			t.Errorf("failAfter=%d: output dir holds %v", failAfter, got)
		}
	}
	// Header read, then one encode per batch of two of the four frames.
	for _, failAfter := range []int{2, 3} {
		p := requireStreamingProcessor(t)
		rec := record(p)
		rec.failAfter = failAfter
		out := t.TempDir()
		_, _, err := p.encodeIndices(context.Background(), writeGIF(t, solid, 12), Encoding{}, []int{0, 3, 6, 9}, out)
		if err == nil {
			t.Fatalf("Indices failAfter=%d: operation succeeded", failAfter)
		}
		if got := dirNames(t, out); len(got) != 0 {
			t.Errorf("Indices failAfter=%d: output dir holds %v", failAfter, got)
		}
	}
}

func TestGoldenArgv(t *testing.T) {
	t.Parallel()
	wide := func() string { return heicFixture(t, "adobe-64x32") }
	type run func(p *Processor, input, out string) error
	still := func(enc Encoding) run {
		return func(p *Processor, input, out string) error {
			_, _, err := p.encodeFirst(context.Background(), input, enc, out)
			return err
		}
	}
	frames := func(enc Encoding, maxFrames int) run {
		return func(p *Processor, input, out string) error {
			_, _, err := p.encodeEven(context.Background(), input, enc, maxFrames, out)
			return err
		}
	}
	cases := []struct {
		name  string
		input func() string
		run   run
	}{
		{"still-jpeg-default", func() string { return writeHEIC(t) }, still(Encoding{})},
		{
			"still-png-bounded", func() string { return writeHEIC(t) },
			still(Encoding{Format: PNG, MaxEdge: 400, MaxArea: 100000, Quality: 75}),
		},
		{"still-profiled", wide, still(Encoding{})},
		{"frames-gif-default", func() string { return writeGIF(t, solid, 20) }, frames(Encoding{}, defaultFrames)},
		{
			"frames-gif-webp-bounded", func() string { return writeGIF(t, solid, 10) },
			frames(Encoding{Format: WebP, MaxEdge: 32, MaxArea: 3_750_000}, 3),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			rec := record(p)
			input := c.input()
			out := t.TempDir()
			if err := c.run(p, input, out); err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.name, goldenArgv(rec.calls, out))
		})
	}
}

// neutralLinkDir matches the input directory of a Processor.
var neutralLinkDir = regexp.MustCompile(`/[^\s:]*/tmp/in-[A-Za-z0-9]+`)

// goldenArgv renders the recorded invocations with the input and output
// directories replaced by placeholders.
func goldenArgv(calls [][]string, out string) string {
	var b strings.Builder
	for _, call := range calls {
		for _, arg := range call {
			arg = neutralLinkDir.ReplaceAllLiteralString(arg, "$IN")
			// Off Linux a tool reads the input through a link, which the
			// golden files name as on Linux.
			arg = strings.ReplaceAll(arg, "$IN/input", inputFDPath)
			arg = strings.ReplaceAll(arg, out, "$OUT")
			b.WriteString(strconv.Quote(arg))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden-argv", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("argv differs from %s:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}
