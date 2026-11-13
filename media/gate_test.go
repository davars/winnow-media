package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCURFileIsReadAsIcon(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := gradient(".cur")(t)
	img, _, err := stillAs(context.Background(), p, path, "image/vnd.microsoft.icon", Encoding{Format: PNG}, t.TempDir())
	if err != nil || img.Width != 32 || img.Height != 24 {
		t.Errorf("CUR as icon: %+v, %v", img, err)
	}
}

func TestSniffedTypeOutsideTypesIsRefusedBeforeAnyToolRuns(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessorWith(t, Config{Types: []string{"image/png"}})
	rec := record(p)
	forbidCompositor(t, p)
	animated := buildWebP(t, webpCase{codec: "lossless", frames: 2})
	cases := map[string]string{
		"gif":  writeGIF(t, solid, 3),
		"webp": animated,
		"heic": heicFixtureFile(t, "adobe-64x32"),
		"avif": committed("gradient-32x24.avif", nil)(t),
		"mp4":  sdrClip(t, env, "160x120", 10, 1),
	}
	for name, path := range cases {
		if _, _, err := p.Open(context.Background(), path, OpenOptions{}); !isUnsupported(err) {
			t.Errorf("%s: err = %v, want ErrUnsupportedType", name, err)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("%d tool runs, calls %q", n, rec.calls)
	}
}

func TestDangerousContentIsRefusedWithoutDecoding(t *testing.T) {
	t.Parallel()
	secret := writeFile(t, "secret.txt", []byte("secret"))
	reference := `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="8" height="8"><image width="8" height="8" xlink:href="file://` + secret + `"/></svg>`
	cases := map[string][]byte{
		"svg":        []byte(reference),
		"svg no xml": []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"><rect width="8" height="8"/></svg>`),
		"mvg":        []byte("push graphic-context\nviewbox 0 0 64 64\nimage over 0,0 0,0 'text:" + secret + "'\npop graphic-context\n"),
		"msl":        []byte(`<?xml version="1.0"?><image><read filename="` + secret + `"/><write filename="/tmp/media-gate-msl.png"/></image>`),
		"ps":         []byte("%!PS-Adobe-3.0\n/Helvetica findfont 12 scalefont setfont 0 0 moveto (x) show showpage\n"),
		"eps":        []byte("%!PS-Adobe-3.0 EPSF-3.0\n%%BoundingBox: 0 0 8 8\nshowpage\n"),
		"pdf":        []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 8 8]>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n"),
		"text":       []byte("just some words\n"),
		"empty":      nil,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			rec := record(p)
			path := writeFile(t, "input", data)
			if _, _, err := p.Open(context.Background(), path, OpenOptions{}); !isUnsupported(err) {
				t.Errorf("err = %v, want ErrUnsupportedType", err)
			}
			onlyTypeDetection(t, rec)
		})
	}
	t.Cleanup(func() {
		if _, err := os.Stat("/tmp/media-gate-msl.png"); err == nil {
			t.Error("an MSL script wrote a file")
		}
	})
}

func TestMisleadingExtensionMakesNoDifference(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	png := gradient(".png")(t)
	data, err := os.ReadFile(png)
	if err != nil {
		t.Fatal(err)
	}
	enc := Encoding{Format: PNG}
	var want []byte
	for _, name := range []string{"x.png", "x.svg", "x.jpg", "x.gif", "x.heic", "x"} {
		path := writeFile(t, name, data)
		out := t.TempDir()
		img, _, err := p.encodeFirst(context.Background(), path, enc, out)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(out, img.Name))
		if err != nil {
			t.Fatal(err)
		}
		if want == nil {
			want = got
		} else if !bytes.Equal(got, want) {
			t.Errorf("%s decodes to different bytes", name)
		}
	}

	// ImageMagick names a TGA, which has no signature, by its extension.
	tga, err := os.ReadFile(gradient(".tga")(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x.tga", "x.png", "x"} {
		_, _, err := p.encodeFirst(context.Background(), writeFile(t, name, tga), enc, t.TempDir())
		if !isUnsupported(err) {
			t.Errorf("TGA bytes named %s: err = %v, want ErrUnsupportedType", name, err)
		}
	}
	// And a vector file keeps no safe name.
	svg := writeFile(t, "x.png", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="8" height="8"/>`))
	if _, _, err := p.encodeFirst(context.Background(), svg, enc, t.TempDir()); !isUnsupported(err) {
		t.Errorf("SVG named .png: err = %v, want ErrUnsupportedType", err)
	}
}

func TestContentNamingADeniedCoderIsRefusedUnderThePolicy(t *testing.T) {
	t.Parallel()
	// Neither format has a write rule that would deny it on its own.
	for typ, ext := range map[string]string{"image/tiff": ".tiff", "image/bmp": ".bmp", "image/x-pcx": ".pcx"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			path := gradient(ext)(t)
			p := requireProcessorWith(t, Config{Types: []string{"image/jpeg"}})
			_, _, err := p.encodeFirst(context.Background(), path, Encoding{}, t.TempDir())
			if !isUnsupported(err) {
				t.Fatalf("err = %v, want ErrUnsupportedType", err)
			}
			if !strings.Contains(err.Error(), "no supported coder reads") {
				t.Errorf("err = %v, want the refusal of a coder the policy denies", err)
			}
			allowed := requireProcessorWith(t, Config{Types: []string{typ}})
			if _, _, err := allowed.encodeFirst(context.Background(), path, Encoding{}, t.TempDir()); err != nil {
				t.Errorf("with Types naming %s: %v", typ, err)
			}
		})
	}
}

func TestOpenAcceptsOnlyConfiguredTypes(t *testing.T) {
	t.Parallel()
	p := requireProcessorWith(t, Config{Types: []string{"image/png", "image/gif"}})
	ctx := context.Background()
	for _, path := range []string{gradient(".png")(t), writeGIF(t, solid, 3)} {
		src, _, err := p.Open(ctx, path, OpenOptions{})
		if err != nil {
			t.Errorf("Open of a configured type: %v", err)
			continue
		}
		_ = src.Close()
	}
	if _, _, err := p.Open(ctx, gradient(".jpg")(t), OpenOptions{}); !isUnsupported(err) {
		t.Errorf("Open of JPEG: err = %v, want ErrUnsupportedType", err)
	}
}

func TestNeutralLinkIsRemoved(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	png := gradient(".png")(t)
	if _, _, err := p.encodeFirst(context.Background(), png, Encoding{}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.encodeFirst(context.Background(), writeFile(t, "x", []byte("text")), Encoding{}, t.TempDir()); err == nil {
		t.Fatal("text was read")
	}
	entries, err := os.ReadDir(p.tmpDir())
	if err != nil || len(entries) != 0 {
		t.Errorf("tmp holds %v, %v", entries, err)
	}
}

func TestConfigTypes(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	ctx := context.Background()
	for name, types := range map[string][]string{
		"unknown":        {"image/png", "image/x-unknown"},
		"svg":            {"image/svg+xml"},
		"pdf":            {"application/pdf"},
		"empty non-nil":  {},
		"different case": {"IMAGE/PNG"},
	} {
		if _, err := NewProcessor(ctx, Config{Types: types}); err == nil {
			t.Errorf("%s: Types %q accepted", name, types)
		}
	}
	p, err := NewProcessor(ctx, Config{Types: []string{"image/png", "image/png", "image/gif"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if !slices.Equal(p.types, []string{"image/gif", "image/png"}) {
		t.Errorf("types = %v", p.types)
	}
}

func TestConfigCaps(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	ctx := context.Background()
	bad := map[string]Caps{
		"MaxInputBytes negative":      {MaxInputBytes: -1},
		"MaxFramePixels negative":     {MaxFramePixels: -1},
		"MaxFramePixels over":         {MaxFramePixels: ceilingCaps.framePixels + 1},
		"MaxReturnedPixels negative":  {MaxReturnedPixels: -1},
		"MaxDecodedPixels negative":   {MaxDecodedPixels: -1},
		"MaxAnimationFrames negative": {MaxAnimationFrames: -1},
		"MaxAnimationFrames over":     {MaxAnimationFrames: maxAnimationFrames + 1},
	}
	for name, c := range bad {
		if _, err := NewProcessor(ctx, Config{Caps: c}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ceiling := Caps{MaxFramePixels: ceilingCaps.framePixels, MaxAnimationFrames: maxAnimationFrames, MaxInputBytes: 1 << 62}
	p, err := NewProcessor(ctx, Config{Caps: ceiling})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	zero, err := Caps{}.resolve()
	if err != nil || zero != ceilingCaps {
		t.Errorf("zero Caps resolve to %+v, %v; want the ceilings %+v", zero, err, ceilingCaps)
	}
}

// capCase is one Caps field with a file just at the limit it names.
type capCase struct {
	name string
	// file builds an input and returns its path and the cap value it just
	// meets; the operation runs it through p.
	setup func(t *testing.T) (path string, limit int64)
	cfg   func(c *Caps, limit int64)
	run   func(p *Processor, path, dir string) error
}

func TestCapsAreEnforcedAtTheirLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	animation := func(p *Processor, path, dir string) error {
		_, _, err := p.encodeEven(ctx, path, Encoding{}, 16, dir)
		return err
	}
	cases := []capCase{
		{
			"MaxInputBytes", func(t *testing.T) (string, int64) {
				path := writeGIF(t, solid, 3)
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				return path, info.Size()
			},
			func(c *Caps, n int64) { c.MaxInputBytes = n }, animation,
		},
		{
			"MaxFramePixels still", func(t *testing.T) (string, int64) { return gradient(".png")(t), 32 * 24 },
			func(c *Caps, n int64) { c.MaxFramePixels = n },
			func(p *Processor, path, dir string) error {
				_, _, err := p.encodeFirst(ctx, path, Encoding{}, dir)
				return err
			},
		},
		{
			"MaxFramePixels animation", func(t *testing.T) (string, int64) { return writeGIF(t, solid, 3), 64 * 48 },
			func(c *Caps, n int64) { c.MaxFramePixels = n }, animation,
		},
		{
			"MaxAnimationFrames", func(t *testing.T) (string, int64) { return writeGIF(t, solid, 5), 5 },
			func(c *Caps, n int64) { c.MaxAnimationFrames = int(n) }, animation,
		},
		{
			"MaxDecodedPixels", func(t *testing.T) (string, int64) { return writeGIF(t, solid, 5), 5 * 64 * 48 },
			func(c *Caps, n int64) { c.MaxDecodedPixels = n }, animation,
		},
		{
			"MaxReturnedPixels", func(t *testing.T) (string, int64) { return writeGIF(t, solid, 5), 5 * 64 * 48 },
			func(c *Caps, n int64) { c.MaxReturnedPixels = n }, animation,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path, limit := c.setup(t)
			for _, tc := range []struct {
				name    string
				limit   int64
				refused bool
			}{{"at the limit", limit, false}, {"one under", limit - 1, true}} {
				var caps Caps
				c.cfg(&caps, tc.limit)
				p := requireProcessorWith(t, Config{Caps: caps})
				err := c.run(p, path, t.TempDir())
				if got := errorsIsTooLarge(err); got != tc.refused {
					t.Errorf("%s: err = %v, refused = %v, want %v", tc.name, err, got, tc.refused)
				}
				if !tc.refused && err != nil {
					t.Errorf("%s: %v", tc.name, err)
				}
			}
		})
	}
}

func errorsIsTooLarge(err error) bool { return errors.Is(err, ErrTooLarge) }

func TestFourThreadOutputIsIdenticalRunAfterRun(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessorWith(t, Config{Resources: Resources{Threads: 4}})
	dir := t.TempDir()
	inputs := map[string]string{
		"jpeg":       filepath.Join(dir, "plasma.jpg"),
		"16-bit tif": filepath.Join(dir, "plasma16.tiff"),
	}
	magick(t, env, "-seed", "7", "-size", "1200x900", "plasma:red-blue", "-quality", "92", inputs["jpeg"])
	magick(t, env, "-seed", "7", "-size", "1200x900", "plasma:green-yellow", "-depth", "16", inputs["16-bit tif"])
	for name, path := range inputs {
		for _, format := range []Format{WebP, JPEG, PNG} {
			enc := Encoding{Format: format, MaxEdge: 500}
			var first []byte
			for run := range 4 {
				out := t.TempDir()
				img, _, err := p.encodeFirst(context.Background(), path, enc, out)
				if err != nil {
					t.Fatalf("%s %s run %d: %v", name, format, run, err)
				}
				got, err := os.ReadFile(filepath.Join(out, img.Name))
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case run == 0:
					first = got
				case !bytes.Equal(got, first):
					t.Fatalf("%s %s: run %d differs from run 0", name, format, run)
				}
			}
		}
	}
}

func TestBigTIFFIsReadAsTIFF(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "big.tiff")
	magick(t, requireMagick(t), "-size", "32x24", "gradient:red-blue", "tiff64:"+path)
	img, _, err := p.encodeFirst(context.Background(), path, Encoding{Format: PNG}, t.TempDir())
	if err != nil || img.Width != 32 || img.Height != 24 {
		t.Errorf("BigTIFF: %+v, %v", img, err)
	}
}

// libheif chooses the codec from the primary item, not from the brand that
// sniff types the file by, so a brand of one type over items of another
// reaches the decoder of the others. Types therefore names all of HEIC, HEIF
// and AVIF or none, and such a file is read whatever its brand says.
func TestISOBaseMediaTypesAreNamedTogether(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	family := []string{"image/heic", "image/heif", "image/avif"}
	for _, subset := range [][]string{{"image/heic"}, {"image/avif"}, {"image/heif", "image/avif"}, {"image/png", "image/heic"}} {
		if _, err := NewProcessor(context.Background(), Config{Types: subset}); err == nil {
			t.Errorf("Types %v accepted", subset)
		}
	}
	p := requireProcessorWith(t, Config{Types: append([]string{"image/png"}, family...)})
	data, err := os.ReadFile(heicFixtureFile(t, "adobe-64x32"))
	if err != nil {
		t.Fatal(err)
	}
	copy(data[8:12], "avif")
	img, _, err := p.encodeFirst(context.Background(), writeFile(t, "input", data), Encoding{Format: PNG}, t.TempDir())
	if err != nil || img.Width != 64 {
		t.Errorf("HEVC items under an avif brand: %+v, %v", img, err)
	}
}

// reducedTIFF is a 32x24 8-bit grey TIFF whose only directory is marked as
// a reduced-resolution image (NewSubfileType 1): the shape of a camera raw
// file, whose first directory is its preview and whose full image is in a
// SubIFD the TIFF coder does not read.
func reducedTIFF() []byte {
	const w, h = 32, 24
	type entry struct {
		tag, typ uint16
		val      uint32
	}
	entries := []entry{
		{254, 4, 1},
		{256, 3, w},
		{257, 3, h},
		{258, 3, 8},
		{259, 3, 1},
		{262, 3, 1},
		{273, 4, 0},
		{277, 3, 1},
		{278, 3, h},
		{279, 4, w * h},
	}
	pixelsAt := uint32(8 + 2 + 12*len(entries) + 4)
	out := []byte("II*\x00\x08\x00\x00\x00")
	out = append(out, byte(len(entries)), 0)
	for _, e := range entries {
		v := e.val
		if e.tag == 273 {
			v = pixelsAt
		}
		out = append(out, byte(e.tag), byte(e.tag>>8), byte(e.typ), byte(e.typ>>8), 1, 0, 0, 0)
		out = append(out, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	out = append(out, 0, 0, 0, 0)
	for i := range w * h {
		out = append(out, byte(i))
	}
	return out
}

// ImageMagick names a camera raw file a TIFF and decodes its preview without
// error; the decoder reports the directory it read as a reduced-resolution
// image (%[tiff:subfiletype] is REDUCEDIMAGE), so the call must not succeed
// with that preview as the file's first image.
func TestReducedResolutionTIFFIsNotDecodedAsTheImage(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := writeFile(t, "raw", reducedTIFF())
	img, _, err := p.encodeFirst(context.Background(), path, Encoding{Format: PNG}, t.TempDir())
	if !isUnsupported(err) {
		t.Errorf("decoded the reduced-resolution directory as %dx%d, err = %v; want ErrUnsupportedType", img.Width, img.Height, err)
	}
}

func TestJPEGStillCostsOneHeaderReadAndOneEncode(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	rec := record(p)
	if _, _, err := p.encodeFirst(context.Background(), gradient(".jpg")(t), Encoding{}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if n := rec.count(); n != 2 {
		t.Errorf("%d tool runs, want 2: %q", n, rec.calls)
	}
}

func TestTIFFWithoutASubfileTypeIsRead(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	img, _, err := p.encodeFirst(context.Background(), gradient(".tiff")(t), Encoding{Format: PNG}, t.TempDir())
	if err != nil || img.Width != 32 {
		t.Errorf("TIFF: %+v, %v", img, err)
	}
}

func TestOutputDirectoryIsCheckedBeforeAnyToolRuns(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	rec := record(p)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	src := openSourceOn(t, p, gradient(".png")(t), OpenOptions{})
	before := rec.count()
	if _, _, err := src.Encode(context.Background(), dir, Select{}, Encoding{}); err == nil {
		t.Fatal("a non-empty directory was accepted")
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("%d tool runs: %q", n, rec.calls[before:])
	}
}
