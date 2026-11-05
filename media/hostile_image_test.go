package media

import (
	"bytes"
	"compress/lzw"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// oneColourGIF returns a GIF of n frames, each w x h pixels of a single colour
// filling the canvas, compressed by LZW to a small fraction of the pixel count.
func oneColourGIF(t *testing.T, w, h, n int) []byte {
	t.Helper()
	var packed bytes.Buffer
	zw := lzw.NewWriter(&packed, lzw.LSB, 8)
	row := make([]byte, w)
	for range h {
		if _, err := zw.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	frame := []byte{0x2C, 0, 0, 0, 0}
	frame = binary.LittleEndian.AppendUint16(frame, uint16(w))
	frame = binary.LittleEndian.AppendUint16(frame, uint16(h))
	frame = append(frame, 0, 8)
	for data := packed.Bytes(); len(data) > 0; {
		k := min(len(data), 255)
		frame = append(frame, byte(k))
		frame = append(frame, data[:k]...)
		data = data[k:]
	}
	frame = append(frame, 0)

	b := []byte("GIF89a")
	b = binary.LittleEndian.AppendUint16(b, uint16(w))
	b = binary.LittleEndian.AppendUint16(b, uint16(h))
	b = append(b, 0x80, 0, 0, 0, 0, 0, 255, 255, 255)
	for range n {
		b = append(b, frame...)
	}
	return append(b, 0x3B)
}

// losslessWebP returns a lossless WebP whose header declares w x h and whose
// pixel data is one colour coded in zero bits per pixel.
func losslessWebP(w, h int) []byte {
	// VP8L: signature, then 14 bits each of width-1 and height-1, one
	// alpha bit and three version bits.
	head := uint32(w-1) | uint32(h-1)<<14
	stream := []byte{0x2F}
	stream = binary.LittleEndian.AppendUint32(stream, head)
	// No transform, no colour cache, no meta prefix codes, then five prefix
	// codes that are each "simple, one symbol, 1-bit symbol 0".
	var bits uint32
	var n uint
	put := func(v uint32, width uint) { bits |= v << n; n += width }
	put(0, 3)
	for range 5 {
		put(1, 1)
		put(0, 3)
	}
	for ; n > 0; n = n - min(n, 8) {
		stream = append(stream, byte(bits))
		bits >>= 8
	}
	if len(stream)%2 == 1 {
		stream = append(stream, 0)
	}
	chunk := []byte("VP8L")
	chunk = binary.LittleEndian.AppendUint32(chunk, uint32(len(stream)))
	chunk = append(chunk, stream...)
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(4+len(chunk)))
	out = append(out, "WEBP"...)
	return append(out, chunk...)
}

// setHEICExtents rewrites the width and height of every ispe box in a
// HEIC file, which is where the container states each image's size.
func setHEICExtents(t *testing.T, heic []byte, w, h uint32) int {
	t.Helper()
	var rewritten int
	var walk func(b []byte)
	walk = func(b []byte) {
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b))
			if size < 8 || size > len(b) {
				t.Fatalf("unsupported box size %d", size)
			}
			body := b[8:size]
			switch string(b[4:8]) {
			case "meta":
				walk(body[4:])
			case "iprp", "ipco":
				walk(body)
			case "ispe":
				binary.BigEndian.PutUint32(body[4:], w)
				binary.BigEndian.PutUint32(body[8:], h)
				rewritten++
			}
			b = b[size:]
		}
	}
	walk(heic)
	return rewritten
}

// refusalBounds are the wall time and peak memory a refusal must stay under,
// set at about ten times what a 12-CPU x86_64 host measured.
type refusalBounds struct {
	wall   time.Duration
	maxRSS int64
}

// requireRefusal calls op with a fresh recording of tool runs and requires
// ErrTooLarge after exactly one tool run, inside bounds.
func requireRefusal(t *testing.T, p *Processor, bounds refusalBounds, op func(path, dir string) (Usage, error), data []byte) {
	t.Helper()
	rec := record(p)
	defer func() { p.run = rec.inner }()
	forbidCompositor(t, p)
	out := t.TempDir()
	start := time.Now()
	usage, err := op(writeFile(t, "input", data), out)
	if wall := time.Since(start); wall > bounds.wall {
		t.Errorf("refusal took %v, the bound is %v", wall, bounds.wall)
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if rec.count() != 1 || !slices.Contains(rec.calls[0], "-ping") {
		t.Errorf("runs before refusal: %q, want the one header read", rec.calls)
	}
	if usage.MaxRSS > bounds.maxRSS {
		t.Errorf("peak RSS %d bytes, the bound is %d", usage.MaxRSS, bounds.maxRSS)
	}
	if got := dirNames(t, out); len(got) != 0 {
		t.Errorf("output dir holds %v", got)
	}
}

func animationOp(p *Processor) func(path, dir string) (Usage, error) {
	return func(path, dir string) (Usage, error) {
		_, u, err := p.AnimationFrames(context.Background(), path, Encoding{}, 0, dir)
		return u, err
	}
}

func stillOp(p *Processor) func(path, dir string) (Usage, error) {
	return func(path, dir string) (Usage, error) {
		_, u, err := p.Still(context.Background(), path, Encoding{}, dir)
		return u, err
	}
}

func TestGIFFrameAtTheCapDecodesWithinTheBounds(t *testing.T) {
	p := requireProcessor(t)
	const side = 8000
	if side*side != maxFramePixels {
		t.Fatalf("fixture is %d pixels, the cap is %d", side*side, int64(maxFramePixels))
	}
	data := oneColourGIF(t, side, side, 1)
	if len(data) > 1<<20 {
		t.Fatalf("bomb is %d bytes, want a small file", len(data))
	}
	start := time.Now()
	images, usage, err := p.AnimationFrames(context.Background(), writeFile(t, "bomb.gif", data), Encoding{}, 0, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 {
		t.Errorf("%d images, want 1", len(images))
	}
	if wall := time.Since(start); wall > 2*time.Minute {
		t.Errorf("took %v, the bound is 2m", wall)
	}
	if usage.MaxRSS > 4<<30 {
		t.Errorf("peak RSS %d bytes, the bound is 4 GiB", usage.MaxRSS)
	}
}

func TestAnimationAtThePixelCapDecodesWithinTheBounds(t *testing.T) {
	p := requireProcessor(t)
	const frames, side = 1000, 400
	if frames*side*side != maxAnimationPixels || frames > maxAnimationFrames {
		t.Fatalf("fixture is %d frames of %d pixels, the caps are %d and %d", frames, side*side, maxAnimationFrames, int64(maxAnimationPixels))
	}
	start := time.Now()
	images, usage, err := p.AnimationFrames(context.Background(), writeFile(t, "full.gif", oneColourGIF(t, side, side, frames)), Encoding{}, 0, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(images) == 0 {
		t.Error("no images")
	}
	if wall := time.Since(start); wall > 2*time.Minute {
		t.Errorf("took %v, the bound is 2m", wall)
	}
	if usage.MaxRSS > 4<<30 {
		t.Errorf("peak RSS %d bytes, the bound is 4 GiB", usage.MaxRSS)
	}
}

func TestGIFFrameOverTheCapIsRefusedAfterTheHeaderRead(t *testing.T) {
	p := requireProcessor(t)
	bounds := refusalBounds{wall: 2 * time.Second, maxRSS: 256 << 20}
	for name, side := range map[string][2]int{
		"one pixel row over the cap": {8000, 8001},
		"100 megapixels":             {10000, 10000},
	} {
		t.Run(name, func(t *testing.T) {
			requireRefusal(t, p, bounds, animationOp(p), oneColourGIF(t, side[0], side[1], 1))
		})
	}
}

func TestWebPOfMaximumDimensionsIsRefusedAfterTheHeaderRead(t *testing.T) {
	p := requireProcessor(t)
	requireRefusal(t, p, refusalBounds{wall: 2 * time.Second, maxRSS: 256 << 20}, animationOp(p), losslessWebP(16383, 16383))
}

func TestLosslessWebPFixtureIsOneColour(t *testing.T) {
	env := requireMagick(t)
	path := writeFile(t, "small.webp", losslessWebP(64, 48))
	out := string(magick(t, env, "webp:"+path, "-format", "%w %h %k", "info:"))
	if out != "64 48 1" {
		t.Errorf("identify = %q, want %q", out, "64 48 1")
	}
}

func TestHEICDeclaringHugeDimensionsIsRefusedAfterTheHeaderRead(t *testing.T) {
	p := requireProcessor(t)
	original, err := os.ReadFile(filepath.Join("testdata", "heic", "gradient-righttop.heic"))
	if err != nil {
		t.Fatal(err)
	}
	bounds := refusalBounds{wall: 2 * time.Second, maxRSS: 256 << 20}
	for name, side := range map[string]uint32{
		"over the dimension cap": 20000,
		"over the pixel cap":     16000,
	} {
		t.Run(name, func(t *testing.T) {
			data := slices.Clone(original)
			if n := setHEICExtents(t, data, side, side); n == 0 {
				t.Fatal("no ispe box in the fixture")
			}
			requireRefusal(t, p, bounds, stillOp(p), data)
		})
	}
}

func TestManyFrameGIFIsRefusedAfterTheHeaderRead(t *testing.T) {
	p := requireProcessor(t)
	requireRefusal(t, p, refusalBounds{wall: time.Second, maxRSS: 512 << 20}, animationOp(p), manyFrameGIF(1, 1, 10_000))
}
