package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/davars/winnow-media/tool"
)

// requireWebPBuilders skips the test unless cwebp and webpmux, which build
// the animated WebPs the tests need, and the webp-frames tool are available.
func requireWebPBuilders(t *testing.T) {
	t.Helper()
	for _, name := range []string{"cwebp", "webpmux"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " not available")
		}
	}
	if _, err := tool.Resolve(webpFramesTool); err != nil {
		t.Skip(webpFramesTool + " not available")
	}
}

// webpCase describes an animated WebP built from frames encoded one by one.
type webpCase struct {
	codec   string // "lossy", "lossless" or "mixed", which alternates the two
	alpha   bool
	blend   bool
	dispose int // 0 none, 1 background
	sub     bool
	frames  int
}

func (c webpCase) String() string {
	return fmt.Sprintf("%s alpha=%t blend=%t dispose=%d sub=%t", c.codec, c.alpha, c.blend, c.dispose, c.sub)
}

const (
	webpCanvasW = 40
	webpCanvasH = 30
)

// webpFrameImage is the k-th frame's pixels: smooth colour that changes from
// frame to frame and, with alpha, regions of full opacity, partial opacity
// and none.
func webpFrameImage(k, w, h int, alpha bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			a := uint8(255)
			if alpha {
				switch {
				case x < w/3:
					a = 255
				case x < 2*w/3:
					a = uint8(30 + (y*190/h+k*37)%200)
				default:
					a = 0
					if y%5 == 0 {
						a = 128
					}
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x*5 + k*40), G: uint8(y*7 + k*20), B: uint8((x^y)*9 + k*60), A: a,
			})
		}
	}
	return img
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("%s %q: %v: %s%s", name, args, err, out, stderr.String())
	}
}

// builtWebPs holds each webpCase's file once built: the build is
// deterministic, and costs a cwebp run a frame.
var builtWebPs sync.Map // webpCase -> *builtWebP

type builtWebP struct {
	once sync.Once
	data []byte
}

// buildWebP writes an animated WebP for c in a new directory and returns its
// path. Frames are encoded separately and assembled with webpmux, which sets
// each frame's offset, disposal and blending. Each case is built once per
// test binary; every call gets a copy of its own.
func buildWebP(t *testing.T, c webpCase) string {
	t.Helper()
	if c.frames == 0 {
		c.frames = 5
	}
	v, _ := builtWebPs.LoadOrStore(c, &builtWebP{})
	built := v.(*builtWebP)
	built.once.Do(func() { built.data = assembleWebP(t, c) })
	if built.data == nil {
		t.Fatalf("building %v failed in another test", c)
	}
	return writeFile(t, "anim.webp", built.data)
}

func assembleWebP(t *testing.T, c webpCase) []byte {
	t.Helper()
	dir := t.TempDir()
	args := []string{}
	for k := range c.frames {
		w, h, x, y := webpCanvasW, webpCanvasH, 0, 0
		if c.sub {
			w, h, x, y = 24, 16, (k*6)%16, (k*4)%14
		}
		pngPath := filepath.Join(dir, fmt.Sprintf("f%d.png", k))
		var buf bytes.Buffer
		if err := png.Encode(&buf, webpFrameImage(k, w, h, c.alpha)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pngPath, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		webpPath := filepath.Join(dir, fmt.Sprintf("f%d.webp", k))
		codec := []string{"-lossless", "-exact"}
		if c.codec == "lossy" || (c.codec == "mixed" && k%2 == 0) {
			codec = []string{"-q", "70", "-alpha_q", "100"}
		}
		run(t, "cwebp", append(append([]string{"-quiet"}, codec...), pngPath, "-o", webpPath)...)
		blend := "-b"
		if c.blend {
			blend = "+b"
		}
		args = append(args, "-frame", webpPath, fmt.Sprintf("+100+%d+%d+%d%s", x, y, c.dispose, blend))
	}
	out := filepath.Join(dir, "anim.webp")
	run(t, "webpmux", append(args, "-loop", "0", "-bgcolor", "0,0,0,0", "-o", out)...)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// pamImage is a decoded PAM of tuple type RGB_ALPHA.
type pamImage struct {
	w, h int
	pix  []byte // straight RGBA
}

func readPAM(t *testing.T, path string) pamImage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const end = "ENDHDR\n"
	i := bytes.Index(data, []byte(end))
	if i < 0 {
		t.Fatalf("%s: no PAM header", path)
	}
	var img pamImage
	depth := 4
	for _, line := range strings.Split(string(data[:i]), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "WIDTH":
			img.w, _ = strconv.Atoi(f[1])
		case len(f) == 2 && f[0] == "HEIGHT":
			img.h, _ = strconv.Atoi(f[1])
		case len(f) == 2 && f[0] == "DEPTH":
			depth, _ = strconv.Atoi(f[1])
		}
	}
	raw := data[i+len(end):]
	if (depth != 3 && depth != 4) || len(raw) != img.w*img.h*depth {
		t.Fatalf("%s: %d pixel bytes for %dx%d at depth %d", path, len(raw), img.w, img.h, depth)
	}
	if depth == 4 {
		img.pix = raw
		return img
	}
	for j := 0; j < len(raw); j += 3 {
		img.pix = append(img.pix, raw[j], raw[j+1], raw[j+2], 255)
	}
	return img
}

// runWebPFrames runs the webp-frames tool directly, outside the sandbox, over
// every frame of path and returns the frames' PAMs in order.
func runWebPFrames(t *testing.T, path string, w, h, frames int) []pamImage {
	t.Helper()
	exe, err := tool.Resolve(webpFramesTool)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	idx := make([]string, frames)
	for i := range idx {
		idx[i] = strconv.Itoa(i)
	}
	cmd := exec.Command(exe, path, dir, strconv.Itoa(frames+1), strconv.Itoa(w), strconv.Itoa(h), strconv.Itoa(frames), strings.Join(idx, ","), "60000")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, err := cmd.Output(); err != nil {
		t.Fatalf("webp-frames: %v: %s", err, stderr.String())
	}
	out := make([]pamImage, frames)
	for i := range out {
		out[i] = readPAM(t, filepath.Join(dir, gifSampleName(i)))
	}
	return out
}

// magickCoalesced decodes every frame of path with ImageMagick's -coalesce
// into straight RGBA.
func magickCoalesced(t *testing.T, env Env, path string, w, h, frames int) []pamImage {
	t.Helper()
	res, err := env.Run(context.Background(), "magick", "webp:"+path, "-coalesce", "-alpha", "set", "-depth", "8", "rgba:-")
	if err != nil {
		t.Fatal(err)
	}
	size := w * h * 4
	if len(res.Stdout) != frames*size {
		t.Fatalf("magick wrote %d bytes, want %d frames of %d", len(res.Stdout), frames, size)
	}
	out := make([]pamImage, frames)
	for i := range out {
		out[i] = pamImage{w: w, h: h, pix: res.Stdout[i*size : (i+1)*size]}
	}
	return out
}

// pixelDiff is the largest differences between two RGBA images, by class of
// pixel in the reference.
type pixelDiff struct {
	alpha   int // any pixel
	opaque  int // colour where both are opaque
	partial int // colour where both have 0 < alpha < 255
	// uncovered counts pixels that are transparent black in got and opaque
	// black in want, which is how the two treat canvas no frame has painted
	// when the file does not declare alpha. They count in no other field.
	uncovered int
}

func (d *pixelDiff) merge(o pixelDiff) {
	d.alpha, d.opaque = max(d.alpha, o.alpha), max(d.opaque, o.opaque)
	d.partial, d.uncovered = max(d.partial, o.partial), d.uncovered+o.uncovered
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func diffImages(got, want pamImage) pixelDiff {
	var d pixelDiff
	for i := 0; i < len(got.pix); i += 4 {
		ga, wa := int(got.pix[i+3]), int(want.pix[i+3])
		if ga == 0 && wa == 255 && bytes.Equal(got.pix[i:i+3], []byte{0, 0, 0}) && bytes.Equal(want.pix[i:i+3], []byte{0, 0, 0}) {
			d.uncovered++
			continue
		}
		d.alpha = max(d.alpha, abs(ga-wa))
		colour := 0
		for c := range 3 {
			colour = max(colour, abs(int(got.pix[i+c])-int(want.pix[i+c])))
		}
		switch {
		case ga == 255 && wa == 255:
			d.opaque = max(d.opaque, colour)
		case ga > 0 && wa > 0:
			d.partial = max(d.partial, colour)
		}
	}
	return d
}

// webpHeader returns the canvas size and frame count of an animated WebP
// from its VP8X and ANMF chunks.
func webpHeader(t *testing.T, data []byte) (w, h, frames int) {
	t.Helper()
	w = 1 + int(data[24]) | int(data[25])<<8 | int(data[26])<<16
	h = 1 + int(data[27]) | int(data[28])<<8 | int(data[29])<<16
	for pos := 12; pos+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		if string(data[pos:pos+4]) == "ANMF" {
			frames++
		}
		pos += 8 + size + size&1
	}
	return w, h, frames
}

// exifOrientation returns a minimal little-endian EXIF block whose only tag
// is the orientation.
func exifOrientation(orientation uint16) []byte {
	b := []byte("II*\x00\x08\x00\x00\x00")
	b = binary.LittleEndian.AppendUint16(b, 1) // one entry
	b = binary.LittleEndian.AppendUint16(b, 0x0112)
	b = binary.LittleEndian.AppendUint16(b, 3) // SHORT
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = binary.LittleEndian.AppendUint16(b, orientation)
	b = append(b, 0, 0)
	return append(b, 0, 0, 0, 0) // no next IFD
}

// withWebPChunk returns a copy of the WebP at path with the chunk of the
// given webpmux kind ("icc" or "exif") set to data.
func withWebPChunk(t *testing.T, path, kind string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "chunk")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.webp")
	run(t, "webpmux", "-set", kind, in, path, "-o", out)
	return out
}

// toPAM views img as the straight-RGBA image diffImages compares.
func toPAM(img *image.NRGBA) pamImage {
	return pamImage{w: img.Bounds().Dx(), h: img.Bounds().Dy(), pix: img.Pix}
}
