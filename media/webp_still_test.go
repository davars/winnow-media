package media

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// stillWebP encodes the image at pngPath as a still WebP and returns its path.
func stillWebP(t *testing.T, pngPath string, codec ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "still.webp")
	args := append([]string{"-quiet"}, codec...)
	run(t, "cwebp", append(args, pngPath, "-o", out)...)
	return out
}

// photoPNG is a w x h image with detail at every scale, as plasma has;
// alpha gives it an opacity that falls from the centre outwards.
func photoPNG(t *testing.T, env toolEnv, w, h int, alpha bool) string {
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

func withOrientation(t *testing.T, env toolEnv, path string, n int) string {
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

// encoded is an output file of an encode, with its size.
type encoded struct {
	Path          string
	Width, Height int
	Frames        []Frame
}

// encodeStill makes one Encode of the first image of src into a fresh
// directory and returns the output files.
func encodeStill(t *testing.T, src *Source, targets ...Encoding) ([]encoded, error) {
	t.Helper()
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, Select{}, targets...)
	if err != nil {
		return nil, err
	}
	out := make([]encoded, len(images))
	for i, img := range images {
		out[i] = encoded{Path: filepath.Join(dir, img.Name), Width: img.Width, Height: img.Height, Frames: img.Frames}
	}
	return out, nil
}

// magickReference encodes the WebP at path at target the way ImageMagick's
// own decode does: one read of the whole image, oriented, converted and
// resized in one run, with the size ImageMagick computes. It returns the
// output and the runs made.
func magickReference(t *testing.T, path string, opts OpenOptions, target Encoding) (encoded, [][]string) {
	t.Helper()
	env, calls := countingEnv(requireMagick(t))
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
	orientation, err := orientationName(opts.Orientation)
	if err != nil {
		t.Fatal(err)
	}
	enc := mustResolve(t, target)
	out := filepath.Join(t.TempDir(), "reference"+enc.ext)
	before := len(calls())
	res, err := env.runEncode(ctx, encodeArgs([]encodeFrame{frame}, encodeSource{orientation: orientation}, enc,
		encodeOutput{kind: outputStill, path: out}), encodeCheck{frames: 1})
	if err != nil {
		t.Fatal(err)
	}
	return encoded{Path: out, Width: res[0].width, Height: res[0].height}, calls()[before:]
}

// An animation, including one of a single frame, is composited by webp-frames
// whatever the targets; a still WebP is not.
func TestAnimatedWebPIsComposited(t *testing.T) {
	t.Parallel()
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
			p, rec := sourceProcessor(t)
			src := openSourceOn(t, p, path, OpenOptions{})
			if src.hdr.route == routeStill {
				t.Fatal("the source is routed as a still")
			}
			if _, err := encodeStill(t, src, target); err != nil {
				t.Fatal(err)
			}
			if n := webpFramesRuns(rec.calls); n != 1 {
				t.Errorf("webp-frames ran %d times, want 1", n)
			}
		})
	}
}

// webpFramesRuns counts the runs of webp-frames among calls.
func webpFramesRuns(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if c[0] == webpFramesTool {
			n++
		}
	}
	return n
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

// An animation whose RIFF size is 12 or 20 bytes more than the file is repaired
// as ImageMagick repairs it, in the tool too: it composites the same frames as
// the file with the right size, and another excess is refused as ImageMagick
// refuses it.
func TestAnimatedWebPWithAnOversizedRIFFSizeIsRepaired(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	real := requireMagick(t)
	exact := mustReadFile(t, buildWebP(t, webpCase{codec: "lossy", frames: 3}))
	target := Encoding{Format: PNG, MaxEdge: 20, MaxArea: 400}
	encode := func(t *testing.T, data []byte) (int, error) {
		src, _, err := requireProcessor(t).Open(context.Background(), writeFile(t, "a.webp", data), OpenOptions{})
		if err != nil {
			return 0, err
		}
		defer func() { _ = src.Close() }()
		images, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: All()}, target)
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

// A still WebP is decoded by ImageMagick, in the one run that encodes it, and
// the output is what ImageMagick's own decode gives, pixel for pixel, whatever
// the codec.
func TestStillWebPIsDecodedByImageMagick(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	real := requireMagick(t)
	opaque, translucent := photoPNG(t, real, 1800, 1200, false), photoPNG(t, real, 1800, 1200, true)
	target := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}
	for name, path := range map[string]string{
		"lossy":          stillWebP(t, opaque, "-q", "80"),
		"lossless":       stillWebP(t, opaque, "-lossless"),
		"lossy, alpha":   stillWebP(t, translucent, "-q", "80"),
		"lossless alpha": stillWebP(t, translucent, "-lossless"),
	} {
		t.Run(name, func(t *testing.T) {
			p, rec := sourceProcessor(t)
			out, err := encodeStill(t, openSourceOn(t, p, path, OpenOptions{}), target)
			if err != nil {
				t.Fatal(err)
			}
			if n := webpFramesRuns(rec.calls); n != 0 {
				t.Errorf("webp-frames ran %d times for a still", n)
			}
			if n := fullReads(rec.calls); n != 0 {
				t.Errorf("%d whole-image identify reads; the profile of this file is certain at the header", n)
			}
			encodes := 0
			for _, c := range rec.calls {
				if c[0] == "magick" && c[1] != "identify" {
					encodes++
				}
			}
			if encodes != 1 {
				t.Errorf("%d magick encode runs, want the one that decodes and encodes", encodes)
			}
			ref, _ := magickReference(t, path, OpenOptions{}, target)
			if out[0].Width != ref.Width || out[0].Height != ref.Height {
				t.Fatalf("output %dx%d, ImageMagick's %dx%d", out[0].Width, out[0].Height, ref.Width, ref.Height)
			}
			if mean, _ := pngDifference(t, out[0].Path, ref.Path); mean != 0 {
				t.Errorf("output differs from ImageMagick's by %.4f levels", mean)
			}
		})
	}
}

// A profile in a still WebP is converted to sRGB, not just recorded: the
// Adobe RGB file comes out as ImageMagick's conversion, and not as the same
// pixels without the profile.
func TestStillWebPConvertsItsProfile(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	real := requireMagick(t)
	plain := stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80")
	profiled := withWebPChunk(t, plain, "icc", mustAdobeProfile(t))
	target := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}
	encode := func(path string) encoded {
		p, _ := sourceProcessor(t)
		out, err := encodeStill(t, openSourceOn(t, p, path, OpenOptions{}), target)
		if err != nil {
			t.Fatal(err)
		}
		return out[0]
	}
	converted, unconverted := encode(profiled), encode(plain)
	ref, _ := magickReference(t, profiled, OpenOptions{}, target)
	if mean, _ := pngDifference(t, converted.Path, ref.Path); mean != 0 {
		t.Errorf("the conversion differs from ImageMagick's by %.4f", mean)
	}
	if mean, _ := pngDifference(t, converted.Path, unconverted.Path); mean < 3 {
		t.Errorf("the profile changed the pixels by only %.3f levels: it was labelled, not converted", mean)
	}
}

// A still WebP is turned by the orientation of its EXIF chunk, as ImageMagick
// turns it, and by the one OpenOptions gives instead.
func TestStillWebPOrientationsTurnTheOutput(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	real := requireMagick(t)
	simple := mustReadFile(t, stillWebP(t, photoPNG(t, real, 1800, 1200, false), "-q", "80"))
	target := Encoding{Format: PNG, MaxEdge: 400, MaxArea: 400 * 400}
	check := func(t *testing.T, path string, opts OpenOptions) {
		p, _ := sourceProcessor(t)
		out, err := encodeStill(t, openSourceOn(t, p, path, opts), target)
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := magickReference(t, path, opts, target)
		if out[0].Width != ref.Width || out[0].Height != ref.Height {
			t.Fatalf("output %dx%d, ImageMagick's %dx%d", out[0].Width, out[0].Height, ref.Width, ref.Height)
		}
		if mean, _ := pngDifference(t, out[0].Path, ref.Path); mean != 0 {
			t.Errorf("output differs from ImageMagick's by %.4f levels", mean)
		}
	}
	for n := 1; n <= 8; n++ {
		t.Run("EXIF "+strconv.Itoa(n), func(t *testing.T) {
			check(t, webpWithEXIF(t, simple, 1800, 1200, true, tiffOrientation(binary.LittleEndian, 3, 1, uint16(n))), OpenOptions{})
		})
	}
	t.Run("override", func(t *testing.T) {
		check(t, webpWithEXIF(t, simple, 1800, 1200, true, tiffOrientation(binary.LittleEndian, 3, 1, 3)), OpenOptions{Orientation: 6})
	})
}

// A WebP over the still caps at its stored size is refused at the header,
// before anything decodes it: not an ImageMagick read of the whole image for
// its profile either.
func TestWebPOverTheStoredSizeCapIsRefusedBeforeAnyDecode(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	real := requireMagick(t)
	big := filepath.Join(t.TempDir(), "big.png")
	magick(t, real, "-size", "7300x7200", "xc:gray", big)
	lossy := mustReadFile(t, stillWebP(t, big, "-q", "5", "-m", "0"))
	lossless := mustReadFile(t, stillWebP(t, big, "-lossless", "-z", "0"))
	twoProfiles := append(riffChunk("ICCP", []byte("a")), riffChunk("ICCP", []byte("b"))...)
	for name, path := range map[string]string{
		"lossy":             writeFile(t, "big.webp", lossy),
		"lossless":          writeFile(t, "big.webp", lossless),
		"uncertain profile": webpWithFlags(t, lossy, 7300, 7200, webpICCFlag, twoProfiles, nil),
	} {
		t.Run(name, func(t *testing.T) {
			p, rec := sourceProcessorWith(t, Config{Caps: measuredCapsConfig})
			_, _, err := p.Open(context.Background(), path, OpenOptions{})
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("err = %v, want ErrTooLarge", err)
			}
			for _, c := range rec.calls {
				if fullReads([][]string{c}) > 0 {
					t.Errorf("ran %q before refusing", c)
				}
			}
		})
	}
}

// A file whose chunk list does not settle its profile is read whole once, for
// the profile.
func TestOneWholeImageReadForAnUncertainWebPProfile(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	real := requireMagick(t)
	simple := mustReadFile(t, stillWebP(t, photoPNG(t, real, 900, 600, false), "-q", "80"))
	// An empty ICCP chunk leaves the profile for a whole-image read to settle.
	path := webpWithFlags(t, simple, 900, 600, webpICCFlag, riffChunk("ICCP", nil), nil)
	if _, ok := webpICC(inputAt(t, path)); ok {
		t.Fatal("the profile is not uncertain")
	}
	p, rec := sourceProcessor(t)
	if _, err := encodeStill(t, openSourceOn(t, p, path, OpenOptions{}), Encoding{Format: PNG, MaxEdge: 200, MaxArea: 200 * 200}); err != nil {
		t.Fatal(err)
	}
	if n := fullReads(rec.calls); n != 1 {
		t.Errorf("%d whole-image reads, want 1", n)
	}
}
