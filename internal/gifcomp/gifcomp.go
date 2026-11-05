// Package gifcomp composites the frames of a GIF one at a time onto a single
// canvas, the way ImageMagick's -coalesce and browsers do.
package gifcomp

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	"io"
)

// Frames decodes data and calls emit with the composited canvas after each
// frame whose index is in selected, in increasing order of index. The canvas
// passed to emit is reused for the next frame, so emit must not keep it.
// Compositing stops after the last selected frame.
//
// Background disposal clears the frame's rectangle to transparent; frames
// without a graphic control extension are frames like any other.
func Frames(data []byte, selected []int, emit func(index int, canvas *image.NRGBA) error) error {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return Composite(g, selected, emit)
}

// Composite is Frames over an already decoded GIF.
func Composite(g *gif.GIF, selected []int, emit func(index int, canvas *image.NRGBA) error) error {
	want := map[int]bool{}
	last := -1
	for _, i := range selected {
		want[i] = true
		last = max(last, i)
	}
	w, h := g.Config.Width, g.Config.Height
	if (w == 0 || h == 0) && len(g.Image) > 0 {
		b := g.Image[0].Bounds()
		w, h = b.Max.X, b.Max.Y
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
	var saved *image.NRGBA
	var prevRect image.Rectangle
	var prevDisposal byte
	for i, frame := range g.Image {
		if i > last {
			break
		}
		switch prevDisposal {
		case gif.DisposalBackground:
			r := prevRect.Intersect(canvas.Bounds())
			for y := r.Min.Y; y < r.Max.Y; y++ {
				row := canvas.Pix[canvas.PixOffset(r.Min.X, y):][:r.Dx()*4]
				clear(row)
			}
		case gif.DisposalPrevious:
			if saved != nil {
				copy(canvas.Pix, saved.Pix)
			}
		}
		disposal := byte(0)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		if disposal == gif.DisposalPrevious {
			if saved == nil {
				saved = image.NewNRGBA(canvas.Bounds())
			}
			copy(saved.Pix, canvas.Pix)
		}
		drawOver(canvas, frame)
		prevRect, prevDisposal = frame.Bounds(), disposal
		if want[i] {
			if err := emit(i, canvas); err != nil {
				return err
			}
		}
	}
	return nil
}

// drawOver paints the opaque pixels of src onto dst.
func drawOver(dst *image.NRGBA, src *image.Paletted) {
	r := src.Bounds().Intersect(dst.Bounds())
	opaque := make([]bool, len(src.Palette))
	rgba := make([][4]uint8, len(src.Palette))
	for i, c := range src.Palette {
		cr, cg, cb, ca := c.RGBA()
		opaque[i] = ca != 0
		rgba[i] = [4]uint8{uint8(cr >> 8), uint8(cg >> 8), uint8(cb >> 8), 255}
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		in := src.Pix[src.PixOffset(r.Min.X, y):][:r.Dx()]
		out := dst.Pix[dst.PixOffset(r.Min.X, y):][:r.Dx()*4]
		for x, idx := range in {
			if int(idx) < len(opaque) && opaque[idx] {
				copy(out[x*4:], rgba[idx][:])
			}
		}
	}
}

// WritePAM writes canvas as a PAM image of tuple type RGB_ALPHA, which
// ImageMagick reads as the "pam" coder without any compression.
func WritePAM(w io.Writer, canvas *image.NRGBA) error {
	b := canvas.Bounds()
	if _, err := fmt.Fprintf(w, "P7\nWIDTH %d\nHEIGHT %d\nDEPTH 4\nMAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\n", b.Dx(), b.Dy()); err != nil {
		return err
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if _, err := w.Write(canvas.Pix[canvas.PixOffset(b.Min.X, y):][:b.Dx()*4]); err != nil {
			return err
		}
	}
	return nil
}
