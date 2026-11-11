package media

import (
	"bytes"
	"fmt"
	"os"
	"slices"
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
	// crop, when set, is cut from each frame once it is upright, before it is
	// sized. upright is the size the frames must have at that point; the
	// run reports the size it found and is refused if it differs.
	crop    region
	upright [2]int
}

type outputKind int

const (
	// outputStill writes the one frame to path.
	outputStill outputKind = iota
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

// reportFormat is the -format of the report an encode run prints, one line
// per frame: the size of the frame, preceded by the signature of its pixels
// when signed. Computing a signature costs a pass over every frame, so only
// the frame-file route that compares them asks for it.
func reportFormat(signed bool) string {
	if signed {
		return "%#|%w|%h\n"
	}
	return "%w|%h\n"
}

// encodedFrame is one line of that report. signature is empty unless the run
// was signed.
type encodedFrame struct {
	signature     string
	width, height int
}

// encodeBound is what the size of one encoded frame must satisfy.
type encodeBound struct {
	maxEdge, maxArea int
	// width and height, when both are set, are the exact size.
	width, height int
}

// boundOf is the bound of a frame of enc encoded with src.
func boundOf(enc encoding, src encodeSource) encodeBound {
	return encodeBound{enc.maxEdge, enc.maxArea, src.width, src.height}
}

// verify returns an error if a frame of size w x h breaks the bound.
func (b encodeBound) verify(w, h int) error {
	switch {
	case b.width > 0 && b.height > 0 && (w != b.width || h != b.height):
		return fmt.Errorf("encoded frame is %dx%d, want exactly %dx%d", w, h, b.width, b.height)
	case max(w, h) > b.maxEdge || w*h > b.maxArea:
		return fmt.Errorf("encoded frame is %dx%d, above the bounds of %d on a side and %d pixels", w, h, b.maxEdge, b.maxArea)
	}
	return nil
}

// encodeCheck is what runEncode holds the run to. The zero value checks
// nothing.
type encodeCheck struct {
	// bounds holds the bound of each reported frame, or one bound for all of
	// them.
	bounds []encodeBound
	// frames is the number of frames the run must report; 0 leaves it
	// unchecked.
	frames int
	// outputs are the files the run writes. They are removed when the run
	// fails or is refused.
	outputs []string
	// signed says the run reports a pixel signature with each frame (see
	// reportFormat).
	signed bool
	// upright is the upright size every frame must report before it is
	// cropped; the zero value means the run crops nothing and reports none.
	upright [2]int
}

// timeLimitMessage is what ImageMagick prints when a run exceeds its time
// limit.
var timeLimitMessage = []byte("time limit exceeded")

// timeLimitError returns err, made to wrap ErrTimeLimit if ImageMagick
// reported its time limit on stderr, whatever its exit status; runErr is the
// error of the run, nil when it exited 0. A run that exited 0 yields an error
// quoting stderr; the error of a failed run already does.
func timeLimitError(stderr []byte, runErr error) error {
	switch {
	case !bytes.Contains(stderr, timeLimitMessage):
		return runErr
	case runErr != nil:
		return fmt.Errorf("%w: %w", ErrTimeLimit, runErr)
	}
	return fmt.Errorf("magick: %w: %s", ErrTimeLimit, bytes.TrimSpace(stderr))
}

// checkFrames returns an error for a frame outside its bound, which is how a
// resize that did not happen shows when stderr cannot be read.
func (c encodeCheck) checkFrames(frames []encodedFrame) error {
	want := max(c.frames, len(c.bounds))
	if c.frames > 0 || len(c.bounds) > 1 {
		if len(frames) != want {
			return fmt.Errorf("encode reported %d frames, want %d", len(frames), want)
		}
	}
	for i, f := range frames {
		if len(c.bounds) == 0 {
			break
		}
		if err := c.bounds[min(i, len(c.bounds)-1)].verify(f.width, f.height); err != nil {
			return fmt.Errorf("magick: frame %d: %w", i, err)
		}
	}
	return nil
}

// removeOutputs deletes the files of the run, ignoring errors.
func (c encodeCheck) removeOutputs() {
	for _, p := range c.outputs {
		_ = os.Remove(p)
	}
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
// The result carries no resource limits; Env.runMagick adds them.
// outputFiles requires out.path to be a magick filename pattern containing
// %d.
func encodeArgs(frames []encodeFrame, src encodeSource, enc encoding, out encodeOutput) []string {
	var args []string
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
	return append(args, "-format", reportFormat(out.kind == outputFiles), "info:")
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
	if !src.crop.zero() {
		// ImageMagick clips a crop outside the frame without failing, so the
		// run reports the size it cropped.
		args = append(args, "-print", uprightPrefix+"%w|%h\\n",
			"-crop", fmt.Sprintf("%dx%d+%d+%d", src.crop.width, src.crop.height, src.crop.x, src.crop.y), "+repage")
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

// uprightPrefix starts each line a crop makes the encode run print: the size
// of the frame before the crop.
const uprightPrefix = "upright|"

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
// input is the decode reference. The result carries no resource limits;
// Env.runMagick adds them.
//
// A single target needs no clone.
func fusedArgs(readOps []string, src encodeSource, targets []fusedTarget) []string {
	args := slices.Clone(readOps)
	args = append(args, targets[0].frame.input)
	for _, t := range targets {
		ops := append(frameOps(t.frame, t.source(src), t.enc), encodeTail(t.enc)...)
		ops = append(ops, "-write", t.path, "-format", reportFormat(false))
		if len(targets) == 1 {
			return append(append(args, ops...), "info:")
		}
		args = append(args, "(", "+clone")
		args = append(append(args, ops...), "-write", "info:", "+delete", ")")
	}
	return append(args, "null:")
}

// splitUpright separates the upright lines of an encode run's output from the
// report, and returns the report and the sizes the lines give.
func splitUpright(out []byte) ([]byte, [][2]int, error) {
	var report []string
	var sizes [][2]int
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(line, uprightPrefix)
		if !ok {
			report = append(report, line)
			continue
		}
		var size [2]int
		w, h, found := strings.Cut(rest, "|")
		var err1, err2 error
		size[0], err1 = strconv.Atoi(w)
		size[1], err2 = strconv.Atoi(h)
		if !found || err1 != nil || err2 != nil {
			return nil, nil, fmt.Errorf("encode report line %q has an unreadable size", line)
		}
		sizes = append(sizes, size)
	}
	return []byte(strings.Join(report, "\n")), sizes, nil
}

// checkUpright returns an error unless the run printed the upright size c
// expects once for each of frames reported frames, or nothing when c expects
// no crop.
func (c encodeCheck) checkUpright(got [][2]int, frames int) error {
	if c.upright == [2]int{} {
		if len(got) > 0 {
			return fmt.Errorf("encode reported %d upright sizes for a run that crops nothing", len(got))
		}
		return nil
	}
	if len(got) != frames {
		return fmt.Errorf("encode reported %d upright sizes for %d frames", len(got), frames)
	}
	for i, size := range got {
		if size != c.upright {
			return fmt.Errorf("magick: frame %d is %dx%d upright, the region was checked against %dx%d",
				i, size[0], size[1], c.upright[0], c.upright[1])
		}
	}
	return nil
}

// parseEncoded reads the report of an encode run, signed as reportFormat says.
func parseEncoded(out []byte, signed bool) ([]encodedFrame, error) {
	fields := 2
	if signed {
		fields = 3
	}
	var frames []encodedFrame
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) != fields {
			return nil, fmt.Errorf("encode report line %q missing fields", line)
		}
		var signature string
		if signed {
			signature, f = f[0], f[1:]
		}
		w, err1 := strconv.Atoi(f[0])
		h, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("encode report line %q has an unreadable size", line)
		}
		frames = append(frames, encodedFrame{signature: signature, width: w, height: h})
	}
	return frames, nil
}
