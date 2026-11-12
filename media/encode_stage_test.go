package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func argsContainInOrder(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func webpEncoding(edge int) encoding {
	return encoding{format: WebP, ext: ".webp", quality: testQuality, maxEdge: edge, maxArea: edge * edge}
}

// The size is asked of ImageMagick as fx expressions, evaluated on the frame
// after it has been oriented, for the encoding's bounds, and never as a size
// computed from the header.
func TestEncodeArgsAskForTheSizeOfTheOrientedFrame(t *testing.T) {
	t.Parallel()
	enc := webpEncoding(100)
	args := encodeArgs([]encodeFrame{{input: "pam:/in"}}, encodeSource{}, enc, encodeOutput{kind: outputStill, path: "/out.webp"})
	w, h := fitExpr(enc.maxEdge, enc.maxArea)
	if !argsContainInOrder(args, "-auto-orient", "-filter", "Lanczos", "-resize", w+"x"+h+"!", "-strip") {
		t.Errorf("args %q do not resize to the fx size after -auto-orient", args)
	}
	if slices.Contains(args, "-thumbnail") {
		t.Errorf("args %q use -thumbnail", args)
	}
	t.Logf("fitExpr(100, 10000) = %s x %s", w, h)
}

// fxSizes evaluates fitExpr(maxEdge, maxArea) in ImageMagick for each of
// sizes, with the size substituted for w and h, and returns what it gives.
func fxSizes(t *testing.T, env toolEnv, maxEdge, maxArea int, sizes [][2]int) [][2]int {
	t.Helper()
	w, h := fitExpr(maxEdge, maxArea)
	w, h = strings.TrimSuffix(strings.TrimPrefix(w, "%[fx:"), "]"), strings.TrimSuffix(strings.TrimPrefix(h, "%[fx:"), "]")
	literal := func(expr string, sw, sh int) string {
		var b strings.Builder
		for _, r := range expr {
			switch r {
			case 'w':
				fmt.Fprint(&b, sw)
			case 'h':
				fmt.Fprint(&b, sh)
			default:
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	args := []string{"-size", "1x1", "xc:"}
	for _, sz := range sizes {
		args = append(args, "-print", "%[fx:"+literal(w, sz[0], sz[1])+"] %[fx:"+literal(h, sz[0], sz[1])+"]\n")
	}
	lines := strings.Split(strings.TrimSpace(string(magick(t, env, append(args, "null:")...))), "\n")
	if len(lines) != len(sizes) {
		t.Fatalf("%d results for %d sizes", len(lines), len(sizes))
	}
	out := make([][2]int, len(lines))
	for i, line := range lines {
		if _, err := fmt.Sscanf(line, "%d %d", &out[i][0], &out[i][1]); err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
	}
	return out
}

// fitSizeWithTheLoop states fitSize's area bound iteratively, as an oracle
// for the closed form.
func fitSizeWithTheLoop(w, h, maxEdge, maxArea int) (tw, th int, resize bool) {
	scale := math.Min(1, float64(maxEdge)/float64(max(w, h)))
	scale = math.Min(scale, math.Sqrt(float64(maxArea)/(float64(w)*float64(h))))
	if scale >= 1 {
		return w, h, false
	}
	const eps = 1e-9
	tw = max(1, int(math.Floor(float64(w)*scale+eps)))
	th = max(1, int(math.Floor(float64(h)*scale+eps)))
	for tw*th > maxArea && (tw > 1 || th > 1) {
		if tw >= th && tw > 1 {
			tw--
		} else {
			th--
		}
	}
	return tw, th, true
}

// sweepSizes returns sizes up to 16384 on a side: the corners and edges, the
// rows of the sizing table, shapes on the bounds of the areas under test, and
// random sizes spread over the magnitudes.
func sweepSizes(areas []int) [][2]int {
	sizes := [][2]int{
		{1, 1},
		{1, 16384},
		{16384, 1},
		{16384, 16384},
		{2, 3},
		{3, 2},
		{255, 255},
		{256, 1},
		{1, 256},
		{257, 1},
		{4000, 3001},
		{3001, 4000},
		{4000, 3000},
		{2048, 1537},
		{2048, 1536},
		{800, 601},
		{1537, 2048},
		{6000, 4000},
		{4000, 1},
		{4000, 2},
		{1, 4000},
		{16383, 16384},
		{16384, 16383},
		{8192, 8192},
		{5000, 5000},
	}
	for _, a := range areas {
		r := int(math.Sqrt(float64(a)))
		for _, d := range []int{-1, 0, 1} {
			if x := r + d; x >= 1 && x <= 16384 {
				sizes = append(sizes, [2]int{x, x})
			}
		}
		for _, h := range []int{1, 2, 3, 7, 16, 100, 1000} {
			for _, d := range []int{-1, 0, 1} {
				if w := a/h + d; w >= 1 && w <= 16384 {
					sizes = append(sizes, [2]int{w, h}, [2]int{h, w})
				}
			}
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 700 {
		sizes = append(sizes, [2]int{1 + int(math.Exp(rng.Float64()*math.Log(16384))) - 1, 1 + int(math.Exp(rng.Float64()*math.Log(16384))) - 1})
	}
	for range 300 {
		sizes = append(sizes, [2]int{1 + rng.IntN(16384), 1 + rng.IntN(16384)})
	}
	for i, s := range sizes {
		sizes[i] = [2]int{min(max(s[0], 1), 16384), min(max(s[1], 1), 16384)}
	}
	return sizes
}

// The fx expressions give fitSize's size, to the pixel, for every bound the
// encodings allow, and fitSize agrees with the iterative oracle.
func TestFitExprEqualsFitSize(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	edges := []int{16, 24, 100, 1024, 2048, 2576, 16384}
	areas := []int{256, 768, 100000, 1048576, 3_750_000, 4194304, 100_000_000}
	var combos [][2]int
	for _, e := range edges {
		combos = append(combos, [2]int{e, e * e}) // a rung: no bound beyond the edge
		for _, a := range areas {
			combos = append(combos, [2]int{e, a})
		}
	}
	sizes := sweepSizes(areas)
	var checked, resized, mismatches atomic.Int64
	t.Run("combos", func(t *testing.T) {
		for _, c := range combos {
			t.Run(fmt.Sprintf("edge %d area %d", c[0], c[1]), func(t *testing.T) {
				t.Parallel()
				got := fxSizes(t, env, c[0], c[1], sizes)
				for i, sz := range sizes {
					tw, th, resize := fitSize(sz[0], sz[1], c[0], c[1])
					if lw, lh, _ := fitSizeWithTheLoop(sz[0], sz[1], c[0], c[1]); lw != tw || lh != th {
						t.Errorf("fitSize(%v, edge %d, area %d) = %dx%d, the loop form gave %dx%d", sz, c[0], c[1], tw, th, lw, lh)
					}
					if got[i] != [2]int{tw, th} {
						if mismatches.Add(1) < 20 {
							t.Errorf("fx size of %v at edge %d, area %d is %v, fitSize gives %dx%d", sz, c[0], c[1], got[i], tw, th)
						}
					}
					if tw*th > c[1] && (tw > 1 && th > 1) || max(tw, th) > max(c[0], max(sz[0], sz[1])) {
						t.Errorf("fitSize(%v, edge %d, area %d) = %dx%d exceeds a bound", sz, c[0], c[1], tw, th)
					}
					checked.Add(1)
					if resize {
						resized.Add(1)
					}
				}
			})
		}
	})
	t.Logf("%d (size, bounds) pairs checked, %d of them resized, %d mismatches", checked.Load(), resized.Load(), mismatches.Load())
}

// The sizing rows the plan names, through ImageMagick: sides round down, so
// 4000x3001 at 2048 is 2048x1536.
func TestFitExprRoundsDown(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	for _, c := range []struct {
		w, h, edge, area int
		tw, th           int
	}{
		{4000, 3001, 2048, 2048 * 2048, 2048, 1536},
		{3001, 4000, 2048, 2048 * 2048, 1536, 2048},
		{800, 601, 512, 512 * 512, 512, 384},
		{40, 20, 640, 640 * 640, 40, 20},
		{4000, 3000, 16384, 3_750_000, 2236, 1677},
		{4000, 1, 100, 100 * 100, 100, 1},
		{4000, 1, 16384, 256, 256, 1},
	} {
		if got := fxSizes(t, env, c.edge, c.area, [][2]int{{c.w, c.h}})[0]; got != [2]int{c.tw, c.th} {
			t.Errorf("%dx%d at edge %d, area %d is %v, want %dx%d", c.w, c.h, c.edge, c.area, got, c.tw, c.th)
		}
	}
}

// An encode's size, read back from the run that wrote it, is the fitSize one.
func TestEncodeSizesWithFitSize(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "wide.png")
	magick(t, env, "-size", "800x601", "gradient:red-blue", path)
	out, images := encodeFile(t, p, stage(t, path), OpenOptions{}, Select{}, webpAtEdge(512))
	if w, h := images[0].Width, images[0].Height; w != 512 || h != 384 {
		t.Errorf("Encode reports %dx%d, want 512x384", w, h)
	}
	if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%w %h", out))); got != "512 384" {
		t.Errorf("the image is %s, want 512 384", got)
	}
}

// The order of the stages: orient, then convert, then flatten (JPEG only),
// then resize, and strip last, for every kind of output.
func TestEncodeArgsStages(t *testing.T) {
	t.Parallel()
	src := encodeSource{orientation: "RightTop"}
	frame := encodeFrame{input: "pam:/f", convertTo: "/srgb.icc", assign: "/own.icc", delayCS: 7}
	for _, c := range []struct {
		name string
		enc  encoding
		out  encodeOutput
	}{
		{"still", webpEncoding(100), encodeOutput{kind: outputStill, path: "/o.webp"}},
		{"animation", webpEncoding(100), encodeOutput{kind: outputAnimation, path: "/o.webp"}},
		{"files", webpEncoding(100), encodeOutput{kind: outputFiles, path: "/o-%d.webp"}},
		{"jpeg", encoding{format: JPEG, ext: ".jpg", quality: 90, maxEdge: 100, maxArea: 10000}, encodeOutput{kind: outputStill, path: "/o.jpg"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fitW, fitH := fitExpr(c.enc.maxEdge, c.enc.maxArea)
			args := encodeArgs([]encodeFrame{frame}, src, c.enc, c.out)
			want := []string{
				"-orient", "RightTop", "-auto-orient", "-orient", "TopLeft", "-profile", "/own.icc", "-profile", "/srgb.icc",
			}
			if c.enc.format == JPEG {
				want = append(want, "-background", "white", "-alpha", "remove", "-alpha", "off")
			}
			want = append(want, "-filter", "Lanczos", "-resize", fitW+"x"+fitH+"!", "-strip", "-depth", "8", "-quality", fmt.Sprint(c.enc.quality))
			if !argsContainInOrder(args, want...) {
				t.Errorf("args %q lack the stages %q", args, want)
			}
			if slices.Contains(args, "-limit") {
				t.Errorf("args %q carry limits, which toolEnv.runMagick adds", args)
			}
			if (c.enc.format == JPEG) != slices.Contains(args, "-alpha") {
				t.Errorf("args %q flatten by something other than the output format", args)
			}
			tail := args[len(args)-3:]
			if !slices.Equal(tail, []string{"-format", reportFormat(c.out.kind == outputFiles), "info:"}) {
				t.Errorf("args end with %q", tail)
			}
			if !slices.Contains(args, "-write") {
				t.Errorf("args %q: no -write", args)
			}
			if got := slices.Contains(args, "delay"); got != (c.out.kind == outputAnimation) {
				t.Errorf("args %q: delay present is %v", args, got)
			}
			if got := slices.Contains(args, "+adjoin"); got != (c.out.kind == outputFiles) {
				t.Errorf("args %q: +adjoin present is %v", args, got)
			}
		})
	}
}

// Several frames are each read inside their own parentheses, and each frame
// is converted by its own profile.
func TestEncodeArgsPerFrameProfiles(t *testing.T) {
	t.Parallel()
	frames := []encodeFrame{
		{input: "pam:/a", convertTo: "/srgb.icc", assign: "/a.icc"},
		{input: "pam:/b"},
		{input: "pam:/c", convertTo: "/srgb.icc", assign: "/c.icc"},
	}
	args := encodeArgs(frames, encodeSource{}, webpEncoding(100), encodeOutput{kind: outputFiles, path: "/o-%d.webp"})
	for _, want := range [][]string{
		{"(", "pam:/a", "-auto-orient", "-profile", "/a.icc", "-profile", "/srgb.icc", "-filter"},
		{"(", "pam:/b", "-auto-orient", "-filter"},
		{"(", "pam:/c", "-auto-orient", "-profile", "/c.icc", "-profile", "/srgb.icc", "-filter"},
	} {
		if !argsContainInOrder(args, want...) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if got := strings.Count(strings.Join(args, " "), "/srgb.icc"); got != 2 {
		t.Errorf("%d conversions, want 2: %q", got, args)
	}
}

// An unprofiled frame is never given a profile, however many are available.
func TestEncodeArgsDoNotAssignAProfile(t *testing.T) {
	t.Parallel()
	args := encodeArgs([]encodeFrame{{input: "pam:/a", assign: "/a.icc"}}, encodeSource{},
		webpEncoding(100), encodeOutput{kind: outputStill, path: "/o.webp"})
	if slices.Contains(args, "-profile") {
		t.Errorf("args %q have a profile for a frame that converts to none", args)
	}
}

func TestEncodeArgsAnimationDelays(t *testing.T) {
	t.Parallel()
	args := encodeArgs([]encodeFrame{{input: "pam:/a", delayCS: 0}, {input: "pam:/b", delayCS: 33}, {input: "pam:/c", delayCS: -1}},
		encodeSource{}, webpEncoding(100), encodeOutput{kind: outputAnimation, path: "/o.webp"})
	for _, want := range [][]string{
		{"(", "pam:/a", "-set", "delay", "0", "-auto-orient"}, {"(", "pam:/b", "-set", "delay", "33", "-auto-orient"}, {"(", "pam:/c", "-auto-orient"},
	} {
		if !argsContainInOrder(args, want...) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
}

// A frame that keeps its own delay keeps it after a frame that set one: a
// delay setting outlives the parentheses it was given in.
func TestAFrameKeepingItsDelayKeepsItAfterADelayedFrame(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	dir := t.TempDir()
	a, d7, out := filepath.Join(dir, "a.png"), filepath.Join(dir, "d7.gif"), filepath.Join(dir, "out.webp")
	magick(t, env, "-size", "8x8", "xc:red", a)
	magick(t, env, "-size", "8x8", "xc:blue", "-set", "delay", "7", d7)
	frames := []encodeFrame{
		{input: "png:" + a, delayCS: 33},
		{input: "gif:" + d7, prelude: []string{"-coalesce"}, delayCS: -1},
	}
	_, err := env.runEncode(context.Background(), encodeArgs(frames, encodeSource{}, webpEncoding(8),
		encodeOutput{kind: outputAnimation, path: out}), encodeCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%T ", out))); got != "33 7" {
		t.Errorf("delays %q, want \"33 7\"", got)
	}
}

func TestParseEncoded(t *testing.T) {
	t.Parallel()
	got, err := parseEncoded([]byte("abc|12|8\ndef|6|4\n"), true)
	want := []encodedFrame{{"abc", 12, 8}, {"def", 6, 4}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("parseEncoded = %v, %v, want %v", got, err, want)
	}
	got, err = parseEncoded([]byte("12|8\n6|4\n"), false)
	want = []encodedFrame{{"", 12, 8}, {"", 6, 4}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("unsigned parseEncoded = %v, %v, want %v", got, err, want)
	}
	for _, bad := range []string{"", "abc|1", "abc|x|2", "abc|1|2|3"} {
		if _, err := parseEncoded([]byte(bad), true); err == nil {
			t.Errorf("parseEncoded(%q, signed) did not fail", bad)
		}
	}
	for _, bad := range []string{"", "1", "x|2", "1|2|3"} {
		if _, err := parseEncoded([]byte(bad), false); err == nil {
			t.Errorf("parseEncoded(%q, unsigned) did not fail", bad)
		}
	}
}

// exifNames are ImageMagick's names of the EXIF orientations 1 to 8.
var exifNames = [...]string{"", "TopLeft", "TopRight", "BottomRight", "BottomLeft", "LeftTop", "RightTop", "RightBottom", "LeftBottom"}

// quadrants is an image of four flat colours, red and green above blue and
// yellow, so that each of the eight orientations gives a different picture.
func quadrants(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	colours := [2][2]color.NRGBA{
		{{255, 0, 0, 255}, {0, 255, 0, 255}},
		{{0, 0, 255, 255}, {255, 255, 0, 255}},
	}
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, colours[y*2/h][x*2/w])
		}
	}
	return img
}

// upright is src as a viewer shows it when its EXIF orientation is n, from the
// definition of the eight orientations and nothing of ImageMagick's.
func upright(src *image.NRGBA, n int) *image.NRGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	w, h := sw, sh
	if n >= 5 {
		w, h = sh, sw
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			var xs, ys int
			switch n {
			case 1:
				xs, ys = x, y
			case 2:
				xs, ys = sw-1-x, y
			case 3:
				xs, ys = sw-1-x, sh-1-y
			case 4:
				xs, ys = x, sh-1-y
			case 5:
				xs, ys = y, x
			case 6:
				xs, ys = y, sh-1-x
			case 7:
				xs, ys = sw-1-y, sh-1-x
			case 8:
				xs, ys = sw-1-y, x
			}
			out.SetNRGBA(x, y, src.NRGBAAt(xs, ys))
		}
	}
	return out
}

func decodePNG(t *testing.T, env toolEnv, path string) *image.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(magick(t, env, path+"[0]", "png:-")))
	if err != nil {
		t.Fatal(err)
	}
	out := image.NewNRGBA(img.Bounds())
	for y := range out.Bounds().Dy() {
		for x := range out.Bounds().Dx() {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}

func writePNG(t *testing.T, img image.Image, name string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, name, buf.Bytes())
}

// requireUpright fails unless got is want. A lossy output is compared at the
// middle of each quadrant's quarter, within tol per channel.
func requireUpright(t *testing.T, got, want *image.NRGBA, tol int) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("output is %v, want %v", got.Bounds().Size(), want.Bounds().Size())
	}
	w, h := want.Bounds().Dx(), want.Bounds().Dy()
	for _, fy := range []int{1, 3} {
		for _, fx := range []int{1, 3} {
			x, y := fx*w/4, fy*h/4
			g, e := got.NRGBAAt(x, y), want.NRGBAAt(x, y)
			for _, d := range []int{int(g.R) - int(e.R), int(g.G) - int(e.G), int(g.B) - int(e.B)} {
				if d > tol || d < -tol {
					t.Fatalf("pixel (%d,%d) is %v, want %v", x, y, g, e)
				}
			}
		}
	}
	if tol == 0 && !bytes.Equal(got.Pix, want.Pix) {
		t.Fatal("pixels differ from the upright picture")
	}
}

// withHeaderOrientation makes the header read of p report name as the
// orientation of every file, as one that carries the tag in a form
// ImageMagick reads from a GIF or an animated WebP would.
func withHeaderOrientation(p *Processor, name string) {
	inner := p.run
	p.run = func(ctx context.Context, in *os.File, tool string, args ...string) (runResult, error) {
		res, err := inner(ctx, in, tool, args...)
		if slices.Contains(args, "identify") && slices.Contains(args, "-ping") {
			res.Stdout = headerWithOrientation(res.Stdout, name)
		}
		return res, err
	}
}

// The eight orientations come out upright on every route an image takes into
// the encode: a still, a still whose orientation the caller gives, a GIF and
// an animated WebP composited outside ImageMagick, and a one-frame
// animation.
func TestOrientationIsAppliedTheSameWayOnEveryPath(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	src := quadrants(48, 32)
	srcPNG := writePNG(t, src, "quad.png")
	// Everything is shrunk to half, so that the size depends on the upright
	// size the route reports; the picture is compared where each quadrant is
	// flat.
	const edge = 24
	enc := Encoding{Format: PNG, MaxEdge: edge}

	for n := 1; n <= 8; n++ {
		name := exifNames[n]
		want := upright(quadrants(24, 16), n)

		t.Run(name+"/still read from the file", func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			tiff := filepath.Join(t.TempDir(), "q.tif")
			magick(t, real, srcPNG, "-orient", name, tiff)
			dir := t.TempDir()
			img, _, err := stillAs(context.Background(), p, tiff, "", enc, dir)
			if err != nil {
				t.Fatal(err)
			}
			if img.Width != want.Bounds().Dx() || img.Height != want.Bounds().Dy() {
				t.Errorf("reported %dx%d, want %v", img.Width, img.Height, want.Bounds().Size())
			}
			requireUpright(t, decodePNG(t, real, filepath.Join(dir, img.Name)), want, 3)
		})

		t.Run(name+"/still given the orientation", func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			content := writeFile(t, "content", mustRead(t, srcPNG))
			out, images := encodeFile(t, p, content, OpenOptions{Orientation: n}, Select{}, webpAtEdge(edge))
			if w, h := images[0].Width, images[0].Height; w != want.Bounds().Dx() || h != want.Bounds().Dy() {
				t.Errorf("reported %dx%d, want %v", w, h, want.Bounds().Size())
			}
			requireUpright(t, decodePNG(t, real, out), want, 40)
		})

		t.Run(name+"/GIF given the orientation", func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			gif := filepath.Join(t.TempDir(), "q.gif")
			other := writePNG(t, quadrantsShifted(48, 32), "other.png")
			magick(t, real, "-delay", "5", srcPNG, "-delay", "5", other, "-loop", "0", gif)
			out, images := encodeFile(t, p, stage(t, gif), OpenOptions{Orientation: n}, Select{Frames: Even(2)}, animatedWebPAt(edge))
			w, h, mapping := images[0].Width, images[0].Height, images[0].Frames
			if len(mapping) != 2 || w != want.Bounds().Dx() || h != want.Bounds().Dy() {
				t.Fatalf("reported %dx%d with %d frames, want %v and 2", w, h, len(mapping), want.Bounds().Size())
			}
			first := filepath.Join(t.TempDir(), "first.png")
			magick(t, real, out+"[0]", first)
			requireUpright(t, decodePNG(t, real, first), want, 40)
		})

		t.Run(name+"/one-frame GIF given the orientation", func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			gif := filepath.Join(t.TempDir(), "q.gif")
			magick(t, real, srcPNG, gif)
			out, images := encodeFile(t, p, stage(t, gif), OpenOptions{Orientation: n}, Select{Frames: Even(1)}, animatedWebPAt(edge))
			if w, h := images[0].Width, images[0].Height; w != want.Bounds().Dx() || h != want.Bounds().Dy() {
				t.Fatalf("reported %dx%d, want %v", w, h, want.Bounds().Size())
			}
			requireUpright(t, decodePNG(t, real, out), want, 40)
		})

		t.Run(name+"/GIF composited by the operation", func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			withHeaderOrientation(p, name)
			gif := filepath.Join(t.TempDir(), "q.gif")
			other := writePNG(t, quadrantsShifted(48, 32), "other.png")
			magick(t, real, "-delay", "5", srcPNG, "-delay", "5", other, "-loop", "0", gif)
			images, dir := framesInto(t, p, stage(t, gif), enc, 2)
			if len(images) != 2 {
				t.Fatalf("%d frames, want 2", len(images))
			}
			requireUpright(t, decodePNG(t, real, filepath.Join(dir, images[0].Name)), want, 3)
			// The PNG must not also say the picture is to be turned.
			if got := strings.TrimSpace(string(magick(t, real, "identify", "-format", "%[orientation]", filepath.Join(dir, images[0].Name)))); got != "TopLeft" && got != "Undefined" {
				t.Errorf("the upright PNG carries orientation %s", got)
			}
		})

		t.Run(name+"/animated WebP composited by the operation", func(t *testing.T) {
			t.Parallel()
			requireWebPBuilders(t)
			p := requireProcessor(t)
			withHeaderOrientation(p, name)
			path := buildWebP(t, webpCase{codec: "lossless", frames: 3})
			images, dir := framesInto(t, p, stage(t, writeFile(t, "copy", mustRead(t, path))), Encoding{Format: PNG}, 3)
			if len(images) != 3 {
				t.Fatalf("%d frames, want 3", len(images))
			}
			for k, img := range images {
				got := decodePNG(t, real, filepath.Join(dir, img.Name))
				if !bytes.Equal(got.Pix, upright(webpFrameImage(k, webpCanvasW, webpCanvasH, false), n).Pix) {
					t.Errorf("frame %d is not the upright frame", k)
				}
			}
			small, _ := framesInto(t, p, stage(t, path), Encoding{Format: PNG, MaxEdge: 20}, 3)
			fw, fh := webpCanvasW, webpCanvasH
			if n >= 5 {
				fw, fh = fh, fw
			}
			if w, h := fw*20/max(fw, fh), fh*20/max(fw, fh); small[0].Width != w || small[0].Height != h {
				t.Errorf("resized frames are %dx%d, want %dx%d", small[0].Width, small[0].Height, w, h)
			}
		})

		t.Run(name+"/WebP still with the orientation in an EXIF chunk, as one WebP", func(t *testing.T) {
			t.Parallel()
			requireWebPBuilders(t)
			p := requireProcessor(t)
			plain := filepath.Join(t.TempDir(), "q.webp")
			magick(t, real, srcPNG, "-define", "webp:lossless=true", plain)
			content := writeFile(t, "content", mustRead(t, withWebPChunk(t, plain, "exif", exifOrientation(uint16(n)))))
			out, images := encodeFile(t, p, content, OpenOptions{}, Select{}, webpAtEdge(edge))
			if w, h := images[0].Width, images[0].Height; w != want.Bounds().Dx() || h != want.Bounds().Dy() {
				t.Fatalf("reported %dx%d, want %v", w, h, want.Bounds().Size())
			}
			requireUpright(t, decodePNG(t, real, out), want, 40)
		})

		t.Run(name+"/WebP still with the orientation in an EXIF chunk, as PNG", func(t *testing.T) {
			t.Parallel()
			requireWebPBuilders(t)
			p := requireProcessor(t)
			plain := filepath.Join(t.TempDir(), "q.webp")
			magick(t, real, srcPNG, "-define", "webp:lossless=true", plain)
			path := withWebPChunk(t, plain, "exif", exifOrientation(uint16(n)))
			images, dir := framesInto(t, p, stage(t, path), enc, 1)
			requireUpright(t, decodePNG(t, real, filepath.Join(dir, images[0].Name)), want, 3)
		})

		// ImageMagick may or may not apply the EXIF orientation of an
		// animation; either way the size must be that of the picture.
		t.Run(name+"/one-frame animated WebP with an EXIF chunk", func(t *testing.T) {
			t.Parallel()
			requireWebPBuilders(t)
			p := requireProcessor(t)
			path := withWebPChunk(t, buildWebP(t, webpCase{codec: "lossless", frames: 1}), "exif", exifOrientation(uint16(n)))
			images, dir := framesInto(t, p, stage(t, path), Encoding{Format: PNG, MaxEdge: 20}, 1)
			got := decodePNG(t, real, filepath.Join(dir, images[0].Name))
			stored := webpFrameImage(0, webpCanvasW, webpCanvasH, false)
			for _, k := range []int{1, n} {
				if want := upright(stored, k); want.Bounds().Dx()*got.Bounds().Dy() == want.Bounds().Dy()*got.Bounds().Dx() {
					// Same aspect ratio: compare against the shrunk picture.
					ref := filepath.Join(t.TempDir(), "ref.png")
					src := writePNG(t, want, "src.png")
					magick(t, real, src, "-filter", "Lanczos", "-resize", fmt.Sprintf("%dx%d!", got.Bounds().Dx(), got.Bounds().Dy()), ref)
					if d := meanAbsoluteDifference(t, real, filepath.Join(dir, images[0].Name), ref); d < 0.02 {
						return
					}
				}
			}
			t.Errorf("a %dx%d frame is neither the stored nor the upright picture", got.Bounds().Dx(), got.Bounds().Dy())
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// quadrantsShifted is quadrants with a pixel changed, so that two frames
// differ.
func quadrantsShifted(w, h int) *image.NRGBA {
	img := quadrants(w, h)
	img.SetNRGBA(1, 1, color.NRGBA{10, 10, 10, 255})
	return img
}

// Every output of every route is free of metadata, including the converted
// sRGB profile the encode itself applied.
func TestEveryOutputIsStripped(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	adobe := writeAdobeProfile(t, t.TempDir())
	srgb := writeSRGBProfile(t)
	profiles := func(path string) string {
		return strings.TrimSpace(string(magick(t, real, "identify", "-format", "%[profiles]", path+"[0]")))
	}

	wide := filepath.Join(t.TempDir(), "wide.png")
	magick(t, real, "-size", "64x48", "gradient:#0020ff-#ffb000", "-profile", srgb, "-profile", adobe, wide)
	wideGIF := filepath.Join(t.TempDir(), "wide.gif")
	magick(t, real, "-size", "64x48", "gradient:#0020ff-#ffb000", "-profile", srgb, "-profile", adobe,
		"-delay", "5", "(", "+clone", "-negate", ")", "-loop", "0", wideGIF)
	if !strings.Contains(profiles(wide), "icc") {
		t.Fatalf("the fixture carries no profile: %q", profiles(wide))
	}

	pr := requireProcessor(t)
	rung := func(name, path string, animated bool, frames int) {
		t.Run("encode "+name, func(t *testing.T) {
			content := writeFile(t, "content", mustRead(t, path))
			enc := webpAtEdge(32)
			sel := Select{}
			if animated {
				enc.Animate, sel = true, Select{Frames: Even(frames)}
			}
			out, _ := encodeFile(t, pr, content, OpenOptions{}, sel, enc)
			if got := profiles(out); got != "" {
				t.Errorf("output carries %q", got)
			}
		})
	}
	rung("still", wide, false, 1)
	rung("composited animation", wideGIF, true, 2)

	t.Run("operations", func(t *testing.T) {
		p := requireProcessor(t)
		heic := heicFixtureFile(t, "adobe-64x64")
		for _, format := range []Format{JPEG, WebP, PNG} {
			img, dir := stillInto(t, p, heic, Encoding{Format: format, MaxEdge: 32})
			if got := profiles(filepath.Join(dir, img.Name)); got != "" {
				t.Errorf("%s still carries %q", format, got)
			}
		}
		images, dir := framesInto(t, p, stage(t, wideGIF), Encoding{Format: WebP}, 2)
		for _, img := range images {
			if got := profiles(filepath.Join(dir, img.Name)); got != "" {
				t.Errorf("animation frame carries %q", got)
			}
		}
	})
}

// A file whose orientation -ping cannot see, because it sits after the image
// data, in a text chunk or in an XMP packet, is oriented by what the decode
// finds, and sized for the picture that results, not for the header.
func TestOrientationTheHeaderDoesNotReport(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	ctx := context.Background()
	const edge = 32
	enc := Encoding{Format: PNG, MaxEdge: edge}
	for _, c := range []struct{ file, mime string }{
		{"exif-after-idat.png", "image/png"},
		{"ztxt-exif.png", "image/png"},
		{"ztxt-after-idat.png", "image/png"},
		{"xmp.webp", "image/webp"},
		{"exif.webp", "image/webp"},
	} {
		t.Run(c.file, func(t *testing.T) {
			path := writeFile(t, "content", mustRead(t, filepath.Join("testdata", "orient", c.file)))
			var storedW, storedH int
			var decoded string
			if _, err := fmt.Sscanf(strings.TrimSpace(string(magick(t, env, path+"[0]", "-format", "%[orientation] %w %h", "info:"))), "%s %d %d", &decoded, &storedW, &storedH); err != nil {
				t.Fatal(err)
			}
			var uw, uh int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(magick(t, env, path+"[0]", "-auto-orient", "-format", "%w %h", "info:"))), "%d %d", &uw, &uh); err != nil {
				t.Fatal(err)
			}
			if uw == storedW {
				t.Fatalf("the fixture is not rotated by its decode (%s, %dx%d)", decoded, storedW, storedH)
			}
			heads, err := identifyHeaders(ctx, env, path, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("decode: %s %dx%d, upright %dx%d; -ping says %q", decoded, storedW, storedH, uw, uh, heads[0].orientation)
			tw, th, _ := fitSize(uw, uh, edge, edge*edge)
			reference := filepath.Join(t.TempDir(), "reference.png")
			size := fmt.Sprintf("%dx%d!", tw, th)
			magick(t, env, path+"[0]", "-auto-orient", "-filter", "Lanczos", "-resize", size, "-strip", "-depth", "8", reference)

			t.Run("webp", func(t *testing.T) {
				out, images := encodeFile(t, requireProcessor(t), path, OpenOptions{}, Select{}, webpAtEdge(edge))
				if w, h := images[0].Width, images[0].Height; w != tw || h != th {
					t.Fatalf("reported %dx%d, want %dx%d", w, h, tw, th)
				}
				lossy := filepath.Join(t.TempDir(), "reference.webp")
				magick(t, env, path+"[0]", "-auto-orient", "-filter", "Lanczos", "-resize", size, "-strip", "-quality", strconv.Itoa(testQuality), lossy)
				if d := meanAbsoluteDifference(t, env, out, lossy); d > 0.004 {
					t.Errorf("output differs from magick -auto-orient and the same resize by %.6f", d)
				}
			})
			t.Run("operation", func(t *testing.T) {
				var img Image
				var dir string
				if strings.HasSuffix(c.file, ".png") {
					dir = t.TempDir()
					var err error
					if img, _, err = stillAs(ctx, requireProcessor(t), path, "", enc, dir); err != nil {
						t.Fatal(err)
					}
				} else {
					p := requireProcessor(t)
					images, d := framesInto(t, p, stage(t, writeFile(t, "copy", mustRead(t, path))), enc, 1)
					img, dir = images[0], d
				}
				if img.Width != tw || img.Height != th {
					t.Fatalf("reported %dx%d, want %dx%d", img.Width, img.Height, tw, th)
				}
				if d := meanAbsoluteDifference(t, env, filepath.Join(dir, img.Name), reference); d != 0 {
					t.Errorf("output differs from magick -auto-orient and the same resize by %.6f", d)
				}
			})
		})
	}
}

// A source of more than 8 bits per channel is written as an 8-bit PNG.
func TestOutputsAreEightBit(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	dir := t.TempDir()
	for _, depth := range []string{"16", "10"} {
		src := filepath.Join(dir, "src"+depth+".png")
		magick(t, env, "-size", "32x24", "gradient:#0020ff-#ffb000", "-depth", depth, src)
		if depth == "16" && strings.TrimSpace(string(magick(t, env, "identify", "-format", "%z", src))) != "16" {
			t.Fatal("the source is not 16-bit")
		}
		out := t.TempDir()
		img, _, err := stillAs(context.Background(), p, src, "", Encoding{Format: PNG}, out)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%z", filepath.Join(out, img.Name)))); got != "8" {
			t.Errorf("a %s-bit source gave a %s-bit PNG", depth, got)
		}
	}
	// The same through the Processor, from a 10-bit-capable HEIC fixture.
	heic := heicFixtureFile(t, "adobe-64x64")
	img, out := stillInto(t, p, heic, Encoding{Format: PNG})
	if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%z", filepath.Join(out, img.Name)))); got != "8" {
		t.Errorf("a HEIC still gave a %s-bit PNG", got)
	}
}

// transparentCorner writes a 48x32 PNG of opaque red whose top-left 8x8
// corner is fully transparent.
func transparentCorner(t *testing.T, name string) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 48, 32))
	for y := range 32 {
		for x := range 48 {
			if x >= 8 || y >= 8 {
				img.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, name, buf.Bytes())
}

func TestOpaqueEncodingHasNoAlphaAndWhiteWhereTransparent(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	path := transparentCorner(t, "a.png")
	probe := func(t *testing.T, file string) string {
		return string(magick(t, env, file, "-format", "%[opaque] %[hex:p{0,0}] %[hex:p{40,20}]", "info:"))
	}
	for _, format := range []Format{PNG, WebP} {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			src := openSourceOn(t, p, path, OpenOptions{})
			for _, opaque := range []bool{false, true} {
				dir := t.TempDir()
				images, _, err := src.Encode(context.Background(), dir, Select{}, Encoding{Format: format, Opaque: opaque})
				if err != nil {
					t.Fatal(err)
				}
				got := probe(t, filepath.Join(dir, images[0].Name))
				if opaque && !strings.HasPrefix(got, "True FFFFFF FF0") {
					t.Errorf("Opaque: %q, want an opaque image, a white corner and a red body", got)
				}
				if !opaque && !strings.HasPrefix(got, "False ") {
					t.Errorf("not Opaque: %q, want an alpha channel", got)
				}
			}
		})
	}
}
