package media

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pdfDoc builds a PDF, with the cross-reference table computed.
type pdfDoc struct{ objs [][]byte }

// add appends an object and returns its number.
func (d *pdfDoc) add(body string) int {
	d.objs = append(d.objs, []byte(body))
	return len(d.objs)
}

// stream appends a stream object whose dictionary entries are dict.
func (d *pdfDoc) stream(dict string, data []byte) int {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<< %s /Length %d >>\nstream\n", dict, len(data))
	b.Write(data)
	b.WriteString("\nendstream")
	d.objs = append(d.objs, b.Bytes())
	return len(d.objs)
}

// reserve returns the number the next added object will have.
func (d *pdfDoc) reserve() int { return len(d.objs) + 1 }

func (d *pdfDoc) bytes(root int) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(d.objs))
	for i, o := range d.objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n", i+1)
		b.Write(o)
		b.WriteString("\nendobj\n")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(d.objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(d.objs)+1, root, xref)
	return b.Bytes()
}

// onePage builds a document of one page of the given resources, content and
// extra page entries, and returns its bytes.
func onePage(d *pdfDoc, box, resources, content, extra string) []byte {
	c := d.stream("", []byte(content))
	pages := d.reserve() + 1
	page := d.add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox %s /Contents %d 0 R /Resources << %s >> %s >>", pages, box, c, resources, extra))
	d.add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", page))
	return d.bytes(d.add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pages)))
}

func writePDFFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	return writeFile(t, name, data)
}

// pdfFixture is a PDF the identity tests render.
type pdfFixture struct {
	name string
	path string
}

// pdfFixtures returns the PDFs of the differential tests: stored files and
// generated ones, each exercising something the renderer reads besides plain
// vector drawing.
func pdfFixtures(t *testing.T) []pdfFixture {
	t.Helper()
	stored := func(file string) string {
		abs, err := filepath.Abs(filepath.Join("testdata", "pdf", file))
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	fx := []pdfFixture{
		{"non-embedded base-14 font, one page rotated", stored("base14-two-pages.pdf")},
		{"embedded TrueType subset", stored("embedded-truetype.pdf")},
		{"media box larger than crop box, rotated", stored("crop-and-rotate.pdf")},
	}
	add := func(name string, data []byte) {
		fx = append(fx, pdfFixture{name, writePDFFile(t, "fixture.pdf", data)})
	}

	// A transparency group and soft alpha.
	d := &pdfDoc{}
	form := d.stream("/Type /XObject /Subtype /Form /BBox [0 0 400 400] /Group << /S /Transparency /I true /CS /DeviceRGB >>",
		[]byte("0 1 0 rg 100 100 150 150 re f"))
	add("transparency group", onePage(d, "[0 0 400 400]",
		fmt.Sprintf("/ExtGState << /G1 << /ca 0.5 /CA 0.5 /BM /Multiply >> >> /XObject << /Fm1 %d 0 R >>", form),
		"/G1 gs 1 0 0 rg 50 50 200 200 re f 0 0 1 rg 150 150 200 200 re f /Fm1 Do", ""))

	// CMYK and ICC-based images.
	d = &pdfDoc{}
	cmyk := make([]byte, 8*8*4)
	for i := range cmyk {
		cmyk[i] = byte(i * 37)
	}
	cm := d.stream("/Type /XObject /Subtype /Image /Width 8 /Height 8 /ColorSpace /DeviceCMYK /BitsPerComponent 8", cmyk)
	icc := d.stream("/N 3 /Alternate /DeviceRGB", srgbProfile)
	rgb := make([]byte, 8*8*3)
	for i := range rgb {
		rgb[i] = byte(i * 11)
	}
	ic := d.stream(fmt.Sprintf("/Type /XObject /Subtype /Image /Width 8 /Height 8 /ColorSpace [/ICCBased %d 0 R] /BitsPerComponent 8", icc), rgb)
	add("CMYK and ICC-based images", onePage(d, "[0 0 300 150]",
		fmt.Sprintf("/XObject << /C %d 0 R /I %d 0 R >>", cm, ic),
		"q 140 0 0 140 5 5 cm /C Do Q q 140 0 0 140 155 5 cm /I Do Q", ""))

	// A JPEG 2000 image.
	env := requireMagick(t)
	jp2 := filepath.Join(t.TempDir(), "image.jp2")
	magick(t, env, "-size", "64x48", "gradient:#ff0000-#0000ff", "-depth", "8", jp2)
	jpx, err := os.ReadFile(jp2)
	if err != nil {
		t.Fatal(err)
	}
	d = &pdfDoc{}
	j := d.stream("/Type /XObject /Subtype /Image /Width 64 /Height 48 /Filter /JPXDecode", jpx)
	add("JPEG 2000 image", onePage(d, "[0 0 200 150]", fmt.Sprintf("/XObject << /J %d 0 R >>", j),
		"q 192 0 0 144 4 3 cm /J Do Q", ""))

	// Non-embedded CJK text, which reads poppler-data's CMaps.
	d = &pdfDoc{}
	cid := d.add("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /HeiseiMin-W3 /CIDSystemInfo << /Registry (Adobe) /Ordering (Japan1) /Supplement 2 >> >>")
	f := d.add(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /HeiseiMin-W3 /Encoding /UniJIS-UCS2-H /DescendantFonts [%d 0 R] >>", cid))
	add("non-embedded CID font", onePage(d, "[0 0 300 100]", fmt.Sprintf("/Font << /F1 %d 0 R >>", f),
		"BT /F1 36 Tf 10 40 Td <30423044304630483044> Tj ET", ""))

	// An annotation with an appearance stream.
	d = &pdfDoc{}
	ap := d.stream("/Type /XObject /Subtype /Form /BBox [0 0 100 100]", []byte("1 0 0 rg 0 0 100 100 re f 0 0 0 RG 4 w 2 2 96 96 re S"))
	add("annotation appearance", onePage(d, "[0 0 200 200]", "",
		"0.9 g 0 0 200 200 re f",
		fmt.Sprintf("/Annots [ << /Type /Annot /Subtype /Square /Rect [50 50 150 150] /F 4 /AP << /N %d 0 R >> >> ]", ap)))

	add("three pages sharing a font and an image", sharedResourcePages(t))
	return fx
}

// sharedResourcePages is a document of three pages that share a font and an
// image XObject.
func sharedResourcePages(t *testing.T) []byte {
	t.Helper()
	d := &pdfDoc{}
	var raw bytes.Buffer
	zw := zlib.NewWriter(&raw)
	pix := make([]byte, 32*32*3)
	for i := range pix {
		pix[i] = byte(i*7 + i/96)
	}
	_, _ = zw.Write(pix)
	_ = zw.Close()
	img := d.stream("/Type /XObject /Subtype /Image /Width 32 /Height 32 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode", raw.Bytes())
	font := d.add("<< /Type /Font /Subtype /Type1 /BaseFont /Times-Roman >>")
	pagesNum := 9
	var kids []string
	for i := range 3 {
		c := d.stream("", fmt.Appendf(nil, "BT /F1 24 Tf 20 150 Td (Page %d of three) Tj ET q 100 0 0 100 20 20 cm /Im Do Q", i+1))
		pg := d.add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 250 200] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> /XObject << /Im %d 0 R >> >> >>", pagesNum, c, font, img))
		kids = append(kids, fmt.Sprintf("%d 0 R", pg))
	}
	if got := d.add(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count 3 >>", strings.Join(kids, " "))); got != pagesNum {
		t.Fatalf("pages object is %d, want %d", got, pagesNum)
	}
	return d.bytes(d.add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesNum)))
}
