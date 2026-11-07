package media

import (
	"fmt"
	"strconv"
	"strings"
)

// encodeFrame is one frame an encode reads.
type encodeFrame struct {
	// input is the ImageMagick input reference of the frame, such as
	// "pam:/dir/f.pam" or "heic:/in/photo[0]".
	input string
	// prelude holds operators applied as soon as the frame is read, before
	// orientation.
	prelude []string
	// delayCS is the frame's delay in centiseconds in an animated output; -1
	// keeps the delay the input carries. Other outputs ignore it.
	delayCS int
	// convertTo is the path of the sRGB profile the frame is converted to. It
	// is empty for a frame that carries no profile: its pixels are left as
	// they are, never assigned one.
	convertTo string
	// assign is the path of the ICC profile the frame is given before the
	// conversion, for a frame whose pixels were stored without it. It is empty
	// when the profile is embedded in the input. It is ignored when convertTo
	// is empty.
	assign string
	// filter is the resize filter; the zero value is filterLanczos.
	filter resizeFilter
	// fits says the frame is already within the bounds of the encoding, so it
	// is not resampled: a filter other than Lanczos blurs even at the same
	// size.
	fits bool
}

// encodeSource is what the header read decided about the frames of an
// encode.
type encodeSource struct {
	// orientation is the magick orientation name that replaces the one the
	// frames carry; "" and "Undefined" leave a frame's own tag to
	// -auto-orient.
	orientation string
	// width and height, when both are set, are the exact output size. A
	// decode stage that delivers frames below the source's size sets them from
	// the source's header, since the size computed from a reduced frame can
	// differ from the one computed from the source by a pixel.
	width, height int
}

type outputKind int

const (
	// outputProbe writes nothing; the run reports what the others would write.
	outputProbe outputKind = iota
	// outputStill writes the one frame to path.
	outputStill
	// outputAnimation writes every frame to path as one animated file, each
	// frame displayed for its delay.
	outputAnimation
	// outputFiles writes one file per frame; path is a magick filename pattern
	// containing %d.
	outputFiles
)

// encodeOutput says where an encode writes.
type encodeOutput struct {
	kind outputKind
	path string
}

// resizeFilter is the resampling filter of a resize.
type resizeFilter string

const (
	// filterLanczos is the filter of a frame whose source is opaque
	// truecolour, including video. It is the zero filter of encodeFrame.
	filterLanczos resizeFilter = "Lanczos"
	// filterMitchell is the filter of a frame whose source is palette-based
	// or declares alpha: Lanczos rings beside hard alpha edges and makes
	// such graphics larger.
	filterMitchell resizeFilter = "Mitchell"
)

// filterFor returns the filter for a source frame that is palette-based or
// declares alpha when soft is set. It is ImageMagick's own default rule, read
// from what the source file declares rather than from the intermediate a path
// hands ImageMagick, so every route a file can take picks the same filter.
func filterFor(soft bool) resizeFilter {
	if soft {
		return filterMitchell
	}
	return filterLanczos
}

// fitExpr returns the width and height of the output as ImageMagick fx
// expressions of the size of the image they are evaluated on, which are
// fitSize's (see there for the arithmetic) for the bounds maxEdge and
// maxArea: the same operations in the same order, on doubles. Used in a
// -resize argument, they are evaluated on the frame after -auto-orient.
func fitExpr(maxEdge, maxArea int) (width, height string) {
	// eps keeps a product such as (E/m)*m that lands a hair below the
	// integer E from flooring to E-1; it matches fitSize's.
	const eps = "0.000000001"
	scale := fmt.Sprintf("min(1,min(%d/max(w,h),sqrt(%d/(w*h))))", maxEdge, maxArea)
	tw := fmt.Sprintf("max(1,floor(w*%s+%s))", scale, eps)
	th := fmt.Sprintf("max(1,floor(h*%s+%s))", scale, eps)
	tw = fmt.Sprintf("max(1,min(%s,floor(%d/%s)))", tw, maxArea, th)
	th = fmt.Sprintf("max(1,min(%s,floor(%d/%s)))", th, maxArea, tw)
	return "%[fx:" + tw + "]", "%[fx:" + th + "]"
}

// encodeFormat is the -format of the report an encode run prints, one line
// per frame.
const encodeFormat = "%#|%w|%h\n"

// encodedFrame is one line of that report: the signature of the pixels
// written and the size of the frame.
type encodedFrame struct {
	signature     string
	width, height int
}

// encodeArgs returns the arguments of the magick run that is the one encode
// stage of every image output. Each frame is read inside its own parentheses
// when there are several, oriented, converted to sRGB if it carries a
// profile, flattened onto white for JPEG output and resized, so ImageMagick
// never holds more than one frame at full size. The output size is fitSize of
// the frame as ImageMagick has oriented it, computed by ImageMagick (see
// fitExpr) so that it follows the rotation actually applied, whatever the
// header said, and asked for exactly. Metadata is stripped last, after the
// conversion that needed it, and every output is written at 8 bits per channel
// whatever the depth of the source. The run prints one encodedFrame line per frame,
// which parseEncoded reads.
//
// The result starts with LimitArgs. outputFiles requires out.path to be a
// magick filename pattern containing %d.
func encodeArgs(frames []encodeFrame, src encodeSource, enc encoding, out encodeOutput) []string {
	args := LimitArgs()
	grouped := len(frames) > 1
	for _, f := range frames {
		if grouped {
			args = append(args, "(")
		}
		args = append(args, f.input)
		args = append(args, f.prelude...)
		if out.kind == outputAnimation && f.delayCS >= 0 {
			// Set on the frame once read: the -delay setting would outlive the
			// parentheses and override the delay of a frame read after it.
			args = append(args, "-set", "delay", strconv.Itoa(f.delayCS))
		}
		args = append(args, frameOps(f, src, enc)...)
		if grouped {
			args = append(args, ")")
		}
	}
	args = append(args, encodeTail(enc)...)
	switch out.kind {
	case outputStill, outputAnimation:
		args = append(args, "-write", out.path)
	case outputFiles:
		args = append(args, "+adjoin", "-scene", "0", "-write", out.path)
	}
	return append(args, "-format", encodeFormat, "info:")
}

// frameOps are the operators applied to a frame once it is read: orientation,
// conversion to sRGB, flattening for JPEG and the resize.
func frameOps(f encodeFrame, src encodeSource, enc encoding) []string {
	var args []string
	if src.orientation != "" && src.orientation != "Undefined" {
		args = append(args, "-orient", src.orientation, "-auto-orient",
			// -orient also sets the orientation the writers record, which
			// a PNG would carry on pixels already turned upright.
			"-orient", "TopLeft")
	} else {
		args = append(args, "-auto-orient")
	}
	if f.convertTo != "" {
		if f.assign != "" {
			args = append(args, "-profile", f.assign)
		}
		args = append(args, "-profile", f.convertTo)
	}
	if enc.format == JPEG {
		args = append(args, "-background", "white", "-alpha", "remove", "-alpha", "off")
	}
	if f.fits {
		return args
	}
	width, height := fitExpr(enc.maxEdge, enc.maxArea)
	if src.width > 0 && src.height > 0 {
		width, height = strconv.Itoa(src.width), strconv.Itoa(src.height)
	}
	filter := f.filter
	if filter == "" {
		filter = filterLanczos
	}
	return append(args, "-filter", string(filter), "-resize", width+"x"+height+"!")
}

// encodeTail are the operators that follow the last frame: metadata is
// stripped after the conversion that needed it, and the output is written at
// 8 bits per channel whatever the depth of the source.
func encodeTail(enc encoding) []string {
	return []string{"-strip", "-depth", "8", "-quality", strconv.Itoa(enc.quality)}
}

// fusedTarget is one output of a fusedArgs run.
type fusedTarget struct {
	frame encodeFrame
	enc   encoding
	path  string
	// width and height, when both are set, are the exact output size.
	width, height int
}

// source is src with the exact size of the target.
func (t fusedTarget) source(src encodeSource) encodeSource {
	src.width, src.height = t.width, t.height
	return src
}

// fusedArgs returns the arguments of the one magick run that decodes a still
// once and writes every target, each from its own clone of the decoded image,
// and reports one encodedFrame line per target in order. readOps are applied
// to the read, before any target (a decode size hint). Each target's frame
// input is the decode reference, and the result starts with LimitArgs.
//
// A single target needs no clone.
func fusedArgs(readOps []string, src encodeSource, targets []fusedTarget) []string {
	args := append(LimitArgs(), readOps...)
	args = append(args, targets[0].frame.input)
	for _, t := range targets {
		ops := append(frameOps(t.frame, t.source(src), t.enc), encodeTail(t.enc)...)
		ops = append(ops, "-write", t.path, "-format", encodeFormat)
		if len(targets) == 1 {
			return append(append(args, ops...), "info:")
		}
		args = append(args, "(", "+clone")
		args = append(append(args, ops...), "-write", "info:", "+delete", ")")
	}
	return append(args, "null:")
}

// parseEncoded reads the report of an encode run.
func parseEncoded(out []byte) ([]encodedFrame, error) {
	var frames []encodedFrame
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 3 {
			return nil, fmt.Errorf("encode report line %q missing fields", line)
		}
		w, err1 := strconv.Atoi(f[1])
		h, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("encode report line %q has an unreadable size", line)
		}
		frames = append(frames, encodedFrame{signature: f[0], width: w, height: h})
	}
	return frames, nil
}
