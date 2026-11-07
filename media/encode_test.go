package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testQuality = 80

func TestProfileDetectionAndAnimationPolicy(t *testing.T) {
	for _, profiles := range []string{"icc", "ICC", "8bim,exif,icc,iptc", "xmp icm"} {
		if !profileListHasICC(profiles) {
			t.Errorf("profile list %q was not recognized as ICC", profiles)
		}
	}
	for _, profiles := range []string{"", "xmp", "exif,iptc", "myiccprofile"} {
		if profileListHasICC(profiles) {
			t.Errorf("profile list %q was falsely recognized as ICC", profiles)
		}
	}

	frames := []FrameInfo{{hasICC: true}, {hasICC: false}, {hasICC: true}}
	if profiled, err := selectedProfiled(frames, []int{0, 2}); err != nil || !profiled {
		t.Fatalf("all-profiled selection = (%v, %v), want (true, nil)", profiled, err)
	}
	if profiled, err := selectedProfiled(frames, []int{1}); err != nil || profiled {
		t.Fatalf("unprofiled selection = (%v, %v), want (false, nil)", profiled, err)
	}
	if _, err := selectedProfiled(frames, []int{0, 1}); err == nil {
		t.Fatal("mixed profiled/unprofiled selection unexpectedly succeeded")
	}
	if _, err := selectedProfiled(frames, []int{3}); err == nil {
		t.Fatal("out-of-range selection unexpectedly succeeded")
	}
	if !AnyProfiled(frames) || AnyProfiled(frames[1:2]) {
		t.Fatal("AnyProfiled disagrees with the frames' profile flags")
	}
}

// An untagged image must not be assigned an RGB profile merely because a
// destination profile is available.
func TestColorArgsAreConditional(t *testing.T) {
	profile := "/profiles/sRGB2014.icc"
	if got := colorArgs(profile, false); len(got) != 0 {
		t.Fatalf("unprofiled color args = %q, want no color transform", got)
	}
	if got, want := colorArgs(profile, true), []string{"-profile", profile}; !slices.Equal(got, want) {
		t.Fatalf("profiled color args = %q, want %q", got, want)
	}
}

// The profile must be applied after orientation but before thumbnail discards
// pixels, in the still encoder and in both sides of the animation dedup
// contract.
func TestColorStagePlacement(t *testing.T) {
	const profile = "/profiles/sRGB2014.icc"

	still := stillArgs("/in", "/out.webp", 640, "", profile, "", true, testQuality)
	assertProfileBetween(t, "still", still, profile, "-auto-orient", "-thumbnail")
	stillUntagged := stillArgs("/in", "/out.webp", 640, "", profile, "", false, testQuality)
	assertNoProfile(t, "unprofiled still", stillUntagged)

	animation := animationPipeline("/in", 640, "", profile, true)
	assertProfileBetween(t, "animation probe/encode", animation, profile, "-auto-orient", "-thumbnail")
	animationUntagged := animationPipeline("/in", 640, "", profile, false)
	assertNoProfile(t, "unprofiled animation", animationUntagged)
}

func TestStillArgsOrderOrientBeforeAutoOrient(t *testing.T) {
	args := strings.Join(stillArgs("/in", "/out.webp", 640, "", "", "LeftBottom", false, testQuality), " ")
	if !strings.Contains(args, "-orient LeftBottom -auto-orient") {
		t.Errorf("args = %q, want -orient LeftBottom immediately before -auto-orient", args)
	}
	if none := strings.Join(stillArgs("/in", "/out.webp", 640, "", "", "", false, testQuality), " "); strings.Contains(none, " -orient ") {
		t.Errorf("args without orient = %q, want no -orient", none)
	}
}

// The rung sequence itself, which the encoding tests only observe indirectly.
func TestLadderOrder(t *testing.T) {
	limits := Limits{Ceilings: []int{2048, 1536, 1024}, MaxFrames: 16}

	animated := Ladder(limits, true, 1000)
	want := []Rung{
		{2048, 16}, {2048, 8}, {2048, 4}, {2048, 2}, {2048, 1}, {1536, 1}, {1024, 1},
	}
	if !slices.Equal(animated, want) {
		t.Errorf("animated ladder = %v, want %v", animated, want)
	}

	still := Ladder(limits, false, 1)
	wantStill := []Rung{{2048, 1}, {1536, 1}, {1024, 1}}
	if !slices.Equal(still, wantStill) {
		t.Errorf("still ladder = %v, want %v", still, wantStill)
	}

	// A source shorter than the budget collapses the rungs that would encode
	// identically: at 5 source frames the 16- and 8-frame caps both sample 5.
	short := Ladder(limits, true, 5)
	wantShort := []Rung{{2048, 16}, {2048, 4}, {2048, 2}, {2048, 1}, {1536, 1}, {1024, 1}}
	if !slices.Equal(short, wantShort) {
		t.Errorf("5-frame ladder = %v, want %v", short, wantShort)
	}

	if got := Ladder(Limits{MaxFrames: 16}, true, 10); got != nil {
		t.Errorf("ladder without ceilings = %v, want nil", got)
	}
}

// Frame 0 is always present, and the last frame appears only when the source
// is no longer than the budget.
func TestSelectFramesIsEvenlySpaced(t *testing.T) {
	cases := []struct {
		n, sourceFrames int
		want            []int
	}{
		{16, 20, []int{0, 1, 2, 3, 5, 6, 7, 8, 10, 11, 12, 13, 15, 16, 17, 18}},
		{4, 20, []int{0, 5, 10, 15}},
		{5, 5, []int{0, 1, 2, 3, 4}},
		{1, 20, []int{0}},
		{1, 1, []int{0}},
	}
	for _, c := range cases {
		got := selectFrames(c.n, c.sourceFrames)
		if !slices.Equal(got, c.want) {
			t.Errorf("selectFrames(%d, %d) = %v, want %v", c.n, c.sourceFrames, got, c.want)
		}
		if got[0] != 0 {
			t.Errorf("selectFrames(%d, %d) does not start at frame 0", c.n, c.sourceFrames)
		}
	}
}

func TestDeleteExpr(t *testing.T) {
	for _, c := range []struct {
		keep   []int
		frames int
		want   string
	}{
		{[]int{0, 2}, 5, "1,3,4"},
		{[]int{0, 1, 2}, 3, ""},
		{[]int{4}, 5, "0,1,2,3"},
	} {
		if got := DeleteExpr(c.keep, c.frames); got != c.want {
			t.Errorf("DeleteExpr(%v, %d) = %q, want %q", c.keep, c.frames, got, c.want)
		}
	}
}

func TestTimestampsClampShortDelays(t *testing.T) {
	got := timestamps([]int{0, 5, 1, 20})
	want := []int64{0, 100, 150, 250}
	if !slices.Equal(got, want) {
		t.Errorf("timestamps = %v, want %v", got, want)
	}
}

func TestCoderForMIME(t *testing.T) {
	if coder, ok := CoderForMIME("image/x-tga"); !ok || coder != "tga" {
		t.Errorf("CoderForMIME(image/x-tga) = %q, %v", coder, ok)
	}
	for _, mime := range []string{"image/png", ""} {
		if coder, ok := CoderForMIME(mime); ok {
			t.Errorf("CoderForMIME(%q) = %q, want no coder", mime, coder)
		}
	}
}

func TestRunMagickWithCoderFallbackRecoversTGA(t *testing.T) {
	env := requireMagick(t)
	dir := t.TempDir()
	extPath := filepath.Join(dir, "src.tga")
	magick(t, env, "-size", "4x4", "xc:red", extPath)
	content := renameToContent(t, extPath)

	build := func(coder string) []string {
		in := content
		if coder != "" {
			in = coder + ":" + in
		}
		return []string{"identify", in}
	}
	if _, err := env.RunMagickWithCoderFallback(context.Background(), "image/x-tga", build); err != nil {
		t.Fatalf("mapped mime type did not recover: %v", err)
	}
	if _, err := env.RunMagickWithCoderFallback(context.Background(), "image/png", build); err == nil {
		t.Fatal("unmapped mime type unexpectedly succeeded on a bare-undecodable input")
	}
}

func TestEncodeRungStillRecoversTGAViaCoderFallback(t *testing.T) {
	env := requireMagick(t)
	extPath := filepath.Join(t.TempDir(), "src.tga")
	magick(t, env, "-size", "4x4", "xc:red", extPath)
	content := renameToContent(t, extPath)

	frames, err := env.ProbeFrames(context.Background(), content, "image/x-tga")
	if err != nil {
		t.Fatal(err)
	}
	out, cleanup, w, h, mapping, err := env.EncodeRung(context.Background(), content, "image/x-tga",
		false, Rung{CeilingPx: 64, MaxFrames: 1}, frames, "", "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if w != 4 || h != 4 || !slices.Equal(mapping, []Frame{{SourceFrame: 0, SourceTimeMS: 0}}) {
		t.Errorf("EncodeRung = %dx%d %v, want 4x4 and frame 0", w, h, mapping)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Errorf("staged artifact %s: %v", out, err)
	}
}

func writeGrayAnimGIF(t *testing.T, env Env, levels, delaysCS []int) string {
	t.Helper()
	dir := t.TempDir()
	var args []string
	for k, level := range levels {
		frame := filepath.Join(dir, fmt.Sprintf("f%03d.png", k))
		magick(t, env, "-size", "120x80", fmt.Sprintf("xc:gray(%d)", level), frame)
		args = append(args, "-delay", strconv.Itoa(delaysCS[k]), frame)
	}
	extPath := filepath.Join(dir, "anim.gif")
	magick(t, env, append(args, "-loop", "0", extPath)...)
	return renameToContent(t, extPath)
}

// Frames are sampled by source index, the mapping carries each frame's
// original timestamp, and a zero stored delay is clamped to the GIF display
// default.
func TestEncodeRungAnimationMapsSampledFrames(t *testing.T) {
	env := requireMagick(t)
	content := writeGrayAnimGIF(t, env, []int{10, 70, 130, 190}, []int{0, 20, 20, 20})

	frames, err := env.ProbeFrames(context.Background(), content, "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4 {
		t.Fatalf("ProbeFrames found %d frames, want 4", len(frames))
	}
	out, cleanup, _, _, mapping, err := env.EncodeRung(context.Background(), content, "image/gif",
		true, Rung{CeilingPx: 64, MaxFrames: 2}, frames, "", "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	want := []Frame{{SourceFrame: 0, SourceTimeMS: 0}, {SourceFrame: 2, SourceTimeMS: 300}}
	if !slices.Equal(mapping, want) {
		t.Errorf("mapping = %v, want %v", mapping, want)
	}
	scenes, _, _, err := env.IdentifyArtifact(context.Background(), out)
	if err != nil || scenes != 2 {
		t.Errorf("IdentifyArtifact = %d scenes, %v; want 2", scenes, err)
	}
}

func TestEncodeRungNeverUpscalesAStill(t *testing.T) {
	env := requireMagick(t)
	extPath := filepath.Join(t.TempDir(), "src.png")
	magick(t, env, "-size", "40x20", "xc:blue", extPath)
	content := renameToContent(t, extPath)
	frames, err := env.ProbeFrames(context.Background(), content, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	_, cleanup, w, h, _, err := env.EncodeRung(context.Background(), content, "image/png",
		false, Rung{CeilingPx: 640, MaxFrames: 1}, frames, "", "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if w != 40 || h != 20 {
		t.Errorf("encoded %dx%d, want the original 40x20", w, h)
	}
}

func writeSRGBProfile(t *testing.T) string {
	t.Helper()
	encoded, err := os.ReadFile("profiles/sRGB2014.icc.base64")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sRGB2014.icc")
	if err := os.WriteFile(path, profile, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeAdobeProfile(t *testing.T, dir string) string {
	t.Helper()
	adobe := filepath.Join(dir, "adobe-rgb-1998.icc")
	profile, err := base64.StdEncoding.DecodeString(adobeRGB1998Profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adobe, profile, 0o600); err != nil {
		t.Fatal(err)
	}
	return adobe
}

func writeWideGamutStill(t *testing.T, env Env, srgb string) string {
	t.Helper()
	dir := t.TempDir()
	adobe := writeAdobeProfile(t, dir)
	withExtension := filepath.Join(dir, "wide.png")
	magick(t, env, "-size", "256x96", "gradient:#0020ff-#ffb000", "-profile", srgb, "-profile", adobe, withExtension)
	return renameToContent(t, withExtension)
}

// The fixture is stored in Adobe RGB, not merely tagged as sRGB: omitting the
// destination -profile gives a visibly different WebP.
func TestEncodeRungWideGamutStillConvertsBeforeWebP(t *testing.T) {
	env := requireMagick(t)
	destination := writeSRGBProfile(t)
	input := writeWideGamutStill(t, env, destination)

	frames, err := env.ProbeFrames(context.Background(), input, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if !AnyProfiled(frames) {
		t.Fatalf("fixture has no embedded ICC profile: %+v", frames)
	}
	actual, cleanup, _, _, _, err := env.EncodeRung(context.Background(), input, "image/png",
		false, Rung{CeilingPx: 128, MaxFrames: 1}, frames, destination, "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	dir := t.TempDir()
	reference := filepath.Join(dir, "reference.webp")
	unconverted := filepath.Join(dir, "unconverted.webp")
	magick(t, env, append(LimitArgs(), input+"[0]", "-auto-orient", "-profile", destination,
		"-thumbnail", "128x128>", "-quality", strconv.Itoa(testQuality), reference)...)
	magick(t, env, stillArgs(input, unconverted, 128, "", destination, "", false, testQuality)...)

	if diff := meanAbsoluteDifference(t, env, actual, reference); diff > 0.004 {
		t.Fatalf("corrected WebP differs from explicit sRGB reference by %.6f, want <= 0.004", diff)
	}
	if diff := meanAbsoluteDifference(t, env, actual, unconverted); diff < 0.015 {
		t.Fatalf("corrected WebP differs from unconverted reinterpretation by %.6f, want >= 0.015; fixture no longer proves conversion", diff)
	}
}

// A JXL still stored in Adobe RGB must be reported as profiled and converted
// like the PNG above. ImageMagick's JXL coder does not read the ICC profile
// under -ping, which the header-only probe uses, so the still is encoded as if
// it were untagged.
func TestProbeFramesReportsTheProfileOfAJXLStill(t *testing.T) {
	env := requireMagick(t)
	destination := writeSRGBProfile(t)
	dir := t.TempDir()
	adobe := writeAdobeProfile(t, dir)
	withExtension := filepath.Join(dir, "wide.jxl")
	magick(t, env, "-size", "256x96", "gradient:#0020ff-#ffb000", "-profile", destination, "-profile", adobe, withExtension)
	if !profileListHasICC(string(magick(t, env, "identify", "-format", "%[profiles]", withExtension))) {
		t.Skip("this ImageMagick does not write an ICC profile into JXL")
	}
	input := renameToContent(t, withExtension)

	frames, err := env.ProbeFrames(context.Background(), input, "image/jxl")
	if err != nil {
		t.Fatal(err)
	}
	if !AnyProfiled(frames) {
		t.Fatalf("ProbeFrames reports no ICC profile on a profiled JXL still: %+v", frames)
	}
	actual, cleanup, _, _, _, err := env.EncodeRung(context.Background(), input, "image/jxl",
		false, Rung{CeilingPx: 128, MaxFrames: 1}, frames, destination, "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	reference := filepath.Join(dir, "reference.webp")
	magick(t, env, append(LimitArgs(), input+"[0]", "-auto-orient", "-profile", destination,
		"-thumbnail", "128x128>", "-quality", strconv.Itoa(testQuality), reference)...)
	if diff := meanAbsoluteDifference(t, env, actual, reference); diff > 0.004 {
		t.Fatalf("corrected WebP differs from explicit sRGB reference by %.6f, want <= 0.004", diff)
	}
}

// The frames of the animated WebP carry an Adobe RGB profile ImageMagick does
// not read from an animation, so the conversion comes from the compositor's
// report of the profile with each frame.
func TestEncodeRungWideGamutAnimationConvertsBeforeWebP(t *testing.T) {
	requireWebPBuilders(t)
	env := requireMagick(t)
	destination := writeSRGBProfile(t)
	adobe := writeAdobeProfile(t, t.TempDir())
	adobeBytes, err := os.ReadFile(adobe)
	if err != nil {
		t.Fatal(err)
	}
	input := withWebPChunk(t, buildWebP(t, webpCase{codec: "lossless", frames: 2}), "icc", adobeBytes)

	frames, err := env.ProbeFrames(context.Background(), input, "image/webp")
	if err != nil {
		t.Fatal(err)
	}

	// The stored pixels, converted from Adobe RGB and not.
	dir := t.TempDir()
	var convertedArgs, plainArgs []string
	for k := range 2 {
		path := filepath.Join(dir, fmt.Sprintf("f%d.png", k))
		var buf bytes.Buffer
		if err := png.Encode(&buf, webpFrameImage(k, webpCanvasW, webpCanvasH, false)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		convertedArgs = append(convertedArgs, "(", "-delay", "10", path, "-profile", adobe, "-profile", destination, ")")
		plainArgs = append(plainArgs, "(", "-delay", "10", path, ")")
	}
	reference := filepath.Join(dir, "reference.webp")
	unconverted := filepath.Join(dir, "unconverted.webp")
	magick(t, env, append(convertedArgs, "-quality", strconv.Itoa(testQuality), reference)...)
	magick(t, env, append(plainArgs, "-quality", strconv.Itoa(testQuality), unconverted)...)

	// An empty profilePath, as for a caller that saw no profile in the
	// headers, falls back to the package's own sRGB profile.
	for _, profilePath := range []string{destination, ""} {
		actual, cleanup, _, _, mapping, err := env.EncodeRung(context.Background(), input, "image/webp",
			true, Rung{CeilingPx: 128, MaxFrames: 2}, frames, profilePath, "", testQuality)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if len(mapping) != 2 {
			t.Fatalf("animation mapping has %d frames, want 2", len(mapping))
		}
		for i := range mapping {
			diff := animationFrameDifference(t, env, actual, reference, i)
			plain := animationFrameDifference(t, env, actual, unconverted, i)
			t.Logf("profilePath %q frame %d: %.6f from the converted reference, %.6f from the unconverted one", profilePath, i, diff, plain)
			if diff > 0.004 {
				t.Errorf("profilePath %q: corrected animation frame %d differs from explicit sRGB reference by %.6f, want <= 0.004", profilePath, i, diff)
			}
			if plain < 0.01 {
				t.Errorf("profilePath %q: corrected animation frame %d differs from unconverted reinterpretation by %.6f, want >= 0.01", profilePath, i, plain)
			}
		}
	}
}

// The orientation the header read reports is applied to the composited
// frames, which carry none, before the resize.
func TestEncodeRungAppliesTheHeaderOrientationToAnimatedWebP(t *testing.T) {
	requireWebPBuilders(t)
	real := requireMagick(t)
	env := real
	env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		res, err := real.Run(ctx, name, args...)
		if slices.Contains(args, "identify") && slices.Contains(args, "-ping") {
			res.Stdout = bytes.ReplaceAll(res.Stdout, []byte("|Undefined|END"), []byte("|RightTop|END"))
		}
		return res, err
	}
	env = stagedEnv(env, t.TempDir())
	path := buildWebP(t, webpCase{codec: "lossless", blend: true, frames: 4})
	frames, err := env.ProbeFrames(context.Background(), path, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	_, cleanup, w, h, _, err := env.EncodeRung(context.Background(), path, "image/webp",
		true, Rung{CeilingPx: 128, MaxFrames: 4}, frames, "", "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if w != webpCanvasH || h != webpCanvasW {
		t.Errorf("artifact is %dx%d, want %dx%d", w, h, webpCanvasH, webpCanvasW)
	}
}

func TestEncodeRungUnprofiledStillIsNotColourConverted(t *testing.T) {
	env := requireMagick(t)
	withExtension := filepath.Join(t.TempDir(), "untagged.png")
	magick(t, env, "-size", "256x96", "gradient:#0020ff-#ffb000", "-strip", withExtension)
	input := renameToContent(t, withExtension)

	frames, err := env.ProbeFrames(context.Background(), input, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if AnyProfiled(frames) {
		t.Fatalf("unprofiled fixture reports an ICC profile: %+v", frames)
	}
	actual, cleanup, _, _, _, err := env.EncodeRung(context.Background(), input, "image/png",
		false, Rung{CeilingPx: 128, MaxFrames: 1}, frames, "", "", testQuality)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	unconverted := filepath.Join(t.TempDir(), "unconverted.webp")
	magick(t, env, append(LimitArgs(), input+"[0]", "-auto-orient",
		"-thumbnail", "128x128>", "-quality", strconv.Itoa(testQuality), unconverted)...)
	if diff := meanAbsoluteDifference(t, env, actual, unconverted); diff > 0.004 {
		t.Fatalf("unprofiled WebP differs from plain pipeline by %.6f, want <= 0.004", diff)
	}
}

func assertProfileBetween(t *testing.T, name string, args []string, profile, before, after string) {
	t.Helper()
	beforeAt := slices.Index(args, before)
	profileAt := slices.Index(args, "-profile")
	afterAt := slices.Index(args, after)
	if beforeAt < 0 || profileAt < 0 || afterAt < 0 {
		t.Fatalf("%s args = %q; need %q, -profile, and %q", name, args, before, after)
	}
	if profileAt+1 >= len(args) || args[profileAt+1] != profile {
		t.Fatalf("%s profile argument = %q, want -profile %q", name, args, profile)
	}
	if beforeAt >= profileAt || profileAt >= afterAt {
		t.Fatalf("%s color stage has indexes %s=%d, -profile=%d, %s=%d; want orientation < profile < thumbnail; args=%q", name, before, beforeAt, profileAt, after, afterAt, args)
	}
}

func assertNoProfile(t *testing.T, name string, args []string) {
	t.Helper()
	if at := slices.Index(args, "-profile"); at >= 0 {
		t.Fatalf("%s unexpectedly has -profile at %d: %q", name, at, args)
	}
}

// IdentifyArtifact needs the scene count and the first scene's size, which the
// artifact's container headers state. Without -ping identify decodes every
// scene: 94 ms for a 2048x1365 WebP still against 25 ms, on every rung of every
// item, and the whole animation on an animated artifact.
func TestIdentifyArtifactReadsHeadersOnly(t *testing.T) {
	real := requireMagick(t)
	env, calls := countingEnv(real)
	artifact := filepath.Join(t.TempDir(), "artifact.webp")
	magick(t, real, "-size", "12x8", "xc:red", "xc:blue", artifact)
	scenes, w, h, err := env.IdentifyArtifact(context.Background(), artifact)
	if err != nil || scenes != 2 || w != 12 || h != 8 {
		t.Fatalf("scenes %d size %dx%d err %v, want 2 scenes of 12x8", scenes, w, h, err)
	}
	for _, call := range calls() {
		if !slices.Contains(call, "-ping") {
			t.Errorf("the artifact was decoded to count its scenes: %q", call)
		}
	}
}
