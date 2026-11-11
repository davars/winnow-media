package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRegionShape(t *testing.T) {
	const w, h = 100, 50
	for _, c := range []struct {
		name string
		r    region
		ok   bool
	}{
		{"whole frame", region{}, true},
		{"one pixel at the origin", region{0, 0, 1, 1}, true},
		{"one pixel at the far corner", region{99, 49, 1, 1}, true},
		{"the frame itself", region{0, 0, w, h}, true},
		{"negative x", region{-1, 0, 10, 10}, false},
		{"negative y", region{0, -1, 10, 10}, false},
		{"zero width", region{0, 0, 0, 10}, false},
		{"zero height", region{0, 0, 10, 0}, false},
		{"a corner alone", region{3, 4, 0, 0}, false},
		{"negative width", region{0, 0, -5, 10}, false},
		{"too wide", region{0, 0, w + 1, 10}, false},
		{"too tall", region{0, 0, 10, h + 1}, false},
		{"past the right edge", region{91, 0, 10, 10}, false},
		{"past the bottom edge", region{0, 41, 10, 10}, false},
		{"corner far outside", region{1 << 62, 0, 1 << 62, 1}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.r.check(w, h); (err == nil) != c.ok {
				t.Errorf("check(%d, %d) of %+v = %v, want ok %v", w, h, c.r, err, c.ok)
			}
		})
	}
}

// uprightSources are 480x640 pictures held as a tag over a 640x480 frame
// (the JPEG) or as one the decoder turns itself (the HEIC), opened with a
// counting Env.
var uprightSources = map[string]func(t *testing.T) (*Source, func() [][]string){
	"heic": func(t *testing.T) (*Source, func() [][]string) {
		env, calls := sourceEnv(t)
		return openSource(t, env, heicFixtureFile(t, "gradient-righttop"), Hints{Still: true}), calls
	},
	"jpeg": func(t *testing.T) (*Source, func() [][]string) {
		env, calls := sourceEnv(t)
		return openSource(t, env, quadrantJPEG(t, env), Hints{Still: true}), calls
	},
}

func heicSource(t *testing.T) (*Source, func() [][]string) { return uprightSources["heic"](t) }

func pngEncoding() encoding {
	return encoding{format: PNG, ext: ".png", quality: 90, maxEdge: 100, maxArea: 10000}
}

// The region is checked against the upright frame of a rotated image, whose
// width is the stored height.
func TestRegionEdgesAreThoseOfTheUprightFrame(t *testing.T) {
	const w, h = 480, 640
	for _, c := range []struct {
		name string
		r    region
		ok   bool
	}{
		{"x+width is the upright width", region{w - 100, 0, 100, 100}, true},
		{"x+width is one more", region{w - 99, 0, 100, 100}, false},
		{"y+height is the upright height", region{0, h - 100, 100, 100}, true},
		{"y+height is one more", region{0, h - 99, 100, 100}, false},
		{"a width the stored frame has", region{0, 0, 500, 100}, false},
		{"the whole upright frame", region{0, 0, w, h}, true},
	} {
		for kind, open := range uprightSources {
			t.Run(kind+" "+c.name, func(t *testing.T) {
				src, calls := open(t)
				before := len(calls())
				art, err := src.encode(context.Background(), c.r, 0, []encoding{pngEncoding()})
				if c.ok {
					if err != nil {
						t.Fatal(err)
					}
					art[0].Cleanup()
					return
				}
				if err == nil || !strings.Contains(err.Error(), "outside") {
					t.Fatalf("err = %v, want a refusal that the region is outside the frame", err)
				}
				if n := len(calls()); n != before {
					t.Errorf("the refused call ran %d tools, want none", n-before)
				}
			})
		}
	}
}

func TestRegionIsRefusedWithoutRunningATool(t *testing.T) {
	src, calls := heicSource(t)
	before := len(calls())
	for _, r := range []region{{-1, 0, 10, 10}, {0, -1, 10, 10}, {0, 0, 0, 10}, {2, 2, 0, 0}} {
		if _, err := src.encode(context.Background(), r, 0, []encoding{pngEncoding()}); err == nil {
			t.Errorf("region %+v accepted", r)
		}
	}
	if n := len(calls()); n != before {
		t.Errorf("refused calls ran %d tools, want none", n-before)
	}
}

func TestStillImageRefusesARegionOutsideTheFrame(t *testing.T) {
	env, calls := sourceEnv(t)
	dir := t.TempDir()
	_, err := env.stillRegion(context.Background(), region{400, 0, 100, 100}, decoder{"magick", "heic"}, heicFixtureFile(t, "gradient-righttop"), pngEncoding(), dir)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("err = %v, want a refusal that the region is outside the 480x640 frame", err)
	}
	for _, call := range calls() {
		if slices.Contains(call, "-crop") {
			t.Errorf("the refused call ran %q", call)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refused call left %d files", len(entries))
	}
}

// The crop follows the orientation and precedes the conversion and the
// resize, so that pixel work runs on the region only.
func TestEncodeArgsCropAfterOrientation(t *testing.T) {
	src := encodeSource{orientation: "RightTop", crop: region{4, 6, 30, 20}, upright: [2]int{50, 60}, width: 15, height: 10}
	frame := encodeFrame{input: "pam:/f", convertTo: "/srgb.icc", assign: "/own.icc"}
	enc := webpEncoding(100)
	got := encodeArgs([]encodeFrame{frame}, src, enc, encodeOutput{kind: outputStill, path: "/o.webp"})
	want := []string{
		"pam:/f",
		"-orient", "RightTop", "-auto-orient", "-orient", "TopLeft",
		"+repage", "-print", `upright|%w|%h\n`, "-crop", "30x20+4+6", "+repage",
		"-profile", "/own.icc", "-profile", "/srgb.icc",
		"-filter", "Lanczos", "-resize", "15x10!",
		"-strip", "-depth", "8", "-quality", "80",
		"-write", "/o.webp", "-format", reportFormat(false), "info:",
	}
	if !slices.Equal(got, want) {
		t.Errorf("args\n%q\nwant\n%q", got, want)
	}
}

// A frame that is already within the bounds is cropped too.
func TestEncodeArgsCropPrecedesTheFitsReturn(t *testing.T) {
	src := encodeSource{crop: region{1, 2, 3, 4}, upright: [2]int{9, 9}}
	got := frameOps(encodeFrame{input: "pam:/f", fits: true}, src, webpEncoding(100))
	want := []string{"-auto-orient", "+repage", "-print", `upright|%w|%h\n`, "-crop", "3x4+1+2", "+repage"}
	if !slices.Equal(got, want) {
		t.Errorf("ops %q, want %q", got, want)
	}
}

func TestEncodeArgsWithoutARegionDoNotCrop(t *testing.T) {
	for _, args := range [][]string{
		encodeArgs([]encodeFrame{{input: "pam:/f"}}, encodeSource{orientation: "RightTop"}, webpEncoding(100), encodeOutput{kind: outputStill, path: "/o.webp"}),
		fusedArgs(nil, encodeSource{}, []fusedTarget{{frame: encodeFrame{input: "a"}, enc: webpEncoding(100), path: "/a"}, {frame: encodeFrame{input: "a"}, enc: webpEncoding(50), path: "/b"}}),
	} {
		for _, a := range args {
			if a == "-crop" || a == "-print" || a == "+repage" {
				t.Errorf("args %q crop without a region", args)
			}
		}
	}
}

// Every target of a fused run crops, each from its own clone.
func TestFusedArgsCropEveryTarget(t *testing.T) {
	src := encodeSource{crop: region{0, 0, 8, 8}, upright: [2]int{16, 16}}
	args := fusedArgs(nil, src, []fusedTarget{
		{frame: encodeFrame{input: "a"}, enc: webpEncoding(100), path: "/a"},
		{frame: encodeFrame{input: "a"}, enc: webpEncoding(50), path: "/b"},
	})
	n := 0
	for _, a := range args {
		if a == "-crop" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("args %q crop %d times, want once per target", args, n)
	}
}

// quadrantJPEG writes a 640x480 JPEG, stored with red, lime, blue and yellow
// in its top-left, top-right, bottom-left and bottom-right quarters, tagged
// RightTop. Upright it is 480x640 with blue top-left and lime bottom-right.
func quadrantJPEG(t *testing.T, env Env) string {
	t.Helper()
	requireExiftool(t)
	path := filepath.Join(t.TempDir(), "quadrants.jpg")
	magick(t, env, "-size", "320x240", "xc:red", "xc:lime", "+append",
		"(", "-size", "320x240", "xc:blue", "xc:yellow", "+append", ")", "-append",
		"-quality", "100", path)
	exiftool(t, env, "-overwrite_original", "-Orientation#=6", path)
	return path
}

func nearColour(got [3]int, want [3]int) bool {
	for i := range got {
		if d := got[i] - want[i]; d < -40 || d > 40 {
			return false
		}
	}
	return true
}

// A crop placed before the orientation reads the stored frame, whose corner
// is another colour.
func TestRegionIsCutFromTheUprightPicture(t *testing.T) {
	env, _ := sourceEnv(t)
	path := quadrantJPEG(t, env)
	for _, c := range []struct {
		name string
		r    region
		want [3]int
	}{
		{"upright top-left", region{0, 0, 100, 100}, [3]int{0, 0, 255}},
		{"upright bottom-right", region{380, 540, 100, 100}, [3]int{0, 255, 0}},
		{"upright top-right", region{380, 0, 100, 100}, [3]int{255, 0, 0}},
		{"upright bottom-left", region{0, 540, 100, 100}, [3]int{255, 255, 0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			img, err := env.stillRegion(context.Background(), c.r, decoder{"magick", "jpeg"}, path, pngEncoding(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := pixelRGB(t, env, filepath.Join(dir, img.Name)); !nearColour(got, c.want) {
				t.Errorf("the crop is %v, want about %v", got, c.want)
			}
		})
	}
}

// The output is the region fitted to the bounds, and its pixels are those of
// the upright frame cut and resized by ImageMagick directly.
func TestRegionOutputEqualsAPlainMagickCrop(t *testing.T) {
	env, calls := sourceEnv(t)
	path := heicFixtureFile(t, "gradient-righttop")
	for _, c := range []struct {
		name   string
		r      region
		w, h   int
		resize string
	}{
		{"larger than the bounds", region{50, 100, 300, 200}, 100, 66, "100x66!"},
		{"within the bounds", region{420, 580, 60, 60}, 60, 60, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			before := len(calls())
			img, err := env.stillRegion(context.Background(), c.r, decoder{"magick", "heic"}, path, pngEncoding(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if w, h, _ := fitSize(c.r.width, c.r.height, 100, 10000); w != c.w || h != c.h {
				t.Fatalf("fitSize = %dx%d, the test expects %dx%d", w, h, c.w, c.h)
			}
			if img.Width != c.w || img.Height != c.h {
				t.Errorf("output is %dx%d, want fitSize of the region, %dx%d", img.Width, img.Height, c.w, c.h)
			}
			if c.resize == "" {
				for _, call := range calls()[before:] {
					if slices.Contains(call, "-write") && slices.Contains(call, "-resize") {
						t.Errorf("a region within the bounds was resized: %q", call)
					}
				}
			}
			ref := filepath.Join(t.TempDir(), "ref.png")
			args := []string{
				"heic:" + path + "[0]", "-auto-orient",
				"-crop", sizeAt(c.r), "+repage",
			}
			if c.resize != "" {
				args = append(args, "-filter", "Lanczos", "-resize", c.resize)
			}
			magick(t, env, append(args, "-strip", "-depth", "8", ref)...)
			if d := meanAbsoluteDifference(t, env, filepath.Join(dir, img.Name), ref); d != 0 {
				t.Errorf("the output differs from a plain crop by %g", d)
			}
		})
	}
}

func sizeAt(r region) string { return fmt.Sprintf("%dx%d+%d+%d", r.width, r.height, r.x, r.y) }

func jpegSizeDefines(calls [][]string) []string {
	var out []string
	for _, c := range calls {
		for _, a := range c {
			if strings.HasPrefix(a, "jpeg:size=") {
				out = append(out, a)
			}
		}
	}
	return out
}

// A region turns the JPEG's reduced decode off: a crop of a reduced frame
// would need fractional coordinates.
func TestRegionTurnsJPEGReductionOff(t *testing.T) {
	env, calls := sourceEnv(t)
	path := filepath.Join(t.TempDir(), "big.jpg")
	magick(t, env, "-size", "1600x1200", "gradient:red-blue", "-quality", "90", path)
	enc := pngEncoding()
	enc.format, enc.ext = JPEG, ".jpg"

	img, err := env.still(context.Background(), decoder{"magick", "jpeg"}, path, enc, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := jpegSizeDefines(calls()); len(got) == 0 {
		t.Fatalf("the control run, with no region, set no jpeg:size (%dx%d)", img.Width, img.Height)
	}

	env2, calls2 := sourceEnv(t)
	img, err = env2.stillRegion(context.Background(), region{100, 100, 800, 600}, decoder{"magick", "jpeg"}, path, enc, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := jpegSizeDefines(calls2()); len(got) != 0 {
		t.Errorf("a region still reduced the decode: %q", got)
	}
	if img.Width != 100 || img.Height != 75 {
		t.Errorf("output is %dx%d, want 100x75", img.Width, img.Height)
	}
}

// A still WebP is decoded at its stored size when a region is set, and its
// region is checked against the orientation its decode reveals.
func TestRegionStillWebP(t *testing.T) {
	requireWebPBuilders(t)
	requireExiftool(t)
	real := requireMagick(t)
	path := withOrientation(t, real, stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80"), 6)
	enc := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}.mustResolve(t)

	var r region
	env, calls := sourceEnv(t)
	art, err := openSource(t, env, path, Hints{}).encode(context.Background(), r, 0, []encoding{enc})
	if err != nil {
		t.Fatal(err)
	}
	art[0].Cleanup()
	if got := decodedAt(calls()); !slices.Equal(got, [][2]int{{800, 534}}) {
		t.Fatalf("the control decode is at %v, want 800x534", got)
	}

	// The picture is 1200x1800 upright.
	r = region{1100, 0, 100, 100}
	env, calls = sourceEnv(t)
	art, err = openSource(t, env, path, Hints{}).encode(context.Background(), r, 0, []encoding{enc})
	if err != nil {
		t.Fatal(err)
	}
	art[0].Cleanup()
	if got := decodedAt(calls()); !slices.Equal(got, [][2]int{{1800, 1200}}) {
		t.Errorf("a region decoded at %v, want the stored size 1800x1200", got)
	}
	if art[0].Width != 100 || art[0].Height != 100 {
		t.Errorf("output is %dx%d, want 100x100", art[0].Width, art[0].Height)
	}

	r = region{1101, 0, 100, 100}
	env, _ = sourceEnv(t)
	staged := t.TempDir()
	env.Stage = stagerIn(staged)
	if _, err := openSource(t, env, path, Hints{}).encode(context.Background(), r, 0, []encoding{enc}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("err = %v, want a refusal that the region is outside the 1200x1800 frame", err)
	}
	if entries, _ := os.ReadDir(staged); len(entries) != 0 {
		t.Errorf("the refused call left %d files", len(entries))
	}
}

func (e Encoding) mustResolve(t *testing.T) encoding {
	t.Helper()
	enc, err := e.resolve()
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// rewriteMagick returns env with the stdout of each magick encode run passed
// through fn.
func rewriteMagick(env Env, fn func(string) string) Env {
	inner := env.Run
	env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		res, err := inner(ctx, name, args...)
		if name == "magick" && slices.Contains(args, "-write") {
			res.Stdout = []byte(fn(string(res.Stdout)))
		}
		return res, err
	}
	return env
}

// ImageMagick does not refuse a crop outside the picture, so the size the run
// reports for the picture is held to the size the region was checked against.
func TestRegionRefusesAForgedUprightReport(t *testing.T) {
	real := requireMagick(t)
	path := heicFixtureFile(t, "gradient-righttop")
	for _, c := range []struct {
		name string
		fn   func(string) string
	}{
		{"another size", func(out string) string { return strings.Replace(out, "upright|480|640", "upright|640|480", 1) }},
		{"no line", func(out string) string { return strings.Replace(out, "upright|480|640\n", "", 1) }},
		{"a second line", func(out string) string { return "upright|480|640\n" + out }},
		{"an unreadable line", func(out string) string { return strings.Replace(out, "upright|480|640", "upright|x|640", 1) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := stagedEnv(rewriteMagick(real, c.fn), t.TempDir())
			dir := t.TempDir()
			if _, err := env.stillRegion(context.Background(), region{10, 10, 50, 50}, decoder{"magick", "heic"}, path, pngEncoding(), dir); err == nil {
				t.Fatal("the call succeeded")
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("the refused call left %d files in the output directory", len(entries))
			}
		})
	}
	t.Run("the report is accepted as printed", func(t *testing.T) {
		seen := false
		env := stagedEnv(rewriteMagick(real, func(out string) string {
			seen = seen || strings.Contains(out, "upright|480|640\n")
			return out
		}), t.TempDir())
		if _, err := env.stillRegion(context.Background(), region{10, 10, 50, 50}, decoder{"magick", "heic"}, path, pngEncoding(), t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Error("the run did not print the upright size")
		}
	})
}

// A run that crops nothing reports no upright size.
func TestEncodeRefusesAnUprightLineWhenNothingIsCropped(t *testing.T) {
	real := requireMagick(t)
	env := stagedEnv(rewriteMagick(real, func(out string) string { return "upright|480|640\n" + out }), t.TempDir())
	dir := t.TempDir()
	_, err := env.stillRegion(context.Background(), region{}, decoder{"magick", "heic"}, heicFixtureFile(t, "gradient-righttop"), pngEncoding(), dir)
	if err == nil {
		t.Fatal("the call succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refused call left %d files", len(entries))
	}
}

func TestSplitUpright(t *testing.T) {
	rest, sizes, err := splitUpright([]byte("upright|3|4\n5|6\nupright|7|8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, []byte("5|6\n")) || !slices.Equal(sizes, [][2]int{{3, 4}, {7, 8}}) {
		t.Errorf("rest %q, sizes %v", rest, sizes)
	}
	if _, _, err := splitUpright([]byte("upright|3\n")); err == nil {
		t.Error("a line without a height was accepted")
	}
}

// A GIF carries no orientation, so the hint rotates it; the region is in the
// rotated canvas.
func TestRegionOnAnAnimationIsInTheUprightCanvas(t *testing.T) {
	env, _ := sourceEnv(t)
	gif := filepath.Join(t.TempDir(), "two.gif")
	magick(t, env,
		"(", "-size", "30x40", "xc:red", "-size", "30x40", "xc:blue", "+append", "-set", "delay", "10", ")",
		"(", "-size", "30x40", "xc:blue", "-size", "30x40", "xc:red", "+append", "-set", "delay", "10", ")",
		"-loop", "0", gif)
	// Stored 60x40, red on the left; hinted RightTop it is 40x60 with red above.
	open := func(t *testing.T) *Source {
		return openSource(t, env, gif, Hints{Orientation: 6})
	}
	for _, c := range []struct {
		name string
		r    region
		want [3]int
		ok   bool
	}{
		{"top half", region{0, 0, 40, 30}, [3]int{255, 0, 0}, true},
		{"bottom half", region{0, 30, 40, 30}, [3]int{0, 0, 255}, true},
		{"x+width is the upright width", region{10, 0, 30, 10}, [3]int{255, 0, 0}, true},
		{"x+width is one more", region{11, 0, 30, 10}, [3]int{}, false},
		{"y+height is the upright height", region{0, 50, 10, 10}, [3]int{0, 0, 255}, true},
		{"y+height is one more", region{0, 51, 10, 10}, [3]int{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			images, err := open(t).encodeFiles(context.Background(), c.r, pngEncoding(), dir)
			if !c.ok {
				if err == nil || !strings.Contains(err.Error(), "outside") {
					t.Fatalf("err = %v, want a refusal that the region is outside the canvas", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(images) == 0 || images[0].Width != c.r.width || images[0].Height != c.r.height {
				t.Fatalf("images %+v, want the region's size %dx%d", images, c.r.width, c.r.height)
			}
			if got := pixelRGB(t, env, filepath.Join(dir, images[0].Name)); !nearColour(got, c.want) {
				t.Errorf("the first frame is %v, want about %v", got, c.want)
			}
		})
	}
}

// The caps stay on the canvas: a region does not let a call return more
// frames of it than the cap allows.
func TestRegionDoesNotRelaxTheReturnedPixelCap(t *testing.T) {
	env, _ := sourceEnv(t)
	gif := filepath.Join(t.TempDir(), "big.gif")
	// Five frames of 16 MP are 80 MP, over the 64 MP a call may return.
	frame := []string{"-size", "4000x4000", "xc:red", "-set", "delay", "10"}
	args := slices.Concat(frame, frame, frame, frame, frame, []string{"-loop", "0", gif})
	magick(t, env, args...)
	dir := t.TempDir()
	in := admitted{path: gif, dir: dir, decoder: decoder{"magick", "gif"}}
	all := func(n int) ([]int, error) { return []int{0, 1, 2, 3, 4}[:n], nil }
	_, err := env.encodeAnimationFrames(context.Background(), region{0, 0, 1, 1}, in, pngEncoding(), all)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refused call left %d files", len(entries))
	}
}

// A PNG whose orientation -ping cannot see is turned by its decode. The run
// reports the size it turned to, and a region inside that frame is cut from
// it, as for a still WebP whose orientation only its decode reveals.
func TestRegionOfAPNGWhoseOrientationThePingMisses(t *testing.T) {
	env := requireMagick(t)
	for _, file := range []string{"exif-after-idat.png", "ztxt-exif.png", "ztxt-after-idat.png"} {
		t.Run(file, func(t *testing.T) {
			path := writeFile(t, "content", mustRead(t, filepath.Join("testdata", "orient", file)))
			var uw, uh int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(magick(t, env, path+"[0]", "-auto-orient", "-format", "%w %h", "info:"))), "%d %d", &uw, &uh); err != nil {
				t.Fatal(err)
			}
			r := region{0, 0, 1, 1}
			if _, err := env.stillRegion(context.Background(), r, decoder{"magick", "png"}, path, pngEncoding(), t.TempDir()); err != nil {
				t.Errorf("region %+v of the %dx%d upright frame: %v", r, uw, uh, err)
			}
			if _, err := env.stillRegion(context.Background(), region{0, 0, uw, uh}, decoder{"magick", "png"}, path, pngEncoding(), t.TempDir()); err != nil {
				t.Errorf("the whole %dx%d upright frame: %v", uw, uh, err)
			}
			dir := t.TempDir()
			_, err := env.stillRegion(context.Background(), region{uw - 1, 0, 2, 2}, decoder{"magick", "png"}, path, pngEncoding(), dir)
			if err == nil || !strings.Contains(err.Error(), "outside") {
				t.Errorf("err = %v, want a refusal that the region is outside the %dx%d frame", err, uw, uh)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("the refused call left %d files", len(entries))
			}
		})
	}
}

// ImageMagick crops in the coordinates of the virtual canvas, which a PNG's
// oFFs chunk (or a TIFF's position) offsets from the pixels. The region is in
// the picture's own pixels, as the output without a region shows them.
func TestRegionIgnoresAPageOffset(t *testing.T) {
	env, _ := sourceEnv(t)
	path := filepath.Join(t.TempDir(), "offset.png")
	magick(t, env, "-size", "400x300", "gradient:red-blue", "-page", "1000x1000+30+20", path)
	r := region{100, 50, 200, 200}
	dir := t.TempDir()
	img, err := env.stillRegion(context.Background(), r, decoder{"magick", "png"}, path, pngEncoding(), dir)
	if err != nil {
		return // a refusal is safe; wrong pixels are not
	}
	ref := filepath.Join(t.TempDir(), "ref.png")
	magick(t, env, path, "+repage", "-crop", sizeAt(r), "+repage", "-filter", "Lanczos", "-resize", "100x100!", "-strip", "-depth", "8", ref)
	if d := meanAbsoluteDifference(t, env, filepath.Join(dir, img.Name), ref); d > 0.001 {
		t.Errorf("the %dx%d output differs from the region of the picture by %g", img.Width, img.Height, d)
	}
}
