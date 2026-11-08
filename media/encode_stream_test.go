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

// stagedEnv is real with a Stage that creates files in dir.
func stagedEnv(real Env, dir string) Env {
	real.Scratch = testScratch
	real.Stage = stagerIn(dir)
	return real
}

func rungArgs(t *testing.T, env Env, path, mime string, maxFrames int) (string, []Frame, error) {
	t.Helper()
	frames, err := env.ProbeFrames(context.Background(), path, mime)
	if err != nil {
		t.Fatal(err)
	}
	out, cleanup, _, _, mapping, err := env.EncodeRung(context.Background(), path, mime,
		true, Rung{CeilingPx: 64, MaxFrames: maxFrames}, frames, "", "", testQuality)
	if err != nil {
		return "", nil, err
	}
	t.Cleanup(cleanup)
	return out, mapping, nil
}

// ImageMagick is handed the selected frames of a many-frame animation, one
// full canvas each, and never the file or an instruction to composite it whole.
func TestEncodeRungGivesImageMagickOnlyTheSelectedFrames(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	levels, delays := make([]int, 40), make([]int, 40)
	for i := range levels {
		levels[i], delays[i] = 5*i, 4
	}
	gifPath := writeGrayAnimGIF(t, real, levels, delays)
	webpPath := buildWebP(t, webpCase{codec: "lossless", frames: 20})
	for _, c := range []struct {
		name, path, mime string
	}{{"GIF", gifPath, "image/gif"}, {"WebP", webpPath, "image/webp"}} {
		t.Run(c.name, func(t *testing.T) {
			env, calls := countingEnv(stagedEnv(real, t.TempDir()))
			const maxFrames = 4
			_, mapping, err := rungArgs(t, env, c.path, c.mime, maxFrames)
			if err != nil {
				t.Fatal(err)
			}
			if len(mapping) != maxFrames {
				t.Fatalf("mapping %v, want %d frames", mapping, maxFrames)
			}
			var pixelRuns int
			for _, call := range calls() {
				if slices.Contains(call, "identify") {
					continue
				}
				if call[0] != "magick" {
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
// gone cannot be hidden by the one after it. The tool is a real webp-frames
// whose report is rewritten.
func TestOpenRefusesACompositorReportOfTheWrongFrames(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	path := buildWebP(t, webpCase{codec: "lossless", frames: 6})
	report := "0 0 0\n1 0 0\n2 0 0\n3 0 0\n4 0 0\n5 0 0\n"
	for _, c := range []struct {
		name, report, want string
	}{
		{"one frame fewer", report[:len(report)-6], "composited 5 of 6 frames"},
		{"no frames", "", "composited 0 of 6 frames"},
		{"one frame more", report + "6 0 0\n", "the compositor wrote more frames than selected"},
		{"out of order", "1 0 0\n0 0 0\n2 0 0\n3 0 0\n4 0 0\n5 0 0\n", `compositor reported "1 0 0", expected frame 0`},
		{"a repeated ordinal", "0 0 0\n0 0 0\n2 0 0\n3 0 0\n4 0 0\n5 0 0\n", `compositor reported "0 0 0", expected frame 1`},
		{"a line of two fields", "0 0 0\n1 0\n", `compositor reported "1 0", expected frame 1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := stagedEnv(real, t.TempDir())
			env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
				res, err := real.Run(ctx, name, args...)
				if err == nil && name == webpFramesTool {
					res.Stdout = []byte(c.report)
				}
				return res, err
			}
			src, err := env.Open(context.Background(), path, Hints{})
			if err == nil {
				src.Close()
				t.Fatal("Open succeeded")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to say %q", err, c.want)
			}
		})
	}
}

// A still WebP is decoded to exactly one image.
func TestEncodeRefusesAStillWebPDecodeThatWritesTheWrongImages(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	path := stillWebP(t, photoPNG(t, real, 300, 200, false), "-q", "80")
	for _, c := range []struct {
		name, report, want string
	}{
		{"no image", "", "webp-frames wrote 0 images, want 1"},
		{"two images", "0 0 0\n1 0 0\n", "webp-frames wrote more than one image"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := stagedEnv(real, t.TempDir())
			env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
				res, err := real.Run(ctx, name, args...)
				if err == nil && name == webpFramesTool {
					res.Stdout = []byte(c.report)
				}
				return res, err
			}
			src, err := env.Open(context.Background(), path, Hints{})
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			out, err := src.Encode(context.Background(), 0, Encoding{Format: WebP})
			if err == nil {
				out[0].Cleanup()
				t.Fatal("Encode succeeded")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to say %q", err, c.want)
			}
		})
	}
}

// The Processor, which encodes frames as the compositor writes them, refuses a
// compositor that writes one frame more or fewer than were selected, and
// leaves nothing behind.
func TestAnimationFramesRefuseACompositorThatWritesTheWrongFrames(t *testing.T) {
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
			p.compose = func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
				var last composedFrame
				seen := 0
				usage, err := inner(ctx, req, func(f composedFrame) error {
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
			if _, _, err := p.AnimationFrames(context.Background(), path, Encoding{}, 0, out); err == nil || !strings.Contains(err.Error(), c.want) {
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
func TestEncodeRungCompositesAOneFrameGIF(t *testing.T) {
	real := requireMagick(t)
	env, calls := countingEnv(stagedEnv(real, t.TempDir()))
	path := writeFile(t, "content", oneColourGIF(t, 16, 16, 1))
	if _, mapping, err := rungArgs(t, env, path, "image/gif", 16); err != nil || len(mapping) != 1 {
		t.Fatalf("mapping %v, err %v", mapping, err)
	}
	composited := false
	for _, call := range calls() {
		if slices.Contains(call, "-coalesce") {
			t.Errorf("a one-frame GIF was read whole: %q", call)
		}
		for _, a := range call {
			composited = composited || strings.HasPrefix(a, "pam:")
		}
	}
	if !composited {
		t.Error("the one-frame GIF was not composited")
	}
}

// A still WebP carrying an ICC profile encodes as a one-frame animation,
// which is how winnow passes every WebP, with the profile ProbeFrames reports
// from its chunk list.
func TestEncodeRungEncodesAProfiledStillWebPAsAnAnimation(t *testing.T) {
	real := requireMagick(t)
	env := stagedEnv(real, t.TempDir())
	path := filepath.Join(t.TempDir(), "content")
	if _, err := real.Run(context.Background(), "magick", "-size", "16x12", "gradient:red-blue",
		"-profile", writeSRGBProfile(t), "webp:"+path); err != nil {
		t.Fatal(err)
	}
	frames, err := env.ProbeFrames(context.Background(), path, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	if !AnyProfiled(frames) {
		t.Fatalf("ProbeFrames found no profile in %v", frames)
	}
	_, cleanup, _, _, mapping, err := env.EncodeRung(context.Background(), path, "image/webp",
		true, Rung{CeilingPx: 64, MaxFrames: 16}, frames, writeSRGBProfile(t), "", testQuality)
	if err != nil || len(mapping) != 1 {
		t.Fatalf("mapping %v, err %v", mapping, err)
	}
	cleanup()
}

// The artifact is what ImageMagick makes of the whole animation when it can
// hold it: the same bytes, delays included, for opaque frames.
func TestEncodeRungMatchesTheWholeFileEncode(t *testing.T) {
	real := requireMagick(t)
	env := stagedEnv(real, t.TempDir())
	levels, delays := []int{10, 70, 130, 190, 250}, []int{0, 20, 7, 33, 20}
	path := writeGrayAnimGIF(t, real, levels, delays)
	out, mapping, err := rungArgs(t, env, path, "image/gif", 3)
	if err != nil {
		t.Fatal(err)
	}
	// The stored 0 delay of frame 0 is shown as 10 centiseconds.
	want := []Frame{{SourceFrame: 0, SourceTimeMS: 0}, {SourceFrame: 1, SourceTimeMS: 100}, {SourceFrame: 3, SourceTimeMS: 370}}
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
func TestEncodeRungConvertsProfiledGIFFrames(t *testing.T) {
	real := requireMagick(t)
	env := stagedEnv(real, t.TempDir())
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
		path := writeFile(t, "content", data)
		frames, err := env.ProbeFrames(context.Background(), path, "image/gif")
		if err != nil {
			t.Fatal(err)
		}
		out, cleanup, _, _, _, err := env.EncodeRung(context.Background(), path, "image/gif",
			true, Rung{CeilingPx: 64, MaxFrames: 3}, frames, destination, "", testQuality)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
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
func TestEncodeRungDropsConsecutiveDuplicateFrames(t *testing.T) {
	real := requireMagick(t)
	env := stagedEnv(real, t.TempDir())
	path := writeGrayAnimGIF(t, real, []int{50, 50, 50, 200, 200, 90}, []int{5, 5, 5, 5, 5, 5})
	out, mapping, err := rungArgs(t, env, path, "image/gif", 6)
	if err != nil {
		t.Fatal(err)
	}
	var sources []int
	for _, f := range mapping {
		sources = append(sources, f.SourceFrame)
	}
	if want := []int{0, 3, 5}; !slices.Equal(sources, want) {
		t.Errorf("mapping holds source frames %v, want %v", sources, want)
	}
	if scenes, _, _, err := real.IdentifyArtifact(context.Background(), out); err != nil || scenes != len(mapping) {
		t.Errorf("artifact holds %d scenes (%v), mapping %d", scenes, err, len(mapping))
	}
}

// A request cancelled after the header read stops the compositing before
// ImageMagick runs.
func TestEncodeRungStopsCompositingWhenCancelled(t *testing.T) {
	real := requireMagick(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelling := real
	cancelling.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		res, err := real.Run(ctx, name, args...)
		if slices.Contains(args, "identify") && slices.Contains(args, "-ping") {
			cancel()
		}
		return res, err
	}
	env, calls := countingEnv(stagedEnv(cancelling, t.TempDir()))
	path := writeFile(t, "content", oneColourGIF(t, 16, 16, 6))
	frames, err := real.ProbeFrames(context.Background(), path, "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	_, cleanup, _, _, _, err := env.EncodeRung(ctx, path, "image/gif", true, Rung{CeilingPx: 64, MaxFrames: 4}, frames, "", "", testQuality)
	if err == nil {
		cleanup()
		t.Fatal("a cancelled encode succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := calls(); len(got) != 1 {
		t.Errorf("runs = %q, want only the header read", got)
	}
}

// The staging directory holds the artifact and nothing else, whether the
// encode succeeds or fails.
func TestEncodeRungLeavesNoScratchFiles(t *testing.T) {
	real := requireMagick(t)
	staging := t.TempDir()
	env := stagedEnv(real, staging)
	path := writeFile(t, "content", oneColourGIF(t, 16, 16, 4))
	frames, err := env.ProbeFrames(context.Background(), path, "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	out, cleanup, _, _, _, err := env.EncodeRung(context.Background(), path, "image/gif",
		true, Rung{CeilingPx: 64, MaxFrames: 4}, frames, "", "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, staging); len(got) != 1 || filepath.Join(staging, got[0]) != out {
		t.Errorf("staging holds %v after success, want only the artifact", got)
	}
	cleanup()

	// ImageMagick failing after the frames are composited.
	failing := env
	failing.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		if !slices.Contains(args, "identify") {
			return Result{}, errors.New("magick failed")
		}
		return env.Run(ctx, name, args...)
	}
	if _, _, _, _, _, err := failing.EncodeRung(context.Background(), path, "image/gif",
		true, Rung{CeilingPx: 64, MaxFrames: 4}, frames, "", "", testQuality); err == nil {
		t.Fatal("an encode with a failing magick succeeded")
	}
	if got := dirNames(t, staging); len(got) != 0 {
		t.Errorf("staging holds %v after failure, want nothing", got)
	}
}

// EncodeRung reads an animation's headers and refuses one over a cap before
// decoding anything: the only tool run is the -ping header read. A file at a
// cap is not refused.
func TestEncodeRungRefusesAnimationsOverTheCapsBeforeDecoding(t *testing.T) {
	real := requireMagick(t)
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
			frames, err := real.ProbeFrames(context.Background(), path, "image/gif")
			if err != nil {
				t.Fatal(err)
			}
			// An input at a cap proceeds to the decode, which is cancelled as
			// it starts: the cost of decoding a file at a cap is not the
			// subject, and it would inflate this process for the tests after.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cancelling := real
			cancelling.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
				res, err := real.Run(ctx, name, args...)
				if slices.Contains(args, "identify") && slices.Contains(args, "-ping") {
					cancel()
				}
				return res, err
			}
			env, calls := countingEnv(stagedEnv(cancelling, t.TempDir()))
			_, cleanup, _, _, _, err := env.EncodeRung(ctx, path, "image/gif",
				true, Rung{CeilingPx: 64, MaxFrames: 2}, frames, "", "", testQuality)
			if err == nil {
				cleanup()
			}
			if got := errors.Is(err, ErrTooLarge); got != c.refused {
				t.Fatalf("err = %v, want ErrTooLarge = %v", err, c.refused)
			}
			if !c.refused && !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the decode to start and be cancelled", err)
			}
			if c.refused {
				got := calls()
				if len(got) != 1 || !slices.Contains(got[0], "identify") || !slices.Contains(got[0], "-ping") {
					t.Errorf("runs = %q, want only the -ping header read", got)
				}
			}
		})
	}
}

// A WebP animation is read and capped as a GIF is.
func TestEncodeRungRefusesOversizeWebPAnimationsBeforeDecoding(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	const height = 6500
	width := maxFramePixels / height
	oneFrame := func(w, h int) string {
		dir := t.TempDir()
		src, still := filepath.Join(dir, "one.png"), filepath.Join(dir, "one.webp")
		magick(t, real, "-size", fmt.Sprintf("%dx%d", w, h), "xc:red", src)
		run(t, "cwebp", "-quiet", "-lossless", src, "-o", still)
		out := filepath.Join(dir, "anim.webp")
		run(t, "webpmux", "-frame", still, "+100+0+0+0+b", "-frame", still, "+100+0+0+0+b", "-loop", "0", "-o", out)
		return out
	}
	path := oneFrame(width+1, height)
	frames, err := real.ProbeFrames(context.Background(), path, "image/webp")
	if err != nil || len(frames) != 2 {
		t.Fatalf("%d frames, err %v", len(frames), err)
	}
	env, calls := countingEnv(stagedEnv(real, t.TempDir()))
	_, cleanup, _, _, _, err := env.EncodeRung(context.Background(), path, "image/webp",
		true, Rung{CeilingPx: 64, MaxFrames: 2}, frames, "", "", testQuality)
	if err == nil {
		cleanup()
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got := calls(); len(got) != 1 || !slices.Contains(got[0], "-ping") {
		t.Errorf("runs = %q, want only the -ping header read", got)
	}
}

// Frames that are not the file's own headers are an error, before any decode.
func TestEncodeRungRefusesFramesThatDifferFromTheHeaders(t *testing.T) {
	real := requireMagick(t)
	path := writeFile(t, "content", oneColourGIF(t, 8, 8, 4))
	other, err := real.ProbeFrames(context.Background(), writeFile(t, "content", oneColourGIF(t, 8, 8, 3)), "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	for name, frames := range map[string][]FrameInfo{
		"too few":    other,
		"fabricated": make([]FrameInfo, maxAnimationFrames+1),
		"none":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			env, calls := countingEnv(stagedEnv(real, t.TempDir()))
			_, cleanup, _, _, _, err := env.EncodeRung(context.Background(), path, "image/gif",
				true, Rung{CeilingPx: 64, MaxFrames: 2}, frames, "", "", testQuality)
			if err == nil {
				cleanup()
				t.Fatal("encode succeeded")
			}
			if errors.Is(err, ErrTooLarge) {
				t.Errorf("err = %v, want a mismatch error", err)
			}
			if got := calls(); len(got) != 1 {
				t.Errorf("runs = %q, want only the header read", got)
			}
		})
	}
}

// A compositor that reports fewer frames than were selected, or an encode that
// reports fewer rows, is an error and not a short mapping.
func TestEncodeRungRefusesShortToolOutput(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	path := buildWebP(t, webpCase{codec: "lossless", frames: 6})
	frames, err := real.ProbeFrames(context.Background(), path, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	firstLines := func(out []byte, n int) []byte {
		lines := strings.SplitAfter(string(out), "\n")
		return []byte(strings.Join(lines[:n], ""))
	}
	for _, c := range []struct {
		name string
		hit  func(name string, args []string) bool
		want string
	}{
		{"compositor", func(name string, _ []string) bool { return name == webpFramesTool }, "composited 1 of 4"},
		{"encode", func(name string, args []string) bool {
			return name == "magick" && slices.Contains(args, "info:") && slices.Contains(args, "-format") && !slices.Contains(args, "identify")
		}, "encode reported 1 frames, want 4"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := real
			env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
				res, err := real.Run(ctx, name, args...)
				if err == nil && c.hit(name, args) {
					res.Stdout = firstLines(res.Stdout, 1)
				}
				return res, err
			}
			env = stagedEnv(env, t.TempDir())
			_, cleanup, _, _, _, err := env.EncodeRung(context.Background(), path, "image/webp",
				true, Rung{CeilingPx: 64, MaxFrames: 4}, frames, "", "", testQuality)
			if err == nil {
				cleanup()
				t.Fatal("encode succeeded")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to say %q", err, c.want)
			}
		})
	}
}

// The compositor refuses a GIF that is not what the header read described: a
// different frame count, canvas or set of profiled frames.
func TestGIFJobRefusesAFileThatDiffersFromTheHeaderRead(t *testing.T) {
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
func TestEncodeRungConvertsEachGIFFrameByItsOwnProfile(t *testing.T) {
	real := requireMagick(t)
	env := stagedEnv(real, t.TempDir())
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
	frames, err := env.ProbeFrames(context.Background(), path, "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	out, cleanup, _, _, mapping, err := env.EncodeRung(context.Background(), path, "image/gif",
		true, Rung{CeilingPx: 64, MaxFrames: 4}, frames, destination, "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
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
// every coalesced frame, which the compositors handle per frame. The
// operation accepts it; EncodeRung, which composites the same way, must too.
func TestEncodeRungAcceptsGIFProfiledOnTheFirstFrameOnly(t *testing.T) {
	real := requireMagick(t)
	p := requireProcessor(t)
	dir := t.TempDir()
	profiled := filepath.Join(dir, "profiled.png")
	magick(t, real, "-size", "8x8", "xc:red", "-profile", writeSRGBProfile(t), profiled)
	gifPath := filepath.Join(dir, "anim.gif")
	magick(t, real, "-delay", "7", profiled, "-size", "8x8", "xc:blue", "-size", "8x8", "xc:green", gifPath)
	path := stage(t, gifPath)

	if _, _, err := p.AnimationFrames(context.Background(), path, Encoding{}, 3, t.TempDir()); err != nil {
		t.Fatalf("AnimationFrames refused the file: %v", err)
	}

	env := stagedEnv(real, t.TempDir())
	frames, err := env.ProbeFrames(context.Background(), path, "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || !frames[0].hasICC || frames[1].hasICC {
		t.Fatalf("ProbeFrames = %+v, want a profile on the first frame only", frames)
	}
	_, cleanup, _, _, mapping, err := env.EncodeRung(context.Background(), path, "image/gif",
		true, Rung{CeilingPx: 64, MaxFrames: 3}, frames, writeSRGBProfile(t), "", testQuality)
	if err != nil {
		t.Fatalf("EncodeRung refused the file the operation accepts: %v", err)
	}
	defer cleanup()
	if len(mapping) != 3 {
		t.Errorf("mapping %v, want 3 frames", mapping)
	}
}

// A one-frame animation is decoded once, by the encode, and maps to its one
// frame.
func TestEncodeRungDecodesAOneFrameAnimationOnce(t *testing.T) {
	real := requireMagick(t)
	env, calls := countingEnv(stagedEnv(real, t.TempDir()))
	path := writeFile(t, "content", oneColourGIF(t, 16, 16, 1))
	if _, mapping, err := rungArgs(t, env, path, "image/gif", 16); err != nil || len(mapping) != 1 {
		t.Fatalf("mapping %v, err %v", mapping, err)
	}
	var pixelRuns [][]string
	for _, call := range calls() {
		if call[0] == "magick" && !slices.Contains(call, "identify") {
			pixelRuns = append(pixelRuns, call)
		}
	}
	if len(pixelRuns) != 1 {
		t.Errorf("a one-frame animation was decoded %d times, want once (the encode): %q", len(pixelRuns), pixelRuns)
	}
}

// A multi-frame file the pipeline cannot composite is refused, as
// AnimationFrames refuses it, with ErrUnsupportedType naming the type found
// and before any pixel is decoded.
func TestEncodeRungRefusesOtherFormatsAsAnimationFramesDoes(t *testing.T) {
	real := requireMagick(t)
	env, calls := countingEnv(stagedEnv(real, t.TempDir()))
	tiff := filepath.Join(t.TempDir(), "multi.tif")
	magick(t, real, "-size", "8x8", "xc:red", "-size", "8x8", "xc:blue", tiff)
	frames, err := env.ProbeFrames(context.Background(), tiff, "image/tiff")
	if err != nil || len(frames) != 2 {
		t.Fatalf("%d frames, err %v", len(frames), err)
	}
	before := len(calls())
	_, cleanup, _, _, _, err := env.EncodeRung(context.Background(), tiff, "image/tiff",
		true, Rung{CeilingPx: 64, MaxFrames: 2}, frames, "", "", testQuality)
	if err == nil {
		cleanup()
		t.Fatal("a TIFF was encoded as an animation")
	}
	if !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("err = %v, want ErrUnsupportedType", err)
	}
	if strings.Contains(err.Error(), `""`) {
		t.Errorf("err = %v, names no type", err)
	}
	for _, call := range calls()[before:] {
		if !slices.Contains(call, "identify") {
			t.Errorf("a pixel run before the refusal: %q", call)
		}
	}
	// The same file as a still is its first page.
	_, cleanup, _, _, mapping, err := env.EncodeRung(context.Background(), tiff, "image/tiff",
		false, Rung{CeilingPx: 64, MaxFrames: 1}, frames, "", "", testQuality)
	if err != nil || len(mapping) != 1 {
		t.Fatalf("still of a multi-page TIFF: mapping %v, err %v", mapping, err)
	}
	cleanup()
}

// A profiled still gets its sRGB destination from the profile the package
// embeds when the caller supplies no file, as a composited animation does.
func TestEncodeRungStillWithoutProfilePathUsesTheEmbeddedProfile(t *testing.T) {
	var encodes [][]string
	env := stagedEnv(Env{Run: func(_ context.Context, _ string, args ...string) (Result, error) {
		if slices.Contains(args, "identify") {
			return Result{Stdout: []byte("0|icc|64|64|64|64|PNG|Undefined|DirectClass sRGB|Undefined|END\n")}, nil
		}
		encodes = append(encodes, args)
		return Result{Stdout: []byte("64|64\n")}, nil
	}}, t.TempDir())
	path := writeFile(t, "content", []byte("not a recognised type"))
	_, cleanup, _, _, _, err := env.EncodeRung(context.Background(), path, "", false,
		Rung{CeilingPx: 64, MaxFrames: 1}, []FrameInfo{{hasICC: true}}, "", "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
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
