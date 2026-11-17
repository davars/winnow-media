package media

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hostilePDFs returns PDFs that name the file at target in each way a PDF
// can, with a page that draws something.
func hostilePDFs(target string) map[string][]byte {
	pdfString := "(" + strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(target) + ")"
	traversal := strings.Repeat("..#2F", 24) + strings.ReplaceAll(strings.TrimPrefix(target, "/"), "/", "#2F")
	page := func(d *pdfDoc, resources, content, extra, catalog string) []byte {
		c := d.stream("", []byte(content))
		pages := d.reserve() + 1
		pg := d.add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 200 200] /Contents %d 0 R /Resources << %s >> %s >>", pages, c, resources, extra))
		d.add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
		return d.bytes(d.add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R %s >>", pages, catalog)))
	}
	out := map[string][]byte{}

	d := &pdfDoc{}
	img := d.stream("/Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceRGB /BitsPerComponent 8 /F "+pdfString, nil)
	out["external stream"] = page(d, fmt.Sprintf("/XObject << /Im %d 0 R >>", img), "q 50 0 0 50 10 10 cm /Im Do Q", "", "")

	d = &pdfDoc{}
	ff := d.stream("/Length1 0 /F "+pdfString, nil)
	desc := d.add(fmt.Sprintf("<< /Type /FontDescriptor /FontName /X /Flags 32 /FontBBox [0 0 1000 1000] /ItalicAngle 0 /Ascent 800 /Descent -200 /CapHeight 700 /StemV 80 /FontFile2 %d 0 R >>", ff))
	f := d.add(fmt.Sprintf("<< /Type /Font /Subtype /TrueType /BaseFont /X /FontDescriptor %d 0 R /FirstChar 65 /LastChar 65 /Widths [500] >>", desc))
	out["font file"] = page(d, fmt.Sprintf("/Font << /F1 %d 0 R >>", f), "BT /F1 20 Tf 10 100 Td (A) Tj ET", "", "")

	d = &pdfDoc{}
	cid := d.add("<< /Type /Font /Subtype /CIDFontType0 /BaseFont /X /CIDSystemInfo << /Registry (Adobe) /Ordering (Japan1) /Supplement 0 >> >>")
	f = d.add(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding /%s /DescendantFonts [%d 0 R] >>", traversal, cid))
	out["cmap name traversal"] = page(d, fmt.Sprintf("/Font << /F1 %d 0 R >>", f), "BT /F1 20 Tf 10 100 Td <0041> Tj ET", "", "")

	d = &pdfDoc{}
	annots := "/Annots [ " +
		"<< /Type /Annot /Subtype /Link /Rect [0 0 100 100] /A << /S /Launch /F " + pdfString + " >> >> " +
		"<< /Type /Annot /Subtype /Link /Rect [0 100 100 200] /A << /S /GoToR /F " + pdfString + " /D [0 /Fit] >> >> " +
		"<< /Type /Annot /Subtype /Link /Rect [100 0 200 100] /A << /S /URI /URI (file://" + target + ") >> >> " +
		"<< /Type /Annot /Subtype /Link /Rect [100 100 200 200] /A << /S /URI /URI (http://127.0.0.1:1/x) >> >> ]"
	out["launch, remote go-to and URI actions"] = page(d, "", "0.5 g 0 0 200 200 re f", annots,
		"/OpenAction << /S /Launch /F "+pdfString+" >>")

	d = &pdfDoc{}
	ef := d.stream("/Type /EmbeddedFile", []byte("attached"))
	spec := d.add(fmt.Sprintf("<< /Type /Filespec /F (a.txt) /EF << /F %d 0 R >> >>", ef))
	ext := d.add("<< /Type /Filespec /F " + pdfString + " >>")
	out["attachments"] = page(d, "", "0.5 g 0 0 200 200 re f", "",
		fmt.Sprintf("/Names << /EmbeddedFiles << /Names [(a.txt) %d 0 R (b.txt) %d 0 R] >> >>", spec, ext))

	d = &pdfDoc{}
	out["JavaScript"] = page(d, "", "0.5 g 0 0 200 200 re f", "/AA << /O << /S /JavaScript /JS (app.launchURL('file://"+target+"'); this.importDataObject('x', '"+target+"');) >> >>",
		"/OpenAction << /S /JavaScript /JS (this.getURL('file://"+target+"'))>> /Names << /JavaScript << /Names [(x) << /S /JavaScript /JS (app.alert(1)) >>] >> >>")
	return out
}

// None of the references a PDF can hold to another file is followed, with
// the confinement and without it, which shows that the tool itself leaves
// them alone.
func TestHostilePDFReferencesAreNotFollowed(t *testing.T) {
	requireTools(t, "pdfinfo", "pdftoppm")
	for _, mode := range []struct {
		name string
		cfg  Config
	}{
		{"confined", pdfConfig},
		{"unconfined", Config{RenderTypes: pdfConfig.RenderTypes, Unsandboxed: true}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			dir, check := canaryDir(t, "canary", "sub/canary")
			target := filepath.Join(dir, "canary")
			p := requireProcessorWith(t, mode.cfg)
			for name, data := range hostilePDFs(target) {
				path := writeFile(t, "hostile.pdf", data)
				doc, _, err := p.OpenDocument(context.Background(), path, DocumentOptions{})
				if err != nil {
					t.Errorf("%s: OpenDocument: %v", name, err)
					continue
				}
				// The page is drawn or the tool fails; neither may read the file.
				if _, _, err := doc.Render(context.Background(), t.TempDir(), All(), LongEdge(200)); err != nil {
					var te *ToolError
					if !errors.As(err, &te) {
						t.Errorf("%s: err = %v, want success or a *ToolError", name, err)
					}
				}
				_ = doc.Close()
			}
			check()
		})
	}
}

// The traversal fixture must be able to work for the check to mean anything:
// with the CMap's own name it reads poppler-data, which is in the store.
func TestHostilePDFFixturesAreWellFormed(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	for name, data := range hostilePDFs("/nonexistent/target") {
		doc, _, err := p.OpenDocument(context.Background(), writeFile(t, "hostile.pdf", data), DocumentOptions{})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(doc.Info().Pages) != 1 {
			t.Errorf("%s: %d pages", name, len(doc.Info().Pages))
		}
		_ = doc.Close()
	}
}

func flate(t *testing.T, size int) []byte {
	t.Helper()
	var b bytes.Buffer
	zw, err := zlib.NewWriterLevel(&b, zlib.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(zw, zeroReader{}, int64(size)); err != nil {
		t.Fatal(err)
	}
	_ = zw.Close()
	return b.Bytes()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// A document built to make the renderer work or grow without bound ends within
// the Processor's limits, with a drawing or an error and never a hang.
func TestHostilePDFContentIsBounded(t *testing.T) {
	requireTools(t, "pdfinfo", "pdftoppm")
	bomb := &pdfDoc{}
	c := bomb.stream("/Filter /FlateDecode", flate(t, 256<<20))
	pages := bomb.reserve() + 1
	pg := bomb.add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 100 100] /Contents %d 0 R >>", pages, c))
	bomb.add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
	bombPDF := bomb.bytes(bomb.add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pages)))

	loop := &pdfDoc{}
	form := loop.reserve()
	loop.stream(fmt.Sprintf("/Type /XObject /Subtype /Form /BBox [0 0 100 100] /Resources << /XObject << /Fm %d 0 R >> >>", form),
		[]byte("0.5 g 0 0 50 50 re f /Fm Do /Fm Do"))
	loopPDF := onePage(loop, "[0 0 100 100]", fmt.Sprintf("/XObject << /Fm %d 0 R >>", form), "/Fm Do", "")

	type3 := &pdfDoc{}
	proc := type3.reserve()
	type3.stream("", []byte("1000 0 0 0 750 750 d1 0 0 750 750 re f BT /T3 1 Tf (a) Tj ET"))
	t3 := type3.add(fmt.Sprintf("<< /Type /Font /Subtype /Type3 /FontBBox [0 0 750 750] /FontMatrix [0.001 0 0 0.001 0 0] /CharProcs << /a %d 0 R >> /Encoding << /Type /Encoding /Differences [97 /a] >> /FirstChar 97 /LastChar 97 /Widths [1000] /Resources << /Font << /T3 %d 0 R >> >> >>", proc, type3.reserve()))
	type3PDF := onePage(type3, "[0 0 100 100]", fmt.Sprintf("/Font << /T3 %d 0 R >>", t3), "BT /T3 40 Tf 10 10 Td (aaa) Tj ET", "")

	garbageXref := func() []byte {
		b, err := os.ReadFile("testdata/pdf/base14-two-pages.pdf")
		if err != nil {
			t.Fatal(err)
		}
		i := bytes.LastIndex(b, []byte("startxref"))
		return append(append([]byte{}, b[:i]...), "startxref\n999999\n%%EOF\n"...)
	}
	truncated := func() []byte {
		b, _ := os.ReadFile("testdata/pdf/base14-two-pages.pdf")
		return b[:len(b)*2/3]
	}

	res := Resources{TimeLimit: 5 * time.Second, CPUTime: 10 * time.Second}
	p := requireProcessorWith(t, Config{RenderTypes: pdfConfig.RenderTypes, Resources: res})
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"deflate bomb of 256 MiB", bombPDF},
		{"form that draws itself", loopPDF},
		{"type 3 glyph that draws itself", type3PDF},
		{"xref that points nowhere", garbageXref()},
		{"truncated file", truncated()},
	} {
		start := time.Now()
		doc, _, err := p.OpenDocument(context.Background(), writeFile(t, "hostile.pdf", c.data), DocumentOptions{})
		if err == nil {
			_, _, err = doc.Render(context.Background(), t.TempDir(), All(), LongEdge(100))
			_ = doc.Close()
		}
		var te *ToolError
		if err != nil && !errors.As(err, &te) && !errors.Is(err, ErrTimeLimit) && !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if took := time.Since(start); took > 60*time.Second {
			t.Errorf("%s took %v", c.name, took)
		}
		t.Logf("%s: %v after %v", c.name, err, time.Since(start).Round(time.Millisecond))
	}
}

// An encrypted document that needs a password is a tool failure.
func TestEncryptedPDFThatNeedsAPasswordFails(t *testing.T) {
	p, _ := requirePDFProcessor(t)
	_, _, err := p.OpenDocument(context.Background(), "testdata/pdf/encrypted-user-password.pdf", DocumentOptions{})
	var te *ToolError
	if !errors.As(err, &te) || te.Tool != "pdfinfo" {
		t.Errorf("err = %v, want a pdfinfo *ToolError", err)
	}
}

// A page too slow to draw ends with ErrTimeLimit, and the other limit of a
// run, CPU time, with it too.
func TestRenderTimeLimitIsPerPage(t *testing.T) {
	requireTools(t, "pdfinfo", "pdftoppm")
	p := requireProcessorWith(t, Config{RenderTypes: pdfConfig.RenderTypes, Resources: Resources{TimeLimit: time.Second}})
	doc := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
	inner := p.run
	p.run = func(ctx context.Context, f *os.File, name string, args ...string) (runResult, error) {
		if name == "pdftoppm" {
			<-ctx.Done()
			return runResult{}, fmt.Errorf("pdftoppm: %w", ctx.Err())
		}
		return inner(ctx, f, name, args...)
	}
	dir := t.TempDir()
	_, _, err := doc.Render(context.Background(), dir, All(), LongEdge(100))
	if !errors.Is(err, ErrTimeLimit) {
		t.Errorf("err = %v, want ErrTimeLimit", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("a timed-out call left files")
	}
	// A cancelled context is the caller's, not the limit.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := doc.Render(ctx, t.TempDir(), All(), LongEdge(100)); errors.Is(err, ErrTimeLimit) || !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v, want context.Canceled and not ErrTimeLimit", err)
	}
}
