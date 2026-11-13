package media

import (
	"fmt"
	"slices"
	"strings"
)

// typeEntry says how the library reads one media type.
type typeEntry struct {
	// video is set for a type read by ffmpeg.
	video bool
	// coders are the ImageMagick format names an image of the type reports as
	// %m, in upper case; the first is the coder a caller's naming of the type
	// pins. Empty for a video.
	coders []string
	// demuxer is the ffmpeg demuxer of a video.
	demuxer string
	// alsoDemuxers are other demuxers that read the type: a detected file
	// they read is pinned to the one that read it.
	alsoDemuxers []string
	// sniffed types are named by their leading bytes (sniff.go), so
	// ImageMagick is never asked to detect them.
	sniffed bool
	// detected video types may also be named by ffprobe, restricted to the
	// demuxers of the configured detected types, for a file whose leading
	// bytes name no type (see Processor.detectVideo).
	detected bool
	// keptExact marks a type whose kept decode (see OpenOptions.KeepDecode) gives
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
	typeGIF:  {coders: []string{"GIF"}, sniffed: true},
	typeWebP: {coders: []string{"WEBP"}, sniffed: true, keptExact: true},
	typeHEIC: {coders: []string{"HEIC"}, sniffed: true, keptExact: true},
	typeHEIF: {coders: []string{"HEIC"}, sniffed: true, keptExact: true},
	typeAVIF: {coders: []string{"AVIF"}, sniffed: true, keptExact: true},

	"image/jpeg":                    {coders: []string{"JPEG"}, keptExact: true},
	"image/png":                     {coders: []string{"PNG"}, keptExact: true},
	"image/tiff":                    {coders: []string{"TIFF", "TIFF64"}, keptExact: true},
	"image/bmp":                     {coders: []string{"BMP"}},
	"image/x-tga":                   {coders: []string{"TGA"}},
	"image/vnd.microsoft.icon":      {coders: []string{"ICO"}},
	"image/x-pcx":                   {coders: []string{"PCX"}},
	"image/x-portable-bitmap":       {coders: []string{"PBM"}},
	"image/x-portable-greymap":      {coders: []string{"PGM"}},
	"image/x-portable-pixmap":       {coders: []string{"PPM"}},
	"image/x-portable-arbitrarymap": {coders: []string{"PAM"}},
	"image/g3fax":                   {coders: []string{"G3"}},
	"image/vnd.adobe.photoshop":     {coders: []string{"PSD"}},
	"image/jxl":                     {coders: []string{"JXL"}, keptExact: true},
	"image/jp2":                     {coders: []string{"JP2"}},
	"image/x-exr":                   {coders: []string{"EXR"}},
	"image/x-xcf":                   {coders: []string{"XCF"}},
	"image/vnd.djvu":                {coders: []string{"DJVU"}},
	"image/x-adobe-dng":             {coders: []string{"DNG"}},

	typeMP4:    {video: true, demuxer: "mov", sniffed: true, detected: true},
	typeMOV:    {video: true, demuxer: "mov", sniffed: true, detected: true},
	typeWebM:   {video: true, demuxer: "matroska", sniffed: true},
	typeMKV:    {video: true, demuxer: "matroska", sniffed: true},
	typeAVI:    {video: true, demuxer: "avi", detected: true},
	typeASF:    {video: true, demuxer: "asf", detected: true},
	typeMPEG:   {video: true, demuxer: "mpeg", alsoDemuxers: []string{"mpegvideo"}, detected: true},
	typeMPEGTS: {video: true, demuxer: "mpegts", detected: true},
	typeFLV:    {video: true, demuxer: "flv", detected: true},
}

// The video types that no leading-bytes sniff names. WMV is ASF.
const (
	typeAVI    = "video/x-msvideo"
	typeASF    = "video/x-ms-asf"
	typeMPEG   = "video/mpeg"
	typeMPEGTS = "video/mp2t"
	typeFLV    = "video/x-flv"
)

// readers returns every demuxer that reads the type, its own first.
func (e typeEntry) readers() []string {
	return append([]string{e.demuxer}, e.alsoDemuxers...)
}

// detectedDemuxers returns the demuxers of the detected types among types,
// sorted and without repeats, as ffmpeg's -format_whitelist takes them.
func detectedDemuxers(types []string) string {
	var names []string
	for _, typ := range types {
		if e := typeTable[typ]; e.detected {
			names = append(names, e.readers()...)
		}
	}
	slices.Sort(names)
	return strings.Join(slices.Compact(names), ",")
}

// detectedType returns the accepted detected type that a container of ffprobe
// format_name names, which lists the demuxer's aliases, and the demuxer of the
// type that read it. The mov demuxer reads MP4 and QuickTime alike; preferMOV
// says QuickTime, for a file with no ftyp box, and else it is MP4.
func (p *Processor) detectedType(formatName string, preferMOV bool) (typ, demuxer string, ok bool) {
	names := strings.Split(formatName, ",")
	var found []string
	for _, t := range p.types {
		if e := typeTable[t]; e.detected && slices.ContainsFunc(e.readers(), func(d string) bool { return slices.Contains(names, d) }) {
			found = append(found, t)
		}
	}
	switch {
	case len(found) == 0:
		return "", "", false
	case slices.Contains(names, "mov"):
		// The one demuxer reads both: the file's own kind of ftyp, or the
		// lack of one, says which type, and a type not configured is refused.
		typ = typeMP4
		if preferMOV {
			typ = typeMOV
		}
		if !slices.Contains(found, typ) {
			return "", "", false
		}
	default:
		typ = found[0]
	}
	for _, d := range typeTable[typ].readers() {
		if slices.Contains(names, d) {
			return typ, d, true
		}
	}
	return "", "", false
}

// coderTypes maps the %m ImageMagick reports for content its own detection
// names to the type, for the types that sniff does not name. Content of TGA,
// ICO and G3 is never named this way: ImageMagick cannot identify them from
// their bytes. DNG content is reached through the TIFF reader, which hands it
// to the DNG coder, so it is named this way only when the TIFF coder is
// allowed too.
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

// Types returns the media types the library can read, sorted. It is what a
// nil Config.Types accepts. Later versions may add types; the example prints
// the current ones.
//
// SVG, MVG, MSL, PostScript, EPS, PDF and text are never among them: reading
// those can run other programs or open other files. WMV is the ASF type
// (video/x-ms-asf).
//
// Three of the types are not identified from a file's bytes and are read only
// when OpenOptions.Type names them: image/x-tga, image/vnd.microsoft.icon
// (ICO and CUR) and image/g3fax. A DNG is identified from its bytes only when
// image/tiff is also among Config.Types; otherwise it must be named. A TIFF
// whose first image is a reduced-resolution preview, as in a camera raw file,
// is refused unless its type is named.
//
// A type is read by the tools found at run time: an ImageMagick or ffmpeg
// build other than the flake's may lack the decoder for one.
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
	if e.video {
		return decoder{"ffmpeg", e.demuxer}
	}
	if coder == "" {
		coder = e.coders[0]
	}
	return decoder{"magick", strings.ToLower(coder)}
}
