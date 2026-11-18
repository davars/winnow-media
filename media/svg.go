package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxSVGDPI is the largest SVGRender.DPI.
const maxSVGDPI = 9600

// svgDPI is the resolution at which an SVG is drawn at its own size.
const svgDPI = 96

// svgSniffBytes is how far into a file the first tag may start.
const svgSniffBytes = 4096

// SVGRender says how RenderSVG draws an SVG. The zero value draws it at its
// own size, one pixel per CSS pixel.
type SVGRender struct {
	// DPI is the resolution its lengths are drawn at, 1 to 9600: 96, or zero,
	// is its own size, and 192 twice it.
	DPI int
}

// svgDir is where the policy of SVG renders is installed.
func (p *Processor) svgDir() string { return filepath.Join(p.dir, "svg") }

func (p *Processor) installSVGPolicy(rules []policyRule) error {
	if err := os.Mkdir(p.svgDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.svgDir(), "policy.xml"), policyXML(rules), 0o600)
}

// verifySVGPolicy reads the policy an SVG run will see back from ImageMagick,
// in a run configured as SVG runs are, and refuses unless it is exactly rules.
func (p *Processor) verifySVGPolicy(ctx context.Context, rules []policyRule) error {
	run, err := p.runTempDir()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(run) }()
	ctx = withRenderRun(ctx, renderRun{dir: run, policyDir: p.svgDir()})
	res, err := p.run(ctx, nil, "magick", "-list", "policy")
	if err != nil {
		return fmt.Errorf("listing the SVG policy: %w", err)
	}
	files, err := parsePolicyList(res.Stdout)
	if err != nil {
		return err
	}
	if err := checkEffectivePolicy(files, filepath.Join(p.svgDir(), "policy.xml"), rules); err != nil {
		return fmt.Errorf("SVG policy: %w", err)
	}
	return nil
}

// RenderSVG draws the SVG file at path into dir, which must exist and be
// empty, as one 8-bit PNG named 000.png, and describes it. The drawing is
// opaque: transparent areas are drawn onto white. Nothing outside the file is
// read: an image, stylesheet, entity or other resource is drawn only when the
// file holds it inline, as a data: URL; a reference to another file or to the
// network is not followed, and an external entity fails the call.
//
// The file must be a regular file of at most Caps.MaxInputBytes holding an
// uncompressed SVG document (compressed SVGZ is refused with
// ErrUnsupportedType), and image/svg+xml must be among Config.RenderTypes.
// Its size is read before it is drawn: a drawing with a side over 16384
// pixels or more than Caps.MaxFramePixels pixels is refused with ErrTooLarge.
// Resources bound the run as they bound ImageMagick's.
//
// A call that fails or is cancelled leaves dir as it found it. The Usage
// covers the tool runs of the call. Errors wrap ErrUnsupportedType,
// ErrTooLarge, ErrInvalidRequest, ErrInputChanged, ErrTimeLimit, ErrClosed or
// ctx's error, or are a *ToolError.
func (p *Processor) RenderSVG(ctx context.Context, path, dir string, r SVGRender) (Raster, Usage, error) {
	if !p.beginOpen() {
		return Raster{}, Usage{}, fmt.Errorf("render svg: processor: %w", ErrClosed)
	}
	defer p.opening.Done()
	var m meter
	raster, err := p.renderSVG(ctx, &m, path, dir, r)
	if err != nil {
		return Raster{}, m.usage, fmt.Errorf("render svg: %w", err)
	}
	return raster, m.usage, nil
}

func (p *Processor) renderSVG(ctx context.Context, m *meter, path, dir string, r SVGRender) (Raster, error) {
	if !p.renders[typeSVG] {
		return Raster{}, unrecognized(typeSVG + " is not among Config.RenderTypes")
	}
	if r.DPI < 0 || r.DPI > maxSVGDPI {
		return Raster{}, invalid("DPI %d is outside 1..%d", r.DPI, maxSVGDPI)
	}
	if err := requireEmptyDir(dir); err != nil {
		return Raster{}, err
	}
	f, abs, fi, err := p.openRegular(path)
	if err != nil {
		return Raster{}, err
	}
	defer func() { _ = f.Close() }()
	id := idOf(fi)
	if err := sniffSVG(f, fi.Size()); err != nil {
		return Raster{}, err
	}
	in, release, err := p.inputDir()
	if err != nil {
		return Raster{}, err
	}
	defer release()
	ref, err := inputRef(in, abs)
	if err != nil {
		return Raster{}, err
	}
	run, err := p.runTempDir()
	if err != nil {
		return Raster{}, err
	}
	defer func() { _ = os.RemoveAll(run) }()
	ctx = withRenderRun(ctx, renderRun{dir: run, policyDir: p.svgDir()})
	env := p.env(m, f).pinnedTo(f, id, "while it was drawn")

	var density []string
	if r.DPI != 0 && r.DPI != svgDPI {
		density = []string{"-density", strconv.Itoa(r.DPI)}
	}
	input := "rsvg:" + ref + "[0]"

	// The size is read from the header, which costs no drawing.
	args := append(append([]string{"identify", "-ping"}, density...), "-format", "%w %h\n", input)
	res, err := env.runMagick(ctx, args...)
	if err != nil {
		return Raster{}, fmt.Errorf("reading the size: %w", err)
	}
	var w, h int
	if _, err := fmt.Sscanf(string(res.Stdout), "%d %d", &w, &h); err != nil || w < 1 || h < 1 {
		return Raster{}, unrecognized("the SVG has no size")
	}
	if err := checkSVGSize(p.caps, w, h); err != nil {
		return Raster{}, err
	}

	// Without the exclusion the PNG carries the wall-clock time.
	const out = "000.png"
	args = append(append([]string{}, density...), input, "-depth", "8", "-define", "png:exclude-chunks=date,time", out)
	if _, err := env.runMagick(ctx, args...); err != nil {
		return Raster{}, err
	}
	w, h, err = pngSize(filepath.Join(run, out))
	if err != nil {
		return Raster{}, fmt.Errorf("magick wrote no usable PNG: %w", err)
	}
	if err := checkSVGSize(p.caps, w, h); err != nil {
		return Raster{}, err
	}
	if cur, err := f.Stat(); err != nil {
		return Raster{}, fmt.Errorf("source: %w", withoutPath(err))
	} else if idOf(cur) != id || unlinked(cur) {
		return Raster{}, fmt.Errorf("source: %w since it was opened", ErrInputChanged)
	}
	if err := moveFile(filepath.Join(run, out), filepath.Join(dir, out)); err != nil {
		clearDir(dir)
		return Raster{}, err
	}
	return Raster{Name: out, Width: w, Height: h}, nil
}

func checkSVGSize(c caps, w, h int) error {
	if w > maxDimension || h > maxDimension || int64(w)*int64(h) > c.framePixels {
		return tooLarge("drawing is %dx%d pixels, the limits are %d a side and %d pixels", w, h, maxDimension, c.framePixels)
	}
	return nil
}

// sniffSVG refuses a file that is gzip, or that does not open with a tag. It
// is deliberately lenient: librsvg judges the content, and the coder is
// pinned, so content that is not SVG fails there.
func sniffSVG(f *os.File, size int64) error {
	head := make([]byte, min(size, svgSniffBytes))
	if _, err := f.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("source: %w", withoutPath(err))
	}
	if bytes.HasPrefix(head, []byte{0x1f, 0x8b}) {
		return unrecognized("compressed SVG")
	}
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	if rest := strings.TrimLeft(string(head), " \t\r\n"); !strings.HasPrefix(rest, "<") {
		return unrecognized("not an SVG")
	}
	return nil
}
