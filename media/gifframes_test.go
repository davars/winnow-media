package media

import (
	"bytes"
	"compress/lzw"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// gifFrameSpec describes one frame of a generated GIF.
type gifFrameSpec struct {
	rect     image.Rectangle
	disposal byte
	delay    int
	palette  color.Palette
	shift    int // moves the pattern, so frames differ
}

var (
	paletteA = color.Palette{
		color.RGBA{},
		color.RGBA{255, 0, 0, 255},
		color.RGBA{0, 255, 0, 255},
		color.RGBA{0, 0, 255, 255},
		color.RGBA{250, 250, 20, 255},
	}
	// paletteB holds the transparent entry in the middle and other colours.
	paletteB = color.Palette{
		color.RGBA{10, 20, 30, 255},
		color.RGBA{200, 100, 50, 255},
		color.RGBA{},
		color.RGBA{0, 120, 250, 255},
		color.RGBA{90, 90, 90, 255},
	}
)

// buildGIF encodes frames on a w x h canvas. The pattern in each frame mixes
// transparent and opaque pixels so that disposal and compositing show.
func buildGIF(t *testing.T, w, h int, global color.Palette, frames []gifFrameSpec) []byte {
	t.Helper()
	g := &gif.GIF{Config: image.Config{ColorModel: global, Width: w, Height: h}}
	for _, f := range frames {
		img := image.NewPaletted(f.rect, f.palette)
		for y := f.rect.Min.Y; y < f.rect.Max.Y; y++ {
			for x := f.rect.Min.X; x < f.rect.Max.X; x++ {
				img.SetColorIndex(x, y, uint8((x*3+y*5+f.shift)%len(f.palette)))
			}
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, f.delay)
		g.Disposal = append(g.Disposal, f.disposal)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func readNRGBA(t *testing.T, path string) *image.NRGBA {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := image.NewNRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}

func TestCompositorMatchesMagickCoalesce(t *testing.T) {
	env := requireMagick(t)
	const w, h = 24, 20
	full := image.Rect(0, 0, w, h)
	rects := []image.Rectangle{full, image.Rect(4, 3, 16, 12), image.Rect(10, 8, 24, 20), image.Rect(0, 5, 9, 14), image.Rect(2, 0, 20, 7)}
	sequence := func(disposals []byte, palettes []color.Palette, firstFull bool) []gifFrameSpec {
		var out []gifFrameSpec
		for i, d := range disposals {
			r := rects[i%len(rects)]
			if i == 0 && firstFull {
				r = full
			}
			out = append(out, gifFrameSpec{rect: r, disposal: d, delay: 5, palette: palettes[i%len(palettes)], shift: i})
		}
		return out
	}
	same := func(n int, d byte) []byte { return bytes.Repeat([]byte{d}, n) }
	one := []color.Palette{paletteA}
	cases := map[string][]gifFrameSpec{
		"disposal unspecified":       sequence(same(5, gif.DisposalNone), one, true),
		"disposal none":              sequence(same(5, 1), one, true),
		"disposal background":        sequence(same(5, gif.DisposalBackground), one, true),
		"disposal previous":          sequence(same(5, gif.DisposalPrevious), one, true),
		"mixed disposals":            sequence([]byte{1, 2, 3, 2, 3, 1, 3}, one, true),
		"first frame smaller":        sequence([]byte{2, 1, 3, 2, 1}, one, false),
		"local palettes":             sequence([]byte{1, 2, 3, 1, 2}, []color.Palette{paletteA, paletteB}, true),
		"local palettes, background": sequence(same(4, 2), []color.Palette{paletteB, paletteA}, true),
	}
	zero := sequence([]byte{1, 2, 3, 2, 1}, one, true)
	zero[2].delay = 0
	cases["zero delay frame"] = zero
	allZero := sequence([]byte{1, 2, 3, 2, 1}, one, true)
	for i := range allZero {
		allZero[i].delay = 0
	}
	cases["all frames zero delay"] = allZero
	// The logical screen's background index names an opaque colour.
	opaqueBG := sequence(same(5, 2), []color.Palette{paletteB}, true)
	cases["opaque background colour, disposal background"] = opaqueBG
	globals := map[string]color.Palette{"opaque background colour, disposal background": paletteB}
	opaque := sequence([]byte{1, 2, 3, 1}, []color.Palette{paletteB[:2:2]}, true)
	cases["opaque palette"] = opaque

	globalFor := func(name string) color.Palette {
		if p, ok := globals[name]; ok {
			return p
		}
		return paletteA
	}
	noGCE := writeFile(t, "nogce.gif", withoutGCE(t, 5, false))
	check := func(t *testing.T, path string) {
		t.Helper()
		want := t.TempDir()
		magick(t, env, "gif:"+path, "-coalesce", "-depth", "8", "+adjoin", filepath.Join(want, "%03d.png"))
		wantNames := dirNames(t, want)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := newGIFDecoder(data, 1000)
		if err != nil {
			t.Fatal(err)
		}
		layout := dec.layout
		var all []int
		for i := range wantNames {
			all = append(all, i)
		}
		var frames []*image.NRGBA
		err = compositeGIF(dec.frame, layout.width, layout.height, layout.own, all, func(_ int, c *image.NRGBA, _ int) error {
			cp := image.NewNRGBA(c.Bounds())
			copy(cp.Pix, c.Pix)
			frames = append(frames, cp)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		gotNames := wantNames[:len(frames)]
		if len(gotNames) != len(wantNames) {
			t.Fatalf("compositor produced %d frames, magick %d", len(gotNames), len(wantNames))
		}
		for i := range wantNames {
			a, b := frames[i], readNRGBA(t, filepath.Join(want, wantNames[i]))
			if a.Bounds() != b.Bounds() {
				t.Fatalf("frame %d: compositor %v, magick %v", i, a.Bounds(), b.Bounds())
			}
			var alphaDiff, colourDiff int
			var first string
			for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
				for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
					p, q := a.NRGBAAt(x, y), b.NRGBAAt(x, y)
					if p.A != q.A {
						alphaDiff++
					} else if p.A > 0 && p != q {
						colourDiff++
					} else {
						continue
					}
					if first == "" {
						first = fmt.Sprintf("(%d,%d) compositor %v magick %v", x, y, p, q)
					}
				}
			}
			if alphaDiff+colourDiff > 0 {
				t.Errorf("frame %d: %d alpha and %d colour differences, first %s", i, alphaDiff, colourDiff, first)
			}
		}
	}
	t.Run("frames without a graphic control extension", func(t *testing.T) { check(t, noGCE) })
	t.Run("frames without a graphic control extension, interlaced", func(t *testing.T) {
		check(t, writeFile(t, "i.gif", withoutGCE(t, 5, true)))
	})
	t.Run("no trailer", func(t *testing.T) {
		data := buildGIF(t, w, h, paletteA, cases["mixed disposals"])
		if data[len(data)-1] != 0x3B {
			t.Fatal("fixture does not end with a trailer")
		}
		check(t, writeFile(t, "x.gif", data[:len(data)-1]))
	})
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			check(t, writeFile(t, "x.gif", buildGIF(t, w, h, globalFor(name), frames)))
		})
		t.Run(name+", interlaced", func(t *testing.T) {
			plain := writeFile(t, "x.gif", buildGIF(t, w, h, globalFor(name), frames))
			interlaced := filepath.Join(filepath.Dir(plain), "i.gif")
			magick(t, env, plain, "-interlace", "GIF", interlaced)
			if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%[interlace]\n", interlaced))); !strings.HasPrefix(got, "GIF") {
				t.Fatalf("fixture is not interlaced: %q", got)
			}
			check(t, interlaced)
		})
	}
}

// withoutGCE builds a GIF of n frames of one palette index each, alternating,
// none of which has a graphic control extension.
func withoutGCE(t *testing.T, n int, interlaced bool) []byte {
	t.Helper()
	const w, h = 40, 30
	b := []byte("GIF89a")
	b = binary.LittleEndian.AppendUint16(b, w)
	b = binary.LittleEndian.AppendUint16(b, h)
	b = append(b, 0x80, 0, 0, 0, 0, 0, 255, 255, 255)
	for i := range n {
		var packed bytes.Buffer
		zw := lzw.NewWriter(&packed, lzw.LSB, 8)
		row := bytes.Repeat([]byte{byte(i % 2)}, w)
		for range h {
			if _, err := zw.Write(row); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		b = append(b, 0x2C, 0, 0, 0, 0)
		b = binary.LittleEndian.AppendUint16(b, w)
		b = binary.LittleEndian.AppendUint16(b, h)
		flags := byte(0)
		if interlaced {
			flags = 0x40
		}
		b = append(b, flags, 8)
		for d := packed.Bytes(); len(d) > 0; {
			k := min(len(d), 255)
			b = append(b, byte(k))
			b = append(b, d[:k]...)
			d = d[k:]
		}
		b = append(b, 0)
	}
	return append(b, 0x3B)
}

// TestPAMReadsBackIdentically checks that magick reads a written canvas
// as the same pixels, alpha included.
func TestPAMReadsBackIdentically(t *testing.T) {
	env := requireMagick(t)
	const w, h = 24, 20
	g := buildGIF(t, w, h, paletteA, []gifFrameSpec{
		{rect: image.Rect(0, 0, w, h), disposal: 1, delay: 5, palette: paletteB, shift: 0},
		{rect: image.Rect(4, 3, 16, 12), disposal: 1, delay: 5, palette: paletteA, shift: 3},
	})
	var canvas *image.NRGBA
	dec, err := newGIFDecoder(g, 10)
	if err != nil {
		t.Fatal(err)
	}
	err = compositeGIF(dec.frame, w, h, nil, []int{1}, func(_ int, c *image.NRGBA, _ int) error { canvas = c; return nil })
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writePAM(&buf, canvas); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pam := writeFile(t, "x.pam", buf.Bytes())
	out := filepath.Join(dir, "x.png")
	magick(t, env, "pam:"+pam, "-depth", "8", out)
	got := readNRGBA(t, out)
	for y := range h {
		for x := range w {
			p, q := canvas.NRGBAAt(x, y), got.NRGBAAt(x, y)
			if p.A != q.A || (p.A > 0 && p != q) {
				t.Fatalf("(%d,%d): canvas %v, read back %v", x, y, p, q)
			}
		}
	}
}

// compositorPeakRSS runs the compositor as a process over a one-colour GIF of
// n frames of w x h and returns its peak resident set, having asked for the
// first and last frame.
func compositorPeakRSS(t *testing.T, w, h, n int) int64 {
	t.Helper()
	path := writeFile(t, "x.gif", oneColourGIF(t, w, h, n))
	dir := t.TempDir()
	args := []string{
		path, dir, "2", strconv.Itoa(n), strconv.Itoa(w), strconv.Itoa(h),
		fmt.Sprintf("0,%d", n-1), strings.Repeat("0", n), "60000",
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), compositorEnv+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compositor: %v\n%s", err, out)
	}
	if got := dirNames(t, dir); len(got) != 2 {
		t.Fatalf("compositor wrote %v, want 2 frames", got)
	}
	usage := UsageOf(cmd.ProcessState)
	if usage == nil {
		t.Skip("no resource usage on this platform")
	}
	return usage.MaxRSS
}

// TestCompositorMemoryDoesNotGrowWithFrameCount holds the canvas fixed and
// multiplies the frames by ten. Holding every decoded frame would add 90
// frames of 1.5 MB, 135 MB; one frame at a time adds nothing but noise.
func TestCompositorMemoryDoesNotGrowWithFrameCount(t *testing.T) {
	const w, h = 1500, 1000
	few := compositorPeakRSS(t, w, h, 10)
	many := compositorPeakRSS(t, w, h, 100)
	const bound = 48 << 20
	t.Logf("peak RSS: 10 frames %d MiB, 100 frames %d MiB", few>>20, many>>20)
	if many-few > bound {
		t.Errorf("100 frames peaked %d MiB above 10 frames, the bound is %d MiB", (many-few)>>20, bound>>20)
	}
}

// TestFrameWithoutGraphicControlInheritsDisposal checks that a frame without
// a graphic control extension has the disposal of the frame before it, as in
// ImageMagick.
func TestFrameWithoutGraphicControlInheritsDisposal(t *testing.T) {
	env := requireMagick(t)
	const w, h = 40, 30
	// Frame 0 has disposal 2 and a small rectangle; frame 1 has no extension.
	g := buildGIF(t, w, h, paletteA, []gifFrameSpec{
		{rect: image.Rect(0, 0, w, h), disposal: gif.DisposalNone, delay: 5, palette: paletteA, shift: 0},
		{rect: image.Rect(5, 5, 25, 20), disposal: gif.DisposalBackground, delay: 5, palette: paletteA, shift: 1},
		{rect: image.Rect(10, 8, 30, 25), disposal: gif.DisposalNone, delay: 5, palette: paletteA, shift: 2},
		{rect: image.Rect(0, 0, w, h), disposal: gif.DisposalNone, delay: 5, palette: paletteB, shift: 3},
	})
	// Remove the graphic control extension of frame 2, which then has frame
	// 1's disposal.
	i := bytes.Index(g, []byte{0x21, 0xF9, 0x04})
	for range 2 {
		i += 1 + bytes.Index(g[i+1:], []byte{0x21, 0xF9, 0x04})
	}
	g = append(g[:i:i], g[i+8:]...)
	path := writeFile(t, "x.gif", g)
	want := t.TempDir()
	magick(t, env, "gif:"+path, "-coalesce", "-depth", "8", "+adjoin", filepath.Join(want, "%03d.png"))
	dec, err := newGIFDecoder(g, 10)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	err = compositeGIF(dec.frame, w, h, nil, []int{0, 1, 2, 3}, func(i int, c *image.NRGBA, _ int) error {
		n++
		b := readNRGBA(t, filepath.Join(want, fmt.Sprintf("%03d.png", i)))
		for y := range h {
			for x := range w {
				p, q := c.NRGBAAt(x, y), b.NRGBAAt(x, y)
				if p.A != q.A || (p.A > 0 && p != q) {
					t.Fatalf("frame %d (%d,%d): compositor %v, magick %v", i, x, y, p, q)
				}
			}
		}
		return nil
	})
	if err != nil || n != 4 {
		t.Fatalf("composited %d frames: %v", n, err)
	}
}
