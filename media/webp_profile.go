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
	webpAnimFlag = 0x02
)

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

// webpAnimated reports whether the WebP at path has the VP8X animation flag
// set. It reads only the first chunk header and its flags byte. A file that
// is not a WebP with a leading VP8X chunk is not animated, as no decoder
// reads an animation from it.
func webpAnimated(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var head [21]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	return string(head[:4]) == "RIFF" && string(head[8:16]) == "WEBPVP8X" &&
		binary.LittleEndian.Uint32(head[16:20]) >= 10 && head[20]&webpAnimFlag != 0
}
