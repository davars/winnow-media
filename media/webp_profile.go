package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// maxWebPChunks bounds the chunk headers webpHasICC reads; a still WebP has
// a handful.
const maxWebPChunks = 1024

// webpICCFlag is the ICC bit of the VP8X flags byte.
const webpICCFlag = 0x20

// webpHasICC reports whether the WebP at path carries an ICC profile that
// ImageMagick will read: a leading VP8X chunk with the ICC flag set and an
// ICCP chunk. Either alone yields no profile there. It reads chunk headers
// only, at most maxWebPChunks of them, never chunk data, so cost does not
// depend on the image. A file whose chunk list ends or is cut short before an
// ICCP chunk has no profile, since the header read that precedes this call
// already accepted the file; more than maxWebPChunks chunks is an error.
func webpHasICC(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	var riff [12]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return false, tolerateShort(err)
	}
	if string(riff[:4]) != "RIFF" || string(riff[8:]) != "WEBP" {
		return false, nil
	}
	var head [8]byte
	for n := 0; n < maxWebPChunks; n++ {
		if _, err := io.ReadFull(f, head[:]); err != nil {
			return false, tolerateShort(err)
		}
		size := int64(binary.LittleEndian.Uint32(head[4:]))
		skip := size + size&1 // chunk data is padded to an even length
		switch string(head[:4]) {
		case "ICCP":
			return n > 0, nil
		case "VP8X":
			if n != 0 {
				return false, nil
			}
			var flags [1]byte
			if _, err := io.ReadFull(f, flags[:]); err != nil {
				return false, tolerateShort(err)
			}
			if flags[0]&webpICCFlag == 0 {
				return false, nil
			}
			skip--
		default:
			if n == 0 {
				return false, nil
			}
		}
		if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
			return false, err
		}
	}
	return false, fmt.Errorf("WebP holds more than %d chunks", maxWebPChunks)
}

// tolerateShort turns the end of the file into "no more chunks".
func tolerateShort(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}
