package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/davars/winnow-media/tool"
)

var update = flag.Bool("update", false, "rewrite the golden argv files")

// recorder wraps a Processor's runner, recording every invocation and
// optionally failing one after it has run for real.
type recorder struct {
	inner Runner
	mu    sync.Mutex
	calls [][]string
	// failAfter fails the n-th call (1-based) after running it; 0 disables.
	failAfter int
}

func (r *recorder) run(ctx context.Context, name string, args ...string) (Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	n := len(r.calls)
	r.mu.Unlock()
	res, err := r.inner(ctx, name, args...)
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

func exiftool(t *testing.T, env Env, args ...string) string {
	t.Helper()
	res, err := env.Run(context.Background(), "exiftool", args...)
	if err != nil {
		t.Fatalf("exiftool %q: %v", args, err)
	}
	return string(res.Stdout)
}

// addMetadata writes EXIF, GPS and XMP fields into path.
func addMetadata(t *testing.T, env Env, path string) {
	t.Helper()
	exiftool(t, env, "-overwrite_original", "-Artist=someone", "-GPSLatitude=1", "-GPSLatitudeRef=N",
		"-XMP-dc:Creator=bar", "-f", "-m", "-ext", "*", path)
}

func metadataGroups(t *testing.T, env Env, path string) string {
	t.Helper()
	return exiftool(t, env, "-G1", "-a", "-s", "-EXIF:all", "-GPS:all", "-XMP:all", "-IPTC:all", "-ICC_Profile:all", path)
}

func solid(i int) string {
	return fmt.Sprintf("xc:rgb(%d,%d,%d)", i*13%256, i*71%256, i*37%256)
}

// writeGIF writes frames solid 64x48 frames of 50 ms each, staged without an
// extension.
func writeGIF(t *testing.T, env Env, colour func(i int) string, frames int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.gif")
	args := []string{"-size", "64x48"}
	for i := range frames {
		args = append(args, "-delay", "5", colour(i))
	}
	args = append(args, "-loop", "0", path)
	magick(t, env, args...)
	return stage(t, path)
}

func stillInto(t *testing.T, p *Processor, path string, enc Encoding) (Image, string) {
	t.Helper()
	out := t.TempDir()
	img, _, err := p.Still(context.Background(), path, enc, out)
	if err != nil {
		t.Fatal(err)
	}
	return img, out
}

func framesInto(t *testing.T, p *Processor, path string, enc Encoding, maxFrames int) ([]Image, string) {
	t.Helper()
	out := t.TempDir()
	images, _, err := p.AnimationFrames(context.Background(), path, enc, maxFrames, out)
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
	sum := sha256.Sum256(srgbProfile)
	const want = "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("embedded profile sha256 = %s, want %s", got, want)
	}
}

func TestFitSize(t *testing.T) {
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

func TestOperationsRefuseBeforeRunningTools(t *testing.T) {
	real := requireMagick(t)
	p := requireProcessor(t)
	heic := writeHEIC(t)
	gif := writeGIF(t, real, solid, 2)
	rec := record(p)
	garbage := filepath.Join(t.TempDir(), "garbage")
	if err := os.WriteFile(garbage, []byte("not any media"), 0o600); err != nil {
		t.Fatal(err)
	}
	webm := writeFile(t, "clip.webm", ebmlHeader("webm"))

	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "absent")
	type call func(path string, enc Encoding, dir string) error
	still := func(path string, enc Encoding, dir string) error {
		_, _, err := p.Still(context.Background(), path, enc, dir)
		return err
	}
	framesN := func(n int) call {
		return func(path string, enc Encoding, dir string) error {
			_, _, err := p.AnimationFrames(context.Background(), path, enc, n, dir)
			return err
		}
	}
	cases := []struct {
		name            string
		op              call
		path            string
		enc             Encoding
		dir             string
		unsupportedType bool
	}{
		{"quality too large", still, heic, Encoding{Quality: 101}, t.TempDir(), false},
		{"quality negative", still, heic, Encoding{Quality: -1}, t.TempDir(), false},
		{"max edge too small", still, heic, Encoding{MaxEdge: 15}, t.TempDir(), false},
		{"max edge too large", still, heic, Encoding{MaxEdge: 16385}, t.TempDir(), false},
		{"max area too small", still, heic, Encoding{MaxArea: 255}, t.TempDir(), false},
		{"max area too large", still, heic, Encoding{MaxArea: 100_000_001}, t.TempDir(), false},
		{"unknown format", still, heic, Encoding{Format: "gif"}, t.TempDir(), false},
		{"type not accepted", still, gif, Encoding{}, t.TempDir(), true},
		{"video to still", still, webm, Encoding{}, t.TempDir(), true},
		{"unrecognized content", still, garbage, Encoding{}, t.TempDir(), true},
		{"missing source", still, filepath.Join(t.TempDir(), "absent"), Encoding{}, t.TempDir(), false},
		{"dir not empty", still, heic, Encoding{}, nonEmpty, false},
		{"dir missing", still, heic, Encoding{}, absent, false},
		{"frames: quality too large", framesN(0), gif, Encoding{Quality: 101}, t.TempDir(), false},
		{"frames: too few", framesN(-1), gif, Encoding{}, t.TempDir(), false},
		{"frames: too many", framesN(65), gif, Encoding{}, t.TempDir(), false},
		{"frames: type not accepted", framesN(0), heic, Encoding{}, t.TempDir(), true},
		{"frames: dir not empty", framesN(0), gif, Encoding{}, nonEmpty, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.op(c.path, c.enc, c.dir)
			if err == nil {
				t.Fatal("operation succeeded")
			}
			if got := errors.Is(err, ErrUnsupportedType); got != c.unsupportedType {
				t.Errorf("errors.Is(err, ErrUnsupportedType) = %v, want %v (err %v)", got, c.unsupportedType, err)
			}
			if n := rec.count(); n != 0 {
				t.Errorf("%d tool runs before refusal", n)
			}
		})
	}
	if got := dirNames(t, nonEmpty); !slices.Equal(got, []string{"leftover"}) {
		t.Errorf("refused directory now holds %v", got)
	}
}

func TestStillOutput(t *testing.T) {
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
			if img.Name != wantName || img.Width != 300 || img.Height != 400 || img.Frame != 0 || img.TimeMS != 0 {
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

func TestFramesSelection(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	src := writeGIF(t, env, solid, 20)
	images, out := framesInto(t, p, src, Encoding{}, 0)
	if len(images) != 16 {
		t.Fatalf("%d images, want 16", len(images))
	}
	var names []string
	for i, f := range images {
		wantIdx := i * 20 / 16
		if f.Frame != wantIdx || f.TimeMS != 0 {
			t.Errorf("image %d: source frame %d at %d ms, want %d", i, f.Frame, f.TimeMS, wantIdx)
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
	env := requireMagick(t)
	p := requireProcessor(t)
	src := writeGIF(t, env, func(int) string { return "xc:rgb(10,120,200)" }, 4)
	images, out := framesInto(t, p, src, Encoding{}, 0)
	if len(images) != 1 || images[0].Name != "frame-000.jpg" || images[0].Frame != 0 {
		t.Fatalf("images = %+v", images)
	}
	if got := dirNames(t, out); !slices.Equal(got, []string{"frame-000.jpg"}) {
		t.Errorf("output dir holds %v", got)
	}
}

func TestFramesRenumbersAfterDedup(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	// Frames 0-1 identical, 2-3 identical: two kept frames, numbered 0 and 1.
	src := writeGIF(t, env, func(i int) string { return solid(i / 2) }, 4)
	images, out := framesInto(t, p, src, Encoding{}, 0)
	var got []int
	for _, f := range images {
		got = append(got, f.Frame)
	}
	if !slices.Equal(got, []int{0, 2}) {
		t.Errorf("source frames %v", got)
	}
	if names := dirNames(t, out); !slices.Equal(names, []string{"frame-000.jpg", "frame-001.jpg"}) {
		t.Errorf("output dir holds %v", names)
	}
}

func TestFramesFlattensTransparencyOnWhite(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "t.gif")
	magick(t, env, "-size", "40x40", "-dispose", "background", "-delay", "5",
		"xc:none", "-fill", "red", "-draw", "circle 20,20 20,6",
		"(", "-size", "40x40", "xc:none", "-fill", "blue", "-draw", "circle 20,20 20,12", ")",
		"-loop", "0", path)
	images, out := framesInto(t, p, stage(t, path), Encoding{}, 0)
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
	requireExiftool(t)
	env := requireMagick(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "m.webp")
	magick(t, env, "-size", "300x200", "gradient:red-blue", path)
	addMetadata(t, env, path)
	if got := metadataGroups(t, env, path); !strings.Contains(got, "Artist") {
		t.Fatalf("fixture lacks metadata:\n%s", got)
	}
	images, out := framesInto(t, p, stage(t, path), Encoding{Format: PNG, MaxEdge: 100}, 0)
	if len(images) != 1 || images[0].Width != 100 || images[0].Height != 66 {
		t.Fatalf("images = %+v", images)
	}
	if got := metadataGroups(t, env, filepath.Join(out, images[0].Name)); strings.TrimSpace(got) != "" {
		t.Errorf("output carries metadata:\n%s", got)
	}
}

func TestFramesSizing(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	src := writeGIF(t, env, solid, 3) // 64x48
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
			images, _ := framesInto(t, p, src, c.enc, 0)
			for _, f := range images {
				if f.Width != c.wantW || f.Height != c.wantH {
					t.Errorf("%s is %dx%d, want %dx%d", f.Name, f.Width, f.Height, c.wantW, c.wantH)
				}
			}
		})
	}
}

func TestFramesMaxFrames(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	src := writeGIF(t, env, solid, 20)
	images, _ := framesInto(t, p, src, Encoding{}, 4)
	var idx []int
	for _, f := range images {
		idx = append(idx, f.Frame)
	}
	if !slices.Equal(idx, []int{0, 5, 10, 15}) {
		t.Errorf("source frames %v", idx)
	}
}

func TestOperationsCleanUpOnFailure(t *testing.T) {
	real := requireMagick(t)
	wide := func() string { return heicFixture(t, "adobe-64x64") }
	cases := []struct {
		name      string
		still     bool
		input     func() string
		failAfter int
	}{
		{"still after encode", true, func() string { return writeHEIC(t) }, 2},
		{"still with profile after encode", true, wide, 2},
		{"still at encode", true, func() string { return writeHEIC(t) }, 3},
		{"frames after header read", false, func() string { return writeGIF(t, real, solid, 6) }, 1},
		{"frames after encode", false, func() string { return writeGIF(t, real, solid, 6) }, 2},
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
	real := requireMagick(t)
	old := encodeBatchPixels
	encodeBatchPixels = 2 * 64 * 48
	t.Cleanup(func() { encodeBatchPixels = old })
	for _, failAfter := range []int{2, 3, 4} {
		p := requireProcessor(t)
		rec := record(p)
		rec.failAfter = failAfter
		out := t.TempDir()
		_, _, err := p.AnimationFrames(context.Background(), writeGIF(t, real, solid, 12), Encoding{}, 6, out)
		if err == nil {
			t.Fatalf("failAfter=%d: operation succeeded", failAfter)
		}
		if got := dirNames(t, out); len(got) != 0 {
			t.Errorf("failAfter=%d: output dir holds %v", failAfter, got)
		}
	}
	// Header read, then one encode per batch of two of the four frames.
	for _, failAfter := range []int{2, 3} {
		p := requireProcessor(t)
		rec := record(p)
		rec.failAfter = failAfter
		out := t.TempDir()
		_, _, err := p.AnimationFramesAt(context.Background(), writeGIF(t, real, solid, 12), Encoding{}, []int{0, 3, 6, 9}, out)
		if err == nil {
			t.Fatalf("AnimationFramesAt failAfter=%d: operation succeeded", failAfter)
		}
		if got := dirNames(t, out); len(got) != 0 {
			t.Errorf("AnimationFramesAt failAfter=%d: output dir holds %v", failAfter, got)
		}
	}
}

func TestGoldenArgv(t *testing.T) {
	real := requireMagick(t)
	wide := func() string { return heicFixture(t, "adobe-64x32") }
	type run func(p *Processor, input, out string) error
	still := func(enc Encoding) run {
		return func(p *Processor, input, out string) error {
			_, _, err := p.Still(context.Background(), input, enc, out)
			return err
		}
	}
	frames := func(enc Encoding, maxFrames int) run {
		return func(p *Processor, input, out string) error {
			_, _, err := p.AnimationFrames(context.Background(), input, enc, maxFrames, out)
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
		{"frames-gif-default", func() string { return writeGIF(t, real, solid, 20) }, frames(Encoding{}, 0)},
		{
			"frames-gif-webp-bounded", func() string { return writeGIF(t, real, solid, 10) },
			frames(Encoding{Format: WebP, MaxEdge: 32}, 3),
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
			checkGolden(t, c.name, goldenArgv(rec.calls, input, out))
		})
	}
}

// goldenArgv renders the recorded invocations with the input and output
// directories replaced by placeholders.
func goldenArgv(calls [][]string, input, out string) string {
	var b strings.Builder
	for _, call := range calls {
		for _, arg := range call {
			arg = strings.ReplaceAll(arg, filepath.Dir(input), "$IN")
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

// identifySizes reads back the size of each output, which its headers state.
// Without -ping identify decodes the file: every Still pays a decode of its
// own JPEG output, and animationFrames one per frame file.
func TestIdentifySizesReadsHeadersOnly(t *testing.T) {
	real := requireMagick(t)
	env, calls := countingEnv(real)
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "a.jpg"), filepath.Join(dir, "b.png")}
	magick(t, real, "-size", "12x8", "xc:red", paths[0])
	magick(t, real, "-size", "6x4", "xc:blue", paths[1])
	sizes, err := identifySizes(context.Background(), env, paths)
	if err != nil || !slices.Equal(sizes, [][2]int{{12, 8}, {6, 4}}) {
		t.Fatalf("sizes %v, err %v", sizes, err)
	}
	for _, call := range calls() {
		if !slices.Contains(call, "-ping") {
			t.Errorf("the outputs were decoded to read their sizes: %q", call)
		}
	}
}
