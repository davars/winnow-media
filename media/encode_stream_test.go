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
	real.Stage = func(ext string) (string, func(), error) {
		f, err := os.CreateTemp(dir, "*"+ext)
		if err != nil {
			return "", nil, err
		}
		path := f.Name()
		if err := f.Close(); err != nil {
			return "", nil, err
		}
		return path, func() { _ = os.Remove(path) }, nil
	}
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
			if pixelRuns != 2 {
				t.Errorf("%d pixel runs, want the probe and the encode", pixelRuns)
			}
		})
	}
}

// A one-frame animation is read whole: ImageMagick holds one frame either way.
func TestEncodeRungReadsAOneFrameAnimationWhole(t *testing.T) {
	real := requireMagick(t)
	env, calls := countingEnv(stagedEnv(real, t.TempDir()))
	path := writeFile(t, "content", oneColourGIF(t, 16, 16, 1))
	if _, mapping, err := rungArgs(t, env, path, "image/gif", 16); err != nil || len(mapping) != 1 {
		t.Fatalf("mapping %v, err %v", mapping, err)
	}
	for _, call := range calls() {
		if slices.Contains(call, "-coalesce") && !slices.Contains(call, path) {
			t.Errorf("a whole-file run does not name the file: %q", call)
		}
		for _, a := range call {
			if strings.HasPrefix(a, "pam:") {
				t.Errorf("a one-frame animation was composited: %q", call)
			}
		}
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
	magick(t, real, path, "-coalesce", "-delete", "2,4", "-auto-orient", "-thumbnail", "64x64>", "-quality", strconv.Itoa(testQuality), reference)
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
	magick(t, real, taggedPath, "-coalesce", "-auto-orient", "-profile", destination, "-thumbnail", "64x64>", "-quality", strconv.Itoa(testQuality), reference)
	plainReference := filepath.Join(t.TempDir(), "plain-reference.webp")
	magick(t, real, writeFile(t, "plain.gif", base), "-coalesce", "-auto-orient", "-thumbnail", "64x64>", "-quality", strconv.Itoa(testQuality), plainReference)
	for i := range 3 {
		if d := animationFrameDifferenceN(t, real, plain, plainReference, i); d > 0.004 {
			t.Errorf("unprofiled frame %d differs from the whole-file encode by %.6f", i, d)
		}
		if d := animationFrameDifferenceN(t, real, converted, reference, i); d > 0.004 {
			t.Errorf("frame %d differs from the whole-file conversion by %.6f", i, d)
		}
		if d := animationFrameDifferenceN(t, real, converted, plain, i); d < 0.002 {
			t.Errorf("frame %d is not converted: it differs from the unprofiled encode by %.6f", i, d)
		}
	}
}

// animationFrameDifferenceN is animationFrameDifference for any frame of a
// three-frame animation.
func animationFrameDifferenceN(t *testing.T, env Env, first, second string, frame int) float64 {
	t.Helper()
	pick := func(path string) []string {
		return []string{"(", path, "-coalesce", "-delete", deleteAllBut(frame, 3), ")"}
	}
	out := magick(t, env, append(append(pick(first), pick(second)...),
		"-compose", "difference", "-composite", "-format", "%[fx:mean]", "info:")...)
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func deleteAllBut(keep, n int) string {
	var drop []string
	for i := range n {
		if i != keep {
			drop = append(drop, strconv.Itoa(i))
		}
	}
	return strings.Join(drop, ",")
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

// Only a GIF or a WebP is composited; any other animation is an error and not
// a whole-file read.
func TestEncodeRungRefusesToCompositeOtherFormats(t *testing.T) {
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
	if n := len(calls()) - before; n != 0 {
		t.Errorf("%d tool runs: %q", n, calls()[before:])
	}
}

// A compositor that reports fewer frames than were selected, or a probe that
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
		{"probe", func(name string, args []string) bool {
			return name == "magick" && slices.Contains(args, "info:") && slices.Contains(args, "-format") && !slices.Contains(args, "identify")
		}, "probe returned 1 frames, want 4"},
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
