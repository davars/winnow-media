package media

import (
	"fmt"
	"slices"
	"strings"
)

// mediaKind is how an operation reads a type; a type may have several.
type mediaKind uint8

const (
	kindStill mediaKind = 1 << iota
	kindAnimation
	kindVideo
)

// typeEntry says how the library reads one media type.
type typeEntry struct {
	kind mediaKind
	// coders are the ImageMagick format names an image of the type reports as
	// %m, in upper case; the first is the coder a caller's naming of the type
	// pins. Empty for a video.
	coders []string
	// demuxer is the ffmpeg demuxer of a video.
	demuxer string
	// sniffed types are named by their leading bytes (sniff.go), so
	// ImageMagick is never asked to detect them.
	sniffed bool
	// keptExact marks a type whose kept decode (see OpenOptions.Reuse) gives
	// the bytes of a fresh decode. TestEveryTypeMarkedExactHasAFixture fails
	// for a marked type without a fixture whose kept decode gives them.
	keptExact bool
}

// typeTable is every type the library has a pinned decoder for and that has
// no external-reference or scripting semantics. Its keys are media types: the
// IANA registered one where there is one, else what file(1) reports. A type
// is here only if a fixture of it decodes under the generated policy
// (types_test.go). Never
// here: SVG, MVG, MSL, PostScript, EPS, PDF and text, and anything
// ImageMagick reads through a delegate.
var typeTable = map[string]typeEntry{
	typeGIF:  {kind: kindAnimation, coders: []string{"GIF"}, sniffed: true},
	typeWebP: {kind: kindStill | kindAnimation, coders: []string{"WEBP"}, sniffed: true, keptExact: true},
	typeHEIC: {kind: kindStill, coders: []string{"HEIC"}, sniffed: true, keptExact: true},
	typeHEIF: {kind: kindStill, coders: []string{"HEIC"}, sniffed: true, keptExact: true},
	typeAVIF: {kind: kindStill, coders: []string{"AVIF"}, sniffed: true, keptExact: true},

	"image/jpeg":                    {kind: kindStill, coders: []string{"JPEG"}, keptExact: true},
	"image/png":                     {kind: kindStill, coders: []string{"PNG"}, keptExact: true},
	"image/tiff":                    {kind: kindStill, coders: []string{"TIFF", "TIFF64"}, keptExact: true},
	"image/bmp":                     {kind: kindStill, coders: []string{"BMP"}},
	"image/x-tga":                   {kind: kindStill, coders: []string{"TGA"}},
	"image/vnd.microsoft.icon":      {kind: kindStill, coders: []string{"ICO"}},
	"image/x-pcx":                   {kind: kindStill, coders: []string{"PCX"}},
	"image/x-portable-bitmap":       {kind: kindStill, coders: []string{"PBM"}},
	"image/x-portable-greymap":      {kind: kindStill, coders: []string{"PGM"}},
	"image/x-portable-pixmap":       {kind: kindStill, coders: []string{"PPM"}},
	"image/x-portable-arbitrarymap": {kind: kindStill, coders: []string{"PAM"}},
	"image/g3fax":                   {kind: kindStill, coders: []string{"G3"}},
	"image/vnd.adobe.photoshop":     {kind: kindStill, coders: []string{"PSD"}},
	"image/jxl":                     {kind: kindStill, coders: []string{"JXL"}, keptExact: true},
	"image/jp2":                     {kind: kindStill, coders: []string{"JP2"}},
	"image/x-exr":                   {kind: kindStill, coders: []string{"EXR"}},
	"image/x-xcf":                   {kind: kindStill, coders: []string{"XCF"}},
	"image/vnd.djvu":                {kind: kindStill, coders: []string{"DJVU"}},
	"image/x-adobe-dng":             {kind: kindStill, coders: []string{"DNG"}},

	typeMP4:  {kind: kindVideo, demuxer: "mov", sniffed: true},
	typeMOV:  {kind: kindVideo, demuxer: "mov", sniffed: true},
	typeWebM: {kind: kindVideo, demuxer: "matroska", sniffed: true},
	typeMKV:  {kind: kindVideo, demuxer: "matroska", sniffed: true},
}

// coderTypes maps the %m ImageMagick reports for content its own detection
// names to the type, for the types that sniff does not name. Content of TGA,
// ICO, G3 and DNG is never named this way: ImageMagick cannot identify TGA,
// ICO or G3 from their bytes, and calls a DNG a TIFF.
var coderTypes = func() map[string]string {
	m := map[string]string{}
	for typ, e := range typeTable {
		if e.sniffed {
			continue
		}
		for _, c := range e.coders {
			m[c] = typ
		}
	}
	return m
}()

// Types returns the media types the library has a pinned decoder for, sorted:
// every type of the table, which is the default of Config.Types. SVG, MVG,
// MSL, PostScript, EPS, PDF and text are never among them. TGA, ICO and CUR
// (one type), G3 and DNG are not identified from content: they are decoded
// only when the caller names the type in OpenOptions.Type. A TIFF whose first
// image is a reduced-resolution preview, as in a camera raw file, is likewise
// refused unless its type is named.
func Types() []string {
	out := make([]string, 0, len(typeTable))
	for typ := range typeTable {
		out = append(out, typ)
	}
	slices.Sort(out)
	return out
}

// resolveTypes returns the set requested, or the whole table for nil, as a
// sorted list without repeats; it is an error to name a type the table lacks.
func resolveTypes(requested []string) ([]string, error) {
	if requested == nil {
		return Types(), nil
	}
	if len(requested) == 0 {
		return nil, fmt.Errorf("types: the list is empty")
	}
	out := slices.Clone(requested)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, typ := range out {
		if _, ok := typeTable[typ]; !ok {
			return nil, fmt.Errorf("types: %q is not a type the library supports (see Types)", typ)
		}
	}
	heif := []string{typeHEIC, typeHEIF, typeAVIF}
	var named int
	for _, typ := range heif {
		if slices.Contains(out, typ) {
			named++
		}
	}
	if named != 0 && named != len(heif) {
		return nil, fmt.Errorf("types: %v are read by one decoder, which chooses its codec from the file's items, so all must be named or none", heif)
	}
	return out, nil
}

// decoderFor returns how an input of the table's type typ is read: the
// demuxer of a video, else the ImageMagick coder, which is coder when the
// content named one and the type's own otherwise.
func (e typeEntry) decoderFor(coder string) decoder {
	if e.kind == kindVideo {
		return decoder{"ffmpeg", e.demuxer}
	}
	if coder == "" {
		coder = e.coders[0]
	}
	return decoder{"magick", strings.ToLower(coder)}
}
