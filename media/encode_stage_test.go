package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// The one sizing rule: fitSize in Go, emitted as an exact geometry, and no
// geometry when the frame already fits.
func TestEncodeArgsSizing(t *testing.T) {
	for _, c := range []struct {
		name             string
		w, h, edge, area int
		want             string // "" for no -resize
	}{
		// ImageMagick's -thumbnail gives 1537 here; fitSize floors.
		{"1536 not 1537", 4000, 3001, 2048, 2048 * 2048, "2048x1536!"},
		{"rounds down", 800, 601, 512, 512 * 512, "512x384!"},
		{"portrait", 3001, 4000, 2048, 2048 * 2048, "1536x2048!"},
		{"fits", 40, 20, 640, 640 * 640, ""},
		{"exactly the edge", 640, 480, 640, 640 * 640, ""},
		{"area bound", 4000, 3000, 16384, 3_750_000, "2236x1677!"},
		{"sliver", 4000, 1, 100, 100 * 100, "100x1!"},
	} {
		t.Run(c.name, func(t *testing.T) {
			enc := encoding{format: WebP, ext: ".webp", quality: testQuality, maxEdge: c.edge, maxArea: c.area}
			args := encodeArgs([]encodeFrame{{input: "pam:/in"}}, encodeSource{width: c.w, height: c.h}, enc,
				encodeOutput{kind: outputStill, path: "/out.webp"})
			i := slices.Index(args, "-resize")
			switch {
			case c.want == "" && i >= 0:
				t.Errorf("args %q resize a frame that fits", args)
			case c.want != "" && (i < 0 || args[i+1] != c.want):
				t.Errorf("args %q, want -resize %s", args, c.want)
			}
			if slices.Contains(args, "-thumbnail") {
				t.Errorf("args %q use -thumbnail", args)
			}
		})
	}
}

// A rung's size, read back from the run that wrote it, is the fitSize one.
func TestEncodeRungSizesWithFitSize(t *testing.T) {
	env := stagedEnv(requireMagick(t), t.TempDir())
	path := filepath.Join(t.TempDir(), "wide.png")
	magick(t, env, "-size", "800x601", "gradient:red-blue", path)
	content := renameToContent(t, path)
	frames, err := env.ProbeFrames(context.Background(), content, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	out, cleanup, w, h, _, err := env.EncodeRung(context.Background(), content, "image/png",
		false, Rung{CeilingPx: 512, MaxFrames: 1}, frames, "", "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if w != 512 || h != 384 {
		t.Errorf("EncodeRung reports %dx%d, want 512x384", w, h)
	}
	if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%w %h", out))); got != "512 384" {
		t.Errorf("the artifact is %s, want 512 384", got)
	}
}

// The order of the stages: orient, then convert, then flatten (JPEG only),
// then resize, and strip last, for every kind of output.
func TestEncodeArgsStages(t *testing.T) {
	src := encodeSource{orientation: "RightTop", width: 400, height: 300}
	frame := encodeFrame{input: "pam:/f", convertTo: "/srgb.icc", assign: "/own.icc", delayCS: 7}
	for _, c := range []struct {
		name string
		enc  encoding
		out  encodeOutput
	}{
		{"probe", webpEncoding(100), encodeOutput{kind: outputProbe}},
		{"still", webpEncoding(100), encodeOutput{kind: outputStill, path: "/o.webp"}},
		{"animation", webpEncoding(100), encodeOutput{kind: outputAnimation, path: "/o.webp"}},
		{"files", webpEncoding(100), encodeOutput{kind: outputFiles, path: "/o-%d.webp"}},
		{"jpeg", encoding{format: JPEG, ext: ".jpg", quality: 90, maxEdge: 100, maxArea: 10000}, encodeOutput{kind: outputStill, path: "/o.jpg"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := encodeArgs([]encodeFrame{frame}, src, c.enc, c.out)
			want := []string{
				"pam:/f", "-orient", "RightTop", "-auto-orient", "-profile", "/own.icc", "-profile", "/srgb.icc",
			}
			if c.enc.format == JPEG {
				want = append(want, "-background", "white", "-alpha", "remove", "-alpha", "off")
			}
			want = append(want, "-filter", "Lanczos", "-resize", "100x75!", "-strip", "-quality", fmt.Sprint(c.enc.quality))
			if !argsContainInOrder(args, want...) {
				t.Errorf("args %q lack the stages %q", args, want)
			}
			if !slices.Equal(args[:len(LimitArgs())], LimitArgs()) {
				t.Errorf("args %q do not start with the limits", args)
			}
			if (c.enc.format == JPEG) != slices.Contains(args, "-alpha") {
				t.Errorf("args %q flatten by something other than the output format", args)
			}
			tail := args[len(args)-3:]
			if !slices.Equal(tail, []string{"-format", encodeFormat, "info:"}) {
				t.Errorf("args end with %q", tail)
			}
			if got := slices.Contains(args, "-write"); got != (c.out.kind != outputProbe) {
				t.Errorf("args %q: -write present is %v", args, got)
			}
			if got := slices.Contains(args, "-delay"); got != (c.out.kind == outputAnimation) {
				t.Errorf("args %q: -delay present is %v", args, got)
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
	frames := []encodeFrame{
		{input: "pam:/a", convertTo: "/srgb.icc", assign: "/a.icc"},
		{input: "pam:/b"},
		{input: "pam:/c", convertTo: "/srgb.icc", assign: "/c.icc"},
	}
	args := encodeArgs(frames, encodeSource{width: 10, height: 10}, webpEncoding(100), encodeOutput{kind: outputFiles, path: "/o-%d.webp"})
	for _, want := range [][]string{
		{"(", "pam:/a", "-auto-orient", "-profile", "/a.icc", "-profile", "/srgb.icc", ")"},
		{"(", "pam:/b", "-auto-orient", ")"},
		{"(", "pam:/c", "-auto-orient", "-profile", "/c.icc", "-profile", "/srgb.icc", ")"},
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
	args := encodeArgs([]encodeFrame{{input: "pam:/a", assign: "/a.icc"}}, encodeSource{width: 10, height: 10},
		webpEncoding(100), encodeOutput{kind: outputStill, path: "/o.webp"})
	if slices.Contains(args, "-profile") {
		t.Errorf("args %q have a profile for a frame that converts to none", args)
	}
}

func TestEncodeArgsAnimationDelays(t *testing.T) {
	args := encodeArgs([]encodeFrame{{input: "pam:/a", delayCS: 0}, {input: "pam:/b", delayCS: 33}, {input: "pam:/c", delayCS: -1}},
		encodeSource{width: 10, height: 10}, webpEncoding(100), encodeOutput{kind: outputAnimation, path: "/o.webp"})
	for _, want := range [][]string{
		{"(", "-delay", "0", "pam:/a"}, {"(", "-delay", "33", "pam:/b"}, {"(", "pam:/c"},
	} {
		if !argsContainInOrder(args, want...) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
}

func TestParseEncoded(t *testing.T) {
	got, err := parseEncoded([]byte("abc|12|8\ndef|6|4\n"))
	want := []encodedFrame{{"abc", 12, 8}, {"def", 6, 4}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("parseEncoded = %v, %v, want %v", got, err, want)
	}
	for _, bad := range []string{"", "abc|1", "abc|x|2", "abc|1|2|3"} {
		if _, err := parseEncoded([]byte(bad)); err == nil {
			t.Errorf("parseEncoded(%q) did not fail", bad)
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

func decodePNG(t *testing.T, env Env, path string) *image.NRGBA {
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
	p.run = func(ctx context.Context, tool string, args ...string) (Result, error) {
		res, err := inner(ctx, tool, args...)
		if slices.Contains(args, "identify") && slices.Contains(args, "-ping") {
			res.Stdout = bytes.ReplaceAll(res.Stdout, []byte("|Undefined|END"), []byte("|"+name+"|END"))
		}
		return res, err
	}
}

// The eight orientations come out upright on every route an image takes into
// the encode: a still, a still whose orientation the caller gives, a GIF and
// an animated WebP composited outside ImageMagick, and a one-frame
// animation.
func TestOrientationIsAppliedTheSameWayOnEveryPath(t *testing.T) {
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
			tiff := filepath.Join(t.TempDir(), "q.tif")
			magick(t, real, srcPNG, "-orient", name, tiff)
			ec, _ := enc.resolve()
			dir := t.TempDir()
			img, err := real.still(context.Background(), decoder{"magick", "tiff"}, tiff, ec, dir)
			if err != nil {
				t.Fatal(err)
			}
			if img.Width != want.Bounds().Dx() || img.Height != want.Bounds().Dy() {
				t.Errorf("reported %dx%d, want %v", img.Width, img.Height, want.Bounds().Size())
			}
			requireUpright(t, decodePNG(t, real, filepath.Join(dir, img.Name)), want, 3)
		})

		t.Run(name+"/rung still given the orientation", func(t *testing.T) {
			env := stagedEnv(real, t.TempDir())
			content := writeFile(t, "content", mustRead(t, srcPNG))
			frames, err := env.ProbeFrames(context.Background(), content, "image/png")
			if err != nil {
				t.Fatal(err)
			}
			out, cleanup, w, h, _, err := env.EncodeRung(context.Background(), content, "image/png",
				false, Rung{CeilingPx: edge, MaxFrames: 1}, frames, "", name, testQuality)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if w != want.Bounds().Dx() || h != want.Bounds().Dy() {
				t.Errorf("reported %dx%d, want %v", w, h, want.Bounds().Size())
			}
			requireUpright(t, decodePNG(t, real, out), want, 40)
		})

		t.Run(name+"/rung GIF given the orientation", func(t *testing.T) {
			env := stagedEnv(real, t.TempDir())
			gif := filepath.Join(t.TempDir(), "q.gif")
			other := writePNG(t, quadrantsShifted(48, 32), "other.png")
			magick(t, real, "-delay", "5", srcPNG, "-delay", "5", other, "-loop", "0", gif)
			content := renameToContent(t, gif)
			frames, err := env.ProbeFrames(context.Background(), content, "image/gif")
			if err != nil {
				t.Fatal(err)
			}
			out, cleanup, w, h, mapping, err := env.EncodeRung(context.Background(), content, "image/gif",
				true, Rung{CeilingPx: edge, MaxFrames: 2}, frames, "", name, testQuality)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if len(mapping) != 2 || w != want.Bounds().Dx() || h != want.Bounds().Dy() {
				t.Fatalf("reported %dx%d with %d frames, want %v and 2", w, h, len(mapping), want.Bounds().Size())
			}
			first := filepath.Join(t.TempDir(), "first.png")
			magick(t, real, out+"[0]", first)
			requireUpright(t, decodePNG(t, real, first), want, 40)
		})

		t.Run(name+"/rung one-frame GIF given the orientation", func(t *testing.T) {
			env := stagedEnv(real, t.TempDir())
			gif := filepath.Join(t.TempDir(), "q.gif")
			magick(t, real, srcPNG, gif)
			content := renameToContent(t, gif)
			frames, err := env.ProbeFrames(context.Background(), content, "image/gif")
			if err != nil {
				t.Fatal(err)
			}
			out, cleanup, w, h, _, err := env.EncodeRung(context.Background(), content, "image/gif",
				true, Rung{CeilingPx: edge, MaxFrames: 1}, frames, "", name, testQuality)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if w != want.Bounds().Dx() || h != want.Bounds().Dy() {
				t.Fatalf("reported %dx%d, want %v", w, h, want.Bounds().Size())
			}
			requireUpright(t, decodePNG(t, real, out), want, 40)
		})

		t.Run(name+"/GIF composited by the operation", func(t *testing.T) {
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
		})

		t.Run(name+"/animated WebP composited by the operation", func(t *testing.T) {
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

		t.Run(name+"/one-frame WebP read whole", func(t *testing.T) {
			p := requireProcessor(t)
			withHeaderOrientation(p, name)
			path := filepath.Join(t.TempDir(), "q.webp")
			magick(t, real, srcPNG, "-define", "webp:lossless=true", path)
			images, dir := framesInto(t, p, stage(t, path), enc, 1)
			if len(images) != 1 {
				t.Fatalf("%d frames, want 1", len(images))
			}
			requireUpright(t, decodePNG(t, real, filepath.Join(dir, images[0].Name)), want, 3)
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

	env := stagedEnv(real, t.TempDir())
	rung := func(name, path, mime string, animated bool, frames int) {
		t.Run("rung "+name, func(t *testing.T) {
			content := writeFile(t, "content", mustRead(t, path))
			infos, err := env.ProbeFrames(context.Background(), content, mime)
			if err != nil {
				t.Fatal(err)
			}
			if !AnyProfiled(infos) {
				t.Fatal("the fixture's profile was not found")
			}
			out, cleanup, _, _, _, err := env.EncodeRung(context.Background(), content, mime, animated,
				Rung{CeilingPx: 32, MaxFrames: frames}, infos, "", "", testQuality)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if got := profiles(out); got != "" {
				t.Errorf("output carries %q", got)
			}
		})
	}
	rung("still", wide, "image/png", false, 1)
	rung("composited animation", wideGIF, "image/gif", true, 2)

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
