package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"os"
	"strings"
)

// sniffBytes is how much of a file the type sniffing reads.
const sniffBytes = 4096

// The media types sniff can return.
const (
	typeGIF  = "image/gif"
	typeWebP = "image/webp"
	typeHEIC = "image/heic"
	typeHEIF = "image/heif"
	typeMP4  = "video/mp4"
	typeMOV  = "video/quicktime"
	typeWebM = "video/webm"
	typeMKV  = "video/x-matroska"
)

func unrecognized(what string) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedType, what)
}

// sniffFile returns the media type of the file at path from its leading
// bytes alone.
func sniffFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, sniffBytes))
	if err != nil {
		return "", fmt.Errorf("source: %w", err)
	}
	return sniff(buf)
}

// sniff returns the media type that the leading bytes of a file name. It
// never reads past b and returns an error wrapping ErrUnsupportedType for
// anything else.
func sniff(b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return typeGIF, nil
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return typeWebP, nil
	case bytes.HasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return sniffEBML(b)
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return sniffFtyp(b)
	}
	return "", unrecognized("unrecognized content")
}

func brandSet(brands ...string) map[string]bool {
	m := make(map[string]bool, len(brands))
	for _, b := range brands {
		m[b] = true
	}
	return m
}

var (
	heicBrands = brandSet("heic", "heix", "heim", "heis", "hevc", "hevx")
	avifBrands = brandSet("avif", "avis")
	mp4Brands  = brandSet("isom", "iso2", "iso3", "iso4", "iso5", "iso6", "mp41", "mp42",
		"avc1", "M4V ", "dash", "3gp4", "3gp5", "3gp6", "3g2a")
)

func sniffFtyp(b []byte) (string, error) {
	if len(b) < 16 {
		return "", unrecognized("truncated ftyp box")
	}
	size := binary.BigEndian.Uint32(b)
	if size < 16 {
		return "", unrecognized("malformed ftyp box")
	}
	major := string(b[8:12])
	end := min(uint64(size), uint64(len(b)))
	var compatible []string
	for off := uint64(16); off+4 <= end; off += 4 {
		compatible = append(compatible, string(b[off:off+4]))
	}
	switch {
	case heicBrands[major]:
		return typeHEIC, nil
	case major == "mif1" || major == "msf1":
		var heic, avif bool
		for _, c := range compatible {
			heic = heic || heicBrands[c]
			avif = avif || avifBrands[c]
		}
		if heic && !avif {
			return typeHEIF, nil
		}
	case major == "qt  ":
		return typeMOV, nil
	case mp4Brands[major]:
		return typeMP4, nil
	}
	return "", unrecognized(fmt.Sprintf("ISO base media brand %q", major))
}

// readVint reads an EBML variable-length integer. keepMarker leaves the
// length marker bit in place, as element IDs are written.
func readVint(b []byte, keepMarker bool) (value uint64, n int, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if len(b) < n {
		return 0, 0, false
	}
	value = uint64(b[0])
	if !keepMarker {
		value &= 1<<(8-n) - 1
	}
	for _, c := range b[1:n] {
		value = value<<8 | uint64(c)
	}
	return value, n, true
}

const ebmlDocType = 0x4282

func sniffEBML(b []byte) (string, error) {
	rest := b[4:]
	size, n, ok := readVint(rest, false)
	if !ok || size == 1<<(7*n)-1 {
		return "", unrecognized("malformed EBML header")
	}
	rest = rest[n:]
	if size < uint64(len(rest)) {
		rest = rest[:size]
	}
	for len(rest) > 0 {
		id, n, ok := readVint(rest, true)
		if !ok {
			break
		}
		rest = rest[n:]
		size, n, ok := readVint(rest, false)
		if !ok || size > uint64(len(rest)-n) {
			break
		}
		data := rest[n : n+int(size)]
		rest = rest[n+int(size):]
		if id != ebmlDocType {
			continue
		}
		switch docType := strings.TrimRight(string(data), "\x00"); docType {
		case "webm":
			return typeWebM, nil
		case "matroska":
			return typeMKV, nil
		default:
			return "", unrecognized(fmt.Sprintf("EBML document type %q", docType))
		}
	}
	return "", unrecognized("EBML header without a document type")
}
