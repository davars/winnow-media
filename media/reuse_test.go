package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// encodeAll encodes sel at targets from src into a new directory and returns
// the bytes of each file by name.
func encodeAll(t *testing.T, src *Source, sel Select, targets ...Encoding) map[string][]byte {
	t.Helper()
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, sel, targets...)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, im := range images {
		out[im.Name] = mustRead(t, filepath.Join(dir, im.Name))
	}
	return out
}

func sameBytes(t *testing.T, what string, got, want map[string][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d files, want %d", what, len(got), len(want))
		return
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok || string(g) != string(w) {
			t.Errorf("%s: %s differs from the fused encode (%d bytes, want %d)", what, name, len(g), len(w))
		}
	}
}

// reuseInput is a file Reuse is checked on and a region of it.
type reuseInput struct {
	name   string
	path   func(t *testing.T) string
	region Region
	// edges are the MaxEdge values to encode at; a JPEG's are above the
	// sizes at which an Encode without Reuse decodes it reduced, whose bytes
	// a full-size decode does not give.
	edges []int
}

// reuseDeclined are the inputs a Reuse Source cannot keep exactly: a Lab
// colorspace, a type not marked exact, and a float decode.
var reuseDeclined = map[string]bool{
	"tiff lab": true, "jp2 12-bit": true, "exr above 1": true, "tiff float32": true,
	"tiff float16": true, "tiff float16 hdr": true,
}

func reuseInputs() []reuseInput {
	small := []int{24, 1000}
	heic := func(name string) func(*testing.T) string {
		return func(t *testing.T) string { return heicFixtureFile(t, name) }
	}
	return []reuseInput{
		{"adobe-64x32", heic("adobe-64x32"), Region{10, 6, 30, 20}, small},
		{"adobe-64x64", heic("adobe-64x64"), Region{5, 9, 40, 33}, small},
		{"adobe-256x96", heic("adobe-256x96"), Region{100, 20, 120, 60}, small},
		{"gradient-righttop", heic("gradient-righttop"), Region{50, 70, 200, 150}, small},
		{"tenbit-64x32", heic("tenbit-64x32"), Region{7, 3, 33, 22}, small},
		{"alpha-64x32", heic("alpha-64x32"), Region{7, 3, 33, 22}, small},
		{"jpeg", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "g.jpg")
			magick(t, requireMagick(t), "-size", "320x240", "gradient:#0020ff-#ffb000", "-quality", "100", path)
			return path
		}, Region{40, 30, 150, 110}, []int{200, 1000}},
		{"png16", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "g.png")
			magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000", "-depth", "16", path)
			return path
		}, Region{11, 9, 50, 31}, small},
		{"png phys", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "d.png")
			magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000",
				"-density", "300", "-units", "PixelsPerInch", path)
			return path
		}, Region{11, 9, 50, 31}, small},
		{"tiff 300dpi", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "d.tif")
			magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000",
				"-density", "300", "-units", "PixelsPerInch", path)
			return path
		}, Region{11, 9, 50, 31}, small},
		// A HEIC whose major brand is patched to mif1, which reads as HEIF.
		{"heif adobe-64x32", func(t *testing.T) string {
			data := mustRead(t, heicFixtureFile(t, "adobe-64x32"))
			copy(data[8:12], "mif1")
			return writeFile(t, "image.heif", data)
		}, Region{10, 6, 30, 20}, small},
		// More than 16 bits a channel, which a 16-bit PAM rounds.
		{"tiff float32", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "f.tif")
			magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000", "-define", "quantum:format=floating-point", "-depth", "32", path)
			return path
		}, Region{11, 9, 50, 31}, small},
		// Half-float samples, which a 16-bit PAM rounds, and above 1 clamps.
		{"tiff float16", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "h.tif")
			magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000", "-define", "quantum:format=floating-point", "-depth", "16", path)
			return path
		}, Region{11, 9, 50, 31}, small},
		{"tiff float16 hdr", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "hh.tif")
			magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000", "-evaluate", "multiply", "3",
				"-define", "quantum:format=floating-point", "-depth", "16", path)
			return path
		}, Region{11, 9, 50, 31}, small},
		// The JPEG writer reuses the sampling factor its reader recorded,
		// which the PAM does not carry.
		{"jpeg 4:2:0", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "s420.jpg")
			magick(t, requireMagick(t), "-size", "320x240", "gradient:#0020ff-#ffb000", "-sampling-factor", "2x2", "-quality", "80", path)
			return path
		}, Region{40, 30, 150, 110}, []int{200, 1000}},
		{"jpeg 4:2:2", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "s422.jpg")
			magick(t, requireMagick(t), "-size", "320x240", "gradient:#0020ff-#ffb000", "-sampling-factor", "2x1", "-quality", "75", path)
			return path
		}, Region{40, 30, 150, 110}, []int{200, 1000}},
		// The PAM carries no resolution, which the JPEG writer records.
		{"jpeg 300dpi", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "d.jpg")
			magick(t, requireMagick(t), "-size", "320x240", "gradient:#0020ff-#ffb000",
				"-density", "300", "-units", "PixelsPerInch", "-quality", "100", path)
			return path
		}, Region{40, 30, 150, 110}, []int{200, 1000}},
		// A camera's EXIF resolution, which the HEIC reader takes as the
		// image's: 72 dpi.
		{"heic exif 72dpi", func(t *testing.T) string {
			requireExiftool(t)
			path := heicFixtureFile(t, "adobe-64x32")
			exiftool(t, requireMagick(t), "-overwrite_original",
				"-EXIF:XResolution=72", "-EXIF:YResolution=72", "-EXIF:ResolutionUnit=inches", path)
			return path
		}, Region{10, 6, 30, 20}, small},
		// The PAM writer turns Lab into sRGB, where the fused run resizes in
		// Lab.
		{"tiff lab", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "lab.tif")
			magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000", "-colorspace", "Lab", path)
			return path
		}, Region{11, 9, 50, 31}, small},
		// The JPEG 2000 reader scales 12 bits to fractions a 16-bit PAM
		// rounds.
		{"jp2 12-bit", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "p.jp2")
			magick(t, requireMagick(t), "-size", "160x120", "-seed", "5", "plasma:red-blue", "-blur", "0x2", "-depth", "12", path)
			return path
		}, Region{11, 9, 97, 73}, []int{60, 133}},
		// A 16-bit PAM clamps what the fused run resizes unclamped.
		{"exr above 1", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "hdr.exr")
			magick(t, requireMagick(t), "-size", "160x120", "-seed", "3", "plasma:red-blue", "-evaluate", "multiply", "3", path)
			return path
		}, Region{11, 9, 97, 73}, []int{60, 97}},
	}
}

func TestReuseKeptDecodeGivesTheFusedBytes(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	for _, in := range reuseInputs() {
		for _, format := range []Format{JPEG, WebP, PNG} {
			for _, edge := range in.edges {
				t.Run(fmt.Sprintf("%s/%s/%d", in.name, format, edge), func(t *testing.T) {
					t.Parallel()
					path := in.path(t)
					enc := Encoding{Format: format, MaxEdge: edge}
					plain := openSourceOn(t, p, path, OpenOptions{})
					reuse := openSourceOn(t, p, path, OpenOptions{Reuse: true})
					whole := encodeAll(t, plain, Select{}, enc)
					sameBytes(t, "first encode", encodeAll(t, reuse, Select{}, enc), whole)
					if _, ok := reuse.kept[0]; ok == reuseDeclined[in.name] {
						t.Fatalf("decode kept = %v, want %v", ok, !reuseDeclined[in.name])
					}
					sameBytes(t, "whole image from the kept decode", encodeAll(t, reuse, Select{}, enc), whole)
					sel := Select{Region: in.region}
					sameBytes(t, "region from the kept decode", encodeAll(t, reuse, sel, enc), encodeAll(t, plain, sel, enc))
				})
			}
		}
	}
}

// wantProfile are the fused fixtures whose kept decode carries a profile.
var wantProfile = map[string]bool{
	"jpeg profiled": true, "png16 alpha profiled": true, "webp lossless": true, "heic adobe": true,
}

// The fixtures of the fused still, in every format and colour model the
// decoders read, give the fused bytes from the kept decode.
func TestReuseKeptDecodeOnEveryFusedFixture(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	for _, fx := range fusedFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			path := fx.write(t, env, t.TempDir())
			edges := []int{40, 400}
			if fx.coder == "jpeg" {
				edges = []int{400}
			}
			region := Region{X: 5, Y: 3, Width: 30, Height: 20}
			for _, format := range []Format{JPEG, WebP, PNG} {
				for _, edge := range edges {
					enc := Encoding{Format: format, MaxEdge: edge}
					plain := openSourceOn(t, p, path, OpenOptions{})
					reuse := openSourceOn(t, p, path, OpenOptions{Reuse: true})
					want := encodeAll(t, plain, Select{}, enc)
					sameBytes(t, fmt.Sprintf("%s %d first", format, edge), encodeAll(t, reuse, Select{}, enc), want)
					if kd := reuse.kept[0]; kd != nil && wantProfile[fx.name] && kd.icc == "" {
						t.Errorf("%s %d: no profile kept", format, edge)
					}
					if got, want := len(reuse.kept) == 1, fx.coder != "gif"; got != want {
						t.Errorf("%s %d: decode kept = %v, want %v", format, edge, got, want)
					}
					sameBytes(t, fmt.Sprintf("%s %d whole", format, edge), encodeAll(t, reuse, Select{}, enc), want)
					sel := Select{Region: region}
					sameBytes(t, fmt.Sprintf("%s %d region", format, edge), encodeAll(t, reuse, sel, enc), encodeAll(t, plain, sel, enc))
				}
			}
		})
	}
}

// reusedAfterFirst opens path with Reuse, encodes it once at enc, and returns
// the Source.
func reusedAfterFirst(t *testing.T, p *Processor, path string, enc Encoding) *Source {
	t.Helper()
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	encodeAll(t, src, Select{}, enc)
	if src.kept[0] == nil {
		t.Fatal("the decode was not kept")
	}
	return src
}

// fusedWant is what an Encode without Reuse writes for sel at enc.
func fusedWant(t *testing.T, p *Processor, path string, sel Select, enc Encoding) map[string][]byte {
	t.Helper()
	return encodeAll(t, openSourceOn(t, p, path, OpenOptions{}), sel, enc)
}

func TestReuseFallsBackFromAKeptFileThatChanged(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := heicFixtureFile(t, "adobe-256x96")
	enc := Encoding{Format: PNG, MaxEdge: 64}
	sel := Select{Region: Region{X: 20, Y: 10, Width: 100, Height: 50}}
	want := fusedWant(t, p, path, sel, enc)
	tamper := map[string]func(t *testing.T, kd *keptDecode){
		"renamed": func(t *testing.T, kd *keptDecode) {
			if err := os.Rename(kd.pam, kd.pam+".moved"); err != nil {
				t.Fatal(err)
			}
		},
		"appended": func(t *testing.T, kd *keptDecode) {
			f, err := os.OpenFile(kd.pam, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if _, err := f.Write([]byte{0}); err != nil {
				t.Fatal(err)
			}
		},
		"rewritten": func(t *testing.T, kd *keptDecode) {
			data := mustRead(t, kd.pam)
			// A byte of the region {20,10,100,50}: row 20, column 40 of 256.
			data[len(kd.header)+(20*256+40)*kd.depth*2] ^= 0xff
			if err := os.WriteFile(kd.pam, data, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"replaced": func(t *testing.T, kd *keptDecode) {
			data := mustRead(t, kd.pam)
			if err := os.Remove(kd.pam); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(kd.pam, data, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, kd *keptDecode) {
			other := writeFile(t, "other.pam", mustRead(t, kd.pam))
			if err := os.Remove(kd.pam); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, kd.pam); err != nil {
				t.Fatal(err)
			}
		},
		"forged width": func(t *testing.T, kd *keptDecode) {
			data := mustRead(t, kd.pam)
			if err := os.WriteFile(kd.pam, bytes.Replace(data, []byte("WIDTH 256"), []byte("WIDTH 128"), 1), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"profile rewritten": func(t *testing.T, kd *keptDecode) {
			data := mustRead(t, kd.icc)
			data[len(data)-1] ^= 0xff
			if err := os.WriteFile(kd.icc, data, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range tamper {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			own := requireProcessor(t)
			src := reusedAfterFirst(t, own, path, enc)
			kd := src.kept[0]
			change(t, kd)
			rec := record(own)
			before := rec.count()
			// The changed file is not read: the call decodes the image again,
			// to the bytes it would have had.
			sameBytes(t, "after the change", encodeAll(t, src, sel, enc), want)
			if kd2 := src.kept[0]; kd2 == nil || kd2 == kd {
				t.Error("the decode was not kept afresh")
			}
			if n := rec.count() - before; n != 1 {
				t.Errorf("the call ran %d tools, want the one fused run", n)
			}
		})
	}
}

// The header check holds without the identity check: a forgery that also
// matches the identity recorded for the file is still refused.
func TestReuseChecksTheKeptHeaderApartFromTheIdentity(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
	kd := src.kept[0]
	data := mustRead(t, kd.pam)
	if err := os.WriteFile(kd.pam, bytes.Replace(data, []byte("WIDTH 256"), []byte("WIDTH 128"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(kd.pam)
	if err != nil {
		t.Fatal(err)
	}
	kd.id = idOf(info)
	if path, err := src.copyKept(context.Background(), kd, region{}); err == nil {
		t.Errorf("copyKept read a file with a forged header: %s", path)
	}
}

// A symlink is refused without help from the identity check: the target is a
// copy whose identity is the one recorded.
func TestReuseFollowsNoSymlinkToTheKeptFile(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
	kd := src.kept[0]
	other := writeFile(t, "other.pam", mustRead(t, kd.pam))
	info, err := os.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(kd.pam); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, kd.pam); err != nil {
		t.Fatal(err)
	}
	kd.id = idOf(info)
	if path, err := src.copyKept(context.Background(), kd, region{}); err == nil {
		t.Errorf("copyKept followed a symlink: %s", path)
	}
}

func TestParsePAMHeaderAcceptsOnlyWhatTheWriterWrites(t *testing.T) {
	t.Parallel()
	good := string(pamHeader(64, 32, 3, "RGB"))
	if w, h, d, tuple, err := parsePAMHeader([]byte(good)); err != nil || w != 64 || h != 32 || d != 3 || tuple != "RGB" {
		t.Fatalf("parse = %d %d %d %s %v", w, h, d, tuple, err)
	}
	for name, bad := range map[string]string{
		"depth for tuple": strings.Replace(good, "DEPTH 3", "DEPTH 4", 1),
		"maxval":          strings.Replace(good, "65535", "255", 1),
		"leading zero":    strings.Replace(good, "WIDTH 64", "WIDTH 064", 1),
		"extra line":      strings.Replace(good, "ENDHDR", "COMMENT x\nENDHDR", 1),
		"zero width":      strings.Replace(good, "WIDTH 64", "WIDTH 0", 1),
		"unknown tuple":   strings.Replace(good, "RGB", "YUV", 1),
		"negative height": strings.Replace(good, "HEIGHT 32", "HEIGHT -32", 1),
		"huge width":      strings.Replace(good, "WIDTH 64", "WIDTH 99999999", 1),
	} {
		if _, _, _, _, err := parsePAMHeader([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReuseNoRunNamesTheKeptFileExceptTheOneThatWritesIt(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	rec := record(p)
	path := heicFixtureFile(t, "adobe-256x96")
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	enc := Encoding{Format: WebP, MaxEdge: 64}
	encodeAll(t, src, Select{}, enc)
	kd := src.kept[0]
	if kd == nil {
		t.Fatal("not kept")
	}
	first := rec.count()
	encodeAll(t, src, Select{Region: Region{X: 4, Y: 4, Width: 60, Height: 40}}, enc)
	encodeAll(t, src, Select{}, enc)
	for i, call := range rec.calls {
		for j, arg := range call {
			if !strings.Contains(arg, kd.pam) {
				continue
			}
			if i >= first || call[j-1] != "-write" {
				t.Errorf("run %d names the kept file other than as a -write target: %q", i, call)
			}
		}
	}
	if n := rec.count() - first; n != 2 {
		t.Errorf("two further Encodes ran %d tools, want 2", n)
	}
}

func TestReuseSecondEncodeDecodesNoOriginal(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	rec := record(p)
	src := openSourceOn(t, p, heicFixtureFile(t, "adobe-256x96"), OpenOptions{Reuse: true})
	link := src.link
	encodeAll(t, src, Select{}, Encoding{Format: PNG, MaxEdge: 64})
	before := rec.count()
	encodeAll(t, src, Select{Region: Region{X: 10, Y: 10, Width: 80, Height: 40}}, Encoding{Format: JPEG, MaxEdge: 64}, Encoding{Format: WebP, MaxEdge: 32})
	runs := rec.calls[before:]
	if len(runs) != 1 || runs[0][0] != "magick" {
		t.Fatalf("the second Encode ran %q, want one magick run", runs)
	}
	for _, arg := range runs[0] {
		if strings.Contains(arg, link) {
			t.Errorf("the run reads the original: %q", arg)
		}
	}
	if !slices.ContainsFunc(runs[0], func(a string) bool { return strings.HasPrefix(a, "pam:") && strings.Contains(a, ".call-") }) {
		t.Errorf("the run does not read a per-call copy: %q", runs[0])
	}
	copies, _ := filepath.Glob(filepath.Join(src.dir, ".call-*"))
	if len(copies) != 0 {
		t.Errorf("per-call files left behind: %v", copies)
	}
}

func TestReuseKeepsNothingThatExceedsDisk(t *testing.T) {
	t.Parallel()
	small := requireProcessorWith(t, Config{Resources: Resources{Disk: 64 << 10}})
	large := requireProcessor(t)
	path := heicFixtureFile(t, "adobe-256x96") // 256x96 is a 147 KB PAM, 245 KB budgeted, and 64 KiB is less than that
	enc := Encoding{Format: PNG, MaxEdge: 64}
	sel := Select{Region: Region{X: 20, Y: 10, Width: 100, Height: 50}}
	want := fusedWant(t, large, path, sel, enc)
	rec := record(small)
	src := openSourceOn(t, small, path, OpenOptions{Reuse: true})
	for range 2 {
		sameBytes(t, "over budget", encodeAll(t, src, sel, enc), want)
	}
	if len(src.kept) != 0 {
		t.Error("a decode over Resources.Disk is kept")
	}
	if entries, _ := filepath.Glob(filepath.Join(src.dir, ".kept-*")); len(entries) != 0 {
		t.Errorf("files left: %v", entries)
	}
	for _, call := range rec.ran("magick") {
		if strings.Contains(strings.Join(call, " "), "pam:") && strings.Contains(strings.Join(call, " "), ".kept-") {
			t.Errorf("a run writes a kept file: %q", call)
		}
	}
}

func TestReuseKeepsTheImagesOfTheLatestCall(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := writeFile(t, "pages.tif", tiffPages(0, 0, 0))
	enc := Encoding{Format: PNG, MaxEdge: 16}
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	keys := func() []int {
		var ks []int
		for k := range src.kept {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		return ks
	}
	encodeAll(t, src, Select{Frames: Indices(0, 1)}, enc)
	if got := keys(); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("kept %v, want [0 1]", got)
	}
	encodeAll(t, src, Select{Frames: Indices(1, 2)}, enc)
	if got := keys(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("kept %v, want [1 2]", got)
	}
	if _, err := os.Lstat(filepath.Join(src.dir, ".kept-0.pam")); err == nil {
		t.Error("the decode of image 0 is still on disk")
	}
	want := fusedWant(t, p, path, Select{Frames: Indices(2, 1)}, enc)
	sameBytes(t, "kept images", encodeAll(t, src, Select{Frames: Indices(2, 1)}, enc), want)
}

func TestReuseFirstEncodeSettlesTheSize(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := writeFile(t, "content", mustRead(t, filepath.Join("testdata", "orient", "exif-after-idat.png")))
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	if src.Info().Settled {
		t.Skip("the fixture's size is settled at Open")
	}
	encodeAll(t, src, Select{}, Encoding{Format: PNG, MaxEdge: 16})
	info := src.Info()
	env := requireMagick(t)
	uw, uh := uprightOf(t, env, path)
	if !info.Settled || info.Width != uw || info.Height != uh {
		t.Errorf("Info = %+v, want settled %dx%d", info, uw, uh)
	}
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Region: Region{X: 0, Y: 0, Width: uw + 1, Height: 1}}, Encoding{Format: PNG, MaxEdge: 16}); err == nil {
		t.Error("a region outside the settled size was accepted")
	}
}

func TestReuseSourceIsSafeForConcurrentEncodes(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := heicFixtureFile(t, "adobe-256x96")
	enc := Encoding{Format: WebP, MaxEdge: 48}
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sel := Select{Region: Region{X: i * 10, Y: i, Width: 60, Height: 40}}
			dir := t.TempDir()
			if _, _, err := src.Encode(context.Background(), dir, sel, enc); err != nil {
				t.Error(err)
			}
			_ = src.Info()
		}()
	}
	wg.Wait()
}

func TestReuseIsIgnoredByAnAnimation(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "a.gif")
	magick(t, requireMagick(t), "-size", "16x16", "xc:red", "xc:blue", "-loop", "0", path)
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	if src.Info().Kind != Animation {
		t.Fatalf("Kind = %s", src.Info().Kind)
	}
	encodeAll(t, src, Select{}, Encoding{Format: PNG, MaxEdge: 16})
	if len(src.kept) != 0 {
		t.Error("an animation kept a decode")
	}
}

// A JPEG that an Encode without Reuse decodes reduced is decoded at full size
// by a Reuse Source, which keeps that decode.
func TestReuseDecodesAJPEGAtFullSize(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "big.jpg")
	magick(t, requireMagick(t), "-size", "2000x1500", "gradient:#0020ff-#ffb000", "-quality", "90", path)
	rec := record(p)
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	encodeAll(t, src, Select{}, Encoding{Format: JPEG, MaxEdge: 100})
	for _, call := range rec.ran("magick") {
		if slices.ContainsFunc(call, func(a string) bool { return strings.HasPrefix(a, "jpeg:size=") || strings.Contains(a, "jpeg:size") }) {
			t.Errorf("a Reuse Source reduced the decode: %q", call)
		}
	}
	kd := src.kept[0]
	if kd == nil || kd.w != 2000 || kd.h != 1500 {
		t.Fatalf("kept = %+v, want the 2000x1500 decode", kd)
	}
	sel := Select{Region: Region{X: 500, Y: 400, Width: 300, Height: 200}}
	sameBytes(t, "region", encodeAll(t, src, sel, Encoding{Format: JPEG, MaxEdge: 100}),
		encodeAll(t, openSourceOn(t, p, path, OpenOptions{}), sel, Encoding{Format: JPEG, MaxEdge: 100}))
}

// Decodes that cannot be kept exactly are encoded as without Reuse, and the
// Source remembers it: no later call writes a decode that is then thrown away.
func TestReuseDeclinesWhatItCannotKeepExactly(t *testing.T) {
	t.Parallel()
	for _, in := range reuseInputs() {
		if !reuseDeclined[in.name] {
			continue
		}
		t.Run(in.name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			rec := record(p)
			path := in.path(t)
			src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
			enc := Encoding{Format: JPEG, MaxEdge: in.edges[0]}
			want := fusedWant(t, p, path, Select{}, enc)
			before := rec.count()
			for range 2 {
				sameBytes(t, "declined", encodeAll(t, src, Select{}, enc), want)
			}
			if len(src.kept) != 0 {
				t.Error("a decode was kept")
			}
			writes := 0
			for _, call := range rec.calls[before:] {
				if slices.ContainsFunc(call, func(a string) bool { return strings.Contains(a, ".kept-") }) {
					writes++
				}
			}
			if writes > 1 {
				t.Errorf("%d runs write a kept decode that is then thrown away, want one", writes)
			}
		})
	}
}

// A kept decode that fails its checks falls back, but a failure to copy it is
// an error that leaves the decode kept.
func TestReuseKeepsTheDecodeThroughAnIOFailure(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
	kd := src.kept[0]
	if err := os.Chmod(src.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	_, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, Encoding{Format: PNG, MaxEdge: 64})
	if err := os.Chmod(src.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Skip("the scratch directory stayed writable")
	}
	if src.kept[0] != kd {
		t.Error("an I/O failure dropped the kept decode")
	}
	rec := record(p)
	encodeAll(t, src, Select{}, Encoding{Format: PNG, MaxEdge: 64})
	if n := len(rec.ran("magick")); n != 1 {
		t.Errorf("the next call ran %d magick runs, want 1", n)
	}
}

// Every type marked exact has a fixture whose kept decode gives the bytes of
// a fresh decode, whole and as a region, and a Source declines the types that
// are not marked.
func TestEveryTypeMarkedExactHasAFixture(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	enc := Encoding{Format: PNG, MaxEdge: 1000}
	sel := Select{Region: Region{X: 2, Y: 2, Width: 20, Height: 16}}
	covered := map[string]bool{}
	note := func(name, path string, declined bool) {
		src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
		typ := src.Info().Type
		plain := openSourceOn(t, p, path, OpenOptions{})
		encodeAll(t, src, Select{}, enc)
		if kept := src.kept[0] != nil; kept == declined {
			t.Errorf("%s: decode kept = %v, want %v", name, kept, !declined)
			return
		}
		if declined {
			return
		}
		if !typeTable[typ].keptExact {
			t.Errorf("%s: %s is kept but not marked exact", name, typ)
		}
		ok := true
		for _, sel := range []Select{{}, sel} {
			got, want := encodeAll(t, src, sel, enc), encodeAll(t, plain, sel, enc)
			if len(got) != len(want) {
				ok = false
			}
			for n, w := range want {
				ok = ok && bytes.Equal(got[n], w)
			}
		}
		if ok {
			covered[typ] = true
		} else {
			t.Errorf("%s: the kept decode differs from a fresh one", name)
		}
	}
	for _, in := range reuseInputs() {
		note(in.name, in.path(t), reuseDeclined[in.name])
	}
	for _, fx := range fusedFixtures() {
		if fx.coder != "gif" {
			note(fx.name, fx.write(t, env, t.TempDir()), false)
		}
	}
	for typ, e := range typeTable {
		if e.keptExact && !covered[typ] {
			t.Errorf("%s is marked exact and no fixture of it is kept and byte-equal", typ)
		}
	}
}

func TestReuseKeepsOnlyWhatItsCopiesLeaveRoomFor(t *testing.T) {
	t.Parallel()
	path := heicFixtureFile(t, "adobe-256x96") // a 147 KB PAM, 245 KB budgeted
	enc := Encoding{Format: PNG, MaxEdge: 64}
	for disk, want := range map[int64]bool{300_000: false, 700_000: true} {
		p := requireProcessorWith(t, Config{Resources: Resources{Disk: disk}})
		src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
		encodeAll(t, src, Select{}, enc)
		if got := src.kept[0] != nil; got != want {
			t.Errorf("Disk %d: kept = %v, want %v", disk, got, want)
		}
	}
}

func TestReuseHonoursTheContextOfAReadBack(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	kd := src.kept[0]
	rec := record(p)
	if _, _, err := src.Encode(ctx, t.TempDir(), Select{}, Encoding{Format: PNG, MaxEdge: 64}); !errors.Is(err, context.Canceled) {
		t.Errorf("Encode with a cancelled context = %v", err)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("a cancelled read-back ran %d tools", n)
	}
	if src.kept[0] != kd {
		t.Error("a cancelled call dropped the kept decode")
	}
	if left, _ := filepath.Glob(filepath.Join(src.dir, ".call-*")); len(left) != 0 {
		t.Errorf("per-call files left: %v", left)
	}
}

// Whether a TIFF declares alpha is known only from a full read; a kept decode
// carries what the first run saw, so a later call that resizes uses the same
// filter as a fresh decode.
func TestReuseKeepsTheFilterOfATIFFWithAlpha(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "a.tif")
	magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000",
		"(", "-size", "96x64", "gradient:white-black", ")", "-alpha", "off", "-compose", "copy_opacity", "-composite", path)
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	big, small := Encoding{Format: PNG, MaxEdge: 1000}, Encoding{Format: PNG, MaxEdge: 24}
	encodeAll(t, src, Select{}, big)
	if src.kept[0] == nil {
		t.Fatal("not kept")
	}
	sameBytes(t, "resized from the kept decode", encodeAll(t, src, Select{}, small), fusedWant(t, p, path, Select{}, small))
}

// A kept decode counts its profile against the budget.
func TestReuseBudgetCountsTheKeptProfile(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
	kd := src.kept[0]
	if kd.icc == "" {
		t.Fatal("no profile kept")
	}
	pam, icc := mustRead(t, kd.pam), mustRead(t, kd.icc)
	if got, want := src.keptBytes(), int64(len(pam)+len(icc)); got != want {
		t.Errorf("keptBytes = %d, want %d", got, want)
	}
}

// fakeKeepLine is the keep line of an sRGB 8-bit image without alpha.
const fakeKeepLine = "keep|0|0|Undefined|sRGB|8|DirectClass sRGB|False||"

// recordAgain asks recordKept to record the files of src's kept decode of
// image 0 again, as a run that wrote them would be.
func recordAgain(src *Source, kd *keptDecode) (*keptDecode, error) {
	kp := &keepPlan{pam: kd.pam, icc: kd.icc, line: fakeKeepLine}
	return src.recordKept(kp, src.stills[0])
}

func TestReuseRecordKeptChecksWhatTheRunWrote(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
	kd := src.kept[0]
	if _, err := recordAgain(src, kd); err != nil {
		t.Fatalf("a good record = %v", err)
	}
	es := src.stills[0]
	upright := es.upright

	es.upright = [2]int{upright[1], upright[0]}
	if _, err := recordAgain(src, kd); !failedCheck(err) {
		t.Errorf("an upright size that differs = %v, want a failed check", err)
	}
	es.upright = upright
	es.unsettled = true
	if _, err := recordAgain(src, kd); !failedCheck(err) {
		t.Errorf("an unsettled size = %v, want a failed check", err)
	}
	es.unsettled = false

	size := src.keptBytes()
	other := kd
	// The decode and its profile alone do not fit.
	delete(src.kept, 0)
	p.res.Disk = size*2 - 1
	if _, err := recordAgain(src, kd); !failedCheck(err) {
		t.Errorf("a decode over the budget = %v, want a failed check", err)
	}
	// They fit, but not beside the decodes already kept.
	src.kept[0] = other
	p.res.Disk = size*2 + size
	if _, err := recordAgain(src, kd); err == nil || failedCheck(err) {
		t.Errorf("a decode over the budget beside others = %v, want a failure that is not a failed check", err)
	}
	// The profile counts: the decode alone would fit.
	delete(src.kept, 0)
	pam := mustRead(t, other.pam)
	p.res.Disk = int64(len(pam)) * 2
	if _, err := recordAgain(src, kd); err == nil {
		t.Error("a profile that does not fit was recorded")
	}
}

func TestReuseRemembersOnlyFailuresNoLaterCallWouldFix(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err       error
		permanent bool
	}{
		"an I/O error":      {errors.New("no space left on device"), false},
		"a failed check":    {checkFailed("no"), true},
		"a wrapped failure": {fmt.Errorf("copy: %w", checkFailed("no")), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
			kd := src.kept[0]
			delete(src.kept, 0)
			es := src.stills[0]
			es.keep = &keepPlan{pam: kd.pam, icc: kd.icc, line: fakeKeepLine, err: tc.err}
			src.adoptKept(0, es)
			if src.noKeep[0] != tc.permanent {
				t.Errorf("noKeep = %v, want %v", src.noKeep[0], tc.permanent)
			}
		})
	}
	t.Run("an unreadable profile", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		p := requireProcessor(t)
		src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
		kd := src.kept[0]
		if err := os.Chmod(kd.icc, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := recordAgain(src, kd); err == nil || failedCheck(err) {
			t.Errorf("an unreadable profile = %v, want a failure that is not a failed check", err)
		}
	})
}

func TestParseKeepFactsAcceptsOnlyWhatAPAMKeepsExactly(t *testing.T) {
	t.Parallel()
	line := func(units, cs, depth, sampling, format string) string {
		return strings.Join([]string{"keep", "72", "72", units, cs, depth, "DirectClass sRGB", "False", sampling, format}, "|")
	}
	good := line("PixelsPerInch", "sRGB", "8", "2x2,1x1,1x1", "")
	facts, err := parseKeepFacts(good)
	if err != nil || facts.sampling != "2x2,1x1,1x1" || facts.units != "PixelsPerInch" || facts.xres != "72" {
		t.Fatalf("parse = %+v, %v", facts, err)
	}
	if _, err := parseKeepFacts(line("Undefined", "Gray", "16", "", "unsigned")); err != nil {
		t.Errorf("a gray 16-bit unsigned image: %v", err)
	}
	for name, bad := range map[string]string{
		"units":            line("Furlongs", "sRGB", "8", "", ""),
		"colorspace":       line("Undefined", "Lab", "8", "", ""),
		"depth 32":         line("Undefined", "sRGB", "32", "", ""),
		"depth 0":          line("Undefined", "sRGB", "0", "", ""),
		"depth text":       line("Undefined", "sRGB", "x", "", ""),
		"sampling":         line("Undefined", "sRGB", "8", "2x2;rm", ""),
		"sampling partial": line("Undefined", "sRGB", "8", "2x2,", ""),
		"sampling space":   line("Undefined", "sRGB", "8", "2x2, 1x1", ""),
		"float samples":    line("Undefined", "sRGB", "16", "", "floating-point"),
		"short line":       "keep|1|2",
		"no prefix":        strings.TrimPrefix(good, "keep|"),
	} {
		if _, err := parseKeepFacts(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A call whose keep run fails is encoded without keeping, and so is every
// later one.
func TestReuseEncodesAfterAFailedKeepRun(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := heicFixtureFile(t, "adobe-256x96")
	enc := Encoding{Format: PNG, MaxEdge: 64}
	want := fusedWant(t, p, path, Select{}, enc)
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	// A file cannot be written where a directory with something in it is.
	block := filepath.Join(src.dir, ".kept-0.pam")
	if err := os.Mkdir(block, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(block, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "after a failed keep run", encodeAll(t, src, Select{}, enc), want)
	if len(src.kept) != 0 || !src.noKeep[0] {
		t.Errorf("kept %d, noKeep %v, want nothing kept and the image remembered", len(src.kept), src.noKeep)
	}
	rec := record(p)
	sameBytes(t, "later call", encodeAll(t, src, Select{}, enc), want)
	for _, call := range rec.calls {
		if slices.ContainsFunc(call, func(a string) bool { return strings.Contains(a, ".kept-") }) {
			t.Errorf("a later run writes a kept file: %q", call)
		}
	}
}

// A call that fails for a reason of its own still fails, and the Source keeps
// trying to keep the image afterwards.
func TestReuseFailedEncodeIsNotRememberedAsUnkeepable(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, heicFixtureFile(t, "adobe-256x96"), OpenOptions{Reuse: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := src.Encode(ctx, t.TempDir(), Select{}, Encoding{Format: PNG, MaxEdge: 64}); err == nil {
		t.Fatal("Encode with a cancelled context succeeded")
	}
	if src.noKeep[0] {
		t.Error("a cancelled call marked the image unkeepable")
	}
	encodeAll(t, src, Select{}, Encoding{Format: PNG, MaxEdge: 64})
	if src.kept[0] == nil {
		t.Error("the decode was not kept by a later call")
	}
}

// A JPEG whose full decode exceeds the caps is still encoded, reduced, as
// without Reuse; a region of it is refused as before.
func TestReuseHonoursTheCapsAFullDecodeExceeds(t *testing.T) {
	t.Parallel()
	p := requireProcessorWith(t, Config{Caps: Caps{MaxFramePixels: 1_000_000}})
	path := filepath.Join(t.TempDir(), "big.jpg")
	magick(t, requireMagick(t), "-size", "2000x1500", "gradient:#0020ff-#ffb000", "-quality", "90", path)
	enc := Encoding{Format: JPEG, MaxEdge: 100}
	want := fusedWant(t, p, path, Select{}, enc)
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	sameBytes(t, "over the caps", encodeAll(t, src, Select{}, enc), want)
	if len(src.kept) != 0 {
		t.Error("a decode over the caps was kept")
	}

	sel := Select{Region: Region{X: 100, Y: 100, Width: 300, Height: 200}}
	_, _, perr := openSourceOn(t, p, path, OpenOptions{}).Encode(context.Background(), t.TempDir(), sel, enc)
	_, _, rerr := src.Encode(context.Background(), t.TempDir(), sel, enc)
	if (perr == nil) != (rerr == nil) {
		t.Errorf("a region: without Reuse %v, with Reuse %v", perr, rerr)
	}
}

// A kept file that was replaced is refused before it is read.
func TestReuseChecksTheKeptIdentityBeforeReading(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
	kd := src.kept[0]
	data := mustRead(t, kd.pam)
	if err := os.Remove(kd.pam); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kd.pam, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// A cancelled call reaches the context check only once the file is read.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.copyKept(ctx, kd, region{}); !failedCheck(err) {
		t.Errorf("copyKept of a replaced file = %v, want a failed check", err)
	}
}

// changingContext rewrites a file the first time the copy asks whether the
// call is done, which it does before it reads a row.
type changingContext struct {
	context.Context
	once   sync.Once
	change func()
}

func (c *changingContext) Err() error {
	c.once.Do(c.change)
	return nil
}

// A kept file that changes while it is read is refused, though it was the
// file written when the read began.
func TestReuseChecksTheKeptIdentityAfterReading(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := reusedAfterFirst(t, p, heicFixtureFile(t, "adobe-256x96"), Encoding{Format: PNG, MaxEdge: 64})
	kd := src.kept[0]
	ctx := &changingContext{Context: context.Background(), change: func() {
		data := mustRead(t, kd.pam)
		data[len(kd.header)] ^= 0xff
		if err := os.WriteFile(kd.pam, data, 0o600); err != nil {
			t.Error(err)
		}
	}}
	path, err := src.copyKept(ctx, kd, region{})
	if !failedCheck(err) {
		t.Errorf("copyKept of a file rewritten as it was read = %q, %v, want a failed check", path, err)
	}
	if left, _ := filepath.Glob(filepath.Join(src.dir, ".call-*")); len(left) != 0 {
		t.Errorf("per-call files left: %v", left)
	}
}

// A profile that cannot be copied beside a WebP's decode leaves it unkept for
// this call only.
func TestReuseWebPProfileCopyFailureIsNotRemembered(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	var path string
	for _, fx := range fusedFixtures() {
		if fx.name == "webp lossless" {
			path = fx.write(t, env, t.TempDir())
		}
	}
	if path == "" {
		t.Fatal("no webp lossless fixture")
	}
	enc := Encoding{Format: PNG, MaxEdge: 400}
	want := fusedWant(t, p, path, Select{}, enc)
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	block := filepath.Join(src.dir, ".kept-0.icc")
	if err := os.Mkdir(block, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(block, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "profile not copied", encodeAll(t, src, Select{}, enc), want)
	if len(src.kept) != 0 || src.noKeep[0] {
		t.Errorf("kept %d, noKeep %v, want nothing kept and no memory of it", len(src.kept), src.noKeep)
	}
	if err := os.RemoveAll(block); err != nil {
		t.Fatal(err)
	}
	sameBytes(t, "later call", encodeAll(t, src, Select{}, enc), want)
	if src.kept[0] == nil {
		t.Error("a later call did not keep the decode")
	}
}

// A kept decode of an opaque WebP resizes with the filter of a full-size
// decode.
func TestReuseKeepsTheFilterOfAnOpaqueWebP(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "o.webp")
	magick(t, requireMagick(t), "-size", "96x64", "gradient:#0020ff-#ffb000", "-alpha", "off", "-quality", "70", path)
	src := openSourceOn(t, p, path, OpenOptions{Reuse: true})
	big, small := Encoding{Format: PNG, MaxEdge: 1000}, Encoding{Format: PNG, MaxEdge: 24}
	encodeAll(t, src, Select{}, big)
	if src.kept[0] == nil {
		t.Fatal("not kept")
	}
	// A fresh decode to a small target is reduced, which a full-size decode
	// does not give; the first call of a Reuse Source decodes at full size.
	want := encodeAll(t, openSourceOn(t, p, path, OpenOptions{Reuse: true}), Select{}, small)
	sameBytes(t, "resized from the kept decode", encodeAll(t, src, Select{}, small), want)
}
