package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png" // registers the decoder pngSize reads with
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// The fixed limits of a rendered document.
const (
	// maxDocumentPages is the most pages a document may have; the cost of
	// reading that many page boxes is in docs/measurements.md#rendering.
	maxDocumentPages = 100_000
	// pdfSniffBytes is how far into a file the PDF header may start, as
	// poppler allows.
	pdfSniffBytes = 1024
	// minScaleEdge and maxScaleEdge bound the long edge of a drawn page.
	minScaleEdge = 16
	maxDPI       = 2400
)

// Document is a paged document a Processor admitted for rendering. It is safe
// for concurrent use; its Render calls run one at a time. Until Close it holds
// the input file open, and between calls no process.
type Document struct {
	p    *Processor
	file *os.File
	ref  string
	id   fileID
	abs  string
	info DocumentInfo
	// release removes the document's scratch.
	release func()

	// gate admits one Render or Close at a time.
	gate   chan struct{}
	closed bool
}

// DocumentInfo describes a Document.
type DocumentInfo struct {
	// Type is the media type the input was read as.
	Type string
	// Pages describes each page, in order; Render selects them by index from 0.
	Pages []PageInfo
}

// PageInfo is a page as the document declares it.
type PageInfo struct {
	// Width and Height are the size of the page's media box, the area Render
	// draws, in points (1/72 inch), before Rotation.
	Width, Height float64
	// Rotation is the page's display rotation: 0, 90, 180 or 270 degrees.
	// Render draws a page upright, so a page rotated by 90 or 270 is drawn
	// Height wide.
	Rotation int
}

// PageScale says how large Render draws a page. Build one with Resolution or
// LongEdge. The zero value is Resolution(150, 0).
type PageScale struct {
	kind               scaleKind
	dpi, maxEdge, edge int
}

type scaleKind uint8

const (
	scaleDefault scaleKind = iota
	scaleResolution
	scaleLongEdge
)

// Resolution draws pages at dpi dots per inch, 1 to 2400. A page whose
// drawing at dpi would have a side over maxEdge pixels is drawn instead with
// its longer side maxEdge pixels. maxEdge is 16 to 16384; zero means 16384.
func Resolution(dpi, maxEdge int) PageScale {
	return PageScale{kind: scaleResolution, dpi: dpi, maxEdge: maxEdge}
}

// LongEdge draws every page with its longer side n pixels, 16 to 16384,
// scaling it up or down.
func LongEdge(n int) PageScale { return PageScale{kind: scaleLongEdge, edge: n} }

// defaultDPI is the resolution of the zero PageScale.
const defaultDPI = 150

// resolved checks s and returns it with its defaults filled in.
func (s PageScale) resolved() (PageScale, error) {
	switch s.kind {
	case scaleDefault:
		return PageScale{kind: scaleResolution, dpi: defaultDPI, maxEdge: maxDimension}, nil
	case scaleResolution:
		if s.dpi < 1 || s.dpi > maxDPI {
			return PageScale{}, invalid("resolution %d is outside 1..%d dpi", s.dpi, maxDPI)
		}
		if s.maxEdge == 0 {
			s.maxEdge = maxDimension
		}
		if s.maxEdge < minScaleEdge || s.maxEdge > maxDimension {
			return PageScale{}, invalid("maximum edge %d is outside %d..%d", s.maxEdge, minScaleEdge, maxDimension)
		}
	case scaleLongEdge:
		if s.edge < minScaleEdge || s.edge > maxDimension {
			return PageScale{}, invalid("long edge %d is outside %d..%d", s.edge, minScaleEdge, maxDimension)
		}
	}
	return s, nil
}

// drawing is how pdftoppm draws one page: its flags, the size they give, and
// the size to check against the limits, which is larger where pdfinfo's
// rounding of the page's size can make the drawing larger than the estimate.
type drawing struct {
	flags               []string
	width, height       int
	boundWidth, boundHt int
}

// plan returns the flags that draw a page of info at s, which is resolved,
// and the pixel size they give. The size is ceil of the scaled side, as
// pdftoppm rounds it; Render checks the file's own size afterwards.
func (s PageScale) plan(info PageInfo) drawing {
	w, h := info.Width, info.Height
	if info.Rotation == 90 || info.Rotation == 270 {
		w, h = h, w
	}
	long := math.Max(w, h)
	// The pixel size is computed as pdftoppm does, in the same order of
	// operations.
	side := func(v, resolution float64) int {
		px := math.Ceil(v * resolution / 72)
		return int(min(max(px, 1), math.MaxInt32))
	}
	if s.kind == scaleResolution {
		// pdfinfo prints a size to two decimals, so the drawing can be a pixel
		// larger than the estimate: a page is drawn by resolution only when a
		// pixel more still fits the edge.
		dpi := float64(s.dpi)
		if math.Ceil(long*dpi/72)+1 <= float64(s.maxEdge) {
			w, h := side(w, dpi), side(h, dpi)
			return drawing{flags: []string{"-r", strconv.Itoa(s.dpi)}, width: w, height: h, boundWidth: w + 1, boundHt: h + 1}
		}
		s.edge = s.maxEdge
	}
	resolution := 72.0
	if long > 0 {
		resolution = 72 * float64(s.edge) / long
	}
	dw, dh := side(w, resolution), side(h, resolution)
	return drawing{flags: []string{"-scale-to", strconv.Itoa(s.edge)}, width: dw, height: dh, boundWidth: dw, boundHt: dh}
}

// DocumentOptions are what OpenDocument cannot learn from the file. It has no
// fields yet; later versions may add some, and the zero value always means
// what it means now.
type DocumentOptions struct{}

// OpenDocument admits the PDF at path, which may be relative, for Render. It
// reads the page boxes with two pdfinfo runs, each bounded by
// Resources.TimeLimit, and renders nothing. opts is the zero DocumentOptions.
//
// The file must be a regular file of at most Caps.MaxInputBytes whose first
// 1024 bytes contain "%PDF-", and application/pdf must be among
// Config.RenderTypes; otherwise the error wraps ErrInvalidRequest,
// ErrTooLarge or ErrUnsupportedType as Open's would. A document of more than
// 100,000 pages is refused with ErrTooLarge. As with Open, the file is opened
// once and read only through that descriptor, and every Render first checks
// that it is unchanged.
//
// The Usage covers the tool runs of the call, also when it fails. On an
// error no Document is returned; otherwise the caller must Close it or the
// Processor. Errors are Open's: ErrUnsupportedType, ErrTooLarge,
// ErrInvalidRequest, ErrInputChanged, ErrTimeLimit, ErrClosed, ctx's error
// or a *ToolError (an encrypted document whose password is needed is one).
func (p *Processor) OpenDocument(ctx context.Context, path string, opts DocumentOptions) (*Document, Usage, error) {
	if !p.beginOpen() {
		return nil, Usage{}, fmt.Errorf("open document: processor: %w", ErrClosed)
	}
	defer p.opening.Done()
	var m meter
	doc, err := p.openDocument(ctx, &m, path)
	if err != nil {
		return nil, m.usage, fmt.Errorf("open document: %w", err)
	}
	return doc, m.usage, nil
}

func (p *Processor) openDocument(ctx context.Context, m *meter, path string) (_ *Document, err error) {
	if !p.renders[typePDF] {
		return nil, unrecognized(typePDF + " is not among Config.RenderTypes")
	}
	f, abs, fi, err := p.openRegular(path)
	if err != nil {
		return nil, err
	}
	d := &Document{p: p, file: f, abs: abs, id: idOf(fi), gate: make(chan struct{}, 1)}
	defer func() {
		if err != nil {
			if d.release != nil {
				d.release()
			}
			_ = f.Close()
		}
	}()
	head := make([]byte, min(fi.Size(), pdfSniffBytes))
	if _, rerr := f.ReadAt(head, 0); rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil, fmt.Errorf("source: %w", withoutPath(rerr))
	}
	if !bytes.Contains(head, []byte("%PDF-")) {
		return nil, unrecognized("not a PDF")
	}
	dir, release, err := p.inputDir()
	if err != nil {
		return nil, err
	}
	d.release = release
	if d.ref, err = inputRef(dir, abs); err != nil {
		return nil, err
	}
	if d.info, err = d.readInfo(ctx, m, dir); err != nil {
		return nil, err
	}
	if err = d.checkIdentity(); err != nil {
		return nil, err
	}
	if !p.registerDocument(d) {
		return nil, fmt.Errorf("processor: %w", ErrClosed)
	}
	return d, nil
}

// env returns the toolEnv for a run in a render directory dir: its runs work
// in it and, where the Processor confines render runs, are confined to it and
// to the input.
func (d *Document) env(ctx context.Context, m *meter, dir string) (context.Context, toolEnv) {
	return withRenderRun(ctx, renderRun{dir: dir}), d.p.env(m, d.file).pinnedTo(d.file, d.id, "since it was opened")
}

var (
	pagesLine = regexp.MustCompile(`(?m)^Pages:\s+(\d+)\s*$`)
	boxLine   = regexp.MustCompile(`(?m)^Page\s+(\d+)\s+MediaBox:\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s*$`)
	rotLine   = regexp.MustCompile(`(?m)^Page\s+(\d+)\s+rot:\s+(-?\d+)\s*$`)
)

// readInfo reads the page count and then the page boxes with pdfinfo.
func (d *Document) readInfo(ctx context.Context, m *meter, scratch string) (DocumentInfo, error) {
	run, err := os.MkdirTemp(scratch, "info-")
	if err != nil {
		return DocumentInfo{}, err
	}
	defer func() { _ = os.RemoveAll(run) }()
	ctx, env := d.env(ctx, m, run)
	res, err := d.p.timed(ctx, env, "pdfinfo", d.ref)
	if err != nil {
		return DocumentInfo{}, err
	}
	match := pagesLine.FindSubmatch(res.Stdout)
	if match == nil {
		return DocumentInfo{}, unrecognized("pdfinfo reported no page count")
	}
	n, err := strconv.Atoi(string(match[1]))
	if err != nil || n < 1 {
		return DocumentInfo{}, unrecognized("the document has no pages")
	}
	if n > maxDocumentPages {
		return DocumentInfo{}, tooLarge("document has %d pages, the limit is %d", n, maxDocumentPages)
	}
	res, err = d.p.timed(ctx, env, "pdfinfo", "-box", "-f", "1", "-l", strconv.Itoa(n), d.ref)
	if err != nil {
		return DocumentInfo{}, err
	}
	pages := make([]PageInfo, n)
	seen := make([]bool, n)
	for _, b := range boxLine.FindAllSubmatch(res.Stdout, -1) {
		i, _ := strconv.Atoi(string(b[1]))
		if i < 1 || i > n {
			continue
		}
		var v [4]float64
		for k := range v {
			if v[k], err = strconv.ParseFloat(string(b[2+k]), 64); err != nil || math.IsNaN(v[k]) || math.IsInf(v[k], 0) {
				return DocumentInfo{}, unrecognized(fmt.Sprintf("page %d has an unreadable media box", i))
			}
		}
		pages[i-1].Width, pages[i-1].Height = math.Abs(v[2]-v[0]), math.Abs(v[3]-v[1])
		seen[i-1] = true
	}
	for _, r := range rotLine.FindAllSubmatch(res.Stdout, -1) {
		i, _ := strconv.Atoi(string(r[1]))
		rot, _ := strconv.Atoi(string(r[2]))
		if i >= 1 && i <= n {
			turn := (rot%360 + 360) % 360
			pages[i-1].Rotation = turn - turn%90
		}
	}
	for i, ok := range seen {
		if !ok {
			return DocumentInfo{}, unrecognized(fmt.Sprintf("pdfinfo reported no media box for page %d", i+1))
		}
	}
	return DocumentInfo{Type: typePDF, Pages: pages}, nil
}

// Info describes the Document. The result is a copy.
func (d *Document) Info() DocumentInfo {
	info := d.info
	info.Pages = append([]PageInfo(nil), info.Pages...)
	return info
}

// checkIdentity returns an error unless the open input is as it was when the
// Document was opened.
func (d *Document) checkIdentity() error {
	info, err := d.file.Stat()
	if err != nil {
		return fmt.Errorf("source: %w", withoutPath(err))
	}
	if idOf(info) != d.id || unlinked(info) {
		return fmt.Errorf("source: %w since it was opened", ErrInputChanged)
	}
	return nil
}

func (d *Document) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case d.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Document) unlock() { <-d.gate }

// Render draws the pages pages selects into dir as 8-bit RGB PNGs, one per
// page, named 000.png, 001.png and on in ascending page order, and
// describes them in the same order. The PNGs are the renderer's own: no
// profile, metadata other than the resolution, or further processing. Open
// reads them like any PNG.
//
// pages is read as for a still's images: the zero FrameSelection, All, Even
// and Indices apply, and Times and Consecutive are refused. A page whose
// drawing at scale would have a side over 16384 pixels or more than
// Caps.MaxFramePixels pixels is refused with ErrTooLarge before anything is
// drawn. Resources.TimeLimit bounds the wall-clock time spent on each page.
//
// dir must exist and be empty. A call that fails or is cancelled leaves dir
// as it found it, so one page that cannot be drawn fails the call: render
// pages one call at a time to keep the others.
//
// Errors wrap ErrInvalidRequest, ErrInputChanged, ErrClosed, ErrTooLarge,
// ErrTimeLimit or ctx's error, or are a *ToolError.
func (d *Document) Render(ctx context.Context, dir string, pages FrameSelection, scale PageScale) ([]Raster, Usage, error) {
	var m meter
	rasters, err := d.render(ctx, &m, dir, pages, scale)
	if err != nil {
		return nil, m.usage, fmt.Errorf("render: %w", err)
	}
	return rasters, m.usage, nil
}

func (d *Document) render(ctx context.Context, m *meter, dir string, pages FrameSelection, scale PageScale) ([]Raster, error) {
	if err := d.lock(ctx); err != nil {
		return nil, err
	}
	defer d.unlock()
	if d.closed {
		return nil, fmt.Errorf("document: %w", ErrClosed)
	}
	scale, err := scale.resolved()
	if err != nil {
		return nil, err
	}
	selected, err := pages.images(len(d.info.Pages))
	if err != nil {
		return nil, err
	}
	plans := make([]drawing, len(selected))
	for i, page := range selected {
		plans[i] = scale.plan(d.info.Pages[page])
		if w, h := plans[i].boundWidth, plans[i].boundHt; w > maxDimension || h > maxDimension || int64(w)*int64(h) > d.p.caps.framePixels {
			return nil, tooLarge("page %d would be drawn %dx%d pixels, the limits are %d a side and %d pixels", page, w, h, maxDimension, d.p.caps.framePixels)
		}
	}
	if err := requireEmptyDir(dir); err != nil {
		return nil, err
	}
	if err := d.checkIdentity(); err != nil {
		return nil, err
	}
	rasters := make([]Raster, 0, len(selected))
	for i, page := range selected {
		r, err := d.renderPage(ctx, m, dir, page, i, plans[i])
		if err != nil {
			clearDir(dir)
			return nil, err
		}
		rasters = append(rasters, r)
	}
	if err := d.checkIdentity(); err != nil {
		clearDir(dir)
		return nil, err
	}
	return rasters, nil
}

// renderPage draws one page with pdftoppm in a run directory of its own and
// moves the PNG into dir as the n-th file.
func (d *Document) renderPage(ctx context.Context, m *meter, dir string, page, n int, plan drawing) (Raster, error) {
	run, err := d.p.runTempDir()
	if err != nil {
		return Raster{}, err
	}
	defer func() { _ = os.RemoveAll(run) }()
	ctx, env := d.env(ctx, m, run)
	args := append([]string{"-png", "-f", strconv.Itoa(page + 1), "-singlefile"}, plan.flags...)
	args = append(args, d.ref, "p")
	if _, err := d.p.timed(ctx, env, "pdftoppm", args...); err != nil {
		return Raster{}, fmt.Errorf("page %d: %w", page, err)
	}
	out := filepath.Join(run, "p.png")
	w, h, err := pngSize(out)
	if err != nil {
		return Raster{}, fmt.Errorf("pdftoppm wrote no usable PNG for page %d: %w", page, err)
	}
	if w > maxDimension || h > maxDimension || int64(w)*int64(h) > d.p.caps.framePixels {
		return Raster{}, tooLarge("page %d was drawn %dx%d pixels, the limits are %d a side and %d pixels", page, w, h, maxDimension, d.p.caps.framePixels)
	}
	name := numbered(n, ".png")
	if err := moveFile(out, filepath.Join(dir, name)); err != nil {
		return Raster{}, err
	}
	return Raster{Name: name, Page: page, Width: w, Height: h}, nil
}

// timed runs one tool of env within Resources.TimeLimit of wall-clock time. A
// run that the limit ended, rather than the caller's context, wraps
// ErrTimeLimit.
func (p *Processor) timed(ctx context.Context, env toolEnv, name string, args ...string) (runResult, error) {
	limited, cancel := context.WithTimeout(ctx, p.res.TimeLimit)
	defer cancel()
	res, err := env.run(limited, name, args...)
	if err != nil && ctx.Err() == nil && errors.Is(limited.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %w", ErrTimeLimit, err)
	}
	return res, err
}

// pngSize reads the dimensions of the PNG at path from its header.
func pngSize(path string) (w, h int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, withoutPath(err)
	}
	defer func() { _ = f.Close() }()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	if format != "png" {
		return 0, 0, errors.New("not a PNG")
	}
	return cfg.Width, cfg.Height, nil
}

// moveFile moves src to dst, which must not exist, by rename or, across
// filesystems, by copy.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyCandidate(src, dst); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// Close closes the input and removes the Document's scratch. It waits for a
// running Render. Close is safe to call more than once.
func (d *Document) Close() error {
	d.gate <- struct{}{}
	defer d.unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	d.release()
	d.p.forgetDocument(d)
	return d.file.Close()
}
