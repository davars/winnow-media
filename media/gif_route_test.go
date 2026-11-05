package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// forbidCompositor fails the test if the processor starts the GIF
// compositor, and restores it when the test ends.
func forbidCompositor(t *testing.T, p *Processor) {
	t.Helper()
	old := p.compose
	t.Cleanup(func() { p.compose = old })
	p.compose = func(context.Context, composeRequest, func(string, bool) error) (*Usage, error) {
		t.Error("the compositor was started")
		return nil, nil
	}
}

func TestGIFFramesGoToMagickInBatches(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	// Two 64x48 frames per run.
	old := encodeBatchPixels
	encodeBatchPixels = 2 * 64 * 48
	t.Cleanup(func() { encodeBatchPixels = old })
	rec := record(p)
	images, out := framesInto(t, p, writeGIF(t, env, solid, 12), Encoding{}, 6)
	if len(images) != 6 {
		t.Fatalf("%d images, want 6", len(images))
	}
	var frames []int
	for _, im := range images {
		frames = append(frames, im.Frame)
	}
	if !slices.Equal(frames, []int{0, 2, 4, 6, 8, 10}) {
		t.Errorf("source frames %v", frames)
	}
	inputs, runs := 0, 0
	for _, call := range rec.calls {
		if call[0] != "magick" || slices.Contains(call, "identify") {
			continue
		}
		n := 0
		for _, a := range call[1:] {
			if strings.HasPrefix(a, "pam:") {
				n++
			}
		}
		if n > 2 {
			t.Errorf("magick run names %d frames, the batch is 2: %q", n, call)
		}
		inputs += n
		runs++
	}
	if inputs != 6 || runs != 3 {
		t.Errorf("%d inputs in %d runs, want 6 in 3", inputs, runs)
	}
	if got := dirNames(t, out); len(got) != 6 {
		t.Errorf("output dir holds %v", got)
	}
}

func TestScanGIF(t *testing.T) {
	good := buildGIF(t, 24, 20, paletteA, []gifFrameSpec{
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA},
		{rect: image.Rect(4, 3, 16, 12), disposal: 1, delay: 5, palette: paletteB},
	})
	if l, err := scanGIF(good, 2); err != nil || l.frames != 2 || l.width != 24 || l.height != 20 || l.missingTrailer {
		t.Fatalf("scan of a good GIF = %+v, %v", l, err)
	}
	if l, err := scanGIF(good[:len(good)-1], 2); err != nil || l.frames != 2 || !l.missingTrailer {
		t.Fatalf("scan without the trailer = %+v, %v", l, err)
	}
	wide := append([]byte{}, good...)
	binary.LittleEndian.PutUint16(wide[6:], 8)
	cases := map[string][]byte{
		"truncated in a frame":       good[:len(good)/2],
		"truncated header":           good[:9],
		"frame outside the canvas":   wide,
		"unknown block":              append(append([]byte{}, good[:len(good)-1]...), 0x55, 0x3B),
		"not a GIF":                  []byte("GIF90a" + strings.Repeat("\x00", 20)),
		"no frames":                  append(append([]byte{}, good[:10]...), 0, 0, 0, 0x3B),
		"more frames than the limit": good,
	}
	for name, data := range cases {
		limit := 2
		if name == "more frames than the limit" {
			limit = 1
		}
		if _, err := scanGIF(data, limit); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestBrokenGIFsFailCleanly runs GIFs the header read accepts but the
// compositor cannot decode through AnimationFrames.
func TestBrokenGIFsFailCleanly(t *testing.T) {
	requireMagick(t)
	p := requireProcessor(t)
	good := buildGIF(t, 24, 20, paletteA, []gifFrameSpec{
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA},
		{rect: image.Rect(4, 3, 16, 12), disposal: 1, delay: 5, palette: paletteB, shift: 1},
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA, shift: 2},
	})
	wide := append([]byte{}, good...)
	binary.LittleEndian.PutUint16(wide[6:], 8)
	for name, data := range map[string][]byte{
		"truncated":                good[:len(good)-40],
		"garbage after last frame": append(append([]byte{}, good[:len(good)-1]...), 0x55, 0x3B),
		"frame outside the canvas": wide,
	} {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			_, _, err := p.AnimationFrames(context.Background(), writeFile(t, "x.gif", data), Encoding{}, 0, out)
			if err == nil {
				t.Fatal("no error")
			}
			if got := dirNames(t, out); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}
}

func TestRunCompositorRefusesBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"too few":            {"a"},
		"not a count":        {"a", "b", "x", "1", "1", "1", "0"},
		"frame out of range": {"a", "b", "1", "2", "1", "1", "5"},
		"missing file":       {"/nonexistent.gif", t.TempDir(), "1", "1", "1", "1", "0"},
	} {
		if err := runCompositor(args, &strings.Builder{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// iccExtensionBlock returns the application extension that carries profile.
func iccExtensionBlock(profile []byte) []byte {
	b := append([]byte{0x21, 0xFF, 0x0B}, iccExtension...)
	for len(profile) > 0 {
		k := min(len(profile), 255)
		b = append(b, byte(k))
		b = append(b, profile[:k]...)
		profile = profile[k:]
	}
	return append(b, 0)
}

// withICC returns g with profile inserted before the n-th image, for each n
// in before; n equal to the number of images puts it after the last.
func withICC(t *testing.T, g, profile []byte, before ...int) []byte {
	t.Helper()
	pos := 13
	if flags := g[10]; flags&0x80 != 0 {
		pos += 3 << (1 + flags&7)
	}
	starts := []int{pos} // where an extension for image 0 may go
	for g[pos] != 0x3B {
		switch g[pos] {
		case 0x21:
			pos += 2
			for g[pos] != 0 {
				pos += 1 + int(g[pos])
			}
			pos++
		case 0x2C:
			flags := g[pos+9]
			pos += 10
			if flags&0x80 != 0 {
				pos += 3 << (1 + flags&7)
			}
			pos++
			for g[pos] != 0 {
				pos += 1 + int(g[pos])
			}
			pos++
			starts = append(starts, pos)
		}
	}
	var out []byte
	last := 0
	for _, n := range before {
		out = append(out, g[last:starts[n]]...)
		out = append(out, iccExtensionBlock(profile)...)
		last = starts[n]
	}
	return append(out, g[last:]...)
}

func TestScanGIFProfile(t *testing.T) {
	adobe := mustAdobeProfile(t)
	other := append([]byte{}, adobe...)
	other[len(other)-1] ^= 0xFF
	g := buildGIF(t, 24, 20, paletteA, []gifFrameSpec{
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA},
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA, shift: 1},
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA, shift: 2},
	})
	for name, c := range map[string]struct {
		data []byte
		from int
	}{
		"none":                 {g, -1},
		"before the first":     {withICC(t, g, adobe, 0), 0},
		"before the second":    {withICC(t, g, adobe, 1), 1},
		"after the last":       {withICC(t, g, adobe, 3), -1},
		"the same one twice":   {withICC(t, g, adobe, 0, 2), 0},
		"first of two applies": {withICC(t, g, adobe, 1, 1), 1},
	} {
		l, err := scanGIF(c.data, 10)
		if err != nil || l.profileFrom != c.from || (c.from >= 0 && !bytes.Equal(l.profile, adobe)) {
			t.Errorf("%s: from %d, err %v, want from %d", name, l.profileFrom, err, c.from)
		}
	}
	two := withICC(t, withICC(t, g, adobe, 2), other, 0)
	if _, err := scanGIF(two, 10); err == nil || !strings.Contains(err.Error(), "more than one colour profile") {
		t.Errorf("two different profiles: err = %v", err)
	}
}

func mustAdobeProfile(t *testing.T) []byte {
	t.Helper()
	profile, err := base64.StdEncoding.DecodeString(adobeRGB1998Profile)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestGIFWithoutTrailerGivesTheSameFrames(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	full := writeGIF(t, env, solid, 6)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if data[len(data)-1] != 0x3B {
		t.Fatal("fixture has no trailer")
	}
	want, _ := framesInto(t, p, full, Encoding{Format: PNG}, 0)
	got, out := framesInto(t, p, writeFile(t, "x.gif", data[:len(data)-1]), Encoding{Format: PNG}, 0)
	if len(got) != len(want) || len(got) != 6 {
		t.Fatalf("%d frames without the trailer, %d with, want 6", len(got), len(want))
	}
	for i, im := range got {
		if im.Frame != want[i].Frame {
			t.Errorf("image %d shows frame %d, want %d", i, im.Frame, want[i].Frame)
		}
	}
	if names := dirNames(t, out); len(names) != 6 {
		t.Errorf("output dir holds %v", names)
	}
}

// TestProfiledGIFsMatchImageMagick runs GIFs carrying an Adobe RGB profile
// through AnimationFrames and compares every output pixel with the one
// ImageMagick gives for the same file: -coalesce, the sRGB conversion and the
// resize, in one run.
func TestProfiledGIFsMatchImageMagick(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	srgb := writeSRGBProfile(t)
	adobe := mustAdobeProfile(t)
	rects := []image.Rectangle{image.Rect(0, 0, 24, 20), image.Rect(4, 3, 16, 12), image.Rect(0, 0, 24, 20), image.Rect(2, 2, 20, 14)}
	disposals := map[string][]byte{
		"none":     {1, 1, 1, 1},
		"mixed":    {1, 2, 3, 1},
		"previous": {3, 3, 3, 3},
		"back":     {2, 2, 2, 2},
	}
	placements := map[string][]int{
		"before the first":  {0},
		"before the second": {1},
		"before the third":  {2},
		"before the fourth": {3},
		"after the last":    {4},
		"twice":             {0, 2},
		"twice, late":       {1, 3},
	}
	type fixture struct {
		name string
		data []byte
	}
	var cases []fixture
	for dn, d := range disposals {
		var frames []gifFrameSpec
		for i := range 4 {
			pal := paletteA
			if i%2 == 1 {
				pal = paletteB
			}
			frames = append(frames, gifFrameSpec{rect: rects[i], disposal: d[i], delay: 5, palette: pal, shift: i})
		}
		base := buildGIF(t, 24, 20, paletteA, frames)
		for pn, at := range placements {
			data := withICC(t, base, adobe, at...)
			cases = append(cases, fixture{"disposal " + dn + ", profile " + pn, data})
			if dn == "mixed" {
				cases = append(cases, fixture{"disposal " + dn + ", profile " + pn + ", no trailer", data[:len(data)-1]})
			}
		}
	}
	var worst, total int
	for _, c := range cases {
		name, data := c.name, c.data
		for _, resize := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, resize %v", name, resize), func(t *testing.T) {
				path := writeFile(t, "x.gif", data)
				enc := Encoding{Format: PNG}
				refArgs := []string{"gif:" + path, "-coalesce", "-auto-orient", "-profile", srgb}
				if resize {
					enc.MaxEdge = 16
					refArgs = append(refArgs, "-resize", "16x13!")
				}
				ref := t.TempDir()
				magick(t, env, append(refArgs, "-strip", "-depth", "8", "+adjoin", filepath.Join(ref, "%03d.png"))...)
				images, out := framesInto(t, p, path, enc, 0)
				if len(images) != 4 {
					t.Fatalf("%d images, want 4", len(images))
				}
				var maxDiff, differing int
				for i, im := range images {
					a := readNRGBA(t, filepath.Join(out, im.Name))
					b := readNRGBA(t, filepath.Join(ref, fmt.Sprintf("%03d.png", im.Frame)))
					if a.Bounds() != b.Bounds() {
						t.Fatalf("frame %d: %v against %v", i, a.Bounds(), b.Bounds())
					}
					for j := 0; j < len(a.Pix); j += 4 {
						if a.Pix[j+3] != b.Pix[j+3] {
							differing++
							maxDiff = 255
							continue
						}
						if a.Pix[j+3] == 0 {
							continue
						}
						for c := range 3 {
							d := int(a.Pix[j+c]) - int(b.Pix[j+c])
							if d != 0 {
								differing++
								maxDiff = max(maxDiff, max(d, -d))
							}
						}
					}
				}
				total++
				worst = max(worst, maxDiff)
				if differing != 0 {
					t.Errorf("%d differing samples, largest difference %d", differing, maxDiff)
				}
			})
		}
	}
	t.Logf("%d runs, largest difference %d", total, worst)
}

func TestProfiledGIFConvertsColours(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	adobe := mustAdobeProfile(t)
	base := buildGIF(t, 24, 20, paletteA, []gifFrameSpec{
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA},
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA, shift: 1},
	})
	plain, _ := framesInto(t, p, writeFile(t, "p.gif", base), Encoding{Format: PNG}, 0)
	_, plainOut := framesInto(t, p, writeFile(t, "p.gif", base), Encoding{Format: PNG}, 0)
	_, taggedOut := framesInto(t, p, writeFile(t, "t.gif", withICC(t, base, adobe, 0)), Encoding{Format: PNG}, 0)
	if len(plain) != 2 {
		t.Fatalf("%d images", len(plain))
	}
	if d := meanAbsoluteDifference(t, env, filepath.Join(plainOut, "frame-000.png"), filepath.Join(taggedOut, "frame-000.png")); d < 0.002 {
		t.Errorf("a profiled frame differs from the unprofiled one by %.5f, want a visible conversion", d)
	}
}
