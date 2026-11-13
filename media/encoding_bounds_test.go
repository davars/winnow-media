package media

import (
	"context"
	"image"
	"image/color"
	"testing"
)

func TestEncodingResolvesZeroBoundsToTheCeilings(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		enc              Encoding
		maxEdge, maxArea int
	}{
		{Encoding{}, 16384, 100_000_000},
		{Encoding{Format: PNG}, 16384, 100_000_000},
		{Encoding{Format: WebP}, 16383, 100_000_000},
		{Encoding{MaxEdge: 4096}, 4096, 100_000_000},
		{Encoding{MaxArea: 1 << 20}, 16384, 1 << 20},
		{Encoding{Format: WebP, MaxEdge: 16384}, 16383, 100_000_000},
		{Encoding{Format: WebP, MaxEdge: 2048}, 2048, 100_000_000},
	} {
		got, err := c.enc.resolve()
		if err != nil {
			t.Fatalf("%+v: %v", c.enc, err)
		}
		if got.maxEdge != c.maxEdge || got.maxArea != c.maxArea {
			t.Errorf("%+v resolved to edge %d, area %d, want %d, %d", c.enc, got.maxEdge, got.maxArea, c.maxEdge, c.maxArea)
		}
	}
	for _, enc := range []Encoding{{MaxEdge: 15}, {MaxEdge: 16385}, {MaxArea: 255}, {MaxArea: 100_000_001}, {MaxEdge: -1}, {MaxArea: -1}} {
		if _, err := enc.resolve(); err == nil {
			t.Errorf("%+v was accepted", enc)
		}
	}
}

func solidPNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 251)
	}
	img.SetGray(0, 0, color.Gray{Y: 7})
	return writePNG(t, img, "input.png")
}

// A zero bound is the largest allowed value: the output has the source's size
// when that is within it, never more, and a bound set on one field does not
// bring back a limit on the other.
func TestZeroBoundsKeepTheSourceSize(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	big := solidPNG(t, 4000, 3000)
	for name, c := range map[string]struct {
		enc  Encoding
		w, h int
	}{
		"zero encoding":         {Encoding{}, 4000, 3000},
		"edge only":             {Encoding{MaxEdge: 4096}, 4000, 3000},
		"edge below the source": {Encoding{MaxEdge: 2000}, 2000, 1500},
		"area only":             {Encoding{MaxArea: 3_000_000}, 2000, 1500},
		"webp":                  {Encoding{Format: WebP}, 4000, 3000},
		"png":                   {Encoding{Format: PNG}, 4000, 3000},
	} {
		t.Run(name, func(t *testing.T) {
			img, _, err := p.encodeFirst(context.Background(), big, c.enc, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if img.Width != c.w || img.Height != c.h {
				t.Errorf("size %dx%d, want %dx%d", img.Width, img.Height, c.w, c.h)
			}
		})
	}
}

// A zero bound never asks an encoder for more than its format holds.
func TestZeroBoundsStayWithinTheFormatsMaximum(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	wide := solidPNG(t, maxDimension, 64)
	for format, want := range map[Format]int{JPEG: 16384, PNG: 16384, WebP: 16383} {
		t.Run(string(format), func(t *testing.T) {
			img, _, err := p.encodeFirst(context.Background(), wide, Encoding{Format: format}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if img.Width != want {
				t.Errorf("width %d, want %d", img.Width, want)
			}
		})
	}
}
