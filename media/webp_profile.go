package media

import (
	"encoding/binary"
	"io"
	"os"
)

// maxWebPChunks bounds the chunk headers webpICC reads; a still WebP has a
// handful.
const maxWebPChunks = 1024

// VP8X flag bits.
const (
	webpICCFlag  = 0x20
	webpEXIFFlag = 0x08
	webpAnimFlag = 0x02
)

// maxWebPEXIF bounds the EXIF chunk webpOrientation reads.
const maxWebPEXIF = 4096

// orientationNames are ImageMagick's names of the EXIF orientations 1 to 8,
// as %[orientation] prints them.
var orientationNames = [...]string{"", "TopLeft", "TopRight", "BottomRight", "BottomLeft", "LeftTop", "RightTop", "RightBottom", "LeftBottom"}

// webpICC reports from its chunk headers alone whether ImageMagick reads an
// ICC profile from the WebP at path. known is false when the file's layout is
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
func webpICC(path string) (hasICC, known bool) {
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer func() { _ = f.Close() }()

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

// webpOrientation reports from the chunk list and the EXIF chunk alone the
// orientation ImageMagick reads from the WebP at path, as %[orientation]
// prints it ("Undefined" for none), which a header read under -ping does not
// report. known is false when the answer is not certain and the caller must
// decode the file to find out.
//
// An answer is given for a simple file (one VP8 or VP8L chunk), for an
// extended file whose VP8X flags say there is no EXIF chunk, and for an
// extended file that is not an animation, whose chunks end exactly at the
// size the RIFF header states, and that has exactly one EXIF chunk of at most
// maxWebPEXIF bytes with a TIFF header and one well-formed Orientation entry
// in its first directory, or none. ImageMagick does not apply the EXIF
// orientation of an animation, so none is answered for one that has the chunk.
func webpOrientation(path string) (orientation string, known bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	var riff [12]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return "", false
	}
	if string(riff[:4]) != "RIFF" || string(riff[8:]) != "WEBP" {
		return "", false
	}
	end := 8 + int64(binary.LittleEndian.Uint32(riff[4:]))
	pos := int64(12)
	var exif []byte
	exifChunks := 0
	for n := 0; pos != end; n++ {
		if n == maxWebPChunks || pos+8 > end {
			return "", false
		}
		var head [8]byte
		if _, err := io.ReadFull(f, head[:]); err != nil {
			return "", false
		}
		size := int64(binary.LittleEndian.Uint32(head[4:]))
		next := pos + 8 + size + size&1
		if next > end {
			return "", false
		}
		switch kind := string(head[:4]); {
		case n == 0 && (kind == "VP8 " || kind == "VP8L"):
			return "Undefined", next == end
		case n == 0 && kind == "VP8X" && size >= 10:
			var flags [1]byte
			if _, err := io.ReadFull(f, flags[:]); err != nil {
				return "", false
			}
			if flags[0]&webpEXIFFlag == 0 {
				return "Undefined", true
			}
			if flags[0]&webpAnimFlag != 0 {
				return "", false
			}
		case n == 0:
			return "", false
		case kind == "EXIF":
			if exifChunks++; exifChunks > 1 || size > maxWebPEXIF {
				return "", false
			}
			exif = make([]byte, size)
			if _, err := f.ReadAt(exif, pos+8); err != nil {
				return "", false
			}
		}
		if _, err := f.Seek(next, io.SeekStart); err != nil {
			return "", false
		}
		pos = next
	}
	if exifChunks != 1 {
		return "", false
	}
	return tiffOrientation(exif)
}

// tiffOrientation reads the Orientation entry (tag 0x0112, one SHORT) of the
// first directory of a TIFF-structured EXIF block. known is false for
// anything but a well-formed block with at most one such entry holding 1 to 8.
func tiffOrientation(b []byte) (orientation string, known bool) {
	var order binary.ByteOrder
	switch {
	case len(b) < 8:
		return "", false
	case string(b[:4]) == "II*\x00":
		order = binary.LittleEndian
	case string(b[:4]) == "MM\x00*":
		order = binary.BigEndian
	default:
		return "", false
	}
	ifd := int64(order.Uint32(b[4:]))
	if ifd < 8 || ifd+2 > int64(len(b)) {
		return "", false
	}
	entries := int64(order.Uint16(b[ifd:]))
	if ifd+2+12*entries > int64(len(b)) {
		return "", false
	}
	value := 0
	for i := range entries {
		e := b[ifd+2+12*i:]
		if order.Uint16(e) != 0x0112 {
			continue
		}
		if value != 0 || order.Uint16(e[2:]) != 3 || order.Uint32(e[4:]) != 1 {
			return "", false
		}
		if value = int(order.Uint16(e[8:])); value < 1 || value > 8 {
			return "", false
		}
	}
	if value == 0 {
		return "Undefined", true
	}
	return orientationNames[value], true
}
