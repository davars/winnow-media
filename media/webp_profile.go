package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// maxWebPChunks bounds the chunk headers webpICC reads; a still WebP has a
// handful.
const maxWebPChunks = 1024

// VP8X flag bits.
const (
	webpICCFlag  = 0x20
	webpAnimFlag = 0x02
)

// webpICC reports from its chunk headers alone whether ImageMagick reads an
// ICC profile from the WebP in. known is false when the file's layout is
// not one whose answer is certain, and the caller must decode the file to
// find out.
//
// An answer is given for a simple file (a single VP8 or VP8L chunk) and for
// an extended file whose first chunk is VP8X and either has the ICC flag
// clear or has chunks that end exactly at the size the RIFF header states,
// one ICCP chunk at most (with data), and an image the animation flag
// describes: one VP8 or VP8L chunk and no ANMF chunk, or ANMF chunks and no
// bare image chunk. ICC profile means the VP8X ICC flag is set and an ICCP
// chunk exists; in an animation the profile applies to its frames. Only chunk
// headers are read, at most maxWebPChunks of them, never chunk data.
func webpICC(in input) (hasICC, known bool) {
	f, done, err := in.open()
	if err != nil {
		return false, false
	}
	defer done()

	var riff [12]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return false, false
	}
	if string(riff[:4]) != "RIFF" || string(riff[8:]) != "WEBP" {
		return false, false
	}
	end := 8 + int64(binary.LittleEndian.Uint32(riff[4:]))
	pos := int64(12)
	extended, animated := false, false
	iccp, images, frames := 0, 0, 0
	for n := 0; pos != end; n++ {
		if n == maxWebPChunks || pos+8 > end {
			return false, false
		}
		var head [8]byte
		if _, err := io.ReadFull(f, head[:]); err != nil {
			return false, false
		}
		size := int64(binary.LittleEndian.Uint32(head[4:]))
		next := pos + 8 + size + size&1 // chunk data is padded to an even length
		if next > end {
			return false, false
		}
		switch kind := string(head[:4]); {
		case n == 0 && kind == "VP8X" && size >= 10:
			var flags [1]byte
			if _, err := io.ReadFull(f, flags[:]); err != nil {
				return false, false
			}
			if flags[0]&webpICCFlag == 0 {
				return false, true
			}
			extended, animated = true, flags[0]&webpAnimFlag != 0
		case n == 0 && (kind == "VP8 " || kind == "VP8L"):
			images++
		case !extended || kind == "VP8X":
			return false, false
		case kind == "ICCP":
			if iccp++; iccp > 1 || size == 0 {
				return false, false
			}
		case kind == "VP8 " || kind == "VP8L":
			images++
		case kind == "ANMF":
			frames++
		}
		if _, err := f.Seek(next, io.SeekStart); err != nil {
			return false, false
		}
		pos = next
	}
	if extended && (animated && (frames == 0 || images > 0) || !animated && (frames > 0 || images != 1)) {
		return false, false
	}
	return iccp == 1, true
}

// webpAnimated reports whether the WebP in has the VP8X animation flag
// set. It reads only the first chunk header and its flags byte. A file that
// is not a WebP with a leading VP8X chunk is not animated, as no decoder
// reads an animation from it.
func webpAnimated(in input) bool {
	f, done, err := in.open()
	if err != nil {
		return false
	}
	defer done()
	var head [21]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	return string(head[:4]) == "RIFF" && string(head[8:16]) == "WEBPVP8X" &&
		binary.LittleEndian.Uint32(head[16:20]) >= 10 && head[20]&webpAnimFlag != 0
}

// maxEXIFRead bounds how much of the EXIF chunk a decode of a still WebP
// reads back; the first directory of a real one is in its first kilobytes.
const maxEXIFRead = 64 << 10

// parseEXIFOrientation returns the orientation, 1 to 8, in the first
// directory of the EXIF data b, with or without the "Exif\0\0" prefix, and 0
// when the directory has none. ok is false for data it cannot read with
// certainty: a directory or value out of range, an orientation outside 1 to 8
// or not a single SHORT, and any entry before the orientation (or, without
// one, in the directory) that ImageMagick may stop at or follow first: a type
// outside 1 to 12, a count of 2^31 or more, or a pointer to another directory.
func parseEXIFOrientation(b []byte) (orientation int, ok bool) {
	b = bytes.TrimPrefix(b, []byte("Exif\x00\x00"))
	if len(b) < 8 {
		return 0, false
	}
	var order binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, false
	}
	if order.Uint16(b[2:]) != 42 {
		return 0, false
	}
	dir := int64(order.Uint32(b[4:]))
	if dir+2 > int64(len(b)) {
		return 0, false
	}
	count := int64(order.Uint16(b[dir:]))
	if dir+2+12*count > int64(len(b)) {
		return 0, false
	}
	for i := range count {
		entry := b[dir+2+12*i:][:12]
		tag, typ, n := order.Uint16(entry), order.Uint16(entry[2:]), order.Uint32(entry[4:])
		if tag == 0x0112 {
			v := order.Uint16(entry[8:])
			if typ != 3 || n != 1 || v < 1 || v > 8 {
				return 0, false
			}
			return int(v), true
		}
		pointer := tag == 0x8769 || tag == 0x8825 || tag == 0xA005 // Exif, GPS, interoperability
		if pointer || typ < 1 || typ > 12 || n >= 1<<31 {
			return 0, false
		}
	}
	return 0, true
}

// stillMeta is the metadata webp-frames read from a still WebP the way
// ImageMagick does, through libwebp's mux: nothing when the mux refuses the
// file's chunks.
type stillMeta struct {
	// exif is the file's EXIF chunk, up to maxEXIFRead bytes of it; nil when
	// there is none.
	exif []byte
	// xmp is set when the file has an XMP chunk, from which ImageMagick may
	// read an orientation that this package does not.
	xmp bool
}

// orientation returns the orientation, 1 to 8 or 0 for none, that ImageMagick
// applies to the file m was read from. ok is false when that is not certain
// (see parseEXIFOrientation, and an XMP chunk) and the answer must be had
// from ImageMagick's own read of the file.
func (m stillMeta) orientation() (n int, ok bool) {
	switch {
	case m.xmp:
		return 0, false
	case m.exif == nil:
		return 0, true
	}
	return parseEXIFOrientation(m.exif)
}

// Names of the metadata files webp-frames writes beside a decoded still.
const (
	stillEXIFName = ".exif-1.bin"
	stillXMPName  = ".xmp-1.bin"
)

// readStillMeta reads the metadata files webp-frames left in dir, and removes
// them.
func readStillMeta(dir string) (stillMeta, error) {
	var m stillMeta
	exif := filepath.Join(dir, stillEXIFName)
	defer removeFiles([]string{exif, filepath.Join(dir, stillXMPName)})
	f, err := os.Open(exif)
	switch {
	case err == nil:
		defer func() { _ = f.Close() }()
		if m.exif, err = io.ReadAll(io.LimitReader(f, maxEXIFRead)); err != nil {
			return stillMeta{}, err
		}
		if m.exif == nil {
			m.exif = []byte{}
		}
	case !errors.Is(err, os.ErrNotExist):
		return stillMeta{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, stillXMPName)); err == nil {
		m.xmp = true
	}
	return m, nil
}
