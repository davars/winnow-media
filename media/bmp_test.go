package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBMPNeedsStaging(t *testing.T) {
	junk, err := os.ReadFile(filepath.Join("testdata", "types", "junk-colors.bmp"))
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(junk))
	with := func(f func([]byte)) []byte {
		b := bytes.Clone(junk)
		f(b)
		return b
	}
	for _, c := range []struct {
		name string
		hdr  []byte
		size int64
		want bool
	}{
		{"a color count past the file's size", junk, size, true},
		{"a count equal to the size", with(func(b []byte) { b[46], b[47], b[48], b[49] = byte(size), byte(size>>8), 0, 0 }), size, false},
		{"no count", with(func(b []byte) { clear(b[46:50]) }), size, false},
		{"a palette image", with(func(b []byte) { b[28] = 8 }), size, false},
		{"an old DIB header", with(func(b []byte) { b[14] = 12 }), size, false},
		{"not a BMP", with(func(b []byte) { b[0] = 'X' }), size, false},
		{"a short header", junk[:40], size, false},
	} {
		if got := bmpNeedsStaging(c.hdr, c.size); got != c.want {
			t.Errorf("%s: bmpNeedsStaging = %v, want %v", c.name, got, c.want)
		}
	}
}

// A BMP whose color count is junk decodes as if the count were zero, and the
// output is the same bytes.
func TestOpenBMPWithAJunkColorCount(t *testing.T) {
	p := requireProcessor(t)
	junk := filepath.Join("testdata", "types", "junk-colors.bmp")
	data, err := os.ReadFile(junk)
	if err != nil {
		t.Fatal(err)
	}
	clear(data[46:50])
	fixed := writeFile(t, "fixed.bmp", data)
	enc := Encoding{Format: "png"}
	var want []byte
	for _, c := range []struct{ name, path, typ string }{
		{"zeroed count", fixed, "image/bmp"},
		{"junk count", junk, "image/bmp"},
	} {
		dir := t.TempDir()
		img, _, err := stillAs(context.Background(), p, c.path, c.typ, enc, dir)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if img.Width != 8 || img.Height != 6 {
			t.Errorf("%s: %dx%d, want 8x6", c.name, img.Width, img.Height)
		}
		got, err := os.ReadFile(filepath.Join(dir, img.Name))
		if err != nil {
			t.Fatal(err)
		}
		if want == nil {
			want = got
		} else if !bytes.Equal(got, want) {
			t.Errorf("%s: output differs from that of the file with the count zeroed", c.name)
		}
	}
}

// The copy a Source reads is removed with the Source.
func TestStagedBMPIsRemovedWithTheSource(t *testing.T) {
	p := requireProcessor(t)
	src, _, err := p.Open(context.Background(), filepath.Join("testdata", "types", "junk-colors.bmp"), OpenOptions{Type: "image/bmp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(p.tmpDir()); len(entries) != 0 {
		t.Errorf("%d entries left in the Processor's scratch after Close", len(entries))
	}
}

// bmpV5 is a 4x3, 32-bit BITMAPV5HEADER BMP with the color count clrUsed.
func bmpV5(clrUsed uint32) []byte {
	const w, h = 4, 3
	pixels := make([]byte, 0, w*h*4)
	for i := range w * h {
		pixels = append(pixels, byte(i*20), byte(255-i*10), byte(i*5), 255)
	}
	b := make([]byte, 14+124, 14+124+len(pixels))
	copy(b, "BM")
	binary.LittleEndian.PutUint32(b[2:], uint32(14+124+len(pixels)))
	binary.LittleEndian.PutUint32(b[10:], 14+124)
	binary.LittleEndian.PutUint32(b[14:], 124)
	binary.LittleEndian.PutUint32(b[18:], w)
	binary.LittleEndian.PutUint32(b[22:], h)
	binary.LittleEndian.PutUint16(b[26:], 1)
	binary.LittleEndian.PutUint16(b[28:], 32)
	binary.LittleEndian.PutUint32(b[30:], 3) // BI_BITFIELDS
	binary.LittleEndian.PutUint32(b[34:], uint32(len(pixels)))
	binary.LittleEndian.PutUint32(b[46:], clrUsed)
	binary.LittleEndian.PutUint32(b[54:], 0x00ff0000)
	binary.LittleEndian.PutUint32(b[58:], 0x0000ff00)
	binary.LittleEndian.PutUint32(b[62:], 0x000000ff)
	binary.LittleEndian.PutUint32(b[66:], 0xff000000)
	copy(b[70:], "BGRs")
	return append(b, pixels...)
}

// encodeBMPAs encodes the file at path as a PNG with the type named as typ,
// which may be empty, and returns the bytes.
func encodeBMPAs(t *testing.T, p *Processor, path, typ string) []byte {
	t.Helper()
	dir := t.TempDir()
	img, _, err := stillAs(context.Background(), p, path, typ, Encoding{Format: "png"}, dir)
	if err != nil {
		t.Fatalf("type %q: %v", typ, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, img.Name))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// With no type named, a BMP with a junk color count is staged as well, and
// every BMP that detection reports as BMP3 or BMP2 is typed image/bmp.
func TestOpenUnnamedBMPs(t *testing.T) {
	p := requireProcessor(t)
	junk := filepath.Join("testdata", "types", "junk-colors.bmp")
	data, err := os.ReadFile(junk)
	if err != nil {
		t.Fatal(err)
	}
	clear(data[46:50])
	plain24 := writeFile(t, "plain24.bmp", data)
	v5 := writeFile(t, "v5-junk.bmp", bmpV5(0xABCDEF))
	v5Clean := writeFile(t, "v5.bmp", bmpV5(0))
	for _, c := range []struct{ name, path string }{
		{"24-bit, color count zero", plain24},
		{"24-bit, junk color count", junk},
		{"32-bit V5, junk color count", v5},
		{"32-bit V5, color count zero", v5Clean},
	} {
		t.Run(c.name, func(t *testing.T) {
			named := encodeBMPAs(t, p, c.path, "image/bmp")
			if unnamed := encodeBMPAs(t, p, c.path, ""); !bytes.Equal(unnamed, named) {
				t.Error("the output with no type named differs from the output with image/bmp named")
			}
		})
	}
}

// A BMP changed while it is copied is not the one that was admitted.
func TestStagedBMPRefusesAChangeDuringTheCopy(t *testing.T) {
	p := requireProcessor(t)
	data, err := os.ReadFile(filepath.Join("testdata", "types", "junk-colors.bmp"))
	if err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, "rewritten.bmp", data)
	bmpCopied = func() {
		// The clock's granularity can hide a rewrite from the file's times.
		if err := os.Chtimes(path, time.Now(), time.Now().Add(time.Hour)); err != nil {
			t.Error(err)
		}
	}
	defer func() { bmpCopied = nil }()
	src, _, err := p.Open(context.Background(), path, OpenOptions{Type: "image/bmp"})
	if err == nil {
		_ = src.Close()
	}
	if !errors.Is(err, ErrInputChanged) {
		t.Errorf("Open of a file rewritten during its copy: %v, want ErrInputChanged", err)
	}
}
