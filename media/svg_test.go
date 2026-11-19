package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/davars/winnow-media/tool"
)

var svgConfig = Config{RenderTypes: []string{"image/svg+xml"}}

func requireSVGProcessor(t *testing.T) (*Processor, *recorder) {
	t.Helper()
	return sourceProcessorWith(t, svgConfig)
}

// svgFixtures returns the SVGs of the differential tests.
func svgFixtures(t *testing.T) []pdfFixture {
	t.Helper()
	stored := func(file string) string {
		abs, err := filepath.Abs(filepath.Join("testdata", "svg", file))
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	gen := func(name, svg string) pdfFixture {
		return pdfFixture{name, writeFile(t, "fixture.svg", []byte(svg))}
	}
	const ns = `xmlns="http://www.w3.org/2000/svg"`
	return []pdfFixture{
		{"antialiased shapes with alpha, fractional size", stored("alpha-shapes.svg")},
		{"text, with a CJK fallback", stored("text.svg")},
		{"viewBox only", stored("viewbox-only.svg")},
		{"inline PNG", stored("data-png.svg")},
		gen("gradients", `<svg `+ns+` width="120" height="80"><defs><linearGradient id="g" x1="0" x2="1"><stop offset="0" stop-color="#f00"/><stop offset="1" stop-color="#00f" stop-opacity="0.2"/></linearGradient>`+
			`<radialGradient id="r"><stop offset="0" stop-color="#fff"/><stop offset="1" stop-color="#080"/></radialGradient></defs>`+
			`<rect width="120" height="40" fill="url(#g)"/><circle cx="60" cy="60" r="20" fill="url(#r)"/></svg>`),
		gen("filters", `<svg `+ns+` width="100" height="100"><defs><filter id="b"><feGaussianBlur stdDeviation="3"/></filter>`+
			`<filter id="s"><feDropShadow dx="3" dy="3" stdDeviation="2" flood-color="#000" flood-opacity="0.5"/></filter></defs>`+
			`<rect x="10" y="10" width="40" height="40" fill="#c00" filter="url(#b)"/><circle cx="70" cy="70" r="15" fill="#0a0" filter="url(#s)"/></svg>`),
		gen("units and a transform", `<svg `+ns+` width="2in" height="1.5cm" viewBox="0 0 40 30"><g transform="rotate(15 20 15)"><rect x="5" y="5" width="30" height="20" fill="#06c" opacity="0.7"/></g></svg>`),
	}
}

// refSVG draws path as the command a caller of ImageMagick would: the svg:
// coder, the inherited environment, the default policy, no sandbox, and a PATH
// of the tool directory alone, so that no inkscape is found.
func refSVG(t *testing.T, path string) []byte {
	t.Helper()
	bin, err := tool.Resolve("magick")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "page.png")
	cmd := exec.Command(bin, "-limit", "memory", "1GiB", "-limit", "map", "2GiB", "-limit", "time", "300", "svg:"+path+"[0]", "-depth", "8", out)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "MAGICK_CONFIGURE_PATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(bin))
	if res, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("magick svg:%s: %v\n%s", filepath.Base(path), err, res)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withoutDateChunks drops the chunks of a PNG that record when it was
// written: tIME, and the tEXt chunks whose keyword starts with "date:".
func withoutDateChunks(t *testing.T, png []byte) []byte {
	t.Helper()
	out := append([]byte{}, png[:8]...)
	for i := 8; i < len(png); {
		n := int(binary.BigEndian.Uint32(png[i:]))
		typ := string(png[i+4 : i+8])
		end := i + 12 + n
		keep := typ != "tIME" && (typ != "tEXt" || !bytes.HasPrefix(png[i+8:end-4], []byte("date:")))
		if keep {
			out = append(out, png[i:end]...)
		}
		i = end
	}
	return out
}

func TestPNGChunkFilter(t *testing.T) {
	chunk := func(typ string, data string) []byte {
		b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		b = append(b, typ...)
		b = append(b, data...)
		return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE([]byte(typ+data)))
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", "0123456789abc")...)
	png = append(png, chunk("tIME", "1234567")...)
	png = append(png, chunk("tEXt", "date:create\x002026")...)
	png = append(png, chunk("tEXt", "Comment\x00kept")...)
	png = append(png, chunk("IEND", "")...)
	want := append([]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", "0123456789abc")...)
	want = append(append(want, chunk("tEXt", "Comment\x00kept")...), chunk("IEND", "")...)
	if got := withoutDateChunks(t, png); !bytes.Equal(got, want) {
		t.Errorf("filtered PNG differs from the one without its date chunks")
	}
}

// librsvg through ImageMagick's rsvg: coder under the SVG policy draws what
// the svg: coder draws.
func TestRenderSVGMatchesMagickSVGCoder(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	env := requireMagick(t)
	bin, _ := tool.Resolve("magick")
	t.Logf("magick resolves to %s", bin)
	for _, fx := range svgFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			dir := t.TempDir()
			r, _, err := p.RenderSVG(context.Background(), fx.path, dir, SVGRender{})
			if err != nil {
				t.Fatal(err)
			}
			if r.Name != "000.png" || r.Page != 0 || r.Width < 1 || r.Height < 1 {
				t.Fatalf("Raster = %+v", r)
			}
			got, err := os.ReadFile(filepath.Join(dir, r.Name))
			if err != nil {
				t.Fatal(err)
			}
			want := refSVG(t, fx.path)
			refPath := writeFile(t, "ref.png", want)

			// 1. The same pixels, decoded to 16-bit RGBA by one run.
			pixels := func(path string) []byte {
				return magick(t, env, path, "-depth", "16", "-define", "quantum:format=unsigned", "rgba:-")
			}
			if !bytes.Equal(pixels(filepath.Join(dir, r.Name)), pixels(refPath)) {
				t.Error("decoded pixels differ from the svg: coder's")
			}
			// 2. The same PNG, but for the time it records.
			if !bytes.Equal(got, withoutDateChunks(t, want)) {
				t.Errorf("PNG bytes differ from the svg: coder's with its date chunks dropped (%d against %d bytes)", len(got), len(want))
			}
			if bytes.Contains(got, []byte("tIME")) || bytes.Contains(got, []byte("date:")) {
				t.Error("the PNG records a time")
			}
			// 3. The same outputs of an Encode.
			encodings := []Encoding{{Format: JPEG, MaxEdge: 1024}, {Format: WebP, MaxEdge: 1024}, {Format: PNG, MaxEdge: 512}}
			var outs [2][][]byte
			for k, path := range []string{filepath.Join(dir, r.Name), refPath} {
				src, _, err := p.Open(context.Background(), path, OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				out := t.TempDir()
				images, _, err := src.Encode(context.Background(), out, Select{}, encodings...)
				_ = src.Close()
				if err != nil {
					t.Fatal(err)
				}
				for _, img := range images {
					b, _ := os.ReadFile(filepath.Join(out, img.Name))
					outs[k] = append(outs[k], b)
				}
			}
			if len(outs[0]) != 3 {
				t.Fatalf("%d outputs, want 3", len(outs[0]))
			}
			for i := range outs[0] {
				if !bytes.Equal(outs[0][i], outs[1][i]) {
					t.Errorf("encoding %d differs", i)
				}
			}
		})
	}
}

func TestRenderSVGIsDeterministic(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	for _, fx := range svgFixtures(t) {
		var outs [2][]byte
		for k := range outs {
			dir := t.TempDir()
			if _, _, err := p.RenderSVG(context.Background(), fx.path, dir, SVGRender{}); err != nil {
				t.Fatalf("%s: %v", fx.name, err)
			}
			outs[k], _ = os.ReadFile(filepath.Join(dir, "000.png"))
		}
		if !bytes.Equal(outs[0], outs[1]) {
			t.Errorf("%s: two renders differ", fx.name)
		}
	}
}

func TestRenderSVGDPIScalesTheDrawing(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	path := filepath.Join("testdata", "svg", "alpha-shapes.svg")
	size := func(dpi int) (int, int) {
		r, _, err := p.RenderSVG(context.Background(), path, t.TempDir(), SVGRender{DPI: dpi})
		if err != nil {
			t.Fatalf("DPI %d: %v", dpi, err)
		}
		return r.Width, r.Height
	}
	w0, h0 := size(0)
	w96, h96 := size(96)
	w192, h192 := size(192)
	if w0 != w96 || h0 != h96 {
		t.Errorf("DPI 96 draws %dx%d, DPI 0 draws %dx%d", w96, h96, w0, h0)
	}
	if w192 != 2*w0 || h192 != 2*h0 {
		t.Errorf("DPI 192 draws %dx%d, want twice %dx%d", w192, h192, w0, h0)
	}
	for _, dpi := range []int{-1, 9601} {
		_, _, err := p.RenderSVG(context.Background(), path, t.TempDir(), SVGRender{DPI: dpi})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("DPI %d: err = %v, want ErrInvalidRequest", dpi, err)
		}
	}
}

func TestRenderSVGOutputIsOpaque(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	env := requireMagick(t)
	dir := t.TempDir()
	if _, _, err := p.RenderSVG(context.Background(), "testdata/svg/alpha-shapes.svg", dir, SVGRender{}); err != nil {
		t.Fatal(err)
	}
	png, err := os.ReadFile(filepath.Join(dir, "000.png"))
	if err != nil {
		t.Fatal(err)
	}
	// Colour type 4 and 6 have an alpha channel, and a tRNS chunk gives the
	// other types transparent entries.
	if colour := png[25]; colour == 4 || colour == 6 || bytes.Contains(png, []byte("tRNS")) {
		t.Errorf("PNG colour type %d, tRNS %v: the drawing has transparency", colour, bytes.Contains(png, []byte("tRNS")))
	}
	if got := string(magick(t, env, filepath.Join(dir, "000.png"), "-format", "%[pixel:p{0,0}]", "info:")); got != "srgb(255,255,255)" {
		t.Errorf("corner pixel = %q, want white", got)
	}
}

func TestRenderSVGAdmission(t *testing.T) {
	p, rec := requireSVGProcessor(t)
	ctx := context.Background()
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="5" height="5"/></svg>`)
	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"gzip", append([]byte{0x1f, 0x8b, 8}, make([]byte, 40)...), ErrUnsupportedType},
		{"not markup", []byte("plain text"), ErrUnsupportedType},
		{"empty", nil, ErrUnsupportedType},
	} {
		before := rec.count()
		_, _, err := p.RenderSVG(ctx, writeFile(t, "x.svg", c.data), t.TempDir(), SVGRender{})
		if !errors.Is(err, c.want) || rec.count() != before {
			t.Errorf("%s: err = %v after %d runs, want %v with no tool run", c.name, err, rec.count()-before, c.want)
		}
	}
	// A byte order mark and leading whitespace are fine.
	for _, data := range [][]byte{append([]byte("\xef\xbb\xbf"), svg...), append([]byte("\n\t "), svg...), append([]byte(`<?xml version="1.0"?>`+"\n"), svg...)} {
		if _, _, err := p.RenderSVG(ctx, writeFile(t, "x.svg", data), t.TempDir(), SVGRender{}); err != nil {
			t.Errorf("%q: %v", data[:5], err)
		}
	}
	if _, _, err := p.RenderSVG(ctx, t.TempDir(), t.TempDir(), SVGRender{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a directory: %v, want ErrInvalidRequest", err)
	}
	small := requireProcessorWith(t, Config{RenderTypes: svgConfig.RenderTypes, Caps: Caps{MaxInputBytes: 50}})
	if _, _, err := small.RenderSVG(ctx, writeFile(t, "x.svg", svg), t.TempDir(), SVGRender{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over MaxInputBytes: %v, want ErrTooLarge", err)
	}
	other := requireProcessor(t)
	if _, _, err := other.RenderSVG(ctx, writeFile(t, "x.svg", svg), t.TempDir(), SVGRender{}); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("a Processor without the type: %v, want ErrUnsupportedType", err)
	}
	pdfOnly := requireProcessorWith(t, pdfConfig)
	if _, _, err := pdfOnly.RenderSVG(ctx, writeFile(t, "x.svg", svg), t.TempDir(), SVGRender{}); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("a PDF-only Processor: %v, want ErrUnsupportedType", err)
	}
}

func TestRenderSVGRefusesLargeDrawingsFromTheHeader(t *testing.T) {
	p, rec := requireSVGProcessor(t)
	big := writeFile(t, "big.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="60000" height="60000"><rect width="60000" height="60000"/></svg>`))
	dir := t.TempDir()
	_, _, err := p.RenderSVG(context.Background(), big, dir, SVGRender{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if rec.count() != 1 || rec.calls[0][1] != "identify" {
		t.Errorf("runs = %v, want the size read alone", rec.calls)
	}
	capped := requireProcessorWith(t, Config{RenderTypes: svgConfig.RenderTypes, Caps: Caps{MaxFramePixels: 1000}})
	if _, _, err := capped.RenderSVG(context.Background(), "testdata/svg/alpha-shapes.svg", t.TempDir(), SVGRender{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over MaxFramePixels: %v, want ErrTooLarge", err)
	}
	if _, _, err := p.RenderSVG(context.Background(), "testdata/svg/alpha-shapes.svg", dir, SVGRender{DPI: 9600}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("DPI 9600 of a 201x100 drawing: %v, want ErrTooLarge (a drawing over 16384 pixels)", err)
	}
}

func TestRenderSVGLeavesDirAsItFoundIt(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := "testdata/svg/alpha-shapes.svg"
	if _, _, err := p.RenderSVG(context.Background(), path, nonEmpty, SVGRender{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("non-empty dir: %v", err)
	}
	if _, _, err := p.RenderSVG(context.Background(), path, filepath.Join(t.TempDir(), "absent"), SVGRender{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("missing dir: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	if _, _, err := p.RenderSVG(ctx, path, dir, SVGRender{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	broken := writeFile(t, "broken.svg", []byte("<svg><unclosed"))
	var te *ToolError
	if _, u, err := p.RenderSVG(context.Background(), broken, dir, SVGRender{}); !errors.As(err, &te) || u.CPU <= 0 {
		t.Errorf("broken SVG: %v, usage %+v, want a *ToolError with usage", err, u)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("a failed call left files")
	}
	if entries, _ := os.ReadDir(p.tmpDir()); len(entries) != 0 {
		t.Errorf("%d scratch entries left behind", len(entries))
	}
	_ = p.Close()
	if _, _, err := p.RenderSVG(context.Background(), path, t.TempDir(), SVGRender{}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed: %v, want ErrClosed", err)
	}
}

func TestRenderSVGReportsUsage(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	_, u, err := p.RenderSVG(context.Background(), "testdata/svg/alpha-shapes.svg", t.TempDir(), SVGRender{})
	if err != nil || u.CPU <= 0 || u.MaxRSS <= 0 {
		t.Errorf("usage = %+v (%v)", u, err)
	}
}

func TestRenderSVGArgv(t *testing.T) {
	for _, c := range []struct {
		name string
		r    SVGRender
	}{{"render-svg", SVGRender{}}, {"render-svg-dpi", SVGRender{DPI: 192}}} {
		t.Run(c.name, func(t *testing.T) {
			p, rec, g := goldenProcessor(t, c.name, svgConfig)
			g.out = t.TempDir()
			if _, _, err := p.RenderSVG(context.Background(), "testdata/svg/alpha-shapes.svg", g.out, c.r); err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.name, goldenArgv(rec.calls, "/nonexistent-out"))
		})
	}
}

func TestSVGPolicyAllowsOneReaderAndNoDelegate(t *testing.T) {
	var reads, writes []string
	for _, r := range svgPolicy(defaultResources()) {
		switch {
		case r.domain == "delegate" && (r.rights != "none" || r.pattern != "*"):
			t.Errorf("delegate rule %v", r)
		case r.domain == "coder" && r.rights == "read":
			reads = append(reads, r.pattern)
		case r.domain == "coder" && r.rights == "write":
			writes = append(writes, r.pattern)
		}
	}
	if !slices.Equal(reads, []string{"RSVG"}) || !slices.Equal(writes, []string{"INFO", "NULL", "PNG"}) {
		t.Errorf("reads %v, writes %v", reads, writes)
	}
}

func TestNewProcessorInstallsTheSVGPolicyOnlyForSVG(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	p, err := NewProcessor(context.Background(), Config{RenderTypes: []string{"image/svg+xml", "application/pdf", "image/svg+xml"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if !slices.Equal(slices.Sorted(maps.Keys(p.renders)), []string{"application/pdf", "image/svg+xml"}) {
		t.Errorf("render types = %v, want both once each", slices.Sorted(maps.Keys(p.renders)))
	}
	got, err := os.ReadFile(filepath.Join(p.dir, "svg", "policy.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, policyXML(svgPolicy(p.res))) {
		t.Error("the installed SVG policy is not svgPolicy's")
	}
	// A Processor with no SVG rendering installs no SVG policy.
	q := requireProcessorWith(t, pdfConfig)
	if _, err := os.Stat(filepath.Join(q.dir, "svg")); err == nil {
		t.Error("an SVG policy was installed without image/svg+xml in RenderTypes")
	}
}

// Every run of RenderSVG, the size read and the drawing, is confined to the
// input, a directory of its own and the SVG policy's directory, and reads the
// SVG policy.
func TestRenderSVGRunsAreConfined(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	var mu sync.Mutex
	var specs []runSpec
	inner := p.start
	p.start = func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
		mu.Lock()
		specs = append(specs, spec)
		mu.Unlock()
		return inner(ctx, spec, stdout, stderr)
	}
	if _, _, err := p.RenderSVG(context.Background(), "testdata/svg/alpha-shapes.svg", t.TempDir(), SVGRender{}); err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("%d runs, want the size read and the drawing", len(specs))
	}
	for _, s := range specs {
		c := s.Confine
		if c == nil || !c.Input || len(c.Write) == 0 || !strings.HasPrefix(c.Write[0], p.tmpDir()) || !slices.Contains(c.Read, p.svgDir()) {
			t.Errorf("%v ran with confinement %+v, want the input, a directory under the Processor's and the SVG policy", s.Args, c)
			continue
		}
		for _, w := range c.Write {
			if w == p.dir || w == p.tmpDir() {
				t.Errorf("%v may write in %s, which holds other runs' files", s.Args, w)
			}
		}
		if !slices.Contains(s.Env, "MAGICK_CONFIGURE_PATH="+p.svgDir()) {
			t.Errorf("%v ran with environment %v, want the SVG policy's directory", s.Args, s.Env)
		}
	}
}

// The SVG policy check reads back the policy SVG runs see and refuses any
// other.
func TestVerifySVGPolicyRefusesAnotherPolicy(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	rules := svgPolicy(p.res)
	if err := p.verifySVGPolicy(context.Background(), rules); err != nil {
		t.Fatalf("the installed policy: %v", err)
	}
	wider := append(append([]policyRule{}, rules...), policyRule{domain: "coder", rights: "read", pattern: "SVG"})
	if err := os.WriteFile(filepath.Join(p.svgDir(), "policy.xml"), policyXML(wider), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.verifySVGPolicy(context.Background(), rules); err == nil {
		t.Error("a policy that also lets the SVG coder read was accepted")
	}
}
