package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// encodeInto makes one Encode of sel into a fresh directory on src, failing
// the test on error, and returns the images and the directory.
func encodeInto(t *testing.T, src *Source, sel Select, targets ...Encoding) ([]Image, string) {
	t.Helper()
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, sel, targets...)
	if err != nil {
		t.Fatal(err)
	}
	return images, dir
}

// fullIdentifies counts the identify runs among calls that read the whole file,
// not its headers.
func fullIdentifies(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if slices.Contains(c, "identify") && !slices.Contains(c, "-ping") {
			n++
		}
	}
	return n
}

// encodeFilter returns the -filter of the encode runs among calls, which are
// the runs that resize.
func encodeFilters(calls [][]string) []string {
	var filters []string
	for _, call := range calls {
		for i, a := range call {
			if a == "-filter" && i+1 < len(call) {
				filters = append(filters, call[i+1])
			}
		}
	}
	return filters
}

func TestOpenRoutesByTypeAndStructureNotFrameCount(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	env := requireMagick(t)
	p := requireProcessor(t)
	dir := t.TempDir()
	still := filepath.Join(dir, "still.webp")
	magick(t, env, "-size", "8x8", "xc:red", "-define", "webp:lossless=true", still)
	png := filepath.Join(dir, "x.png")
	magick(t, env, "-size", "8x8", "xc:red", png)
	for _, c := range []struct {
		name string
		path string
		want route
	}{
		{"one-frame GIF", writeFile(t, "a.gif", oneColourGIF(t, 8, 8, 1)), routeGIF},
		{"two-frame GIF", writeFile(t, "b.gif", oneColourGIF(t, 8, 8, 2)), routeGIF},
		{"one-frame animated WebP", oneFrameAnimatedWebP(t, still, nil, false), routeWebP},
		{"two-frame animated WebP", buildWebP(t, webpCase{codec: "lossless", frames: 2}), routeWebP},
		{"still WebP", still, routeStill},
		{"PNG", png, routeStill},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := openSourceOn(t, p, c.path, OpenOptions{})
			if src.hdr.route != c.want {
				t.Errorf("route %d, want %d", src.hdr.route, c.want)
			}
		})
	}

	t.Run("video", func(t *testing.T) {
		path := writeFile(t, "v.mp4", append(ftypBox("isom"), make([]byte, 64)...))
		_, _, err := p.Open(context.Background(), path, OpenOptions{})
		if err == nil || !strings.Contains(err.Error(), "probing video") {
			t.Fatalf("err = %v, want the video probe to refuse it", err)
		}
	})
}

// The filter follows what the source declares, whichever decoder reads it.
func TestFilterFollowsTheSourceFrame(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	real := requireMagick(t)
	dir := t.TempDir()
	mk := func(name string, args ...string) string {
		path := filepath.Join(dir, name)
		magick(t, real, append(append([]string{"-size", "40x30", "gradient:red-blue"}, args...), path)...)
		return path
	}
	alpha := []string{"-alpha", "set", "-channel", "A", "-evaluate", "set", "50%", "+channel"}
	cases := []struct {
		name string
		path string
		want string
	}{
		{"opaque GIF", writeFile(t, "g.gif", oneColourGIF(t, 40, 30, 1)), "Mitchell"},
		{"opaque animated WebP", buildWebP(t, webpCase{codec: "lossless", frames: 2}), "Lanczos"},
		{"alpha animated WebP", buildWebP(t, webpCase{codec: "lossless", alpha: true, frames: 2}), "Mitchell"},
		{"opaque still WebP", mk("o.webp"), "Lanczos"},
		{"alpha still WebP", mk("a.webp", alpha...), "Mitchell"},
		{"truecolour PNG", mk("t.png"), "Lanczos"},
		{"alpha PNG", mk("a.png", alpha...), "Mitchell"},
		{"palette PNG", mk("p.png", "-colors", "16", "-type", "palette"), "Mitchell"},
		{"JPEG", mk("j.jpg"), "Lanczos"},
		{"grayscale JPEG", mk("g.jpg", "-colorspace", "Gray"), "Lanczos"},
		{"alpha TIFF", mk("a.tif", alpha...), "Mitchell"},
		{"truecolour TIFF", mk("t.tif"), "Lanczos"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, rec := sourceProcessor(t)
			src := openSourceOn(t, p, c.path, OpenOptions{})
			encodeInto(t, src, Select{}, Encoding{Format: WebP, Quality: 80, MaxEdge: 32, MaxArea: 32 * 32})
			filters := encodeFilters(rec.calls)
			if len(filters) == 0 {
				t.Fatal("no encode run")
			}
			for _, f := range filters {
				if f != c.want {
					t.Errorf("filter %s, want %s", f, c.want)
				}
			}
		})
	}
}

// A still and the same image composited choose the same filter.
func TestStillAndCompositedEquivalentsChooseTheSameFilter(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	env := requireMagick(t)
	ctx := context.Background()
	filterOf := func(t *testing.T, path string) string {
		t.Helper()
		p, rec := sourceProcessor(t)
		src := openSourceOn(t, p, path, OpenOptions{})
		encodeInto(t, src, Select{}, Encoding{Format: WebP, Quality: 80, MaxEdge: 16, MaxArea: 256})
		filters := encodeFilters(rec.calls)
		if len(filters) != 1 {
			t.Fatalf("filters %v", filters)
		}
		return filters[0]
	}
	t.Run("opaque one-frame GIF read directly and composited", func(t *testing.T) {
		gif := writeFile(t, "g.gif", oneColourGIF(t, 40, 30, 1))
		direct, err := identifyHeaders(ctx, env, "gif:"+gif, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(filterFor(direct[0].soft)); got != filterOf(t, gif) {
			t.Errorf("header read says %s, composited %s", got, filterOf(t, gif))
		}
	})
	t.Run("opaque animated WebP and the same image as a still", func(t *testing.T) {
		still := filepath.Join(t.TempDir(), "still.webp")
		run(t, "cwebp", "-quiet", "-lossless", "-exact", writePNGFrame(t, 0), "-o", still)
		anim := oneFrameAnimatedWebP(t, still, nil, false)
		if a, s := filterOf(t, anim), filterOf(t, still); a != s || a != "Lanczos" {
			t.Errorf("animated %s, still %s, want Lanczos for both", a, s)
		}
	})
	t.Run("alpha animated WebP and the same image as a still", func(t *testing.T) {
		still := filepath.Join(t.TempDir(), "still.webp")
		run(t, "cwebp", "-quiet", "-lossless", "-exact", writePNGFrameAlpha(t, 0), "-o", still)
		anim := oneFrameAnimatedWebP(t, still, nil, true)
		if a, s := filterOf(t, anim), filterOf(t, still); a != s || a != "Mitchell" {
			t.Errorf("animated %s, still %s, want Mitchell for both", a, s)
		}
	})
}

func writePNGFrame(t *testing.T, k int) string {
	t.Helper()
	return writeWebPFramePNG(t, k, false)
}

func writePNGFrameAlpha(t *testing.T, k int) string {
	t.Helper()
	return writeWebPFramePNG(t, k, true)
}

func writeWebPFramePNG(t *testing.T, k int, alpha bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frame.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, webpFrameImage(k, webpCanvasW, webpCanvasH, alpha)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// What the -ping header read declares agrees with what a full decode of the
// same file declares, for every still format, after completeProfile.
func TestHeaderReadDeclaresWhatTheDecoderDoes(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	ctx := context.Background()
	dir := t.TempDir()
	alpha := []string{"-alpha", "set", "-channel", "A", "-evaluate", "set", "50%", "+channel"}
	gray := []string{"-colorspace", "gray"}
	pal := []string{"-colors", "16", "-type", "palette"}
	type fx struct {
		name string
		args []string
		want bool
	}
	var cases []fx
	for _, ext := range []string{"png", "tif", "bmp", "tga", "psd", "webp", "heic", "avif", "jxl", "jp2", "exr", "pcx", "sgi", "ico", "jpg"} {
		cases = append(cases, fx{"rgb." + ext, nil, ext == "ico"}, fx{"alpha." + ext, alpha, ext != "jpg"})
	}
	for _, ext := range []string{"png", "tif", "bmp", "tga", "psd", "pcx"} {
		cases = append(cases, fx{"pal." + ext, pal, true})
	}
	cases = append(cases, fx{"gray.jpg", gray, false}, fx{"gray.png", gray, false}, fx{"gray.tif", gray, false},
		fx{"gray.psd", gray, false}, fx{"rgb.gif", nil, true}, fx{"pal.gif", pal, true})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name)
			args := append([]string{"-size", "64x48", "gradient:red-blue"}, c.args...)
			if out, err := env.run(ctx, "magick", append(args, path)...); err != nil {
				t.Skipf("cannot write %s: %v %s", c.name, err, out.Stdout)
			}
			ref := path + "[0]"
			scenes, err := identifyHeaders(ctx, env, ref, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := completeProfile(ctx, env, ref, inputAt(t, path), scenes); err != nil {
				t.Fatal(err)
			}
			if fullReadFormats[scenes[0].format] {
				soft, err := fullSoft(ctx, env, path)
				if err != nil {
					t.Fatal(err)
				}
				scenes[0].soft = soft
			}
			full, err := identifyHeaders(ctx, env, ref, false)
			if err != nil {
				t.Fatal(err)
			}
			if scenes[0].soft != full[0].soft {
				t.Errorf("header read says soft=%t, a full decode soft=%t", scenes[0].soft, full[0].soft)
			}
			// The PNG container is an oracle for what the file declares.
			if strings.HasSuffix(c.name, ".png") {
				if got := pngDeclaresSoft(t, path); got != scenes[0].soft {
					t.Errorf("PNG container says soft=%t, header read %t", got, scenes[0].soft)
				}
			}
			if c.want != scenes[0].soft {
				t.Errorf("%s: expected soft=%t, the header read says %t", scenes[0].format, c.want, scenes[0].soft)
			}
		})
	}
}

// pngDeclaresSoft reads the IHDR colour type and the chunk list: palette,
// alpha, or a tRNS chunk make a PNG soft.
func pngDeclaresSoft(t *testing.T, path string) bool {
	t.Helper()
	b := mustReadFile(t, path)
	soft := false
	for pos := 8; pos+8 <= len(b); {
		n := int(b[pos])<<24 | int(b[pos+1])<<16 | int(b[pos+2])<<8 | int(b[pos+3])
		kind := string(b[pos+4 : pos+8])
		switch kind {
		case "IHDR":
			if ct := b[pos+8+9]; ct == 3 || ct == 4 || ct == 6 {
				soft = true
			}
		case "tRNS":
			soft = true
		}
		pos += 12 + n
	}
	return soft
}

// The compositors that read a GIF decode no frame after the last one selected.
func TestGIFCompositorStopsAfterTheLastSelectedFrame(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	g := oneColourGIF(t, 8, 8, 3)
	last := bytes.LastIndex(g, []byte{0x2C, 0, 0, 0, 0})
	// Frame 2 keeps its block structure, which the header read walks, but its
	// LZW data names codes that do not exist, so decoding it fails.
	for i := range int(g[last+11]) {
		g[last+12+i] = 0xFF
	}
	src := openSourceOn(t, p, writeFile(t, "x.gif", g), OpenOptions{})
	images, _ := encodeInto(t, src, Select{Frames: Indices(0, 1)}, Encoding{Format: PNG})
	if got := len(images); got < 1 {
		t.Fatalf("%d images, want those of the two selected frames", got)
	}
	dir := t.TempDir()
	if _, _, err := src.Encode(context.Background(), dir, Select{Frames: All()}, Encoding{Format: PNG}); err == nil {
		t.Fatal("a GIF whose last selected frame is corrupt was encoded")
	}
	assertEmptyDir(t, dir)
}

func repeatInt(v, n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = v
	}
	return s
}

// A still is decoded once for all its targets, and each target is what it
// would be alone.
func TestEncodeTwoTargetsOfAStillEqualsTwoEncodes(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	path := filepath.Join(t.TempDir(), "x.png")
	magick(t, real, "-size", "300x200", "gradient:red-blue", "-alpha", "set", "-channel", "A", "-evaluate", "set", "60%", "+channel", path)
	p, rec := sourceProcessor(t)
	small := Encoding{Format: WebP, Quality: 80, MaxEdge: 64, MaxArea: 64 * 64}
	large := Encoding{Format: WebP, Quality: 80, MaxEdge: 128, MaxArea: 128 * 128}
	both, bothDir := encodeInto(t, openSourceOn(t, p, path, OpenOptions{}), Select{}, small, large)
	decodes := 0
	for _, c := range rec.calls {
		if !slices.Contains(c, "identify") {
			decodes++
		}
	}
	if decodes != 1 {
		t.Errorf("%d magick runs that decode, want 1: %q", decodes, rec.calls)
	}
	for i, enc := range []Encoding{small, large} {
		alone, aloneDir := encodeInto(t, openSourceOn(t, p, path, OpenOptions{}), Select{}, enc)
		if a, b := mustReadFile(t, filepath.Join(bothDir, both[i].Name)), mustReadFile(t, filepath.Join(aloneDir, alone[0].Name)); !bytes.Equal(a, b) {
			t.Errorf("target %d: the clone differs from the single encode", i)
		}
		if both[i].Width != alone[0].Width || both[i].Height != alone[0].Height || len(both[i].Frames) != 1 {
			t.Errorf("target %d: %+v vs %+v", i, both[i], alone[0])
		}
	}
}

// Each target of an animation drops the frames that are the same at its own
// size, so its mapping is its own.
func TestEachTargetOfAnAnimationHasItsOwnMapping(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	// Frame 1 differs from frame 0 in one pixel by 12 levels: distinct at
	// full size, identical once shrunk eightfold.
	path := filepath.Join(t.TempDir(), "a.gif")
	magick(t, real, "-size", "128x128", "xc:#323232", "(", "+clone", "-fill", "#3e3e3e", "-draw", "point 3,3", ")",
		"(", "+clone", "-fill", "black", "-draw", "rectangle 0,0 127,127", ")", "-set", "delay", "5", "-loop", "0", path)
	src := openSourceOn(t, requireProcessor(t), path, OpenOptions{})
	if got := src.Info().Frames; got != 3 {
		t.Fatalf("fixture has %d frames", got)
	}
	out, _ := encodeInto(t, src, Select{Frames: All()},
		Encoding{Format: WebP, MaxEdge: 128, MaxArea: 128 * 128, Animate: true}, Encoding{Format: WebP, MaxEdge: 16, MaxArea: 256, Animate: true})
	if len(out[0].Frames) != 3 || len(out[1].Frames) != 2 || out[1].Frames[1].Index != 2 {
		t.Errorf("mappings %v and %v, want 3 frames and {0, 2}", out[0].Frames, out[1].Frames)
	}
}

func TestOpenOrientationReplacesTheFilesOwn(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "x.png")
	magick(t, real, "-size", "40x20", "xc:red", path)
	for _, c := range []struct {
		orientation int
		w, h        int
	}{{0, 40, 20}, {1, 40, 20}, {6, 20, 40}, {8, 20, 40}, {3, 40, 20}} {
		out, _ := encodeInto(t, openSourceOn(t, p, path, OpenOptions{Orientation: c.orientation}), Select{},
			Encoding{Format: PNG, MaxEdge: 64, MaxArea: 64 * 64})
		if out[0].Width != c.w || out[0].Height != c.h {
			t.Errorf("orientation %d: %dx%d, want %dx%d", c.orientation, out[0].Width, out[0].Height, c.w, c.h)
		}
	}
	if _, _, err := p.Open(context.Background(), path, OpenOptions{Orientation: 9}); err == nil {
		t.Error("orientation 9 was accepted")
	}
}

func TestEncodeNamesTheFramesOfAnAnimationOnePerFile(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "a.gif", oneColourGIF(t, 16, 16, 3))
	images, _ := encodeInto(t, openSourceOn(t, requireProcessor(t), path, OpenOptions{}), Select{Frames: All()}, Encoding{Format: PNG, MaxEdge: 16, MaxArea: 256})
	// Identical frames collapse to one.
	if len(images) != 1 || images[0].Frames[0].Index != 0 {
		t.Errorf("images = %+v", images)
	}
}

// ImageMagick sizes the reduced and unreduced decodes of a JPEG alike, for
// every target and a sweep of source sizes that includes near-square ones.
func TestJPEGSizeReductionKeepsTheOutputSize(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	dir := t.TempDir()
	targets := []Encoding{
		{Format: WebP, Quality: 80, MaxEdge: 64, MaxArea: 64 * 64},
		{Format: WebP, Quality: 80, MaxEdge: 160, MaxArea: 160 * 160},
		{Format: WebP, Quality: 80, MaxEdge: 256, MaxArea: 256 * 256},
		{Format: WebP, Quality: 80, MaxEdge: 333, MaxArea: 30000},
	}
	var sizes [][2]int
	for _, side := range []int{600, 777, 1023, 1025, 1201, 1999, 2048} {
		sizes = append(sizes, [2]int{side, side}, [2]int{side + 1, side}, [2]int{side, side + 1}, [2]int{side + 37, side},
			[2]int{side, 2*side/3 + 1}, [2]int{side/3 + 1, side})
	}
	sizes = append(sizes, [2]int{4000, 3001}, [2]int{5712, 4284}, [2]int{8064, 6048}, [2]int{13632, 3058})
	// A large case decodes a JPEG of up to 48 megapixels in each of its
	// encodes, so those take turns.
	large := make(chan struct{}, 4)
	for _, orientation := range []int{0, 6} {
		for i, sz := range sizes {
			if orientation != 0 && i%4 != 0 {
				continue
			}
			name := fmt.Sprintf("%dx%d/o%d", sz[0], sz[1], orientation)
			isLarge := sz[0] >= 4000
			ts := targets
			if isLarge {
				ts = append(slices.Clone(targets),
					Encoding{Format: WebP, Quality: 80, MaxEdge: 1024, MaxArea: 1024 * 1024},
					Encoding{Format: WebP, Quality: 80, MaxEdge: 2048, MaxArea: 2048 * 2048})
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(dir, strings.ReplaceAll(name, "/", "_")+".jpg")
				magick(t, real, "-size", strconv.Itoa(sz[0])+"x"+strconv.Itoa(sz[1]), "gradient:red-blue", "-quality", "50", path)
				// encode returns the sizes of the outputs of one encode of
				// the file, each encode on its own Source.
				encode := func(reduce bool, targets ...Encoding) func() ([][2]int, error) {
					src := openSourceOn(t, requireProcessor(t), path, OpenOptions{Orientation: orientation})
					src.stills[0].jpeg = reduce
					return func() ([][2]int, error) {
						if isLarge {
							large <- struct{}{}
							defer func() { <-large }()
						}
						out, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, targets...)
						if err != nil {
							return nil, err
						}
						sizes := make([][2]int, len(out))
						for i, a := range out {
							sizes[i] = [2]int{a.Width, a.Height}
						}
						return sizes, nil
					}
				}
				// The encodes are independent, and run alongside one
				// another, so that the case's last encode is not queued
				// behind the other tests.
				var wg sync.WaitGroup
				var plain, hinted [][2]int
				var plainErr, hintedErr error
				alone := make([][][2]int, len(ts))
				aloneErr := make([]error, len(ts))
				run := func(f func()) {
					wg.Add(1)
					go func() { defer wg.Done(); f() }()
				}
				planPlain, planHinted := encode(false, ts...), encode(true, ts...)
				run(func() { plain, plainErr = planPlain() })
				run(func() { hinted, hintedErr = planHinted() })
				// Each target alone is the path of a ladder rung.
				for i, target := range ts {
					plan := encode(true, target)
					run(func() { alone[i], aloneErr[i] = plan() })
				}
				wg.Wait()
				if plainErr != nil || hintedErr != nil {
					t.Fatalf("all targets: full decode %v, reduced %v", plainErr, hintedErr)
				}
				for i := range ts {
					if hinted[i] != plain[i] {
						t.Errorf("target %d: reduced %dx%d, full decode %dx%d", i, hinted[i][0], hinted[i][1], plain[i][0], plain[i][1])
					}
					if aloneErr[i] != nil {
						t.Errorf("target %d alone: %v", i, aloneErr[i])
					} else if alone[i][0] != plain[i] {
						t.Errorf("target %d alone: reduced %dx%d, full decode %dx%d", i, alone[i][0][0], alone[i][0][1], plain[i][0], plain[i][1])
					}
				}
			})
		}
	}
}

// One Source encodes the same targets again and again, as a caller that keeps
// it open does, and every encode writes the same bytes and size as the first
// encode of a fresh Source, which is made for each variant. For a JPEG the
// variants are reduced for all targets, reduced for each alone and not
// reduced; for a TIFF, which is read in full to learn whether it has alpha only
// when an encode resizes, the first target fits, so that the full read is first
// made by a later encode.
func TestRepeatedEncodesOfOneSourceAgree(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	jpegTargets := []Encoding{
		{Format: WebP, Quality: 80, MaxEdge: 64, MaxArea: 64 * 64},
		{Format: WebP, Quality: 80, MaxEdge: 256, MaxArea: 256 * 256},
		{Format: WebP, Quality: 80, MaxEdge: 333, MaxArea: 30000},
	}
	tiffTargets := []Encoding{
		{Format: WebP, Quality: 80, MaxEdge: 400, MaxArea: 400 * 400},
		{Format: WebP, Quality: 80, MaxEdge: 64, MaxArea: 64 * 64},
		{Format: WebP, Quality: 80, MaxEdge: 40, MaxArea: 40 * 40},
	}
	type fixture struct {
		name        string
		orientation int
		targets     []Encoding
		write       func(t *testing.T, dir string) string
	}
	var fixtures []fixture
	for _, sz := range [][2]int{{600, 401}, {1999, 1334}, {2048, 2048}} {
		for _, orientation := range []int{0, 6} {
			fixtures = append(fixtures, fixture{
				fmt.Sprintf("jpeg %dx%d/o%d", sz[0], sz[1], orientation), orientation, jpegTargets,
				func(t *testing.T, dir string) string {
					path := filepath.Join(dir, "x.jpg")
					magick(t, real, "-size", strconv.Itoa(sz[0])+"x"+strconv.Itoa(sz[1]), "gradient:red-blue", "-quality", "50", path)
					return path
				},
			})
		}
	}
	fixtures = append(fixtures, fixture{"tiff 16-bit with alpha and a profile", 0, tiffTargets, func(t *testing.T, dir string) string {
		path := filepath.Join(dir, "x.tif")
		magick(t, real, "-seed", "7", "-size", "160x120", "plasma:fractal", "-alpha", "set", "-channel", "A", "-evaluate", "set", "50%", "+channel",
			"-depth", "16", "-profile", writeSRGBProfile(t), "-profile", writeAdobeProfile(t, dir), path)
		return path
	}})
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			path := fx.write(t, t.TempDir())
			type result struct {
				bytes []byte
				size  [2]int
			}
			p := requireProcessor(t)
			open := func() *Source {
				return openSourceOn(t, p, path, OpenOptions{Orientation: fx.orientation})
			}
			encode := func(src *Source, reduce bool, ts ...Encoding) []result {
				t.Helper()
				src.stills[0].jpeg = reduce && strings.HasPrefix(fx.name, "jpeg")
				out, dir := encodeInto(t, src, Select{}, ts...)
				res := make([]result, len(out))
				for i, a := range out {
					res[i] = result{mustReadFile(t, filepath.Join(dir, a.Name)), [2]int{a.Width, a.Height}}
				}
				return res
			}
			variants := func(source func() *Source) map[string][]result {
				got := map[string][]result{}
				// The first target alone, which fits for a TIFF, comes first.
				for i, target := range fx.targets {
					got[fmt.Sprintf("alone %d", i)] = encode(source(), true, target)
				}
				got["all"] = encode(source(), true, fx.targets...)
				got["plain"] = encode(source(), false, fx.targets...)
				return got
			}
			want := variants(open)
			src := open()
			for round := range 3 {
				for variant, got := range variants(func() *Source { return src }) {
					for i := range got {
						if got[i].size != want[variant][i].size || !bytes.Equal(got[i].bytes, want[variant][i].bytes) {
							t.Errorf("round %d, %s, target %d: %v, %d bytes; a fresh Source gave %v, %d bytes", round, variant, i,
								got[i].size, len(got[i].bytes), want[variant][i].size, len(want[variant][i].bytes))
						}
					}
				}
			}
		})
	}
}

// The reduction is the smallest eighth that covers twice the largest target
// on both sides, and is not used when it would not reduce.
func TestJPEGSizeReductionChoice(t *testing.T) {
	t.Parallel()
	s := &frameSource{jpeg: true, stored: [2]int{6000, 4000}, upright: [2]int{6000, 4000}}
	enc := func(edge int) encoding { return encoding{maxEdge: edge, maxArea: edge * edge} }
	for _, c := range []struct {
		encs []encoding
		hint string
	}{
		{[]encoding{enc(256)}, "750x500"},              // N=1
		{[]encoding{enc(1024)}, "2250x1500"},           // N=3
		{[]encoding{enc(256), enc(1024)}, "2250x1500"}, // the largest decides
		{[]encoding{enc(2048)}, "4500x3000"},           // N=6
		{[]encoding{enc(3200)}, ""},                    // N=8: no reduction
		{[]encoding{enc(8000)}, ""},                    // no downscale at all
	} {
		ops, sizes := s.reduction(c.encs)
		got := ""
		if len(ops) == 2 {
			got = strings.TrimPrefix(ops[1], "jpeg:size=")
		}
		if got != c.hint {
			t.Errorf("targets %v: hint %q, want %q", c.encs, got, c.hint)
		}
		if (ops == nil) != (sizes == nil) {
			t.Errorf("targets %v: option and sizes disagree: %v %v", c.encs, ops, sizes)
		}
	}
	// Rotated by the file's orientation, the sides the targets bound are the
	// other way round from the stored ones.
	rotated := &frameSource{jpeg: true, stored: [2]int{6000, 4000}, upright: [2]int{4000, 6000}}
	if ops, _ := rotated.reduction([]encoding{enc(1024)}); len(ops) != 2 || ops[1] != "jpeg:size=2250x1500" {
		t.Errorf("rotated source: %v, want a 2250x1500 hint", ops)
	}
	s.jpeg = false
	if ops, _ := s.reduction([]encoding{enc(256)}); ops != nil {
		t.Errorf("a non-JPEG is reduced: %v", ops)
	}
}

// The fused encode of two targets writes the bytes the materialised encode
// does, with and without the JPEG reduction: the same decode, written to a
// 16-bit PAM and encoded from that.
func TestFusedAndMaterialisedEncodesAgreeOnTwoTargetsAndReducedJPEG(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	ctx := context.Background()
	dir := t.TempDir()
	jpg := filepath.Join(dir, "x.jpg")
	requireExiftool(t)
	magick(t, real, "-size", "3000x2000", "plasma:fractal", "-profile", writeSRGBProfile(t), "-profile", writeAdobeProfile(t, dir),
		"-quality", "92", jpg)
	exiftool(t, real, "-overwrite_original", "-Orientation#=6", jpg)
	scenes, err := identifyHeaders(ctx, real, "jpeg:"+jpg+"[0]", true)
	if err != nil || scenes[0].orientation != "RightTop" {
		t.Fatalf("fixture orientation %v, err %v", scenes, err)
	}

	p := requireProcessor(t)
	targets := []Encoding{
		{Format: WebP, Quality: 80, MaxEdge: 256, MaxArea: 256 * 256},
		{Format: WebP, Quality: 80, MaxEdge: 600, MaxArea: 600 * 600},
	}
	for _, reduce := range []bool{true, false} {
		t.Run(fmt.Sprintf("reduce=%t", reduce), func(t *testing.T) {
			src := openSourceOn(t, p, jpg, OpenOptions{})
			src.stills[0].jpeg = reduce
			fused, fusedDir := encodeInto(t, src, Select{}, targets...)
			readOps, sizes := src.stills[0].reduction([]encoding{mustResolve(t, targets[0]), mustResolve(t, targets[1])})
			if reduce != (readOps != nil) {
				t.Fatalf("reduction = %v with reduce=%t", readOps, reduce)
			}

			// Materialise: the same read, written to a 16-bit PAM beside its
			// profile, then each target encoded from that.
			ref := "jpeg:" + jpg + "[0]"
			mid := filepath.Join(dir, fmt.Sprintf("mid-%t.pam", reduce))
			sidecar := filepath.Join(dir, fmt.Sprintf("mid-%t.icc", reduce))
			decode := append(append(defaultResources().limitArgs(), readOps...), ref, "-depth", "16", "-write", "pam:"+mid, "+delete",
				ref, "-write", "icc:"+sidecar, "null:")
			magick(t, real, decode...)
			for i, target := range targets {
				enc := mustResolve(t, target)
				frame := encodeFrame{input: "pam:" + mid, convertTo: writeSRGBProfile(t), assign: sidecar, filter: filterLanczos}
				src := encodeSource{orientation: scenes[0].orientation}
				if sizes != nil {
					src.width, src.height = sizes[i][0], sizes[i][1]
				}
				out := filepath.Join(dir, fmt.Sprintf("mat-%t-%d.webp", reduce, i))
				if _, err := real.runEncode(ctx, encodeArgs([]encodeFrame{frame}, src, enc,
					encodeOutput{kind: outputStill, path: out}), encodeCheck{}); err != nil {
					t.Fatal(err)
				}
				if a, b := mustReadFile(t, filepath.Join(fusedDir, fused[i].Name)), mustReadFile(t, out); !bytes.Equal(a, b) {
					t.Errorf("target %d: fused %d bytes, materialised %d bytes, they differ", i, len(a), len(b))
				}
			}
		})
	}
}

func mustResolve(t *testing.T, e Encoding) encoding {
	t.Helper()
	enc, err := e.resolve()
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// oneFrameAnimatedWebP wraps the image of the still WebP at path as the
// only frame of an animation, with the VP8X animation flag set, which no WebP
// tool writes for one frame. alpha sets the VP8X alpha flag and icc, when not
// nil, is the file's profile.
func oneFrameAnimatedWebP(t *testing.T, path string, icc []byte, alpha bool) string {
	t.Helper()
	b := mustReadFile(t, path)
	var w, h int
	for _, c := range []string{"%w", "%h"} {
		out := strings.TrimSpace(string(magick(t, requireMagick(t), "identify", "-format", c, path)))
		n, err := strconv.Atoi(out)
		if err != nil {
			t.Fatal(err)
		}
		if c == "%w" {
			w = n
		} else {
			h = n
		}
	}
	chunk := func(kind string, data []byte) []byte {
		out := append([]byte(kind), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
		out = append(out, data...)
		if len(data)&1 == 1 {
			out = append(out, 0)
		}
		return out
	}
	u24 := func(n int) []byte { return []byte{byte(n), byte(n >> 8), byte(n >> 16)} }
	flags := byte(webpAnimFlag)
	if alpha {
		flags |= 0x10
	}
	if icc != nil {
		flags |= webpICCFlag
	}
	vp8x := append(append([]byte{flags, 0, 0, 0}, u24(w-1)...), u24(h-1)...)
	body := []byte("WEBP")
	body = append(body, chunk("VP8X", vp8x)...)
	if icc != nil {
		body = append(body, chunk("ICCP", icc)...)
	}
	body = append(body, chunk("ANIM", []byte{0, 0, 0, 0, 0, 0})...)
	frame := append(append(append(u24(0), u24(0)...), append(u24(w-1), u24(h-1)...)...), append(u24(100), 0)...)
	body = append(body, chunk("ANMF", append(frame, b[12:]...))...)
	out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	return writeFile(t, "anim.webp", append(out, body...))
}

// A one-frame WebP with the animation flag is decoded by webp-frames, which
// honours its ICC profile, as its still twin is by ImageMagick: both are
// converted, not labelled.
func TestOneFrameAnimatedWebPWithAProfileIsConverted(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	real := requireMagick(t)
	still := filepath.Join(t.TempDir(), "still.webp")
	run(t, "cwebp", "-quiet", "-lossless", "-exact", writePNGFrame(t, 0), "-o", still)
	adobe := mustAdobeProfile(t)
	p := requireProcessor(t)
	encode := func(path string) string {
		out, dir := encodeInto(t, openSourceOn(t, p, path, OpenOptions{}), Select{}, Encoding{Format: PNG, MaxEdge: 64, MaxArea: 64 * 64})
		return filepath.Join(dir, out[0].Name)
	}
	converted := encode(oneFrameAnimatedWebP(t, still, adobe, false))
	twin := encode(withWebPChunk(t, still, "icc", adobe))
	plain := encode(oneFrameAnimatedWebP(t, still, nil, false))
	if d := meanAbsoluteDifference(t, real, converted, twin); d > 0.002 {
		t.Errorf("the animation differs from its still twin by %.5f", d)
	}
	if d := meanAbsoluteDifference(t, real, converted, plain); d < 0.005 {
		t.Errorf("the profile changed the animation's pixels by only %.5f: it was labelled, not converted", d)
	}
}

// The Processor decodes a profiled GIF into its output directory while it
// encodes; when it returns, only the frames are left there.
func TestStreamedProfiledGIFLeavesOnlyFrames(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	var specs []gifFrameSpec
	for i := range 3 {
		specs = append(specs, gifFrameSpec{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA, shift: i})
	}
	data := withICC(t, buildGIF(t, 24, 20, paletteA, specs), mustAdobeProfile(t), 0, 1, 2)
	images, dir := framesInto(t, p, writeFile(t, "x.gif", data), Encoding{Format: PNG}, defaultFrames)
	var want []string
	for i := range images {
		want = append(want, frameName(i, ".png"))
	}
	if got := dirNames(t, dir); len(images) != 3 || !slices.Equal(got, want) {
		t.Errorf("%d images, directory holds %v, want %v", len(images), got, want)
	}
}

// The decode that settles whether a TIFF declares alpha is made only when an
// encode resizes, since only a resize uses the filter.
func TestFullReadForAStillRunsOnlyWhenAnEncodeResizes(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	path := filepath.Join(t.TempDir(), "a.tif")
	magick(t, real, "-size", "64x48", "gradient:red-blue", "-alpha", "set", "-channel", "A", "-evaluate", "set", "50%", "+channel", path)
	for _, c := range []struct {
		edge, reads int
		filter      string
	}{{400, 0, ""}, {40, 1, "Mitchell"}} {
		p, rec := sourceProcessor(t)
		src := openSourceOn(t, p, path, OpenOptions{})
		encodeInto(t, src, Select{}, Encoding{Format: WebP, MaxEdge: c.edge, MaxArea: c.edge * c.edge})
		if n := fullIdentifies(rec.calls); n != c.reads {
			t.Errorf("edge %d: %d full reads, want %d", c.edge, n, c.reads)
		}
		if f := encodeFilters(rec.calls); c.filter != "" && !slices.Equal(f, []string{c.filter}) {
			t.Errorf("edge %d: filters %v, want %s", c.edge, f, c.filter)
		}
	}
}

// The full read is made once for a Source, by the first encode that resizes,
// and its answer serves every later encode.
func TestFullReadIsMadeOnceAcrossEncodes(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	path := filepath.Join(t.TempDir(), "a.tif")
	magick(t, real, "-size", "64x48", "gradient:red-blue", "-alpha", "set", "-channel", "A", "-evaluate", "set", "50%", "+channel", path)
	encode := func(src *Source, edge int) []byte {
		out, dir := encodeInto(t, src, Select{}, Encoding{Format: WebP, Quality: 80, MaxEdge: edge, MaxArea: edge * edge})
		return mustReadFile(t, filepath.Join(dir, out[0].Name))
	}
	p, rec := sourceProcessor(t)
	src := openSourceOn(t, p, path, OpenOptions{})
	encode(src, 400)
	first := encode(src, 40)
	second := encode(src, 40)
	if reads := fullIdentifies(rec.calls); reads != 1 {
		t.Errorf("%d full reads across three encodes, want 1", reads)
	}
	if f := encodeFilters(rec.calls); !slices.Equal(f, []string{"Mitchell", "Mitchell"}) {
		t.Errorf("filters %v, want Mitchell for each of the two resizing encodes", f)
	}
	fresh := encode(openSourceOn(t, requireProcessor(t), path, OpenOptions{}), 40)
	if !bytes.Equal(first, fresh) || !bytes.Equal(second, fresh) {
		t.Error("an encode after an earlier one differs from a fresh Source's")
	}
}

// The production still path, one target or two, with and without a resize,
// writes the bytes of the materialised encode, for sources whose filter is
// Mitchell and Lanczos.
func TestSourceEncodeEqualsMaterialisedForPNGs(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	ctx := context.Background()
	alpha := []string{"-alpha", "set", "-channel", "A", "-evaluate", "set", "50%", "+channel"}
	for _, f := range []struct {
		name   string
		args   []string
		filter resizeFilter
	}{
		{"alpha", append([]string{"-depth", "8"}, alpha...), filterMitchell},
		{"palette", []string{"-colors", "16", "-type", "palette"}, filterMitchell},
		{"16-bit opaque", []string{"-depth", "16"}, filterLanczos},
	} {
		t.Run(f.name, func(t *testing.T) {
			dir := t.TempDir()
			png := filepath.Join(dir, "x.png")
			magick(t, real, append(append([]string{"-size", "64x48", "gradient:#0020ff-#ffb000"}, f.args...), png)...)
			mid := filepath.Join(dir, "mid.pam")
			magick(t, real, append(defaultResources().limitArgs(), "png:"+png+"[0]", "-depth", "16", "-write", "pam:"+mid, "null:")...)
			oracle := func(edge int) []byte {
				enc := mustResolve(t, Encoding{Format: WebP, Quality: 80, MaxEdge: edge, MaxArea: edge * edge})
				out := filepath.Join(dir, fmt.Sprintf("mat-%d.webp", edge))
				frame := encodeFrame{input: "pam:" + mid, filter: f.filter, fits: edge >= 64}
				if _, err := real.runEncode(ctx, encodeArgs([]encodeFrame{frame}, encodeSource{}, enc,
					encodeOutput{kind: outputStill, path: out}), encodeCheck{}); err != nil {
					t.Fatal(err)
				}
				return mustReadFile(t, out)
			}
			target := func(edge int) Encoding {
				return Encoding{Format: WebP, Quality: 80, MaxEdge: edge, MaxArea: edge * edge}
			}
			p := requireProcessor(t)
			for _, edges := range [][]int{{40}, {400}, {40, 400}, {40, 20}} {
				var targets []Encoding
				for _, e := range edges {
					targets = append(targets, target(e))
				}
				out, outDir := encodeInto(t, openSourceOn(t, p, png, OpenOptions{}), Select{}, targets...)
				for i, e := range edges {
					if got := mustReadFile(t, filepath.Join(outDir, out[i].Name)); !bytes.Equal(got, oracle(e)) {
						t.Errorf("targets %v: target %d differs from the materialised encode", edges, i)
					}
				}
			}
		})
	}
}
