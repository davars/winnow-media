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
}

// encodeSource is what the header read decided about the frames of an
// encode.
type encodeSource struct {
	// orientation is the magick orientation name applied to every frame; ""
	// and "Undefined" apply the orientation the input carries, if any.
	orientation string
	// width and height are the size of the frames after orientation. They
	// set the output size.
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

// resizeFilter is the resampling filter of every resize. ImageMagick's own
// default depends on the image it is handed (Lanczos, but Mitchell for one
// with a palette or an alpha channel), which would make the output depend on
// the route the pixels took to the encode.
const resizeFilter = "Lanczos"

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
// never holds more than one frame at full size. The output size is the one
// fitSize gives src and enc, asked for exactly. Metadata is stripped last,
// after the conversion that needed it. The run prints one encodedFrame line
// per frame, which parseEncoded reads.
//
// The result starts with LimitArgs. It requires every frame, once oriented,
// to have the size src states; outputFiles requires out.path to be a magick
// filename pattern containing %d.
func encodeArgs(frames []encodeFrame, src encodeSource, enc encoding, out encodeOutput) []string {
	args := LimitArgs()
	tw, th, resize := fitSize(src.width, src.height, enc.maxEdge, enc.maxArea)
	grouped := len(frames) > 1
	for _, f := range frames {
		if grouped {
			args = append(args, "(")
		}
		if out.kind == outputAnimation && f.delayCS >= 0 {
			args = append(args, "-delay", strconv.Itoa(f.delayCS))
		}
		args = append(args, f.input)
		args = append(args, f.prelude...)
		if src.orientation != "" && src.orientation != "Undefined" {
			args = append(args, "-orient", src.orientation)
		}
		args = append(args, "-auto-orient")
		if f.convertTo != "" {
			if f.assign != "" {
				args = append(args, "-profile", f.assign)
			}
			args = append(args, "-profile", f.convertTo)
		}
		if enc.format == JPEG {
			args = append(args, "-background", "white", "-alpha", "remove", "-alpha", "off")
		}
		if resize {
			args = append(args, "-filter", resizeFilter, "-resize", fmt.Sprintf("%dx%d!", tw, th))
		}
		if grouped {
			args = append(args, ")")
		}
	}
	args = append(args, "-strip", "-quality", strconv.Itoa(enc.quality))
	switch out.kind {
	case outputStill, outputAnimation:
		args = append(args, "-write", out.path)
	case outputFiles:
		args = append(args, "+adjoin", "-scene", "0", "-write", out.path)
	}
	return append(args, "-format", encodeFormat, "info:")
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
