package media

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// typeFixture builds a small file of a table type.
type typeFixture struct {
	build func(t *testing.T) string
	// named is set for a type whose bytes no sniff or ImageMagick detection
	// names (TGA, ICO, G3) or names as another type (DNG, a TIFF): its
	// fixture decodes only when the caller names the type.
	named bool
	// w and h are the size of the decoded first image.
	w, h int
}

// gradient builds a 32x24 file in the format ImageMagick writes for ext.
func gradient(ext string, extra ...string) func(t *testing.T) string {
	return func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "fixture"+ext)
		args := append([]string{"-size", "32x24", "gradient:red-blue", "-depth", "8"}, extra...)
		magick(t, requireMagick(t), append(args, path)...)
		return path
	}
}

// committed returns a copy of the named file of testdata/types.
func committed(name string, patch func([]byte)) func(t *testing.T) string {
	return func(t *testing.T) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", "types", name))
		if err != nil {
			t.Fatal(err)
		}
		if patch != nil {
			patch(data)
		}
		return writeFile(t, name, data)
	}
}

// djvuPage is a 64x48 DjVu page with no image data, 33 bytes long.
func djvuPage() []byte {
	info := binary.BigEndian.AppendUint16(nil, 64)
	info = binary.BigEndian.AppendUint16(info, 48)
	info = append(info, 24, 0, 44, 1, 22, 0) // version 0.24, 300 dpi, gamma 2.2
	form := append([]byte("DJVUINFO"), binary.BigEndian.AppendUint32(nil, uint32(len(info)))...)
	form = append(form, info...)
	return append(append([]byte("AT&TFORM"), binary.BigEndian.AppendUint32(nil, uint32(len(form)))...), form...)
}

// dngBytes is a 128x128 uncompressed Bayer-mosaic DNG, the smallest size
// libraw accepts, written directly because no encoder writes one.
func dngBytes() []byte {
	const w, h = 128, 128
	type entry struct {
		tag, typ uint16
		count    uint32
		val      []byte
	}
	u16 := func(v ...uint16) []byte {
		var b []byte
		for _, x := range v {
			b = binary.LittleEndian.AppendUint16(b, x)
		}
		return b
	}
	u32 := func(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
	var rationals []byte
	for _, v := range []uint32{1, 0, 0, 0, 1, 0, 0, 0, 1} {
		rationals = binary.LittleEndian.AppendUint32(rationals, v*1000)
		rationals = binary.LittleEndian.AppendUint32(rationals, 1000)
	}
	var neutral []byte
	for range 3 {
		neutral = append(neutral, append(u32(1000), u32(1000)...)...)
	}
	var pixels []byte
	for y := range h {
		for x := range w {
			pixels = append(pixels, u16(uint16((x*4000+y*1000)&0x3fff))...)
		}
	}
	entries := []entry{
		{254, 4, 1, u32(0)},
		{256, 4, 1, u32(w)},
		{257, 4, 1, u32(h)},
		{258, 3, 1, u16(16)},
		{259, 3, 1, u16(1)},
		{262, 3, 1, u16(32803)},
		{271, 2, 6, []byte("Synth\x00")},
		{272, 2, 6, []byte("Model\x00")},
		{273, 4, 1, nil},
		{277, 3, 1, u16(1)},
		{278, 4, 1, u32(h)},
		{279, 4, 1, u32(uint32(len(pixels)))},
		{284, 3, 1, u16(1)},
		{33421, 3, 2, u16(2, 2)},
		{33422, 1, 4, []byte{0, 1, 1, 2}},
		{50706, 1, 4, []byte{1, 4, 0, 0}},
		{50708, 2, 6, []byte("Synth\x00")},
		{50710, 1, 3, []byte{0, 1, 2}},
		{50717, 3, 1, u16(16383)},
		{50721, 10, 9, rationals},
		{50728, 5, 3, neutral},
		{50778, 3, 1, u16(21)},
	}
	pixelsAt := 8 + 2 + 12*len(entries) + 4
	out := append([]byte("II*\x00"), u32(8)...)
	out = append(out, u16(uint16(len(entries)))...)
	spill := pixelsAt + len(pixels)
	var tail []byte
	for _, e := range entries {
		val := e.val
		if e.tag == 273 {
			val = u32(uint32(pixelsAt))
		}
		out = append(out, u16(e.tag, e.typ)...)
		out = append(out, u32(e.count)...)
		if len(val) <= 4 {
			out = append(out, append(val, make([]byte, 4-len(val))...)...)
			continue
		}
		out = append(out, u32(uint32(spill+len(tail)))...)
		tail = append(tail, val...)
	}
	out = append(out, u32(0)...)
	return append(append(out, pixels...), tail...)
}

// typeFixtures holds a fixture for every type of the table; the type is in the
// table only because its fixture decodes under the generated policy.
var typeFixtures = map[string]typeFixture{
	"image/jpeg":                    {build: gradient(".jpg"), w: 32, h: 24},
	"image/png":                     {build: gradient(".png"), w: 32, h: 24},
	"image/tiff":                    {build: gradient(".tiff"), w: 32, h: 24},
	"image/bmp":                     {build: gradient(".bmp"), w: 32, h: 24},
	"image/x-pcx":                   {build: gradient(".pcx"), w: 32, h: 24},
	"image/x-portable-bitmap":       {build: gradient(".pbm"), w: 32, h: 24},
	"image/x-portable-greymap":      {build: gradient(".pgm"), w: 32, h: 24},
	"image/x-portable-pixmap":       {build: gradient(".ppm"), w: 32, h: 24},
	"image/x-portable-arbitrarymap": {build: gradient(".pam"), w: 32, h: 24},
	"image/vnd.adobe.photoshop":     {build: gradient(".psd"), w: 32, h: 24},
	"image/jxl":                     {build: gradient(".jxl"), w: 32, h: 24},
	"image/jp2":                     {build: gradient(".jp2"), w: 32, h: 24},
	"image/x-exr":                   {build: gradient(".exr"), w: 32, h: 24},
	"image/x-xcf":                   {build: committed("colordist.xcf", nil), w: 200, h: 160},
	"image/vnd.djvu":                {build: func(t *testing.T) string { return writeFile(t, "page.djvu", djvuPage()) }, w: 64, h: 48},
	"image/avif":                    {build: committed("gradient-32x24.avif", nil), w: 32, h: 24},
	"image/heic":                    {build: func(t *testing.T) string { return heicFixtureFile(t, "adobe-64x32") }, w: 64, h: 32},
	"image/heif": {
		build: func(t *testing.T) string {
			path := heicFixtureFile(t, "adobe-64x32")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			copy(data[8:12], "mif1")
			return writeFile(t, "image.heif", data)
		},
		w: 64, h: 32,
	},
	"image/gif":  {build: func(t *testing.T) string { return writeGIF(t, solid, 4) }, w: 64, h: 48},
	"image/webp": {build: gradient(".webp"), w: 32, h: 24},

	"image/x-tga":              {build: gradient(".tga"), named: true, w: 32, h: 24},
	"image/vnd.microsoft.icon": {build: gradient(".ico"), named: true, w: 32, h: 24},
	"image/g3fax":              {build: gradient(".g3"), named: true, w: 2592, h: 24},
	"image/x-adobe-dng": {
		build: func(t *testing.T) string { return writeFile(t, "raw.dng", dngBytes()) },
		named: true, w: 128, h: 128,
	},

	"video/mp4": {build: func(t *testing.T) string { return sdrClip(t, requireFFmpeg(t), "160x120", 10, 1) }},
	"video/quicktime": {build: func(t *testing.T) string {
		return writeClip(t, requireFFmpeg(t), ".mov", append(testsrc("160x120", 10, 1), "-c:v", "libx264", "-pix_fmt", "yuv420p")...)
	}},
	"video/webm": {build: func(t *testing.T) string {
		return writeClip(t, requireFFmpeg(t), ".webm", append(testsrc("160x120", 10, 1), "-c:v", "libvpx")...)
	}},
	"video/x-matroska": {build: func(t *testing.T) string {
		return writeClip(t, requireFFmpeg(t), ".mkv", append(testsrc("160x120", 10, 1), "-c:v", "libx264", "-pix_fmt", "yuv420p")...)
	}},
}

func TestEveryTableTypeHasAFixture(t *testing.T) {
	t.Parallel()
	var have []string
	for typ := range typeFixtures {
		have = append(have, typ)
	}
	slices.Sort(have)
	if want := Types(); !slices.Equal(have, want) {
		t.Errorf("fixtures for %v, table has %v", have, want)
	}
}

// decodeFixture runs the operation that reads typ on the file at path through
// p, naming the type to the pinning path when the bytes cannot.
func decodeFixture(t *testing.T, p *Processor, typ, path, typeName string) (w, h int) {
	t.Helper()
	ctx := context.Background()
	enc := Encoding{Format: PNG, MaxEdge: 4096, MaxArea: 20_000_000}
	switch kind := typeTable[typ].kind; {
	case kind == kindVideo:
		images, _, err := p.VideoFrames(ctx, path, enc, 2, t.TempDir())
		if err != nil || len(images) == 0 {
			t.Fatalf("VideoFrames: %d images, %v", len(images), err)
		}
		return images[0].Width, images[0].Height
	case typ == "image/gif":
		images, _, err := p.AnimationFrames(ctx, path, enc, 4, t.TempDir())
		if err != nil || len(images) != 4 {
			t.Fatalf("AnimationFrames: %d images, %v", len(images), err)
		}
		return images[0].Width, images[0].Height
	}
	img, _, err := p.still(ctx, path, typeName, enc, t.TempDir())
	if err != nil {
		t.Fatalf("Still: %v", err)
	}
	return img.Width, img.Height
}

func TestTableTypesDecodeTheirFixtures(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	for typ, f := range typeFixtures {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			path := f.build(t)
			name := ""
			if f.named {
				name = typ
			}
			w, h := decodeFixture(t, p, typ, path, name)
			if f.w != 0 && (w != f.w || h != f.h) {
				t.Errorf("decoded %dx%d, want %dx%d", w, h, f.w, f.h)
			}
		})
	}
}

func TestUnnameableTypesAreRefusedWithoutTheirName(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	for _, typ := range []string{"image/x-tga", "image/vnd.microsoft.icon", "image/g3fax"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			_, _, err := p.Still(context.Background(), typeFixtures[typ].build(t), Encoding{}, t.TempDir())
			if !isUnsupported(err) {
				t.Errorf("err = %v, want ErrUnsupportedType", err)
			}
		})
	}
}

func TestAnimatedWebPIsReadOnlyByTheAnimationOperations(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := buildWebP(t, webpCase{codec: "lossless", frames: 3})
	images, _, err := p.AnimationFrames(context.Background(), path, Encoding{Format: PNG}, 3, t.TempDir())
	if err != nil || len(images) != 3 {
		t.Fatalf("AnimationFrames: %d images, %v", len(images), err)
	}
	if _, _, err := p.Still(context.Background(), path, Encoding{Format: PNG}, t.TempDir()); !isUnsupported(err) {
		t.Errorf("Still of an animated WebP: err = %v, want ErrUnsupportedType", err)
	}
}

func TestTypesAreSortedAndSafe(t *testing.T) {
	t.Parallel()
	types := Types()
	if !slices.IsSorted(types) {
		t.Errorf("Types() is not sorted: %v", types)
	}
	for _, typ := range types {
		for _, banned := range []string{"svg", "mvg", "msl", "postscript", "pdf", "text", "eps"} {
			if strings.Contains(typ, banned) {
				t.Errorf("Types() lists %s", typ)
			}
		}
		for _, c := range typeTable[typ].coders {
			switch c {
			case "SVG", "MSVG", "MVG", "MSL", "PS", "EPS", "PDF", "TXT", "TEXT":
				t.Errorf("%s reads through the %s coder", typ, c)
			}
		}
	}
	types[0] = "changed"
	if Types()[0] == "changed" {
		t.Error("Types() returns the table's own slice")
	}
}

func TestPolicyIsGeneratedFromTypes(t *testing.T) {
	t.Parallel()
	coders := func(types []string) []string {
		var out []string
		for _, r := range processorPolicy(defaultResources(), types) {
			if r.domain == "coder" && r.pattern != "*" && strings.Contains(r.rights, "read") {
				out = append(out, r.pattern)
			}
		}
		return out
	}
	got := coders([]string{"image/png", "image/tiff"})
	want := []string{"ICC", "PAM", "PNG", "TIFF", "TIFF64"}
	if !slices.Equal(got, want) {
		t.Errorf("read coders of png and tiff = %v, want %v", got, want)
	}
	for _, c := range coders(Types()) {
		if slices.Contains([]string{"SVG", "MSVG", "MVG", "MSL", "PS", "EPS", "PDF", "TXT"}, c) {
			t.Errorf("default policy reads %s", c)
		}
	}
}

func isUnsupported(err error) bool { return errors.Is(err, ErrUnsupportedType) }
