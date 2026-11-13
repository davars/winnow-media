package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// animatedWebPAt is an Encoding of one animated WebP that fits 64 pixels.
func animatedWebPAt(edge int) Encoding {
	enc := webpAtEdge(edge)
	enc.Animate = true
	return enc
}

// rungArgs opens path through p and encodes up to maxFrames of its frames,
// evenly spaced, as one animated WebP of at most 64 pixels on a side. It
// returns the path of the file and its frame mapping.
func rungArgs(t *testing.T, p *Processor, path string, maxFrames int) (string, []Frame, error) {
	t.Helper()
	src, _, err := p.Open(context.Background(), path, OpenOptions{})
	if err != nil {
		return "", nil, err
	}
	t.Cleanup(func() { _ = src.Close() })
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, Select{Frames: Even(maxFrames)}, animatedWebPAt(64))
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, images[0].Name), images[0].Frames, nil
}

// ImageMagick is handed the selected frames of a many-frame animation, one
// full canvas each, and never the file or an instruction to composite it whole.
func TestEncodeGivesImageMagickOnlyTheSelectedFrames(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	levels, delays := make([]int, 40), make([]int, 40)
	for i := range levels {
		levels[i], delays[i] = 5*i, 4
	}
	gifPath := writeGrayAnimGIF(t, requireMagick(t), levels, delays)
	webpPath := buildWebP(t, webpCase{codec: "lossless", frames: 20})
	for _, c := range []struct {
		name, path string
	}{{"GIF", gifPath}, {"WebP", webpPath}} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			src, _, err := p.Open(context.Background(), c.path, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = src.Close() }()
			rec := record(p)
			defer func() { p.run = rec.inner }()
			const maxFrames = 4
			images, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Even(maxFrames)}, animatedWebPAt(64))
			if err != nil {
				t.Fatal(err)
			}
			if len(images[0].Frames) != maxFrames {
				t.Fatalf("mapping %v, want %d frames", images[0].Frames, maxFrames)
			}
			var pixelRuns int
			for _, call := range rec.calls {
				if slices.Contains(call, "identify") || call[0] != "magick" {
					continue
				}
				pixelRuns++
				for _, banned := range []string{"-coalesce", "-delete", c.path} {
					if slices.Contains(call, banned) {
						t.Errorf("magick was given %q: %q", banned, call)
					}
				}
				frames := 0
				for _, a := range call {
					if strings.HasPrefix(a, "pam:") {
						frames++
					}
				}
				if frames == 0 || frames > maxFrames {
					t.Errorf("magick read %d frames, want 1..%d: %q", frames, maxFrames, call)
				}
			}
			if pixelRuns != 1 {
				t.Errorf("%d pixel runs, want the encode alone", pixelRuns)
			}
		})
	}
}

// A compositor that reports other frames than were selected, or reports them
// out of order, is an error whose message names the check, so a check that is
// gone cannot be hidden by the one after it.
func TestFrameLinesRefuseAWrongReport(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, report, want string
	}{
		{"out of order", "1 0 0\n0 0 0\n", `compositor reported "1 0 0", expected frame 0`},
		{"a repeated ordinal", "0 0 0\n0 0 0\n", `compositor reported "0 0 0", expected frame 1`},
		{"a line of two fields", "0 0 0\n1 0\n", `compositor reported "1 0", expected frame 1`},
		{"a line of four fields", "0 0 0 0\n", `compositor reported "0 0 0 0", expected frame 0`},
		{"a profile that is no number", "0 x 0\n", `compositor reported "0 x 0", expected frame 0`},
		{"a soft flag above one", "0 0 2\n", `compositor reported "0 0 2", expected frame 0`},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := &frameLines{dir: t.TempDir(), onFrame: func(composedFrame) error { return nil }}
			_, err := lines.Write([]byte(c.report))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to say %q", err, c.want)
			}
		})
	}
	lines := &frameLines{dir: t.TempDir(), onFrame: func(composedFrame) error { return nil }}
	if _, err := lines.Write([]byte("0 0 0\n1 0 1\n2 3 0\n")); err != nil || lines.next != 3 {
		t.Errorf("a good report: %d frames, %v", lines.next, err)
	}
}

// A still WebP is decoded to exactly one image.
func TestEncodeRefusesAStillWebPDecodeThatWritesTheWrongImages(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	path := stillWebP(t, photoPNG(t, requireMagick(t), 300, 200, false), "-q", "80")
	for _, c := range []struct {
		name   string
		images int
		want   string
	}{
		{"no image", 0, "webp-frames wrote 0 images, want 1"},
		{"two images", 2, "webp-frames wrote more than one image"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			inner := p.compose
			p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
				return inner(ctx, in, req, func(f composedFrame) error {
					for range c.images {
						if err := onFrame(f); err != nil {
							return err
						}
					}
					return nil
				})
			}
			src, _, err := p.Open(context.Background(), path, OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = src.Close() }()
			dir := t.TempDir()
			if _, _, err = src.Encode(context.Background(), dir, Select{}, Encoding{Format: WebP}); err == nil {
				t.Fatal("Encode succeeded")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to say %q", err, c.want)
			}
			if got := dirNames(t, dir); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}
}

// The Processor, which encodes frames as the compositor writes them, refuses a
// compositor that writes one frame more or fewer than were selected, and
// leaves nothing behind.
func TestAnimationFramesRefuseACompositorThatWritesTheWrongFrames(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	path := buildWebP(t, webpCase{codec: "lossless", frames: 6})
	for _, c := range []struct {
		name string
		more bool
		want string
	}{
		{"one frame more", true, "the compositor wrote more frames than selected"},
		{"one frame fewer", false, "composited 5 of 6 frames"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			inner := p.compose
			t.Cleanup(func() { p.compose = inner })
			p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
				var last composedFrame
				seen := 0
				usage, err := inner(ctx, in, req, func(f composedFrame) error {
					last = f
					if seen++; !c.more && seen == len(req.selected) {
						return nil
					}
					return onFrame(f)
				})
				if err == nil && c.more {
					err = onFrame(last)
				}
				return usage, err
			}
			out := t.TempDir()
			if _, _, err := p.encodeEven(context.Background(), path, Encoding{}, 16, out); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to say %q", err, c.want)
			}
			if got := dirNames(t, out); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}
}

// A one-frame GIF is composited like any other: the type decides the decoder,
// not the frame count.
func TestEncodeCompositesAOneFrameGIF(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	rec := record(p)
	path := writeFile(t, "content", oneColourGIF(t, 16, 16, 1))
	composed := false
	inner := p.compose
	p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
		composed = true
		return inner(ctx, in, req, onFrame)
	}
	if _, mapping, err := rungArgs(t, p, path, 16); err != nil || len(mapping) != 1 {
		t.Fatalf("mapping %v, err %v", mapping, err)
	}
	for _, call := range rec.calls {
		if slices.Contains(call, "-coalesce") {
			t.Errorf("a one-frame GIF was read whole: %q", call)
		}
	}
	if !composed {
		t.Error("the one-frame GIF was not composited")
	}
}

// A still WebP carrying an ICC profile, encoded as an animation, encodes as
// one frame.
func TestEncodeAProfiledStillWebPAsAnAnimation(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "content")
	if _, err := testEnv(t).run(context.Background(), "magick", "-size", "16x12", "gradient:red-blue",
		"-profile", writeSRGBProfile(t), "webp:"+path); err != nil {
		t.Fatal(err)
	}
	if _, mapping, err := rungArgs(t, p, path, 16); err != nil || len(mapping) != 1 {
		t.Fatalf("mapping %v, err %v", mapping, err)
	}
}

// The artifact is what ImageMagick makes of the whole animation when it can
// hold it: the same bytes, delays included, for opaque frames.
func TestEncodeMatchesTheWholeFileEncode(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	p := requireProcessor(t)
	levels, delays := []int{10, 70, 130, 190, 250}, []int{0, 20, 7, 33, 20}
	path := writeGrayAnimGIF(t, real, levels, delays)
	out, mapping, err := rungArgs(t, p, path, 3)
	if err != nil {
		t.Fatal(err)
	}
	// The stored 0 delay of frame 0 is shown as 10 centiseconds.
	want := []Frame{{Index: 0, Time: msec(0)}, {Index: 1, Time: msec(100)}, {Index: 3, Time: msec(370)}}
	if !slices.Equal(mapping, want) {
		t.Errorf("mapping %v, want %v", mapping, want)
	}
	reference := filepath.Join(t.TempDir(), "reference.webp")
	magick(t, real, path, "-coalesce", "-delete", "2,4", "-auto-orient", "-filter", "Lanczos", "-resize", "64x42!", "-strip", "-quality", strconv.Itoa(testQuality), reference)
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile(reference)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ref) {
		t.Errorf("artifact differs from the whole-file encode (%d and %d bytes)", len(got), len(ref))
	}
}

// Frames tagged with a profile are converted from it, and untagged frames are
// not.
func TestEncodeConvertsProfiledGIFFrames(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	p := requireProcessor(t)
	adobe := writeAdobeProfile(t, t.TempDir())
	adobeBytes, err := os.ReadFile(adobe)
	if err != nil {
		t.Fatal(err)
	}
	spec := func(shift int) gifFrameSpec {
		return gifFrameSpec{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA, shift: shift}
	}
	base := buildGIF(t, 24, 20, paletteA, []gifFrameSpec{spec(0), spec(1), spec(2)})
	destination := writeSRGBProfile(t)
	encode := func(data []byte) string {
		out, _, err := rungArgs(t, p, writeFile(t, "content", data), 3)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	tagged := withICC(t, base, adobeBytes, 0, 1, 2)
	plain, converted := encode(base), encode(tagged)
	reference := filepath.Join(t.TempDir(), "reference.webp")
	taggedPath := writeFile(t, "tagged.gif", tagged)
	magick(t, real, taggedPath, "-coalesce", "-auto-orient", "-profile", destination, "-strip", "-quality", strconv.Itoa(testQuality), reference)
	plainReference := filepath.Join(t.TempDir(), "plain-reference.webp")
	magick(t, real, writeFile(t, "plain.gif", base), "-coalesce", "-auto-orient", "-strip", "-quality", strconv.Itoa(testQuality), plainReference)
	for i := range 3 {
		if d := animationFrameDifference(t, real, plain, plainReference, i, 3); d > 0.004 {
			t.Errorf("unprofiled frame %d differs from the whole-file encode by %.6f", i, d)
		}
		if d := animationFrameDifference(t, real, converted, reference, i, 3); d > 0.004 {
			t.Errorf("frame %d differs from the whole-file conversion by %.6f", i, d)
		}
		if d := animationFrameDifference(t, real, converted, plain, i, 3); d < 0.002 {
			t.Errorf("frame %d is not converted: it differs from the unprofiled encode by %.6f", i, d)
		}
	}
}

// Consecutive frames that are the same after the resize are encoded once and
// the mapping says so.
func TestEncodeDropsConsecutiveDuplicateFrames(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	p := requireProcessor(t)
	path := writeGrayAnimGIF(t, real, []int{50, 50, 50, 200, 200, 90}, []int{5, 5, 5, 5, 5, 5})
	out, mapping, err := rungArgs(t, p, path, 6)
	if err != nil {
		t.Fatal(err)
	}
	var sources []int
	for _, f := range mapping {
		sources = append(sources, f.Index)
	}
	if want := []int{0, 3, 5}; !slices.Equal(sources, want) {
		t.Errorf("mapping holds source frames %v, want %v", sources, want)
	}
	if delays, _, _, err := testEnv(t).identifyScenes(context.Background(), out); err != nil || len(delays) != len(mapping) {
		t.Errorf("artifact holds %d scenes (%v), mapping %d", len(delays), err, len(mapping))
	}
}

// A request cancelled after the header read stops the compositing before
// ImageMagick runs.
func TestEncodeStopsCompositingWhenCancelled(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := p.run
	var calls [][]string
	p.run = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		calls = append(calls, append([]string{name}, args...))
		res, err := inner(ctx, in, name, args...)
		if slices.Contains(args, "identify") && slices.Contains(args, "-ping") {
			cancel()
		}
		return res, err
	}
	path := writeFile(t, "content", oneColourGIF(t, 16, 16, 6))
	src, _, err := p.Open(ctx, path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if _, _, err := src.Encode(ctx, t.TempDir(), Select{Frames: Even(4)}, animatedWebPAt(64)); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(calls) != 1 {
		t.Errorf("runs = %q, want only the header read", calls)
	}
}

// A Source refuses an animation over a cap before decoding anything: the only
// tool run is the -ping header read. A file at a cap is not refused.
func TestOpenRefusesAnimationsOverTheCapsBeforeDecoding(t *testing.T) {
	t.Parallel()
	const side = maxFramePixels / 6500
	if side*6500 != maxFramePixels {
		t.Fatalf("the frame cap %d is not a multiple of 6500", int64(maxFramePixels))
	}
	for _, c := range []struct {
		name    string
		data    func(t *testing.T) []byte
		refused bool
	}{
		{"frames at the cap", func(t *testing.T) []byte { return oneColourGIF(t, 1, 1, maxAnimationFrames) }, false},
		{"frames over the cap", func(t *testing.T) []byte { return oneColourGIF(t, 1, 1, maxAnimationFrames+1) }, true},
		{"side at the cap", func(t *testing.T) []byte { return oneColourGIF(t, maxDimension, 1, 2) }, false},
		{"side over the cap", func(t *testing.T) []byte { return oneColourGIF(t, maxDimension+1, 1, 2) }, true},
		{"frame pixels at the cap", func(t *testing.T) []byte { return oneColourGIF(t, side, 6500, 2) }, false},
		{"frame pixels over the cap", func(t *testing.T) []byte { return oneColourGIF(t, side, 6501, 2) }, true},
		{"decoded pixels at the cap", func(t *testing.T) []byte { return oneColourGIF(t, 1000, 1000, maxDecodedPixels/1_000_000) }, false},
		{"decoded pixels over the cap", func(t *testing.T) []byte { return oneColourGIF(t, 1000, 1000, maxDecodedPixels/1_000_000+1) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := writeFile(t, "content", c.data(t))
			p := requireProcessorWith(t, Config{Caps: measuredCapsConfig})
			rec := record(p)
			src, _, err := p.Open(context.Background(), path, OpenOptions{})
			if src != nil {
				defer func() { _ = src.Close() }()
			}
			if got := errors.Is(err, ErrTooLarge); got != c.refused {
				t.Fatalf("err = %v, want ErrTooLarge = %v", err, c.refused)
			}
			if got := rec.calls; len(got) != 1 || !slices.Contains(got[0], "identify") || !slices.Contains(got[0], "-ping") {
				t.Errorf("runs = %q, want only the -ping header read", got)
			}
		})
	}
}

// A WebP animation is read and capped as a GIF is.
func TestOpenRefusesOversizeWebPAnimationsBeforeDecoding(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	const height = 6500
	width := maxFramePixels / height
	oneFrame := func(w, h int) string {
		dir := t.TempDir()
		still := writeFile(t, "one.webp", losslessWebP(w, h))
		out := filepath.Join(dir, "anim.webp")
		run(t, "webpmux", "-frame", still, "+100+0+0+0+b", "-frame", still, "+100+0+0+0+b", "-loop", "0", "-o", out)
		return out
	}
	path := oneFrame(width+1, height)
	p := requireProcessorWith(t, Config{Caps: measuredCapsConfig})
	rec := record(p)
	_, _, err := p.Open(context.Background(), path, OpenOptions{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got := rec.calls; len(got) != 1 || !slices.Contains(got[0], "-ping") {
		t.Errorf("runs = %q, want only the -ping header read", got)
	}
}

// An encode that reports fewer rows than it was given frames is an error and
// not a short mapping.
func TestEncodeRefusesAShortEncodeReport(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	p := requireProcessor(t)
	path := buildWebP(t, webpCase{codec: "lossless", frames: 6})
	inner := p.run
	p.run = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		res, err := inner(ctx, in, name, args...)
		if err == nil && name == "magick" && slices.Contains(args, "info:") && slices.Contains(args, "-format") && !slices.Contains(args, "identify") {
			lines := strings.SplitAfter(string(res.Stdout), "\n")
			res.Stdout = []byte(lines[0])
		}
		return res, err
	}
	_, _, err := rungArgs(t, p, path, 4)
	if err == nil {
		t.Fatal("encode succeeded")
	}
	if !strings.Contains(err.Error(), "encode reported 1 frames, want 4") {
		t.Errorf("err = %v, want it to say the encode reported 1 frames, want 4", err)
	}
}

// The compositor refuses a GIF that is not what the header read described: a
// different frame count, canvas or set of profiled frames.
func TestGIFJobRefusesAFileThatDiffersFromTheHeaderRead(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "content", oneColourGIF(t, 8, 8, 3))
	for _, c := range []struct {
		name string
		job  gifJob
		ok   bool
	}{
		{"as read", gifJob{frames: 3, width: 8, height: 8, mask: "000"}, true},
		{"frame count", gifJob{frames: 4, width: 8, height: 8, mask: "0000"}, false},
		{"canvas", gifJob{frames: 3, width: 9, height: 8, mask: "000"}, false},
		{"profiles", gifJob{frames: 3, width: 8, height: 8, mask: "100"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.job.path, c.job.dir, c.job.selected = path, t.TempDir(), []int{0, 2}
			err := c.job.run(context.Background(), nil, func(int, int) error { return nil })
			if (err == nil) != c.ok {
				t.Errorf("err = %v, want success = %v", err, c.ok)
			}
		})
	}
}

// Decoding stops between frames when the context is cancelled.
func TestGIFJobStopsBetweenFramesWhenCancelled(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "content", oneColourGIF(t, 8, 8, 6))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := gifJob{path: path, dir: t.TempDir(), frames: 6, width: 8, height: 8, mask: "000000", selected: []int{0, 1, 2, 3, 4, 5}}
	var written int
	err := job.run(ctx, nil, func(int, int) error {
		written++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || written != 1 {
		t.Errorf("err = %v after %d frames, want context.Canceled after 1", err, written)
	}
}

// Each frame is converted by the profile it carries, and a frame that gets
// another profile than the first is not given the first's.
func TestEncodeConvertsEachGIFFrameByItsOwnProfile(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	p := requireProcessor(t)
	adobe := mustAdobeProfile(t)
	destination := writeSRGBProfile(t)
	var specs []gifFrameSpec
	for i := range 4 {
		specs = append(specs, gifFrameSpec{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA, shift: i})
	}
	// The frames carry Adobe RGB, then the same with red and green exchanged,
	// then each again.
	data := withProfiles(t, buildGIF(t, 24, 20, paletteA, specs), []int{1, 2, 1, 2, 0}, adobe, swappedProfile(t, adobe))
	path := writeFile(t, "content", data)
	out, mapping, err := rungArgs(t, p, path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 4 {
		t.Fatalf("%d frames, want 4", len(mapping))
	}
	reference := filepath.Join(t.TempDir(), "reference.webp")
	magick(t, real, writeFile(t, "x.gif", data), "-coalesce", "-profile", destination, "-strip", "-quality", strconv.Itoa(testQuality), reference)
	split := func(path string) string {
		dir := t.TempDir()
		magick(t, real, path, "-coalesce", "+adjoin", filepath.Join(dir, "%d.png"))
		return dir
	}
	got, want := split(out), split(reference)
	for i := range 4 {
		name := fmt.Sprintf("%d.png", i)
		if d := meanAbsoluteDifference(t, real, filepath.Join(got, name), filepath.Join(want, name)); d > 0.004 {
			t.Errorf("frame %d differs from the whole-file conversion by %.6f", i, d)
		}
	}
}

// A GIF whose ICC extension precedes only its first frame is profiled on
// every coalesced frame, which the compositors handle per frame; an encode of
// every frame accepts it.
func TestEncodeAcceptsGIFProfiledOnTheFirstFrameOnly(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	p := requireProcessor(t)
	dir := t.TempDir()
	profiled := filepath.Join(dir, "profiled.png")
	magick(t, real, "-size", "8x8", "xc:red", "-profile", writeSRGBProfile(t), profiled)
	gifPath := filepath.Join(dir, "anim.gif")
	magick(t, real, "-delay", "7", profiled, "-size", "8x8", "xc:blue", "-size", "8x8", "xc:green", gifPath)
	path := stage(t, gifPath)

	if _, _, err := p.encodeEven(context.Background(), path, Encoding{}, 3, t.TempDir()); err != nil {
		t.Fatalf("an encode of the frames refused the file: %v", err)
	}
	_, mapping, err := rungArgs(t, p, path, 3)
	if err != nil {
		t.Fatalf("an animated encode refused the file: %v", err)
	}
	if len(mapping) != 3 {
		t.Errorf("mapping %v, want 3 frames", mapping)
	}
}

// A one-frame animation is decoded once, by the encode, and maps to its one
// frame.
func TestEncodeDecodesAOneFrameAnimationOnce(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := writeFile(t, "content", oneColourGIF(t, 16, 16, 1))
	src, _, err := p.Open(context.Background(), path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	rec := record(p)
	defer func() { p.run = rec.inner }()
	images, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Even(16)}, animatedWebPAt(64))
	if err != nil || len(images[0].Frames) != 1 {
		t.Fatalf("mapping %v, err %v", images, err)
	}
	var pixelRuns [][]string
	for _, call := range rec.calls {
		if call[0] == "magick" && !slices.Contains(call, "identify") {
			pixelRuns = append(pixelRuns, call)
		}
	}
	if len(pixelRuns) != 1 {
		t.Errorf("a one-frame animation was decoded %d times, want once (the encode): %q", len(pixelRuns), pixelRuns)
	}
}

// A still whose orientation the header read leaves open is refused when its
// encode reports no upright size.
func TestEncodeRefusesAnUnsettledStillWhoseRunReportsNoUprightSize(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := writeFile(t, "content", []byte("not a recognised type"))
	p.run = func(_ context.Context, _ *os.File, _ string, args ...string) (runResult, error) {
		if slices.Contains(args, "identify") {
			return runResult{Stdout: []byte("0||64|64|64|64|PNG|Undefined|DirectClass sRGB|Undefined|END\n")}, nil
		}
		return runResult{Stdout: []byte("64|64\n")}, nil
	}
	src, _, err := p.Open(context.Background(), path, OpenOptions{Type: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	_, _, err = src.Encode(context.Background(), t.TempDir(), Select{}, webpAtEdge(64))
	if err == nil || !strings.Contains(err.Error(), "upright") {
		t.Errorf("err = %v, want a refusal for the missing upright size", err)
	}
}

// A profiled still gets its sRGB destination from the profile the package
// embeds, as a composited animation does.
func TestEncodeAProfiledStillConvertsToTheEmbeddedProfile(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := writeFile(t, "content", []byte("not a recognised type"))
	var encodes [][]string
	p.run = func(_ context.Context, _ *os.File, _ string, args ...string) (runResult, error) {
		if slices.Contains(args, "identify") {
			return runResult{Stdout: []byte("0|icc|64|64|64|64|PNG|Undefined|DirectClass sRGB|Undefined|END\n")}, nil
		}
		encodes = append(encodes, args)
		return runResult{Stdout: []byte("upright|64|64\n64|64\n")}, nil
	}
	src, _, err := p.Open(context.Background(), path, OpenOptions{Type: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, webpAtEdge(64)); err != nil {
		t.Fatal(err)
	}
	if len(encodes) != 1 {
		t.Fatalf("encodes = %q, want one", encodes)
	}
	profiles := 0
	for i, a := range encodes[0] {
		if a != "-profile" {
			continue
		}
		profiles++
		if i+1 >= len(encodes[0]) || encodes[0][i+1] == "" {
			t.Fatalf("encode passes -profile without a destination: %q", encodes[0])
		}
	}
	if profiles != 1 {
		t.Fatalf("encode = %q, want one -profile conversion", encodes[0])
	}
}
