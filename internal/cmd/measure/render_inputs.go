package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"

	"github.com/davars/winnow-media/media"
)

// renderSpecs are the inputs of the rendering operations: the pages of a
// vector PDF at the first-page and page-by-page sizes, the largest page the
// library draws, an SVG with filters, and a candidate set of the most
// embedded previews one call judges.
func renderSpecs() []inputSpec {
	return []inputSpec{
		{
			name: "pdf-vector-200p-first-page", file: "pdf-vector-200p.pdf", op: "pdf", gen: genVectorPDF,
			do: renderPage(0, media.LongEdge(2048)),
		},
		{
			name: "pdf-vector-200p-page-200dpi", file: "pdf-vector-200p.pdf", op: "pdf", gen: genVectorPDF,
			do: renderPage(100, media.Resolution(200, 0)),
		},
		{
			name: "pdf-image-16384-page", file: "pdf-image-16384.pdf", op: "pdf", gen: genLargeImagePDF,
			do: renderPage(0, media.LongEdge(16384)),
		},
		{name: "svg-filters-4000", file: "svg-filters.svg", op: "svg", gen: genFilterSVG, do: renderSVG},
		{name: "preview-16-candidates", file: "preview-16.bin", op: "preview", gen: genPreviewFile, do: extractPreview},
	}
}

func renderPage(page int, scale media.PageScale) func(context.Context, *media.Processor, string, string) (int, media.Usage, error) {
	return func(ctx context.Context, p *media.Processor, path, out string) (int, media.Usage, error) {
		doc, usage, err := p.OpenDocument(ctx, path, media.DocumentOptions{})
		if err != nil {
			return 0, usage, err
		}
		defer func() { _ = doc.Close() }()
		rasters, used, err := doc.Render(ctx, out, media.Indices(page), scale)
		usage.CPU += used.CPU
		usage.MaxRSS = max(usage.MaxRSS, used.MaxRSS)
		return len(rasters), usage, err
	}
}

func renderSVG(ctx context.Context, p *media.Processor, path, out string) (int, media.Usage, error) {
	_, usage, err := p.RenderSVG(ctx, path, out, media.SVGRender{})
	if err != nil {
		return 0, usage, err
	}
	return 1, usage, nil
}

func extractPreview(ctx context.Context, p *media.Processor, path, out string) (int, media.Usage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, media.Usage{}, err
	}
	const n = 16
	table := data[len(data)-n*16:]
	candidates := make([]media.PreviewCandidate, n)
	for i := range candidates {
		candidates[i] = media.PreviewCandidate{
			Offset: int64(binary.BigEndian.Uint64(table[i*16:])),
			Length: int64(binary.BigEndian.Uint64(table[i*16+8:])),
		}
	}
	pv, usage, err := p.ExtractPreview(ctx, path, out, candidates, media.PreviewOptions{})
	if err != nil {
		return 0, usage, err
	}
	if pv.Candidate < 0 {
		return 0, usage, fmt.Errorf("no candidate qualified")
	}
	return 1, usage, nil
}

// pdfWriter builds a PDF, with the cross-reference table computed.
type pdfWriter struct {
	buf     bytes.Buffer
	offsets []int
}

func newPDFWriter() *pdfWriter {
	w := &pdfWriter{}
	w.buf.WriteString("%PDF-1.7\n")
	return w
}

// object writes object n, whose body is the dictionary dict and, when stream
// is not nil, that stream.
func (w *pdfWriter) object(n int, dict string, stream []byte) {
	for len(w.offsets) < n {
		w.offsets = append(w.offsets, 0)
	}
	w.offsets[n-1] = w.buf.Len()
	fmt.Fprintf(&w.buf, "%d 0 obj\n", n)
	if stream == nil {
		fmt.Fprintf(&w.buf, "%s\nendobj\n", dict)
		return
	}
	fmt.Fprintf(&w.buf, "<< %s /Length %d >>\nstream\n", dict, len(stream))
	w.buf.Write(stream)
	w.buf.WriteString("\nendstream\nendobj\n")
}

func (w *pdfWriter) finish(root int) []byte {
	xref := w.buf.Len()
	fmt.Fprintf(&w.buf, "xref\n0 %d\n0000000000 65535 f \n", len(w.offsets)+1)
	for _, off := range w.offsets {
		fmt.Fprintf(&w.buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&w.buf, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(w.offsets)+1, root, xref)
	return w.buf.Bytes()
}

// genVectorPDF writes 200 pages of letter size, each of 1500 filled and
// stroked paths and a line of text.
func genVectorPDF(_ context.Context, path, _ string) error {
	const pages = 200
	rng := rand.New(rand.NewPCG(1, 2))
	w := newPDFWriter()
	w.object(1, "<< /Type /Catalog /Pages 2 0 R >>", nil)
	var kids []string
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 5+2*i))
	}
	w.object(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages), nil)
	w.object(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", nil)
	w.object(4, "<< >>", nil)
	for i := range pages {
		var c bytes.Buffer
		for range 1500 {
			fmt.Fprintf(&c, "%.2f %.2f %.2f rg %.1f %.1f m %.1f %.1f l %.1f %.1f l h f\n",
				rng.Float64(), rng.Float64(), rng.Float64(),
				rng.Float64()*612, rng.Float64()*792, rng.Float64()*612, rng.Float64()*792, rng.Float64()*612, rng.Float64()*792)
		}
		fmt.Fprintf(&c, "0 g BT /F1 24 Tf 40 40 Td (Page %d) Tj ET\n", i+1)
		w.object(6+2*i, "", c.Bytes())
		w.object(5+2*i, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 6+2*i), nil)
	}
	return os.WriteFile(path, w.finish(1), 0o644)
}

// genLargeImagePDF writes one page of 16384 by 16384 points holding an RGB
// image of that many pixels, compressed as it would be if it were flat.
func genLargeImagePDF(_ context.Context, path, _ string) error {
	const side = 16384
	var img bytes.Buffer
	zw, err := zlib.NewWriterLevel(&img, zlib.BestCompression)
	if err != nil {
		return err
	}
	row := make([]byte, side*3)
	for y := range side {
		for x := 0; x < side; x += 64 {
			row[x*3] = byte(y / 64)
		}
		if _, err := zw.Write(row); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	w := newPDFWriter()
	w.object(1, "<< /Type /Catalog /Pages 2 0 R >>", nil)
	w.object(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", nil)
	w.object(3, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Contents 4 0 R /Resources << /XObject << /Im 5 0 R >> >> >>", side, side), nil)
	w.object(4, "", fmt.Appendf(nil, "q %d 0 0 %d 0 0 cm /Im Do Q", side, side))
	w.object(5, fmt.Sprintf("/Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode", side, side), img.Bytes())
	return os.WriteFile(path, w.finish(1), 0o644)
}

// genFilterSVG writes an SVG of 4000 by 4000 pixels with blurred and
// shadowed shapes.
func genFilterSVG(_ context.Context, path, _ string) error {
	rng := rand.New(rand.NewPCG(3, 4))
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" width="4000" height="4000"><defs>` +
		`<filter id="b"><feGaussianBlur stdDeviation="8"/></filter>` +
		`<filter id="s"><feDropShadow dx="6" dy="6" stdDeviation="5" flood-opacity="0.5"/></filter>` +
		`<linearGradient id="g"><stop offset="0" stop-color="#f60"/><stop offset="1" stop-color="#06f" stop-opacity="0.3"/></linearGradient></defs>`)
	for i := range 400 {
		filter := "b"
		if i%2 == 1 {
			filter = "s"
		}
		fmt.Fprintf(&b, `<circle cx="%d" cy="%d" r="%d" fill="url(#g)" filter="url(#%s)"/>`, rng.IntN(4000), rng.IntN(4000), 30+rng.IntN(200), filter)
	}
	b.WriteString(`<text x="100" y="200" font-size="120" font-family="sans-serif">Filters</text></svg>`)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// genPreviewFile writes 16 JPEGs of growing size, the last about 5 megapixels,
// then a table of their offsets and lengths, 16 pairs of big-endian 64-bit
// integers, at the end of the file.
func genPreviewFile(_ context.Context, path, _ string) error {
	rng := rand.New(rand.NewPCG(5, 6))
	var file bytes.Buffer
	file.Write(make([]byte, 1024))
	var table bytes.Buffer
	for i := range 16 {
		w, h := 640+i*160, 480+i*120
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				img.Set(x, y, color.RGBA{uint8(x/3 + rng.IntN(32)), uint8(y/3 + rng.IntN(32)), uint8((x + y) / 5), 255})
			}
		}
		var jpg bytes.Buffer
		if err := jpeg.Encode(&jpg, img, &jpeg.Options{Quality: 90}); err != nil {
			return err
		}
		_ = binary.Write(&table, binary.BigEndian, [2]uint64{uint64(file.Len()), uint64(jpg.Len())})
		if _, err := io.Copy(&file, &jpg); err != nil {
			return err
		}
	}
	file.Write(table.Bytes())
	return os.WriteFile(filepath.Clean(path), file.Bytes(), 0o644)
}
