package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davars/winnow-media/internal/programs"
	"github.com/davars/winnow-media/tool"
)

var pdfConfig = Config{RenderTypes: []string{"application/pdf"}}

func requirePDFProcessor(t *testing.T) (*Processor, *recorder) {
	t.Helper()
	requireTools(t, "pdfinfo", "pdftoppm")
	return sourceProcessorWith(t, pdfConfig)
}

func openDoc(t *testing.T, p *Processor, path string) *Document {
	t.Helper()
	doc, _, err := p.OpenDocument(context.Background(), path, DocumentOptions{})
	if err != nil {
		t.Fatalf("OpenDocument(%s): %v", filepath.Base(path), err)
	}
	t.Cleanup(func() { _ = doc.Close() })
	return doc
}

// direct runs pdftoppm as a caller of it would: the inherited environment,
// the file by its name, no sandbox. It returns the PNG it wrote for a single
// page.
func direct(t *testing.T, path string, args ...string) []byte {
	t.Helper()
	bin, err := tool.Resolve("pdftoppm")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "page")
	cmd := exec.Command(bin, append(append([]string{"-png"}, args...), "-singlefile", path, out)...)
	if res, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pdftoppm %v: %v\n%s", args, err, res)
	}
	data, err := os.ReadFile(out + ".png")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readRaster(t *testing.T, dir string, r Raster) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, r.Name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// What the library writes is what pdftoppm writes when run directly with the
// same flags: a page drawn by its long edge, and every page at a resolution,
// with a page over the maximum edge drawn by its long edge instead.
func TestRenderPDFMatchesDirectPdftoppm(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	bin, _ := tool.Resolve("pdftoppm")
	t.Logf("pdftoppm resolves to %s", bin)
	for _, fx := range pdfFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			doc := openDoc(t, p, fx.path)
			for _, c := range []struct {
				name   string
				scale  PageScale
				direct []string
			}{
				{"long edge 2048", LongEdge(2048), []string{"-scale-to", "2048"}},
				{"200 dpi", Resolution(200, 0), []string{"-r", "200"}},
				{"72 dpi over a 300 pixel edge", Resolution(72, 300), nil},
				{"zero scale", PageScale{}, []string{"-r", "150"}},
			} {
				dir := t.TempDir()
				rasters, _, err := doc.Render(context.Background(), dir, All(), c.scale)
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				if len(rasters) != len(doc.Info().Pages) {
					t.Fatalf("%s: %d rasters for %d pages", c.name, len(rasters), len(doc.Info().Pages))
				}
				for i, r := range rasters {
					want := c.direct
					if want == nil {
						// The drawing that would have a side over the edge is
						// drawn by its long edge.
						want = []string{"-r", "72"}
						if max(r.Width, r.Height) == 300 {
							want = []string{"-scale-to", "300"}
						}
					}
					ref := direct(t, fx.path, append([]string{"-f", strconv.Itoa(i + 1)}, want...)...)
					got := readRaster(t, dir, r)
					if !bytes.Equal(got, ref) {
						t.Errorf("%s, page %d: library PNG (%d bytes) differs from pdftoppm %v (%d bytes)", c.name, i, len(got), want, len(ref))
					}
					info := doc.Info().Pages[i]
					scale, _ := c.scale.resolved()
					if plan := scale.plan(info); plan.width != r.Width || plan.height != r.Height {
						t.Errorf("%s, page %d: planned %dx%d, drawn %dx%d", c.name, i, plan.width, plan.height, r.Width, r.Height)
					}
					if w, h, err := pngSize(filepath.Join(dir, r.Name)); err != nil || w != r.Width || h != r.Height {
						t.Errorf("%s, page %d: Raster says %dx%d, the file is %dx%d (%v)", c.name, i, r.Width, r.Height, w, h, err)
					}
				}
			}
		})
	}
}

// The rasters are the same bytes, so Open and Encode give the same outputs.
func TestRenderedPDFEncodesLikeDirectPdftoppm(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	fx := pdfFixtures(t)[0]
	doc := openDoc(t, p, fx.path)
	dir := t.TempDir()
	rasters, _, err := doc.Render(context.Background(), dir, Indices(0), LongEdge(2048))
	if err != nil {
		t.Fatal(err)
	}
	ref := writeFile(t, "ref.png", direct(t, fx.path, "-f", "1", "-scale-to", "2048"))
	encodings := []Encoding{{Format: JPEG, MaxEdge: 1024}, {Format: WebP, MaxEdge: 1024}, {Format: PNG, MaxEdge: 512}}
	var outs [2][][]byte
	for k, path := range []string{filepath.Join(dir, rasters[0].Name), ref} {
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
			b, err := os.ReadFile(filepath.Join(out, img.Name))
			if err != nil {
				t.Fatal(err)
			}
			outs[k] = append(outs[k], b)
		}
	}
	if len(outs[0]) != 3 {
		t.Fatalf("%d outputs, want 3", len(outs[0]))
	}
	for i := range outs[0] {
		if !bytes.Equal(outs[0][i], outs[1][i]) {
			t.Errorf("encoding %d differs between the library's PNG and pdftoppm's", i)
		}
	}
}

// Pages sharing resources are drawn alike alone and in a range.
func TestRenderPDFPageAloneMatchesRange(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	var path string
	for _, fx := range pdfFixtures(t) {
		if fx.name == "three pages sharing a font and an image" {
			path = fx.path
		}
	}
	doc := openDoc(t, p, path)
	dir := t.TempDir()
	rasters, _, err := doc.Render(context.Background(), dir, All(), Resolution(200, 0))
	if err != nil || len(rasters) != 3 {
		t.Fatalf("Render: %v (%d rasters)", err, len(rasters))
	}
	bin, _ := tool.Resolve("pdftoppm")
	rangeDir := t.TempDir()
	if out, err := exec.Command(bin, "-r", "200", "-png", "-f", "1", "-l", "3", path, filepath.Join(rangeDir, "r")).CombinedOutput(); err != nil {
		t.Fatalf("pdftoppm range: %v\n%s", err, out)
	}
	for i, r := range rasters {
		want, err := os.ReadFile(filepath.Join(rangeDir, fmt.Sprintf("r-%d.png", i+1)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(readRaster(t, dir, r), want) {
			t.Errorf("page %d drawn alone differs from the same page of a range run", i)
		}
	}
}

func TestRenderPDFIsDeterministic(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	for _, fx := range pdfFixtures(t) {
		doc := openDoc(t, p, fx.path)
		var outs [2][]byte
		for k := range outs {
			dir := t.TempDir()
			rasters, _, err := doc.Render(context.Background(), dir, Indices(0), LongEdge(400))
			if err != nil {
				t.Fatalf("%s: %v", fx.name, err)
			}
			outs[k] = readRaster(t, dir, rasters[0])
		}
		if !bytes.Equal(outs[0], outs[1]) {
			t.Errorf("%s: two renders differ", fx.name)
		}
	}
}

func TestDocumentInfoIsThePageBoxes(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	doc := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
	want := []PageInfo{{Width: 612, Height: 792}, {Width: 300, Height: 300, Rotation: 90}}
	info := doc.Info()
	if info.Type != "application/pdf" || !slices.Equal(info.Pages, want) {
		t.Errorf("Info = %+v, want %v", info, want)
	}
	info.Pages[0].Width = 1
	if doc.Info().Pages[0].Width != 612 {
		t.Error("Info returned the Document's own slice")
	}
	// The media box is what is drawn: MediaBox 400x200 rotated 90, with a
	// smaller CropBox that pdfinfo prints as the page size.
	crop := openDoc(t, p, "testdata/pdf/crop-and-rotate.pdf")
	if got := crop.Info().Pages[0]; got != (PageInfo{Width: 400, Height: 200, Rotation: 90}) {
		t.Errorf("page = %+v, want the 400x200 media box rotated 90", got)
	}
	dir := t.TempDir()
	rasters, _, err := crop.Render(context.Background(), dir, All(), Resolution(72, 0))
	if err != nil {
		t.Fatal(err)
	}
	if rasters[0].Width != 200 || rasters[0].Height != 400 {
		t.Errorf("drawn %dx%d, want 200x400: the media box, upright", rasters[0].Width, rasters[0].Height)
	}
}

func TestPageScalePlans(t *testing.T) {
	letter := PageInfo{Width: 612, Height: 792}
	turned := PageInfo{Width: 200, Height: 100, Rotation: 270}
	for _, c := range []struct {
		name  string
		scale PageScale
		page  PageInfo
		flags string
		w, h  int
	}{
		{"zero", PageScale{}, letter, "[-r 150]", 1275, 1650},
		{"resolution", Resolution(200, 0), letter, "[-r 200]", 1700, 2200},
		{"resolution over the edge", Resolution(200, 1000), letter, "[-scale-to 1000]", 773, 1000},
		{"long edge", LongEdge(2048), letter, "[-scale-to 2048]", 1583, 2048},
		{"long edge up", LongEdge(1000), PageInfo{Width: 10, Height: 5}, "[-scale-to 1000]", 1000, 500},
		{"rotated", Resolution(72, 0), turned, "[-r 72]", 100, 200},
	} {
		s, err := c.scale.resolved()
		if err != nil {
			t.Fatal(err)
		}
		d := s.plan(c.page)
		if got := fmt.Sprint(d.flags); got != c.flags || d.width != c.w || d.height != c.h {
			t.Errorf("%s: %s %dx%d, want %s %dx%d", c.name, got, d.width, d.height, c.flags, c.w, c.h)
		}
	}
	for _, bad := range []PageScale{Resolution(0, 0), Resolution(2401, 0), Resolution(72, 15), Resolution(72, 16385), LongEdge(15), LongEdge(16385), LongEdge(0)} {
		if _, err := bad.resolved(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v: err = %v, want ErrInvalidRequest", bad, err)
		}
	}
}

func TestRenderTypesAreSortedAndNeverTypes(t *testing.T) {
	got := RenderTypes()
	if !slices.IsSorted(got) || !slices.Contains(got, "application/pdf") {
		t.Errorf("RenderTypes = %v", got)
	}
	for _, typ := range got {
		if slices.Contains(Types(), typ) {
			t.Errorf("%s is among Types", typ)
		}
	}
}

func TestNewProcessorRenderTypes(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	ctx := context.Background()
	if _, err := NewProcessor(ctx, Config{RenderTypes: []string{"image/png"}}); err == nil {
		t.Error("a type RenderTypes does not list was accepted")
	}
	// Without rendering, no poppler is needed.
	dir := t.TempDir()
	for _, name := range []string{"magick", "ffmpeg", "ffprobe", webpFramesTool} {
		path, err := tool.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	p, err := NewProcessor(ctx, Config{})
	if err != nil {
		t.Fatalf("no RenderTypes, no poppler: %v", err)
	}
	_ = p.Close()
	if _, err := NewProcessor(ctx, pdfConfig); err == nil {
		t.Error("PDF rendering without pdfinfo or pdftoppm was accepted")
	}
	if _, _, err := p.OpenDocument(ctx, "testdata/pdf/base14-two-pages.pdf", DocumentOptions{}); !errors.Is(err, ErrClosed) {
		t.Errorf("OpenDocument on a closed Processor: %v, want ErrClosed", err)
	}
}

func TestOpenDocumentNeedsThePDFType(t *testing.T) {
	p, rec := sourceProcessor(t)
	_, _, err := p.OpenDocument(context.Background(), "testdata/pdf/base14-two-pages.pdf", DocumentOptions{})
	if !errors.Is(err, ErrUnsupportedType) || rec.count() != 0 {
		t.Errorf("err = %v after %d runs, want ErrUnsupportedType with no tool run", err, rec.count())
	}
}

func TestOpenDocumentAdmission(t *testing.T) {
	p, rec := requirePDFProcessor(t)
	body, err := os.ReadFile("testdata/pdf/base14-two-pages.pdf")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		path string
		want error
	}{
		{"header after 1024 bytes", writeFile(t, "late.pdf", append(bytes.Repeat([]byte{'\n'}, 1100), body...)), ErrUnsupportedType},
		{"not a PDF", writeFile(t, "x.pdf", []byte("plain text")), ErrUnsupportedType},
		{"a directory", t.TempDir(), ErrInvalidRequest},
	} {
		before := rec.count()
		_, _, err := p.OpenDocument(context.Background(), c.path, DocumentOptions{})
		if !errors.Is(err, c.want) || rec.count() != before {
			t.Errorf("%s: err = %v after %d runs, want %v with no tool run", c.name, err, rec.count()-before, c.want)
		}
	}
	// The header may start up to 1024 bytes in.
	ok := writeFile(t, "ok.pdf", append(bytes.Repeat([]byte{'\n'}, 1000), body...))
	if doc, _, err := p.OpenDocument(context.Background(), ok, DocumentOptions{}); err != nil {
		t.Errorf("header at byte 1000: %v", err)
	} else {
		_ = doc.Close()
	}
	small := requireProcessorWith(t, Config{RenderTypes: pdfConfig.RenderTypes, Caps: Caps{MaxInputBytes: 100}})
	if _, _, err := small.OpenDocument(context.Background(), "testdata/pdf/base14-two-pages.pdf", DocumentOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a file over MaxInputBytes: %v, want ErrTooLarge", err)
	}
}

func TestOpenDocumentRefusesTooManyPages(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	// poppler counts no more pages than the file has objects, so the pages
	// are real: one more than the limit.
	const n = 100_001
	d := &pdfDoc{}
	var kids strings.Builder
	for i := range n {
		d.add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 1 1] >>", n+1))
		fmt.Fprintf(&kids, "%d 0 R ", i+1)
	}
	d.add(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), n))
	path := writeFile(t, "many.pdf", d.bytes(d.add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", n+1))))
	_, _, err := p.OpenDocument(context.Background(), path, DocumentOptions{})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge for a document declaring 100001 pages", err)
	}
}

func TestRenderRefusesLargePagesBeforeDrawing(t *testing.T) {
	d := &pdfDoc{}
	path := writeFile(t, "huge.pdf", onePage(d, "[0 0 144000 144000]", "", "0 g 0 0 10 10 re f", ""))
	p, rec := sourceProcessorWith(t, Config{RenderTypes: pdfConfig.RenderTypes, Caps: Caps{MaxFramePixels: 1_000_000}})
	doc := openDoc(t, p, path)
	runs := rec.count()
	for _, s := range []PageScale{Resolution(200, 0), LongEdge(2000), {}} {
		dir := t.TempDir()
		if _, _, err := doc.Render(context.Background(), dir, All(), s); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%+v: err = %v, want ErrTooLarge", s, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Error("a refused call left files")
		}
	}
	if rec.count() != runs {
		t.Errorf("pdftoppm ran %d times for refused pages", rec.count()-runs)
	}
	if _, _, err := doc.Render(context.Background(), t.TempDir(), All(), LongEdge(1000)); err != nil {
		t.Errorf("a page within the cap: %v", err)
	}
}

func TestRenderRequestErrors(t *testing.T) {
	p, rec := requirePDFProcessor(t)
	doc := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
	runs := rec.count()
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		dir   string
		pages FrameSelection
		scale PageScale
	}{
		"times":         {t.TempDir(), Times(0), LongEdge(100)},
		"consecutive":   {t.TempDir(), Consecutive(0, 2), LongEdge(100)},
		"page too high": {t.TempDir(), Indices(2), LongEdge(100)},
		"negative page": {t.TempDir(), Indices(-1), LongEdge(100)},
		"bad scale":     {t.TempDir(), All(), LongEdge(1)},
		"dir not empty": {nonEmpty, All(), LongEdge(100)},
		"dir missing":   {filepath.Join(t.TempDir(), "absent"), All(), LongEdge(100)},
	} {
		if _, _, err := doc.Render(context.Background(), c.dir, c.pages, c.scale); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if rec.count() != runs {
		t.Errorf("%d tool runs for refused requests", rec.count()-runs)
	}
	if rasters, _, err := doc.Render(context.Background(), t.TempDir(), Even(1), LongEdge(100)); err != nil || len(rasters) != 1 {
		t.Errorf("Even(1): %v %v", rasters, err)
	}
}

func TestRenderAfterCloseIsErrClosed(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	doc := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
	if err := doc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := doc.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, _, err := doc.Render(context.Background(), t.TempDir(), All(), LongEdge(100)); !errors.Is(err, ErrClosed) {
		t.Errorf("Render after Close: %v, want ErrClosed", err)
	}
	second := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
	_ = p.Close()
	if _, _, err := second.Render(context.Background(), t.TempDir(), All(), LongEdge(100)); !errors.Is(err, ErrClosed) {
		t.Errorf("Render after Processor.Close: %v, want ErrClosed", err)
	}
}

func TestRenderLeavesDirEmptyWhenCancelledOrFailed(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	doc := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
	ctx, cancel := context.WithCancel(context.Background())
	inner := p.run
	p.run = func(ctx context.Context, f *os.File, name string, args ...string) (runResult, error) {
		if name == "pdftoppm" {
			cancel()
		}
		return inner(ctx, f, name, args...)
	}
	dir := t.TempDir()
	if _, _, err := doc.Render(ctx, dir, All(), LongEdge(300)); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a cancelled call left %d files", len(entries))
	}
	if entries, _ := os.ReadDir(p.tmpDir()); len(entries) != 1 {
		// The Document's own scratch is the one entry.
		t.Errorf("%d entries in the Processor's temporary directory after the call, want the Document's", len(entries))
	}

	// A page that fails after another succeeded takes the first away too.
	n := 0
	p.run = func(ctx context.Context, f *os.File, name string, args ...string) (runResult, error) {
		res, err := inner(ctx, f, name, args...)
		if name == "pdftoppm" {
			if n++; n == 2 {
				err = &ToolError{Tool: name, ExitCode: 1}
			}
		}
		return res, err
	}
	dir = t.TempDir()
	var te *ToolError
	if _, _, err := doc.Render(context.Background(), dir, All(), LongEdge(300)); !errors.As(err, &te) {
		t.Errorf("err = %v, want a *ToolError", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a failed call left %d files", len(entries))
	}
}

// usageRecorder wraps p's tool runs and sums what each reports, as a call's
// Usage must: CPU summed, peak RSS the largest.
func usageRecorder(p *Processor) *Usage {
	var mu sync.Mutex
	sum := &Usage{}
	inner := p.run
	p.run = func(ctx context.Context, f *os.File, name string, args ...string) (runResult, error) {
		res, err := inner(ctx, f, name, args...)
		if res.Usage != nil {
			mu.Lock()
			sum.CPU += res.Usage.CPU
			sum.MaxRSS = max(sum.MaxRSS, res.Usage.MaxRSS)
			mu.Unlock()
		}
		return res, err
	}
	return sum
}

func TestDocumentUsageIsTheSumOfItsRuns(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	sum := usageRecorder(p)
	doc, u, err := p.OpenDocument(context.Background(), "testdata/pdf/base14-two-pages.pdf", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = doc.Close() }()
	if u.CPU <= 0 || u.MaxRSS <= 0 || u != *sum {
		t.Errorf("OpenDocument usage = %+v, runs summed to %+v", u, *sum)
	}
	*sum = Usage{}
	_, u, err = doc.Render(context.Background(), t.TempDir(), All(), LongEdge(600))
	if err != nil || u.CPU <= 0 || u.MaxRSS <= 0 || u != *sum {
		t.Errorf("Render usage = %+v, runs summed to %+v (%v)", u, *sum, err)
	}
}

// A run that starts and fails still reports what it used: pdftoppm is made to
// fail on a document that opened and then lost its content.
func TestRenderUsageOnAToolFailure(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	d := &pdfDoc{}
	path := writeFile(t, "doc.pdf", onePage(d, "[0 0 100 100]", "", "0 g 0 0 10 10 re f", ""))
	doc := openDoc(t, p, path)
	sum := usageRecorder(p)
	inner := p.run
	p.run = func(ctx context.Context, f *os.File, name string, args ...string) (runResult, error) {
		if name == "pdftoppm" {
			// A page number past the document's: pdftoppm runs and exits 1.
			for i, a := range args {
				if a == "-f" {
					args = slices.Clone(args)
					args[i+1] = "99"
				}
			}
		}
		return inner(ctx, f, name, args...)
	}
	_, u, err := doc.Render(context.Background(), t.TempDir(), All(), LongEdge(100))
	var te *ToolError
	if !errors.As(err, &te) || te.Tool != "pdftoppm" {
		t.Fatalf("err = %v, want a pdftoppm *ToolError", err)
	}
	if u.CPU <= 0 || u.MaxRSS <= 0 || u != *sum {
		t.Errorf("usage = %+v after the failure, runs summed to %+v", u, *sum)
	}
}

func TestRenderOfARewrittenFileIsErrInputChanged(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	d := &pdfDoc{}
	path := writeFile(t, "broken.pdf", onePage(d, "[0 0 100 100]", "", "0 g 0 0 10 10 re f", ""))
	doc := openDoc(t, p, path)
	if err := os.WriteFile(path, []byte("%PDF-1.4\ngarbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := doc.Render(context.Background(), t.TempDir(), All(), LongEdge(100)); !errors.Is(err, ErrInputChanged) {
		t.Errorf("err = %v, want ErrInputChanged", err)
	}
}

// pdfinfo is bounded by Resources.TimeLimit as each page is.
func TestOpenDocumentTimeLimit(t *testing.T) {
	requireTools(t, "pdfinfo", "pdftoppm")
	p := requireProcessorWith(t, Config{RenderTypes: pdfConfig.RenderTypes, Resources: Resources{TimeLimit: time.Second}})
	inner := p.run
	p.run = func(ctx context.Context, f *os.File, name string, args ...string) (runResult, error) {
		if name == "pdfinfo" {
			<-ctx.Done()
			return runResult{}, fmt.Errorf("pdfinfo: %w", ctx.Err())
		}
		return inner(ctx, f, name, args...)
	}
	start := time.Now()
	_, _, err := p.OpenDocument(context.Background(), "testdata/pdf/base14-two-pages.pdf", DocumentOptions{})
	if !errors.Is(err, ErrTimeLimit) {
		t.Errorf("err = %v, want ErrTimeLimit", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %v for a 1 s limit", took)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := p.OpenDocument(ctx, "testdata/pdf/base14-two-pages.pdf", DocumentOptions{}); errors.Is(err, ErrTimeLimit) || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v, want context.Canceled and not ErrTimeLimit", err)
	}
}

// pdfinfo prints a page's size to two decimals, so a resolution that fits the
// edge only by the estimate draws by the long edge instead, and the drawing
// never exceeds it.
func TestResolutionNeverExceedsTheMaximumEdge(t *testing.T) {
	flags := func(width float64, maxEdge int) string {
		s, err := Resolution(72, maxEdge).resolved()
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(s.plan(PageInfo{Width: width, Height: 10}).flags)
	}
	for _, c := range []struct {
		width   float64
		maxEdge int
		want    string
	}{
		{1000, 2000, "[-r 72]"},
		{1998, 2000, "[-r 72]"},
		{1999, 2000, "[-r 72]"},
		{2000, 2000, "[-scale-to 2000]"},
		{16384, 0, "[-scale-to 16384]"},
		{16383, 0, "[-r 72]"},
	} {
		if got := flags(c.width, c.maxEdge); got != c.want {
			t.Errorf("width %v, edge %d: %s, want %s", c.width, c.maxEdge, got, c.want)
		}
	}

	// The page is 16384.004 points wide and pdfinfo prints 16384.00: drawn at
	// 72 dpi it would be 16385 pixels.
	p, _ := requirePDFProcessor(t)
	d := &pdfDoc{}
	path := writeFile(t, "wide.pdf", onePage(d, "[0 0 16384.004 10]", "", "0 g 0 0 100 5 re f", ""))
	doc := openDoc(t, p, path)
	dir := t.TempDir()
	rasters, _, err := doc.Render(context.Background(), dir, All(), Resolution(72, 0))
	if err != nil || len(rasters) != 1 || rasters[0].Width != 16384 {
		t.Errorf("rasters = %+v, err = %v, want one 16384 pixels wide", rasters, err)
	}
	// Near the pixel cap the refusal is before drawing: the bound adds a pixel
	// to a drawing by resolution.
	small := requireProcessorWith(t, Config{RenderTypes: pdfConfig.RenderTypes, Caps: Caps{MaxFramePixels: 2000 * 100}})
	d = &pdfDoc{}
	box := openDoc(t, small, writeFile(t, "box.pdf", onePage(d, "[0 0 2000 100]", "", "0 g 0 0 10 10 re f", "")))
	if _, _, err := box.Render(context.Background(), t.TempDir(), All(), Resolution(72, 0)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("2000x100 pixels against a cap of exactly that: %v, want ErrTooLarge from the margin", err)
	}
}

// NewProcessor refuses to render on a host where the kernel reports Landlock
// but the confinement does not hold, whether or not it is required.
func TestNewProcessorRefusesRenderingWithBrokenConfinement(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	probeFault = "confine"
	defer func() { probeFault = "" }()
	for _, require := range []bool{false, true} {
		cfg := pdfConfig
		cfg.RequireConfinement = require
		p, err := NewProcessor(context.Background(), cfg)
		if err == nil {
			_ = p.Close()
			t.Errorf("RequireConfinement %v: NewProcessor rendered PDF though the confinement probe failed", require)
		} else if !strings.Contains(err.Error(), "does not enforce it") {
			t.Errorf("RequireConfinement %v: err = %v, want one saying the confinement is not enforced", require, err)
		}
	}
	p, err := NewProcessor(context.Background(), Config{})
	if err != nil {
		t.Errorf("without RenderTypes the confinement is not needed: %v", err)
	} else {
		_ = p.Close()
	}
}

func TestDocumentArgv(t *testing.T) {
	t.Run("document-open", func(t *testing.T) {
		p, rec, _ := goldenProcessor(t, "document-open", pdfConfig)
		doc := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
		_ = doc
		checkGolden(t, "document-open", goldenArgv(rec.calls, "/nonexistent-out"))
	})
	cases := []struct {
		name  string
		file  string
		scale PageScale
	}{
		{"render-pdf-longedge", "base14-two-pages.pdf", LongEdge(2048)},
		{"render-pdf-resolution", "base14-two-pages.pdf", Resolution(200, 0)},
		{"render-pdf-oversize", "base14-two-pages.pdf", Resolution(200, 1000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, rec, g := goldenProcessor(t, c.name, pdfConfig)
			doc := openDoc(t, p, filepath.Join("testdata", "pdf", c.file))
			opened := rec.count()
			g.out = t.TempDir()
			if _, _, err := doc.Render(context.Background(), g.out, Indices(0), c.scale); err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.name, goldenArgv(rec.calls[opened:], "/nonexistent-out"))
		})
	}
}

// Every pdfinfo and pdftoppm run is confined to the input and a directory of
// its own, and no other run is.
func TestRenderRunsAreConfinedAndOthersAreNot(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	var mu sync.Mutex
	confined := map[string][]*confinement{}
	inner := p.start
	p.start = func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
		mu.Lock()
		confined[filepath.Base(spec.Tool)] = append(confined[filepath.Base(spec.Tool)], spec.Confine)
		mu.Unlock()
		return inner(ctx, spec, stdout, stderr)
	}
	doc := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
	dir := t.TempDir()
	rasters, _, err := doc.Render(context.Background(), dir, Indices(0), LongEdge(200))
	if err != nil {
		t.Fatal(err)
	}
	src, _, err := p.Open(context.Background(), filepath.Join(dir, rasters[0].Name), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
	for _, name := range []string{"pdfinfo", "pdftoppm"} {
		runs := confined[name]
		if len(runs) == 0 {
			t.Fatalf("no %s run was seen", name)
		}
		for _, c := range runs {
			if c == nil || !c.Input || len(c.Write) == 0 || !strings.HasPrefix(c.Write[0], p.tmpDir()) {
				t.Errorf("%s ran with confinement %+v, want the input and a directory under the Processor's", name, c)
			}
			for _, w := range c.Write {
				if w == p.dir || w == p.tmpDir() {
					t.Errorf("%s may write in %s, which holds other runs' files", name, w)
				}
			}
		}
	}
	if len(confined["magick"]) == 0 {
		t.Fatal("no magick run was seen")
	}
	for _, c := range confined["magick"] {
		if c != nil {
			t.Errorf("a magick run of Open was confined: %+v", c)
		}
	}
}

// The programs a Processor resolves are the ones package programs lists, which
// the flake's closure check reads: a program the library runs and the list
// lacks would be missing from the check.
func TestProcessorRunsOnlyListedPrograms(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	p := requireProcessorWith(t, pdfConfig)
	var ran []string
	for name := range p.tools {
		ran = append(ran, name)
	}
	slices.Sort(ran)
	want := programs.Names()
	slices.Sort(want)
	if !slices.Equal(ran, want) {
		t.Errorf("a Processor that renders resolves %v, package programs lists %v", ran, want)
	}
}
