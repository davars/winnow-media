package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/davars/winnow-media/tool"
)

// stillRuns returns the webp-frames still-mode runs among calls.
func stillRuns(calls [][]string) [][]string {
	var runs [][]string
	for _, c := range calls {
		if len(c) > 1 && c[0] == webpFramesTool && c[1] == "still" {
			runs = append(runs, c)
		}
	}
	return runs
}

// stillWebP encodes the image at pngPath as a still WebP and returns its path.
func stillWebP(t *testing.T, pngPath string, codec ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "still.webp")
	args := append([]string{"-quiet"}, codec...)
	run(t, "cwebp", append(args, pngPath, "-o", out)...)
	return out
}

// gradientWebP is a w x h still WebP of a smooth gradient, cheap to make at
// any size.
func gradientWebP(t *testing.T, env Env, w, h int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "g.png")
	magick(t, env, "-size", fmt.Sprintf("%dx%d", w, h), "gradient:red-blue", p)
	return stillWebP(t, p, "-q", "50", "-m", "0")
}

// photoPNG is a w x h image with detail at every scale, as plasma has;
// alpha gives it an opacity that falls from the centre outwards.
func photoPNG(t *testing.T, env Env, w, h int, alpha bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "photo.png")
	size := fmt.Sprintf("%dx%d", w, h)
	args := []string{"-seed", "7", "-size", size, "plasma:fractal"}
	if alpha {
		args = append(args, "(", "-size", size, "radial-gradient:white-black", ")",
			"-alpha", "off", "-compose", "copy_opacity", "-composite")
	}
	magick(t, env, append(args, p)...)
	return p
}

func withOrientation(t *testing.T, env Env, path string, n int) string {
	t.Helper()
	requireExiftool(t)
	out := filepath.Join(t.TempDir(), "oriented.webp")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	exiftool(t, env, "-overwrite_original", "-Orientation#="+strconv.Itoa(n), out)
	return out
}

// pngDifference compares two PNG files of one size on premultiplied colour,
// where the colour of a transparent pixel does not count. It returns the mean
// absolute difference per channel in levels of 255, over all four channels,
// and the largest difference of the 99.9% of channel values that differ least.
func pngDifference(t *testing.T, a, b string) (mean float64, p999 int) {
	t.Helper()
	load := func(path string) image.Image {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		img, err := png.Decode(f)
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	ia, ib := load(a), load(b)
	if ia.Bounds() != ib.Bounds() {
		t.Fatalf("sizes %v and %v differ", ia.Bounds(), ib.Bounds())
	}
	var hist [256]int
	var sum, n float64
	for y := ia.Bounds().Min.Y; y < ia.Bounds().Max.Y; y++ {
		for x := ia.Bounds().Min.X; x < ia.Bounds().Max.X; x++ {
			ar, ag, ab, aa := ia.At(x, y).RGBA()
			br, bg, bb, ba := ib.At(x, y).RGBA()
			for _, d := range []int{
				abs(int(ar>>8) - int(br>>8)), abs(int(ag>>8) - int(bg>>8)),
				abs(int(ab>>8) - int(bb>>8)), abs(int(aa>>8) - int(ba>>8)),
			} {
				hist[d]++
				sum += float64(d)
				n++
			}
		}
	}
	seen := 0
	for d, c := range hist {
		if seen += c; float64(seen) >= 0.999*n {
			return sum / n, d
		}
	}
	return sum / n, 255
}

// decodedAt returns the size of each still-mode decode among calls.
func decodedAt(calls [][]string) [][2]int {
	var sizes [][2]int
	for _, c := range stillRuns(calls) {
		w, _ := strconv.Atoi(c[len(c)-2])
		h, _ := strconv.Atoi(c[len(c)-1])
		sizes = append(sizes, [2]int{w, h})
	}
	return sizes
}

// magickReference encodes the WebP at path at target the way ImageMagick's
// own decode does: one read of the whole image, oriented, converted and
// resized in one run, with the size ImageMagick computes. It returns the
// output and the runs made.
func magickReference(t *testing.T, path string, h Hints, target Encoding) (Artifact, [][]string) {
	t.Helper()
	env, calls := countingEnv(stagedEnv(requireMagick(t), t.TempDir()))
	ctx := context.Background()
	ref := "webp:" + path + "[0]"
	ping, err := identifyHeaders(ctx, env, ref, true)
	if err != nil {
		t.Fatal(err)
	}
	full, err := identifyHeaders(ctx, env, ref, false)
	if err != nil {
		t.Fatal(err)
	}
	frame := encodeFrame{input: ref, filter: filterFor(ping[0].soft)}
	if full[0].hasICC {
		frame.convertTo = writeSRGBProfile(t)
	}
	orientation, err := orientationName(h.Orientation)
	if err != nil {
		t.Fatal(err)
	}
	enc := mustResolve(t, target)
	out, cleanup, err := env.Stage(enc.ext)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	before := len(calls())
	res, err := env.runEncode(ctx, encodeArgs([]encodeFrame{frame}, encodeSource{orientation: orientation}, enc,
		encodeOutput{kind: outputStill, path: out}), encodeCheck{frames: 1})
	if err != nil {
		t.Fatal(err)
	}
	return Artifact{Path: out, Width: res[0].width, Height: res[0].height}, calls()[before:]
}

// reducedAndReference encodes path at target through the package and through
// magickReference and returns both outputs and the runs of each. The package
// decodes once, with webp-frames, at a reduced size exactly when wantReduced
// says.
func reducedAndReference(t *testing.T, path string, h Hints, target Encoding, wantReduced bool) (got, ref Artifact, gotCalls, refCalls [][]string) {
	t.Helper()
	env, calls := sourceEnv(t)
	src := openSource(t, env, path, h)
	out, err := src.Encode(context.Background(), 0, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(out[0].Cleanup)
	got, gotCalls = out[0], calls()
	ref, refCalls = magickReference(t, path, h, target)
	decoded := decodedAt(gotCalls)
	if len(decoded) != 1 {
		t.Fatalf("webp-frames decoded %d times, want 1", len(decoded))
	}
	if reduced := decoded[0] != src.stored; reduced != wantReduced {
		t.Errorf("decoded at %v of stored %v: reduced = %t, want %t", decoded[0], src.stored, reduced, wantReduced)
	}
	return
}

// A still WebP, reduced or not, is output at the size ImageMagick's full
// decode gives, for every target and a sweep of source sizes that includes
// near-square ones, upright and turned by a hint, and a source is decoded once
// for all its targets.
func TestScaledWebPDecodeKeepsTheOutputSize(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	targets := []Encoding{
		{Format: WebP, Quality: 80, MaxEdge: 64, MaxArea: 64 * 64},
		{Format: WebP, Quality: 80, MaxEdge: 160, MaxArea: 160 * 160},
		{Format: WebP, Quality: 80, MaxEdge: 256, MaxArea: 256 * 256},
		{Format: WebP, Quality: 80, MaxEdge: 333, MaxArea: 30000},
	}
	type sweep struct {
		size    [2]int
		targets []Encoding
	}
	edge := func(n int) Encoding { return Encoding{Format: WebP, Quality: 80, MaxEdge: n, MaxArea: n * n} }
	var sweeps []sweep
	for _, side := range []int{777, 1999} {
		for _, sz := range [][2]int{{side, side}, {side + 1, side}, {side/3 + 1, side}} {
			sweeps = append(sweeps, sweep{sz, targets})
		}
	}
	// Narrow enough that the group's decode stays at the stored size while
	// its first target alone reduces.
	sweeps = append(sweeps, sweep{[2]int{201, 600}, targets})
	// Large enough that a 1024 and a 2048 target both reduce.
	sweeps = append(sweeps, sweep{[2]int{5712, 4284}, []Encoding{edge(1024), edge(2048)}})
	reduced, stored := 0, 0
	for _, orientation := range []int{0, 6} {
		for i, sw := range sweeps {
			if orientation != 0 && (i%3 != 2 || sw.size[0] >= 4000) {
				continue
			}
			sz, ts := sw.size, sw.targets
			name := fmt.Sprintf("%dx%d/o%d", sz[0], sz[1], orientation)
			path := gradientWebP(t, real, sz[0], sz[1])
			env, calls := sourceEnv(t)
			src := openSource(t, env, path, Hints{Orientation: orientation})
			hinted, err := src.Encode(context.Background(), 0, ts...)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			decoded := decodedAt(calls())
			if len(decoded) != 1 {
				t.Fatalf("%s: webp-frames decoded %d times for %d targets, want 1", name, len(decoded), len(ts))
			}
			if want := src.webpDecodeSize(resolveAll(t, ts)); decoded[0] != want {
				t.Errorf("%s: decoded at %v, webpDecodeSize says %v", name, decoded[0], want)
			}
			if decoded[0] == src.stored {
				stored++
			} else {
				reduced += len(ts)
				// A reduced decode keeps twice every output's size, so the
				// resize that follows has detail to work from.
				dw, dh := decoded[0][0], decoded[0][1]
				if orientation == 6 {
					dw, dh = dh, dw
				}
				for i, h := range hinted {
					if dw < 2*h.Width || dh < 2*h.Height {
						t.Errorf("%s target %d: decoded %dx%d upright for a %dx%d output", name, i, dw, dh, h.Width, h.Height)
					}
				}
			}
			for i, target := range ts {
				plain, _ := magickReference(t, path, Hints{Orientation: orientation}, target)
				if hinted[i].Width != plain.Width || hinted[i].Height != plain.Height {
					t.Errorf("%s target %d: %dx%d, ImageMagick's full decode %dx%d", name, i,
						hinted[i].Width, hinted[i].Height, plain.Width, plain.Height)
				}
				// Alone, the target may decode at a different size from the
				// group's.
				if i == 0 {
					one, err := src.Encode(context.Background(), 0, target)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if one[0].Width != plain.Width || one[0].Height != plain.Height {
						t.Errorf("%s target %d alone: %dx%d, ImageMagick's full decode %dx%d", name, i,
							one[0].Width, one[0].Height, plain.Width, plain.Height)
					}
					one[0].Cleanup()
				}
			}
			for _, h := range hinted {
				h.Cleanup()
			}
		}
	}
	if reduced < 20 {
		t.Errorf("only %d encodes were decoded reduced: the sweep no longer exercises the route", reduced)
	}
	if stored == 0 {
		t.Error("no group was decoded at the stored size: the sweep no longer exercises that branch")
	}
}

// webpDecodeSize ignores the file's orientation, which is only read from the
// decoded file. That holds while fitSize is symmetric under swapping width and
// height, over the bounds an Encoding resolves to.
func TestFitSizeIsSymmetricUnderTurning(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		w, h := 1+r.Intn(16383), 1+r.Intn(16383)
		if i%2 == 0 {
			h = 1 + r.Intn(40)
		}
		edge, area := 16+r.Intn(16369), 256+r.Intn(100_000_000-255)
		tw, th, resize := fitSize(w, h, edge, area)
		rh, rw, rresize := fitSize(h, w, edge, area)
		if tw != rw || th != rh || resize != rresize {
			t.Fatalf("%dx%d at edge %d, area %d: %dx%d, turned %dx%d", w, h, edge, area, tw, th, rw, rh)
		}
	}
}

func resolveAll(t *testing.T, ts []Encoding) []encoding {
	t.Helper()
	encs := make([]encoding, len(ts))
	for i, e := range ts {
		encs[i] = mustResolve(t, e)
	}
	return encs
}

// The decoded size covers twice the largest target on both sides, in the
// stored aspect, and is used only when it is at most 7/8 of the stored size;
// otherwise the stored size is decoded.
func TestScaledWebPReductionChoice(t *testing.T) {
	enc := func(edge int) encoding { return encoding{maxEdge: edge, maxArea: edge * edge} }
	s := &Source{webp: true, reduce: true, stored: [2]int{6000, 4000}, upright: [2]int{6000, 4000}}
	for _, c := range []struct {
		encs []encoding
		want [2]int
	}{
		{[]encoding{enc(256)}, [2]int{512, 342}},
		{[]encoding{enc(1024)}, [2]int{2048, 1366}},
		{[]encoding{enc(256), enc(1024)}, [2]int{2048, 1366}}, // the largest decides
		{[]encoding{enc(2048)}, [2]int{4096, 2731}},
		{[]encoding{enc(2625)}, [2]int{5250, 3500}}, // exactly 7/8
		{[]encoding{enc(2626)}, [2]int{}},           // just over: the stored size
		{[]encoding{enc(8000)}, [2]int{}},           // no downscale at all
		{[]encoding{enc(256), enc(8000)}, [2]int{}}, // one target needs no resize
		{[]encoding{{maxEdge: 6000, maxArea: 600 * 400}}, [2]int{1200, 800}},
	} {
		got := s.webpDecodeSize(c.encs)
		want := c.want
		if want == [2]int{} {
			want = s.stored
		}
		if got != want {
			t.Errorf("targets %v: decode at %v, want %v", c.encs, got, want)
		}
	}
	// Turned by the file's orientation, the sides the targets bound are the
	// other way round from the stored ones.
	rotated := &Source{webp: true, reduce: true, stored: [2]int{6000, 4000}, upright: [2]int{4000, 6000}}
	if got := rotated.webpDecodeSize([]encoding{enc(1024)}); got != [2]int{2048, 1366} {
		t.Errorf("rotated source: decode at %v, want 2048x1366", got)
	}
	// Both sides cover the margin even where the targets round apart.
	for _, sz := range [][2]int{{1001, 1000}, {1000, 1001}, {5001, 3333}, {999, 1500}} {
		s := &Source{webp: true, reduce: true, stored: sz, upright: sz}
		for _, edge := range []int{100, 200, 333} {
			got := s.webpDecodeSize([]encoding{enc(edge)})
			if got == sz {
				t.Errorf("%v at %d: no reduction", sz, edge)
				continue
			}
			out, _, _ := fitSize(sz[0], sz[1], edge, edge*edge)
			if got[0] < 2*out || got[1] > sz[1] || got[0] > sz[0] {
				t.Errorf("%v at %d: decode at %v for output width %d", sz, edge, got, out)
			}
		}
	}
	s.reduce = false
	if got := s.webpDecodeSize([]encoding{enc(256)}); got != s.stored {
		t.Errorf("reduction disabled still gives %v", got)
	}
}

// The reduced decode of a still WebP is the image the full decode gives, in
// the same orientation, with the same alpha, profile and filter. The two
// resample differently (libwebp's area average to twice the target, then
// ImageMagick's filter, against ImageMagick's filter alone), so the pixels are
// close, not equal: the mean difference stays under 1.5 levels of 255 per
// channel and 99.9% of channel values within 14 levels. A wrongly turned,
// unconverted or unblended output differs by far more.
func TestScaledWebPMatchesTheFullDecode(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	adobe := mustAdobeProfile(t)
	opaque := photoPNG(t, real, 1200, 900, false)
	translucent := photoPNG(t, real, 1200, 900, true)
	target := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}

	for _, c := range []struct {
		name   string
		path   func(t *testing.T) string
		hints  Hints
		filter resizeFilter
		w, h   int
	}{
		{"lossy", func(t *testing.T) string { return stillWebP(t, opaque, "-q", "80") }, Hints{}, filterLanczos, 400, 300},
		{"lossy exif 6", func(t *testing.T) string {
			return withOrientation(t, real, stillWebP(t, opaque, "-q", "80"), 6)
		}, Hints{}, filterLanczos, 300, 400},
		{"lossy exif 3", func(t *testing.T) string {
			return withOrientation(t, real, stillWebP(t, opaque, "-q", "80"), 3)
		}, Hints{}, filterLanczos, 400, 300},
		{"lossy exif 8 over a hint of 1", func(t *testing.T) string {
			return withOrientation(t, real, stillWebP(t, opaque, "-q", "80"), 8)
		}, Hints{Orientation: 1}, filterLanczos, 400, 300},
		{
			"lossy hint 6", func(t *testing.T) string { return stillWebP(t, opaque, "-q", "80") },
			Hints{Orientation: 6},
			filterLanczos, 300, 400,
		},
		{
			"lossless with alpha", func(t *testing.T) string { return stillWebP(t, translucent, "-lossless", "-exact") },
			Hints{},
			filterMitchell, 400, 300,
		},
		{
			"lossy with alpha", func(t *testing.T) string { return stillWebP(t, translucent, "-q", "80", "-alpha_q", "100") },
			Hints{},
			filterMitchell, 400, 300,
		},
		{"lossy with alpha, exif 6", func(t *testing.T) string {
			return withOrientation(t, real, stillWebP(t, translucent, "-q", "80", "-alpha_q", "100"), 6)
		}, Hints{}, filterMitchell, 300, 400},
		{"lossy with a profile", func(t *testing.T) string {
			return withWebPChunk(t, stillWebP(t, opaque, "-q", "80"), "icc", adobe)
		}, Hints{}, filterLanczos, 400, 300},
		{"lossless with a profile, alpha and exif 6", func(t *testing.T) string {
			return withOrientation(t, real, withWebPChunk(t, stillWebP(t, translucent, "-lossless", "-exact"), "icc", adobe), 6)
		}, Hints{}, filterMitchell, 300, 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := c.path(t)
			reduced, full, reducedCalls, fullCalls := reducedAndReference(t, path, c.hints, target, true)
			if reduced.Width != c.w || reduced.Height != c.h || full.Width != c.w || full.Height != c.h {
				t.Errorf("reduced %dx%d, full %dx%d, want %dx%d", reduced.Width, reduced.Height, full.Width, full.Height, c.w, c.h)
			}
			rf, ff := encodeFilters(reducedCalls), encodeFilters(fullCalls)
			if !slices.Equal(rf, ff) || len(rf) != 1 || rf[0] != string(c.filter) {
				t.Errorf("filters: reduced %v, full %v, want %s", rf, ff, c.filter)
			}
			mean, p999 := pngDifference(t, reduced.Path, full.Path)
			t.Logf("mean %.3f, 99.9th percentile %d", mean, p999)
			if mean > 1.5 || p999 > 14 {
				t.Errorf("mean difference %.3f, 99.9th percentile %d levels", mean, p999)
			}
		})
	}
}

// A profile in a still WebP is converted to sRGB by the reduced route, not
// just recorded: the Adobe RGB file comes out as the full decode's
// conversion, and not as the same pixels without the profile.
func TestScaledWebPConvertsItsProfile(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	plain := stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80")
	profiled := withWebPChunk(t, plain, "icc", mustAdobeProfile(t))
	target := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}
	converted, full, _, _ := reducedAndReference(t, profiled, Hints{}, target, true)
	unconverted, _, _, _ := reducedAndReference(t, plain, Hints{}, target, true)
	if mean, _ := pngDifference(t, converted.Path, full.Path); mean > 1.5 {
		t.Errorf("the reduced conversion differs from the full decode's by %.3f", mean)
	}
	if mean, _ := pngDifference(t, converted.Path, unconverted.Path); mean < 3 {
		t.Errorf("the profile changed the pixels by only %.3f levels: it was labelled, not converted", mean)
	}
}

// An animation, including one of a single frame, is composited by webp-frames
// frame by frame whatever the targets, never decoded as a scaled still.
func TestAnimatedWebPIsNotDecodedAsAScaledStill(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	still := stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80")
	target := Encoding{Format: WebP, Quality: 80, MaxEdge: 20, MaxArea: 400}
	for name, path := range map[string]string{
		"animation":       buildWebP(t, webpCase{codec: "lossy", frames: 4}),
		"one-frame":       oneFrameAnimatedWebP(t, still, nil, false),
		"profiled frames": withWebPChunk(t, buildWebP(t, webpCase{codec: "lossy", frames: 3}), "icc", mustAdobeProfile(t)),
	} {
		t.Run(name, func(t *testing.T) {
			env, calls := sourceEnv(t)
			src := openSource(t, env, path, Hints{})
			if src.webp {
				t.Fatal("the source is marked as a scaled-decodable still")
			}
			out, err := src.Encode(context.Background(), 0, target)
			if err != nil {
				t.Fatal(err)
			}
			out[0].Cleanup()
			var tool int
			for _, c := range calls() {
				if c[0] == webpFramesTool {
					tool++
					if c[1] == "still" {
						t.Errorf("webp-frames ran in still mode: %q", c)
					}
				}
			}
			if tool != 1 {
				t.Errorf("webp-frames ran %d times, want 1", tool)
			}
		})
	}
}

// Both the Processor, whose compositor runs in the sandbox, and a plain Env
// decode a still WebP reduced, and give the size and pixels the full decode
// gives.
func TestScaledWebPThroughTheProcessorAndEnv(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	p := requireProcessor(t)
	path := withOrientation(t, real, stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80"), 6)
	enc := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}

	var composed []composeRequest
	inner := p.compose
	p.compose = func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
		composed = append(composed, req)
		return inner(ctx, req, onFrame)
	}
	dir := t.TempDir()
	images, _, err := p.AnimationFrames(context.Background(), path, enc, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(composed) != 1 || composed[0].scaled != [2]int{800, 534} {
		t.Errorf("the sandbox compositor ran %+v, want one still decode at 800x534", composed)
	}
	if len(images) != 1 || images[0].Width != 266 || images[0].Height != 400 {
		t.Errorf("Processor output %+v, want one 266x400 image", images)
	}

	env, calls := sourceEnv(t)
	out, err := openSource(t, env, path, Hints{}).Encode(context.Background(), 0, enc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(out[0].Cleanup)
	if runs := stillRuns(calls()); len(runs) != 1 || runs[0][len(runs[0])-2] != "800" || runs[0][len(runs[0])-1] != "534" {
		t.Errorf("Env ran %q, want one still decode at 800x534", runs)
	}
	if out[0].Width != 266 || out[0].Height != 400 {
		t.Errorf("Env output %dx%d, want 266x400", out[0].Width, out[0].Height)
	}
	if mean, p999 := pngDifference(t, filepath.Join(dir, images[0].Name), out[0].Path); mean > 0 || p999 > 0 {
		t.Errorf("the Processor and Env outputs differ: mean %.3f, 99.9th percentile %d", mean, p999)
	}
}

// An encode leaves no decoded frame in the scratch directory, and Close
// leaves nothing of the source, the profile included, in a directory whose
// owner removes it.
func TestScaledWebPLeavesNoDecodedFrame(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	path := withWebPChunk(t, stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80"), "icc", mustAdobeProfile(t))
	env, _ := sourceEnv(t)
	scratch := t.TempDir()
	env.Scratch = func() (string, func(), error) { return scratch, func() {}, nil }
	src, err := env.Open(context.Background(), path, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		out, err := src.Encode(context.Background(), 0, Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400})
		if err != nil {
			t.Fatal(err)
		}
		out[0].Cleanup()
		for _, e := range readDirNames(t, scratch) {
			if strings.HasSuffix(e, ".pam") {
				t.Errorf("%s left in the scratch directory after an encode", e)
			}
		}
	}
	src.Close()
	if left := readDirNames(t, scratch); len(left) != 0 {
		t.Errorf("Close left %v", left)
	}
}

func readDirNames(t *testing.T, dir string) []string {
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

// The tool's still mode agrees with the header it is told, never scales up,
// and refuses animations.
func TestWebPFramesStillMode(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	exe, err := tool.Resolve(webpFramesTool)
	if err != nil {
		t.Fatal(err)
	}
	opaque := stillWebP(t, photoPNG(t, real, 300, 200, false), "-q", "80")
	alpha := stillWebP(t, photoPNG(t, real, 300, 200, true), "-lossless", "-exact")
	tooled := func(path string, args ...string) (stdout string, pam string, err error) {
		dir := t.TempDir()
		cmd := exec.Command(exe, append([]string{"still", path, dir}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", stderr.String(), err
		}
		data, rerr := os.ReadFile(filepath.Join(dir, gifSampleName(0)))
		if rerr != nil {
			t.Fatal(rerr)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Errorf("left %s", e.Name())
			}
		}
		return string(out), string(data), nil
	}
	for _, c := range []struct {
		name     string
		path     string
		args     []string
		report   string
		header   string
		wantFail string
	}{
		{
			name: "scaled", path: opaque, args: []string{"300", "200", "100", "67"}, report: "0 0 0\n",
			header: "P7\nWIDTH 100\nHEIGHT 67\nDEPTH 3\nMAXVAL 255\nTUPLTYPE RGB\nENDHDR\n",
		},
		{
			name: "unscaled", path: opaque, args: []string{"300", "200", "300", "200"}, report: "0 0 0\n",
			header: "P7\nWIDTH 300\nHEIGHT 200\nDEPTH 3\nMAXVAL 255\nTUPLTYPE RGB\nENDHDR\n",
		},
		{
			name: "alpha", path: alpha, args: []string{"300", "200", "150", "100"}, report: "0 0 1\n",
			header: "P7\nWIDTH 150\nHEIGHT 100\nDEPTH 4\nMAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\n",
		},
		{
			name: "profile", path: withWebPChunk(t, opaque, "icc", mustAdobeProfile(t)), args: []string{"300", "200", "30", "20"},
			report: "0 1 0\n", header: "P7\nWIDTH 30\nHEIGHT 20\nDEPTH 3\nMAXVAL 255\nTUPLTYPE RGB\nENDHDR\n",
		},
		{name: "wrong size", path: opaque, args: []string{"301", "200", "100", "67"}, wantFail: "header read found"},
		{name: "upscale", path: opaque, args: []string{"300", "200", "301", "200"}, wantFail: "exceeds"},
		{name: "zero", path: opaque, args: []string{"300", "200", "0", "10"}, wantFail: "outside"},
		{
			name: "animated", path: buildWebP(t, webpCase{codec: "lossy", frames: 3}),
			args: []string{"40", "30", "20", "15"}, wantFail: "animated",
		},
		{name: "not a webp", path: writeFile(t, "x", []byte("hello")), args: []string{"5", "1", "5", "1"}, wantFail: "at least 12"},
	} {
		t.Run(c.name, func(t *testing.T) {
			stdout, pam, err := tooled(c.path, c.args...)
			if c.wantFail != "" {
				if err == nil || !strings.Contains(pam, c.wantFail) {
					t.Errorf("err %v, stderr %q, want a failure mentioning %q", err, pam, c.wantFail)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, pam)
			}
			if stdout != c.report {
				t.Errorf("reported %q, want %q", stdout, c.report)
			}
			if !strings.HasPrefix(pam, c.header) {
				t.Errorf("PAM header %q, want %q", pam[:min(len(pam), len(c.header))], c.header)
			}
			w, _ := strconv.Atoi(strings.Fields(c.header)[2])
			h, _ := strconv.Atoi(strings.Fields(c.header)[4])
			depth, _ := strconv.Atoi(strings.Fields(c.header)[6])
			if len(pam) != len(c.header)+w*h*depth {
				t.Errorf("PAM is %d bytes, want %d", len(pam), len(c.header)+w*h*depth)
			}
		})
	}
}

// webpWithEXIF returns a still WebP of w x h, VP8X flagged as having EXIF
// when flagged, with exif as the EXIF chunk when not nil.
func webpWithEXIF(t *testing.T, simple []byte, w, h int, flagged bool, exif []byte) string {
	t.Helper()
	var flags byte
	if flagged {
		flags = webpEXIFFlag
	}
	return webpWithFlags(t, simple, w, h, flags, nil, exif)
}

// webpWithFlags returns a still WebP of w x h whose VP8X has the given flags,
// with the chunks before the image, and the EXIF chunk after it when exif is
// not nil.
func webpWithFlags(t *testing.T, simple []byte, w, h int, flags byte, before []byte, exif []byte) string {
	t.Helper()
	u24 := func(n int) []byte { return []byte{byte(n), byte(n >> 8), byte(n >> 16)} }
	body := []byte("WEBP")
	body = append(body, riffChunk("VP8X", append(append([]byte{flags, 0, 0, 0}, u24(w-1)...), u24(h-1)...))...)
	body = append(body, before...)
	body = append(body, simple[12:]...)
	if exif != nil {
		body = append(body, riffChunk("EXIF", exif)...)
	}
	out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	return writeFile(t, "exif.webp", append(out, body...))
}

// ifdEntry is one 12-byte directory entry of a little-endian EXIF block.
func ifdEntry(tag, typ uint16, count, value uint32) []byte {
	le := binary.LittleEndian
	e := le.AppendUint16(nil, tag)
	e = le.AppendUint16(e, typ)
	e = le.AppendUint32(e, count)
	return le.AppendUint32(e, value)
}

// ifd0 is a little-endian EXIF block whose first directory holds the entries.
func ifd0(entries ...[]byte) []byte {
	le := binary.LittleEndian
	b := []byte("II*\x00\x08\x00\x00\x00")
	b = le.AppendUint16(b, uint16(len(entries)))
	for _, e := range entries {
		b = append(b, e...)
	}
	return append(b, 0, 0, 0, 0)
}

// tiffOrientation returns EXIF data whose first directory holds an orientation
// tag of the given type, count and value, followed by the entries after.
func tiffOrientation(order binary.AppendByteOrder, typ, count, value uint16, after ...[]byte) []byte {
	hdr := []byte("II*\x00")
	if order == binary.BigEndian {
		hdr = []byte("MM\x00*")
	}
	hdr = order.AppendUint32(hdr, 8)
	entries := 1 + len(after)
	b := order.AppendUint16(hdr, uint16(entries))
	b = order.AppendUint16(b, 0x0112)
	b = order.AppendUint16(b, typ)
	b = order.AppendUint32(b, uint32(count))
	b = order.AppendUint16(b, value)
	b = append(b, 0, 0)
	for _, e := range after {
		b = append(b, e...)
	}
	return append(b, 0, 0, 0, 0)
}

// The size a still WebP is decoded at does not depend on its orientation, which
// is learnt from the decoded file, and covers twice the output in either one:
// for edge- and area-bounded targets, and sources where the two orientations
// round differently.
func TestWebPDecodeSizeIsTheSameForEitherOrientation(t *testing.T) {
	var targets []encoding
	for _, e := range []int{100, 333, 640, 1024, 2048} {
		targets = append(targets, encoding{maxEdge: e, maxArea: e * e}, encoding{maxEdge: 100000, maxArea: e * e * 3 / 4},
			encoding{maxEdge: e, maxArea: e * e * 2 / 3})
	}
	for _, sz := range [][2]int{{6000, 4000}, {4000, 3001}, {3001, 4000}, {5712, 4284}, {1999, 2000}, {2001, 2000}, {8064, 6048}, {13632, 3058}, {1000, 997}} {
		for _, enc := range targets {
			straight := &Source{webp: true, reduce: true, stored: sz, upright: sz}
			turned := &Source{webp: true, reduce: true, stored: sz, upright: [2]int{sz[1], sz[0]}}
			a, b := straight.webpDecodeSize([]encoding{enc}), turned.webpDecodeSize([]encoding{enc})
			if a != b {
				t.Errorf("%v at %+v: %v upright, %v turned", sz, enc, a, b)
			}
			if a == sz {
				continue
			}
			// The decode covers twice the literal output of either orientation.
			for _, up := range [][2]int{sz, {sz[1], sz[0]}} {
				ow, oh, _ := fitSize(up[0], up[1], enc.maxEdge, enc.maxArea)
				if up != sz {
					ow, oh = oh, ow
				}
				if a[0] < decodeMargin*ow || a[1] < decodeMargin*oh {
					t.Errorf("%v at %+v: decoded %v does not cover twice the output %dx%d", sz, enc, a, ow, oh)
				}
			}
		}
	}
}

// VP8X flags.
const (
	webpEXIFFlag = 0x08
	webpXMPFlag  = 0x04
)

// toolOrientation runs webp-frames over the still WebP at path and returns the
// orientation stillMeta.orientation draws from what it read.
func toolOrientation(t *testing.T, path string) (n int, ok bool, err error) {
	t.Helper()
	exe, rerr := tool.Resolve(webpFramesTool)
	if rerr != nil {
		t.Fatal(rerr)
	}
	scenes, rerr := identifyHeaders(context.Background(), requireMagick(t), "webp:"+path, true)
	if rerr != nil {
		return 0, false, rerr
	}
	w, h := strconv.Itoa(scenes[0].width), strconv.Itoa(scenes[0].height)
	dir := t.TempDir()
	if out, rerr := exec.Command(exe, "still", path, dir, w, h, w, h).CombinedOutput(); rerr != nil {
		return 0, false, fmt.Errorf("%w: %s", rerr, out)
	}
	meta, rerr := readStillMeta(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if left := readDirNames(t, dir); slices.ContainsFunc(left, func(n string) bool { return strings.HasPrefix(n, ".exif-") || strings.HasPrefix(n, ".xmp-") }) {
		t.Errorf("readStillMeta left %v", left)
	}
	n, ok = meta.orientation()
	return n, ok, nil
}

// The orientation drawn from what webp-frames reads is the one ImageMagick
// applies, for every layout it answers for, and it declines the layouts it
// cannot be sure of.
func TestWebPStillOrientationAgreesWithImageMagick(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	const w, h = 300, 200
	simplePath := stillWebP(t, photoPNG(t, real, w, h, false), "-q", "80")
	simple := mustReadFile(t, simplePath)
	le := binary.LittleEndian
	subPointer := func() []byte { // an Exif directory pointer, to a directory of its own
		e := le.AppendUint16(nil, 0x8769)
		e = le.AppendUint16(e, 4)
		e = le.AppendUint32(e, 1)
		return le.AppendUint32(e, 0)
	}()
	noOrientation := func(entries ...[]byte) []byte { // first directory without the tag
		b := []byte("II*\x00\x08\x00\x00\x00")
		b = le.AppendUint16(b, uint16(len(entries)))
		for _, e := range entries {
			b = append(b, e...)
		}
		return append(b, 0, 0, 0, 0)
	}
	short := func(tag, v uint16) []byte {
		e := le.AppendUint16(nil, tag)
		e = le.AppendUint16(e, 3)
		e = le.AppendUint32(e, 1)
		e = le.AppendUint16(e, v)
		return append(e, 0, 0)
	}

	// webp-frames refuses an animation and what is no WebP.
	refused := map[string]bool{"animation": true, "not a webp": true}
	for _, c := range []struct {
		name    string
		path    string
		answers bool
		want    int // the orientation, when it answers
	}{
		{"simple file", simplePath, true, 0},
		{"extended, no exif", webpWithEXIF(t, simple, w, h, false, nil), true, 0},
		{"flag without chunk", webpWithEXIF(t, simple, w, h, true, nil), true, 0},
		{"chunk without flag", webpWithEXIF(t, simple, w, h, false, tiffOrientation(le, 3, 1, 6)), true, 0},
		{"little-endian 6", webpWithEXIF(t, simple, w, h, true, tiffOrientation(le, 3, 1, 6)), true, 6},
		{"big-endian 8", webpWithEXIF(t, simple, w, h, true, tiffOrientation(binary.BigEndian, 3, 1, 8)), true, 8},
		{"big-endian 3", webpWithEXIF(t, simple, w, h, true, tiffOrientation(binary.BigEndian, 3, 1, 3)), true, 3},
		{"Exif prefix", webpWithEXIF(t, simple, w, h, true, append([]byte("Exif\x00\x00"), tiffOrientation(le, 3, 1, 6)...)), true, 6},
		{"orientation 1", webpWithEXIF(t, simple, w, h, true, tiffOrientation(le, 3, 1, 1)), true, 1},
		{"first directory wins over a pointer", webpWithEXIF(t, simple, w, h, true, tiffOrientation(le, 3, 1, 3, subPointer)), true, 3},
		{"no tag, other tags", webpWithEXIF(t, simple, w, h, true, noOrientation(short(0x0128, 2))), true, 0},
		{"no tag beside a pointer", webpWithEXIF(t, simple, w, h, true, noOrientation(subPointer)), false, 0},
		{"orientation 0", webpWithEXIF(t, simple, w, h, true, tiffOrientation(le, 3, 1, 0)), false, 0},
		{"orientation 9", webpWithEXIF(t, simple, w, h, true, tiffOrientation(le, 3, 1, 9)), false, 0},
		{"orientation of type LONG", webpWithEXIF(t, simple, w, h, true, tiffOrientation(le, 4, 1, 6)), false, 0},
		{"orientation of count 2", webpWithEXIF(t, simple, w, h, true, tiffOrientation(le, 3, 2, 6)), false, 0},
		{"truncated", webpWithEXIF(t, simple, w, h, true, tiffOrientation(le, 3, 1, 6)[:12]), false, 0},
		{"not exif", webpWithEXIF(t, simple, w, h, true, []byte("hello world, not exif")), false, 0},
		{"empty chunk", webpWithEXIF(t, simple, w, h, true, []byte{}), true, 0}, // the tool takes an empty chunk for none, as ImageMagick does
		{"animation", buildWebP(t, webpCase{codec: "lossy", frames: 2}), false, 0},
		{"ICC flag without a chunk", webpWithFlags(t, simple, w, h, webpICCFlag|webpEXIFFlag, nil, tiffOrientation(le, 3, 1, 6)), true, 0},                           // the mux refuses the file, so ImageMagick sees no EXIF
		{"ICCP chunk without the flag", webpWithFlags(t, simple, w, h, webpEXIFFlag, riffChunk("ICCP", mustAdobeProfile(t)), tiffOrientation(le, 3, 1, 6)), true, 0}, // the mux refuses the file, so ImageMagick sees no EXIF
		{"ICC flag and chunk", webpWithFlags(t, simple, w, h, webpICCFlag|webpEXIFFlag, riffChunk("ICCP", mustAdobeProfile(t)), tiffOrientation(le, 3, 1, 6)), true, 6},
		{"XMP flag without a chunk", webpWithFlags(t, simple, w, h, webpXMPFlag|webpEXIFFlag, nil, tiffOrientation(le, 3, 1, 6)), true, 0}, // the mux refuses the file, so ImageMagick sees no EXIF
		{"XMP flag without a chunk or EXIF", webpWithFlags(t, simple, w, h, webpXMPFlag, nil, nil), true, 0},                               // the mux refuses the file, so ImageMagick sees no EXIF
		{"ICC flag without a chunk or EXIF", webpWithFlags(t, simple, w, h, webpICCFlag, nil, nil), true, 0},                               // the mux refuses the file, so ImageMagick sees no EXIF
		{"entry of type 13 before the orientation", webpWithEXIF(t, simple, w, h, true, ifd0(ifdEntry(0x010E, 13, 1, 0), ifdEntry(0x0112, 3, 1, 6))), false, 0},
		{"entry of type 0 before the orientation", webpWithEXIF(t, simple, w, h, true, ifd0(ifdEntry(0x010E, 0, 1, 0), ifdEntry(0x0112, 3, 1, 6))), false, 0},
		{"entry of count 2^31 before the orientation", webpWithEXIF(t, simple, w, h, true, ifd0(ifdEntry(0x010E, 2, 1<<31, 0), ifdEntry(0x0112, 3, 1, 6))), false, 0},
		{"Exif pointer before the orientation", webpWithEXIF(t, simple, w, h, true, ifd0(ifdEntry(0x010E, 4, 1, 0), ifdEntry(0x8769, 4, 1, 0), ifdEntry(0x0112, 3, 1, 6))), false, 0},
		{"Interop pointer before the orientation", webpWithEXIF(t, simple, w, h, true, ifd0(ifdEntry(0xA005, 4, 1, 0), ifdEntry(0x0112, 3, 1, 6))), false, 0},
		{"entries of every type 1 to 12 before the orientation", webpWithEXIF(t, simple, w, h, true, ifd0(
			ifdEntry(0x0100, 1, 1, 0), ifdEntry(0x0101, 2, 1, 0), ifdEntry(0x0102, 5, 1, 0), ifdEntry(0x0103, 12, 1, 0),
			ifdEntry(0x0104, 7, 1, 0), ifdEntry(0x0105, 9, 1, 0), ifdEntry(0x0112, 3, 1, 6))), true, 6},
		{"xmp chunk", writeFile(t, "xmp.webp", mustReadFile(t, filepath.Join("testdata", "orient", "xmp.webp"))), false, 0},
		{"not a webp", writeFile(t, "x.webp", []byte("RIFF....WAVEfmt ")), false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := toolOrientation(t, c.path)
			if refused[c.name] {
				if err == nil {
					t.Error("the file was not refused")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.answers || (ok && got != c.want) {
				t.Errorf("orientation = %d, %t; want %d, %t", got, ok, c.want, c.answers)
			}
			if !ok {
				return
			}
			out := strings.TrimSpace(string(magick(t, real, "identify", "-format", "%[orientation]", "webp:"+c.path)))
			n, err := orientationNumber(out)
			if err != nil {
				t.Fatal(err)
			}
			if n != got {
				t.Errorf("ImageMagick applies orientation %d (%s), the tool and the parse say %d", n, out, got)
			}
		})
	}
}

// The files of one directory that differ only in their EXIF orientation are
// read with the orientation each carries.
func TestScaledWebPOrientationsEachTurnTheOutput(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	base := stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80")
	simple := mustReadFile(t, base)
	target := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}
	var diffs []float64
	for n := 1; n <= 8; n++ {
		path := webpWithEXIF(t, simple, 1800, 1200, true, tiffOrientation(binary.LittleEndian, 3, 1, uint16(n)))
		reduced, full, _, _ := reducedAndReference(t, path, Hints{}, target, true)
		if reduced.Width != full.Width || reduced.Height != full.Height {
			t.Errorf("orientation %d: reduced %dx%d, full %dx%d", n, reduced.Width, reduced.Height, full.Width, full.Height)
			continue
		}
		mean, p999 := pngDifference(t, reduced.Path, full.Path)
		diffs = append(diffs, mean)
		if mean > 1.5 || p999 > 14 {
			t.Errorf("orientation %d: mean %.3f, 99.9th percentile %d", n, mean, p999)
		}
	}
	sort.Float64s(diffs)
	t.Logf("difference to the full decode over the eight orientations: %.3f to %.3f", diffs[0], math.Round(diffs[len(diffs)-1]*1000)/1000)
}

// fullReads returns the runs among calls that read an image whole to identify
// it, which is how a WebP's orientation is asked of ImageMagick.
func fullReads(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 1 && c[0] == "magick" && c[1] == "identify" && !slices.Contains(c, "-ping") {
			n++
		}
	}
	return n
}

// Every still WebP is decoded by webp-frames, once for all its targets, at
// the stored size when no reduction pays, and never by the fused
// ImageMagick decode.
func TestStillWebPIsDecodedByWebPFramesAtEveryTarget(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	small := stillWebP(t, photoPNG(t, real, 640, 480, false), "-q", "80")
	targets := func(edges ...int) []Encoding {
		var ts []Encoding
		for _, e := range edges {
			ts = append(ts, Encoding{Format: WebP, Quality: 80, MaxEdge: e, MaxArea: e * e})
		}
		return ts
	}
	for _, c := range []struct {
		name    string
		edges   []int
		decoded [2]int
		out     [][2]int
	}{
		{"small, both targets", []int{1024, 256}, [2]int{640, 480}, [][2]int{{640, 480}, {256, 192}}},
		{"one target that reduces", []int{256}, [2]int{512, 384}, [][2]int{{256, 192}}},
		{"below the 7/8 step", []int{300}, [2]int{640, 480}, [][2]int{{300, 225}}},
		{"nothing resizes", []int{1024}, [2]int{640, 480}, [][2]int{{640, 480}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, calls := sourceEnv(t)
			out, err := openSource(t, env, small, Hints{}).Encode(context.Background(), 0, targets(c.edges...)...)
			if err != nil {
				t.Fatal(err)
			}
			if got := decodedAt(calls()); len(got) != 1 || got[0] != c.decoded {
				t.Errorf("decodes %v, want one at %v", got, c.decoded)
			}
			for i, a := range out {
				if a.Width != c.out[i][0] || a.Height != c.out[i][1] {
					t.Errorf("target %d is %dx%d, want %dx%d", i, a.Width, a.Height, c.out[i][0], c.out[i][1])
				}
				a.Cleanup()
			}
			for _, call := range calls() {
				if call[0] == "magick" && slices.Contains(call, "webp:"+small+"[0]") && !slices.Contains(call, "identify") {
					t.Errorf("ImageMagick decoded the WebP: %q", call)
				}
			}
		})
	}
}

// A WebP whose orientation the chunk list cannot settle is still decoded by
// webp-frames; only its orientation is asked of ImageMagick, by one read of
// the whole image, and not at all when a hint replaces it.
func TestUncertainWebPOrientationIsAskedOfImageMagickAlone(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	le := binary.LittleEndian
	// The first directory only points to another, which holds the orientation.
	exif := []byte("II*\x00\x08\x00\x00\x00")
	exif = le.AppendUint16(exif, 1)
	exif = le.AppendUint16(exif, 0x8769)
	exif = le.AppendUint16(exif, 4)
	exif = le.AppendUint32(exif, 1)
	exif = le.AppendUint32(exif, 26)
	exif = le.AppendUint32(exif, 0)
	exif = le.AppendUint16(exif, 1)
	exif = le.AppendUint16(exif, 0x0112)
	exif = le.AppendUint16(exif, 3)
	exif = le.AppendUint32(exif, 1)
	exif = le.AppendUint16(exif, 6)
	exif = append(exif, 0, 0, 0, 0, 0, 0)
	simple := mustReadFile(t, stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80"))
	path := webpWithEXIF(t, simple, 1800, 1200, true, exif)
	if _, ok, err := toolOrientation(t, path); err != nil || ok {
		t.Fatalf("the fixture is not uncertain: %v", err)
	}
	target := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}

	got, ref, calls, _ := reducedAndReference(t, path, Hints{}, target, true)
	if got.Width != 266 || got.Height != 400 || ref.Width != 266 || ref.Height != 400 {
		t.Errorf("output %dx%d, reference %dx%d, want 266x400", got.Width, got.Height, ref.Width, ref.Height)
	}
	if n := fullReads(calls); n != 1 {
		t.Errorf("%d whole-image reads of the orientation, want 1", n)
	}
	if mean, p999 := pngDifference(t, got.Path, ref.Path); mean > 1.5 || p999 > 14 {
		t.Errorf("mean difference %.3f, 99.9th percentile %d", mean, p999)
	}

	_, _, calls, _ = reducedAndReference(t, path, Hints{Orientation: 3}, target, true)
	if n := fullReads(calls); n != 0 {
		t.Errorf("%d whole-image reads under a hint, want 0", n)
	}

	// A file the chunk list settles is not read whole.
	certain := webpWithEXIF(t, simple, 1800, 1200, true, tiffOrientation(le, 3, 1, 6))
	_, _, calls, _ = reducedAndReference(t, certain, Hints{}, target, true)
	if n := fullReads(calls); n != 0 {
		t.Errorf("%d whole-image reads of a settled file, want 0", n)
	}
}

// Every encode of a decoded WebP is held to its bound: the exact size when the
// decode was reduced, the shared bounds otherwise. A run that reports another
// size is an error and leaves no output.
func TestWebPStillEncodesAreChecked(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	path := stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80")
	for _, c := range []struct {
		name     string
		edge     int
		reported string
		want     string
	}{
		{"reduced, a pixel off", 400, "sig|401|266\n", "want exactly 400x266"},
		{"stored size, within the bounds", 1900, "sig|1800|1200\n", ""},
		{"stored size, far over", 1200, "sig|1800|1200\n", "above the bounds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, _ := sourceEnv(t)
			inner := env.Run
			env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
				res, err := inner(ctx, name, args...)
				if name == "magick" && slices.Contains(args, "-write") {
					res.Stdout = []byte(c.reported)
				}
				return res, err
			}
			var staged []string
			stage := env.Stage
			env.Stage = func(ext string) (string, func(), error) {
				p, cl, err := stage(ext)
				staged = append(staged, p)
				return p, cl, err
			}
			out, err := openSource(t, env, path, Hints{}).Encode(context.Background(), 0,
				Encoding{Format: WebP, Quality: 80, MaxEdge: c.edge, MaxArea: c.edge * c.edge})
			if c.want == "" {
				if err != nil {
					t.Fatalf("a size within the bounds was refused: %v", err)
				}
				out[0].Cleanup()
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			for _, p := range staged {
				if _, err := os.Stat(p); err == nil {
					t.Errorf("%s left after a refused encode", p)
				}
			}
		})
	}
}

// A WebP over the still caps at its stored size is refused at the header,
// with the caps on or off, before anything decodes it: not webp-frames, and not
// an ImageMagick read of the whole image for its profile or orientation. A
// reduction in libwebp does not relax the cap: lossless and alpha images are
// decoded at full size before they are scaled.
func TestWebPOverTheStoredSizeCapIsRefusedBeforeAnyDecode(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	big := filepath.Join(t.TempDir(), "big.png")
	magick(t, real, "-size", "7300x7200", "xc:gray", big)
	lossy := mustReadFile(t, stillWebP(t, big, "-q", "5", "-m", "0"))
	lossless := mustReadFile(t, stillWebP(t, big, "-lossless", "-z", "0"))
	// A first directory with a pointer and no orientation: uncertain.
	uncertain := ifd0(ifdEntry(0x8769, 4, 1, 26))
	for name, path := range map[string]string{
		"lossy":                 writeFile(t, "big.webp", lossy),
		"lossless":              writeFile(t, "big.webp", lossless),
		"uncertain orientation": webpWithEXIF(t, lossy, 7300, 7200, true, uncertain),
		"uncertain profile": webpWithFlags(t, lossy, 7300, 7200, webpICCFlag,
			append(riffChunk("ICCP", []byte("a")), riffChunk("ICCP", []byte("b"))...), nil),
		"lossless uncertain all": webpWithFlags(t, lossless, 7300, 7200, webpICCFlag|webpEXIFFlag,
			append(riffChunk("ICCP", []byte("a")), riffChunk("ICCP", []byte("b"))...), uncertain),
	} {
		for _, caps := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/caps=%t", name, caps), func(t *testing.T) {
				env, calls := sourceEnv(t)
				_, err := env.open(context.Background(), openRequest{path: path, caps: caps, hints: Hints{Still: true}})
				if !errors.Is(err, ErrTooLarge) {
					t.Fatalf("err = %v, want ErrTooLarge", err)
				}
				for _, c := range calls() {
					if c[0] == webpFramesTool || fullReads([][]string{c}) > 0 {
						t.Errorf("ran %q before refusing", c)
					}
				}
			})
		}
	}
}

// A file with an uncertain profile and an uncertain orientation is read whole
// once, for the orientation: the tool reports the profile.
func TestOneWholeImageReadForAnUncertainWebP(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	simple := mustReadFile(t, stillWebP(t, photoPNG(t, real, 900, 600, false), "-q", "80"))
	// An empty ICCP chunk leaves the profile for a whole-image read to settle.
	path := webpWithFlags(t, simple, 900, 600, webpICCFlag|webpEXIFFlag, riffChunk("ICCP", nil), ifd0(ifdEntry(0x8769, 4, 1, 26)))
	if _, ok := webpICC(path); ok {
		t.Fatal("the profile is not uncertain")
	}
	env, calls := sourceEnv(t)
	out, err := openSource(t, env, path, Hints{}).Encode(context.Background(), 0, Encoding{Format: PNG, MaxEdge: 200, MaxArea: 200 * 200})
	if err != nil {
		t.Fatal(err)
	}
	out[0].Cleanup()
	if n := fullReads(calls()); n != 1 {
		t.Errorf("%d whole-image reads, want 1", n)
	}
}

// A decode that fails leaves nothing in the scratch directory, the profile
// included: when the tool fails after writing it, and when it fails to decode.
func TestFailedWebPDecodeLeavesNoProfile(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	simple := mustReadFile(t, stillWebP(t, photoPNG(t, real, 900, 600, false), "-q", "80"))
	target := Encoding{Format: PNG, MaxEdge: 200, MaxArea: 200 * 200}
	complete := webpWithFlags(t, simple, 900, 600, webpICCFlag, riffChunk("ICCP", mustAdobeProfile(t)), nil)
	for name, c := range map[string]struct {
		path string
		// afterTool makes the run fail once the tool has written its files.
		afterTool bool
	}{
		"the run fails after the tool": {complete, true},
	} {
		t.Run(name, func(t *testing.T) {
			env, _ := sourceEnv(t)
			scratch := t.TempDir()
			env.Scratch = func() (string, func(), error) { return scratch, func() {}, nil }
			if c.afterTool {
				inner := env.Run
				env.Run = func(ctx context.Context, tool string, args ...string) (Result, error) {
					res, err := inner(ctx, tool, args...)
					if tool == webpFramesTool && err == nil {
						err = errors.New("killed after decoding")
					}
					return res, err
				}
			}
			src, err := env.Open(context.Background(), c.path, Hints{})
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			if _, err := src.Encode(context.Background(), 0, target); err == nil {
				t.Fatal("the encode succeeded")
			}
			if left := readDirNames(t, scratch); len(left) != 0 {
				t.Errorf("a failed decode left %v", left)
			}
		})
	}
}

// The tool writes no profile for an image it fails to decode.
func TestWebPFramesStillWritesNoProfileForAFailedDecode(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	exe, err := tool.Resolve(webpFramesTool)
	if err != nil {
		t.Fatal(err)
	}
	simple := mustReadFile(t, stillWebP(t, photoPNG(t, real, 900, 600, false), "-q", "80"))
	// The image data cut short, in a file whose chunks are all in order.
	cut := append(append([]byte{}, simple[:12]...), riffChunk("VP8 ", simple[20:len(simple)-400])...)
	path := webpWithFlags(t, cut, 900, 600, webpICCFlag, riffChunk("ICCP", mustAdobeProfile(t)), nil)
	dir := t.TempDir()
	out, err := exec.Command(exe, "still", path, dir, "900", "600", "450", "300").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "decoding") {
		t.Fatalf("err = %v, output %q, want a decode failure", err, out)
	}
	if left := readDirNames(t, dir); len(left) != 0 {
		t.Errorf("a failed decode left %v", left)
	}
}

// Every crafted file of testdata/webp-orient (layouts that no flag or chunk
// check could be trusted to settle: flags that disagree with chunks, misplaced
// or repeated ALPH chunks, directories with odd entries, XMP, EXIF in odd
// places) is oriented as ImageMagick's own read orients it: what
// webp-frames and the parse answer with certainty is ImageMagick's answer, and
// the output of Open and Encode has the size of ImageMagick's. A file
// ImageMagick cannot read is refused, not decoded.
func TestCraftedWebPsAreOrientedAsImageMagickOrientsThem(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	files, err := filepath.Glob(filepath.Join("testdata", "webp-orient", "*.webp"))
	if err != nil || len(files) < 30 {
		t.Fatalf("found %d files, err %v", len(files), err)
	}
	target := Encoding{Format: PNG, MaxEdge: 100, MaxArea: 100 * 100}
	var certain, uncertain, refused, turned int
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			out, rerr := real.runMagick(context.Background(), "identify", "-format", "%[orientation]", "webp:"+path)
			env, calls := sourceEnv(t)
			src, oerr := env.Open(context.Background(), path, Hints{})
			if rerr != nil {
				if oerr == nil {
					src.Close()
					t.Fatal("ImageMagick cannot read the file and Open accepted it")
				}
				refused++
				return
			}
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer src.Close()
			want, err := orientationNumber(strings.TrimSpace(string(out.Stdout)))
			if err != nil {
				want = 0
			}
			got, ok, terr := toolOrientation(t, path)
			if terr != nil {
				t.Fatalf("webp-frames refused a file ImageMagick reads: %v", terr)
			}
			if ok {
				certain++
				if got != want {
					t.Errorf("answered %d with certainty, ImageMagick applies %d", got, want)
				}
			} else {
				uncertain++
			}
			arts, err := src.Encode(context.Background(), 0, target)
			if err != nil {
				t.Fatal(err)
			}
			defer arts[0].Cleanup()
			ref, _ := magickReference(t, path, Hints{}, target)
			if arts[0].Width != ref.Width || arts[0].Height != ref.Height {
				t.Errorf("output %dx%d, ImageMagick's %dx%d", arts[0].Width, arts[0].Height, ref.Width, ref.Height)
			}
			if want >= 5 && want <= 8 {
				turned++
			}
			if wantReads := map[bool]int{true: 0, false: 1}[ok]; fullReads(calls()) != wantReads {
				t.Errorf("%d whole-image reads, want %d", fullReads(calls()), wantReads)
			}
		})
	}
	t.Logf("%d answered with certainty, %d asked of ImageMagick, %d refused, %d turned", certain, uncertain, refused, turned)
	if certain < 15 || uncertain < 8 || refused < 3 || turned < 5 {
		t.Error("the corpus no longer exercises every outcome")
	}
}

// An animation whose RIFF size is 12 or 20 bytes more than the file is repaired
// as ImageMagick repairs it, in the tool too: it composites the same frames as
// the file with the right size, and another excess is refused as ImageMagick
// refuses it.
func TestAnimatedWebPWithAnOversizedRIFFSizeIsRepaired(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	exact := mustReadFile(t, buildWebP(t, webpCase{codec: "lossy", frames: 3}))
	target := Encoding{Format: PNG, MaxEdge: 20, MaxArea: 400}
	encode := func(t *testing.T, data []byte) (int, error) {
		env, _ := sourceEnv(t)
		src, err := env.Open(context.Background(), writeFile(t, "a.webp", data), Hints{})
		if err != nil {
			return 0, err
		}
		defer src.Close()
		dir := t.TempDir()
		images, err := src.EncodeFrames(context.Background(), target, dir)
		return len(images), err
	}
	want, err := encode(t, exact)
	if err != nil || want == 0 {
		t.Fatalf("the exact file: %d frames, %v", want, err)
	}
	for _, extra := range []uint32{8, 12, 20} {
		data := append([]byte{}, exact...)
		binary.LittleEndian.PutUint32(data[4:], binary.LittleEndian.Uint32(data[4:])+extra)
		_, rerr := real.runMagick(context.Background(), "identify", "-ping", "webp:"+writeFile(t, "b.webp", data))
		got, err := encode(t, data)
		if (rerr == nil) != (err == nil) {
			t.Errorf("RIFF size +%d: ImageMagick err %v, here %v", extra, rerr, err)
		}
		if err == nil && got != want {
			t.Errorf("RIFF size +%d: %d frames, want %d", extra, got, want)
		}
		if extra == 8 && err == nil {
			t.Error("a RIFF size 8 over was accepted")
		}
	}
}

// An "XMP " chunk whose VP8X flag is clear is not read by ImageMagick, so the
// tool reports no XMP and the file costs no whole-image read; the "XMP\0"
// spelling is read whatever the flag. (libwebp's mux refuses a file whose XMP
// flag and chunk disagree, so the tool's own flag test never decides this.)
func TestXMPChunkWithItsFlagClearIsNotReported(t *testing.T) {
	requireWebPBuilders(t)
	for name, c := range map[string]struct {
		file  string
		reads int
	}{
		"XMP chunk, flag clear": {"n-xmp-only-6-noflag.webp", 0},
		"XMP chunk, flag set":   {"n-xmp-only-6.webp", 1},
		"XMP\\0 chunk, no flag": {"n-xmp0-only-6.webp", 1},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", "webp-orient", c.file)
			if _, ok, err := toolOrientation(t, path); err != nil || ok != (c.reads == 0) {
				t.Errorf("certain = %t (err %v), want %t", ok, err, c.reads == 0)
			}
			env, calls := sourceEnv(t)
			out, err := openSource(t, env, path, Hints{}).Encode(context.Background(), 0, Encoding{Format: PNG, MaxEdge: 100, MaxArea: 100 * 100})
			if err != nil {
				t.Fatal(err)
			}
			out[0].Cleanup()
			if n := fullReads(calls()); n != c.reads {
				t.Errorf("%d whole-image reads, want %d", n, c.reads)
			}
		})
	}
}
