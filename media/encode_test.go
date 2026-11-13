package media

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

const testQuality = 80

func TestProfileListDetection(t *testing.T) {
	t.Parallel()
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
}

// Frame 0 is always present, and the last frame appears only when the source
// is no longer than the budget.
func TestSelectFramesIsEvenlySpaced(t *testing.T) {
	t.Parallel()
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

func TestTimestampsClampShortDelays(t *testing.T) {
	t.Parallel()
	got := timestamps([]int{0, 5, 1, 20})
	want := []int64{0, 100, 150, 250}
	if !slices.Equal(got, want) {
		t.Errorf("timestamps = %v, want %v", got, want)
	}
}

// webpAtEdge is a WebP Encoding that fits edge by edge.
func webpAtEdge(edge int) Encoding {
	return Encoding{Format: WebP, Quality: testQuality, MaxEdge: edge, MaxArea: edge * edge}
}

// encodeFile opens path as the Processor does, makes one Encode of sel into a
// fresh directory and returns the path of the first image, and the images.
func encodeFile(t *testing.T, p *Processor, path string, opts OpenOptions, sel Select, enc Encoding) (string, []Image) {
	t.Helper()
	src, _, err := p.Open(context.Background(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, sel, enc)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, images[0].Name), images
}

func writeGrayAnimGIF(t *testing.T, env toolEnv, levels, delaysCS []int) string {
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
	return stage(t, extPath)
}

// Frames are sampled by source index, the mapping carries each frame's
// original timestamp, and a zero stored delay is clamped to the GIF display
// default.
func TestAnimationMapsSampledFrames(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	content := writeGrayAnimGIF(t, requireMagick(t), []int{10, 70, 130, 190}, []int{0, 20, 20, 20})

	enc := webpAtEdge(64)
	enc.Animate = true
	out, images := encodeFile(t, p, content, OpenOptions{}, Select{Frames: Even(2)}, enc)
	want := []Frame{{Index: 0, Time: msec(0)}, {Index: 2, Time: msec(300)}}
	if !slices.Equal(images[0].Frames, want) {
		t.Errorf("mapping = %v, want %v", images[0].Frames, want)
	}
	src, _, err := p.Open(context.Background(), out, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if got := src.Info().Animation.Frames; got != 2 {
		t.Errorf("the animation holds %d frames, want 2", got)
	}
}

func TestStillIsNeverUpscaled(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	extPath := filepath.Join(t.TempDir(), "src.png")
	magick(t, requireMagick(t), "-size", "40x20", "xc:blue", extPath)
	_, images := encodeFile(t, p, stage(t, extPath), OpenOptions{}, Select{}, webpAtEdge(640))
	if w, h := images[0].Width, images[0].Height; w != 40 || h != 20 {
		t.Errorf("encoded %dx%d, want the original 40x20", w, h)
	}
}

func writeSRGBProfile(t *testing.T) string {
	t.Helper()
	return writeFile(t, "sRGB2014.icc", srgbProfile)
}

func writeAdobeProfile(t *testing.T, dir string) string {
	t.Helper()
	adobe := filepath.Join(dir, "adobe-rgb-1998.icc")
	if err := os.WriteFile(adobe, mustAdobeProfile(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return adobe
}

func writeWideGamutStill(t *testing.T, env toolEnv, srgb string) string {
	t.Helper()
	dir := t.TempDir()
	adobe := writeAdobeProfile(t, dir)
	withExtension := filepath.Join(dir, "wide.png")
	magick(t, env, "-size", "256x96", "gradient:#0020ff-#ffb000", "-profile", srgb, "-profile", adobe, withExtension)
	return stage(t, withExtension)
}

// The fixture is stored in Adobe RGB, not merely tagged as sRGB: omitting the
// destination -profile gives a visibly different WebP.
func TestWideGamutStillConvertsBeforeWebP(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	destination := writeSRGBProfile(t)
	input := writeWideGamutStill(t, env, destination)

	actual, _ := encodeFile(t, p, input, OpenOptions{}, Select{}, webpAtEdge(128))

	dir := t.TempDir()
	reference := filepath.Join(dir, "reference.webp")
	unconverted := filepath.Join(dir, "unconverted.webp")
	magick(t, env, append(defaultResources().limitArgs(), input+"[0]", "-auto-orient", "-profile", destination,
		"-filter", "Lanczos", "-resize", "128x48!", "-strip", "-quality", strconv.Itoa(testQuality), reference)...)
	magick(t, env, append(defaultResources().limitArgs(), input+"[0]", "-auto-orient",
		"-filter", "Lanczos", "-resize", "128x48!", "-strip", "-quality", strconv.Itoa(testQuality), unconverted)...)

	if diff := meanAbsoluteDifference(t, env, actual, reference); diff > 0.004 {
		t.Fatalf("corrected WebP differs from explicit sRGB reference by %.6f, want <= 0.004", diff)
	}
	if diff := meanAbsoluteDifference(t, env, actual, unconverted); diff < 0.015 {
		t.Fatalf("corrected WebP differs from unconverted reinterpretation by %.6f, want >= 0.015; fixture no longer proves conversion", diff)
	}
}

// A JXL still stored in Adobe RGB must be converted like the PNG above,
// though ImageMagick's JXL coder reports no profile under -ping, which the
// header read uses.
func TestJXLStillIsConvertedFromItsProfile(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	destination := writeSRGBProfile(t)
	dir := t.TempDir()
	adobe := writeAdobeProfile(t, dir)
	withExtension := filepath.Join(dir, "wide.jxl")
	magick(t, env, "-size", "256x96", "gradient:#0020ff-#ffb000", "-profile", destination, "-profile", adobe, withExtension)
	if !profileListHasICC(string(magick(t, env, "identify", "-format", "%[profiles]", withExtension))) {
		t.Skip("this ImageMagick does not write an ICC profile into JXL")
	}
	input := stage(t, withExtension)

	actual, _ := encodeFile(t, p, input, OpenOptions{}, Select{}, webpAtEdge(128))
	reference := filepath.Join(dir, "reference.webp")
	magick(t, env, append(defaultResources().limitArgs(), input+"[0]", "-auto-orient", "-profile", destination,
		"-filter", "Lanczos", "-resize", "128x48!", "-strip", "-quality", strconv.Itoa(testQuality), reference)...)
	if diff := meanAbsoluteDifference(t, env, actual, reference); diff > 0.004 {
		t.Fatalf("corrected WebP differs from explicit sRGB reference by %.6f, want <= 0.004", diff)
	}
}

// The frames of the animated WebP carry an Adobe RGB profile ImageMagick does
// not read from an animation, so the conversion comes from the compositor's
// report of the profile with each frame.
func TestWideGamutAnimationConvertsBeforeWebP(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	env := requireMagick(t)
	p := requireProcessor(t)
	destination := writeSRGBProfile(t)
	adobe := writeAdobeProfile(t, t.TempDir())
	adobeBytes, err := os.ReadFile(adobe)
	if err != nil {
		t.Fatal(err)
	}
	input := withWebPChunk(t, buildWebP(t, webpCase{codec: "lossless", frames: 2}), "icc", adobeBytes)

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

	enc := webpAtEdge(128)
	enc.Animate = true
	actual, images := encodeFile(t, p, input, OpenOptions{}, Select{Frames: Even(2)}, enc)
	if len(images[0].Frames) != 2 {
		t.Fatalf("animation mapping has %d frames, want 2", len(images[0].Frames))
	}
	for i := range images[0].Frames {
		diff := animationFrameDifference(t, env, actual, reference, i, 2)
		plain := animationFrameDifference(t, env, actual, unconverted, i, 2)
		t.Logf("frame %d: %.6f from the converted reference, %.6f from the unconverted one", i, diff, plain)
		if diff > 0.004 {
			t.Errorf("corrected animation frame %d differs from explicit sRGB reference by %.6f, want <= 0.004", i, diff)
		}
		if plain < 0.01 {
			t.Errorf("corrected animation frame %d differs from unconverted reinterpretation by %.6f, want >= 0.01", i, plain)
		}
	}
}

func TestUnprofiledStillIsNotColourConverted(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	withExtension := filepath.Join(t.TempDir(), "untagged.png")
	magick(t, env, "-size", "256x96", "gradient:#0020ff-#ffb000", "-strip", withExtension)
	input := stage(t, withExtension)

	actual, _ := encodeFile(t, p, input, OpenOptions{}, Select{}, webpAtEdge(128))
	unconverted := filepath.Join(t.TempDir(), "unconverted.webp")
	magick(t, env, append(defaultResources().limitArgs(), input+"[0]", "-auto-orient",
		"-filter", "Lanczos", "-resize", "128x48!", "-strip", "-quality", strconv.Itoa(testQuality), unconverted)...)
	if diff := meanAbsoluteDifference(t, env, actual, unconverted); diff > 0.004 {
		t.Fatalf("unprofiled WebP differs from plain pipeline by %.6f, want <= 0.004", diff)
	}
}
