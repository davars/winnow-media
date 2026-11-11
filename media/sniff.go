package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"os"
	"slices"
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
	typeAVIF = "image/avif"
	typeMP4  = "video/mp4"
	typeMOV  = "video/quicktime"
	typeWebM = "video/webm"
	typeMKV  = "video/x-matroska"
)

func unrecognized(what string) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedType, what)
}

// errUnnamed is what sniff returns for leading bytes that name no container:
// the content is left to ImageMagick's detection. Every other sniff error
// refuses a file that does name one.
var errUnnamed = unrecognized("unrecognized content")

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
	return "", errUnnamed
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
		if avif && !heic {
			return typeAVIF, nil
		}
	case major == "avif":
		return typeAVIF, nil
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

// mpegTSPacket is the size of a transport stream packet, each of which starts
// with the sync byte 0x47.
const mpegTSPacket = 188

// quickTimeAtoms are the top-level atoms a QuickTime file may open with when
// it has no ftyp.
var quickTimeAtoms = [...]string{"moov", "mdat", "wide", "free", "skip", "pnot"}

// videoContainerOf returns the decoder of the video container the leading
// bytes of a file open, when sniff names no type for them: a QuickTime file
// without an ftyp, which is pinned to mov, and AVI, ASF, FLV, an MPEG program
// or elementary video stream or an MPEG transport stream, which are left to
// ffmpeg's detection among detectedFormats. It is a routing hint for Env.Open,
// not a verdict; the probe decides whether the file is a video.
func videoContainerOf(b []byte) (decoder, bool) {
	switch {
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "AVI ":
		return unnamedVideoDecoder, true
	case bytes.HasPrefix(b, []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11}):
		return unnamedVideoDecoder, true
	case bytes.HasPrefix(b, []byte("FLV\x01")):
		return unnamedVideoDecoder, true
	case bytes.HasPrefix(b, []byte{0, 0, 1, 0xBA}), bytes.HasPrefix(b, []byte{0, 0, 1, 0xB3}):
		return unnamedVideoDecoder, true
	case len(b) >= 8 && slices.Contains(quickTimeAtoms[:], string(b[4:8])):
		return decoder{"ffmpeg", "mov"}, true
	}
	if len(b) > 2*mpegTSPacket && b[0] == 0x47 && b[mpegTSPacket] == 0x47 && b[2*mpegTSPacket] == 0x47 {
		return unnamedVideoDecoder, true
	}
	return decoder{}, false
}

// videoRoute says how Env.Open treats a file's leading bytes.
type videoRoute int

const (
	// routeNotVideo: the bytes name an image type, or nothing that looks like
	// a video.
	routeNotVideo videoRoute = iota
	// routeSniffed: sniff names a video type.
	routeSniffed
	// routeLooksLike: sniff names no type and the bytes open a video container.
	routeLooksLike
)

// videoRouteOf returns the route of the file at path and, for a video route,
// its decoder. A file sniff names an image type is never looked at further.
func videoRouteOf(path string) (videoRoute, decoder) {
	f, err := os.Open(path)
	if err != nil {
		return routeNotVideo, decoder{}
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, sniffBytes))
	if err != nil {
		return routeNotVideo, decoder{}
	}
	mediaType, err := sniff(buf)
	if err == nil {
		if e := typeTable[mediaType]; e.kind == kindVideo {
			return routeSniffed, e.decoderFor("")
		}
		return routeNotVideo, decoder{}
	}
	if dec, ok := videoContainerOf(buf); ok {
		return routeLooksLike, dec
	}
	return routeNotVideo, decoder{}
}
