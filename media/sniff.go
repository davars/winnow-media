package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
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

// errOtherBrand is what sniff returns for an ISO base media file whose brand
// is none it names and is not that of a still image: the container may still
// be a video, which only a probe can say.
var errOtherBrand = unrecognized("ISO base media brand")

// readHead returns the leading bytes of the input that sniffing reads.
func readHead(in input) ([]byte, error) {
	r, done, err := in.open()
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer done()
	buf, err := io.ReadAll(io.LimitReader(r, sniffBytes))
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	return buf, nil
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

// stillBrands are the brands of ISO base media files that hold still images,
// camera raw or JPEG 2000 rather than video, and of the HEIF family whose
// members sniff cannot tell apart. They are never probed as a video: ffmpeg's
// mov demuxer reads such a file as if it had a video stream.
var stillBrands = brandSet("mif1", "mif2", "msf1", "miaf", "heif", "avci", "avcs", "crx ", "jp2 ", "jpx ", "jpm ")

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
	if isStillBrand(major) || slices.ContainsFunc(compatible, isStillBrand) {
		return "", unrecognized(fmt.Sprintf("ISO base media brand %q", major))
	}
	return "", fmt.Errorf("%w %q", errOtherBrand, major)
}

// isStillBrand reports whether an ISO base media brand marks a file of still
// images, whether sniff names its type or not.
func isStillBrand(brand string) bool {
	return stillBrands[brand] || heicBrands[brand] || avifBrands[brand]
}

// leadsStill reports whether the top-level ftyp box among the first boxes of
// b, which may follow a free or wide box, has a brand of a still-image file.
func leadsStill(b []byte) bool {
	off := uint64(0)
	for range 8 {
		if uint64(len(b)) < off+16 {
			return false
		}
		size := uint64(binary.BigEndian.Uint32(b[off:]))
		if string(b[off+4:off+8]) == "ftyp" {
			end := min(off+size, uint64(len(b)))
			for at := off + 8; at+4 <= end; at += 4 {
				if at != off+12 && isStillBrand(string(b[at:at+4])) { // 12..16 is the minor version
					return true
				}
			}
			return false
		}
		if size < 8 {
			return false
		}
		off += size
	}
	return false
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

// videoContainerOf reports whether the leading bytes of a file open a video
// container, when sniff names no type for them: a QuickTime file without an
// ftyp, AVI, ASF, FLV, an MPEG program or elementary video stream or an MPEG
// transport stream. It is a routing hint, not a verdict; the probe decides
// whether the file is a video.
func videoContainerOf(b []byte) bool {
	switch {
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "AVI ":
		return true
	case bytes.HasPrefix(b, []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11}):
		return true
	case bytes.HasPrefix(b, []byte("FLV\x01")):
		return true
	case bytes.HasPrefix(b, []byte{0, 0, 1, 0xBA}), bytes.HasPrefix(b, []byte{0, 0, 1, 0xB3}):
		return true
	case len(b) >= 8 && slices.Contains(quickTimeAtoms[:], string(b[4:8])):
		return true
	}
	return len(b) > 2*mpegTSPacket && b[0] == 0x47 && b[mpegTSPacket] == 0x47 && b[2*mpegTSPacket] == 0x47
}
