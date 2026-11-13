package media

import (
	"fmt"
	"os"
	"path/filepath"
)

// Format is the file format of an output image. Later versions may add
// formats.
type Format string

// The output formats. The zero Format is JPEG.
const (
	JPEG Format = "jpeg"
	WebP Format = "webp"
	PNG  Format = "png"
)

// Encoding is how Encode sizes and encodes each output image. The zero
// Encoding is a JPEG of quality 90 at the source's size, as far as the largest
// MaxEdge and MaxArea allow.
//
// Every output is upright, has 8 bits per channel and carries no metadata. A
// source with an ICC profile is converted to sRGB, and the profile is not
// embedded; a source without one is left as it is. Transparency is flattened
// onto white for a JPEG, or when Opaque is set.
//
// The size is the largest within MaxEdge and MaxArea that keeps the aspect
// ratio of the upright frame, or of the Region when one is given, each side
// rounded down: a 4000x3001 image at MaxEdge 2048 is 2048x1536. An output is
// never larger than its source.
type Encoding struct {
	// Format is the file format; the zero value is JPEG.
	Format Format
	// Quality is the encoder's quality setting, 1 to 100; zero means 90. A
	// PNG is lossless whatever the value.
	Quality int
	// MaxEdge bounds the longest side in pixels, 16 to 16384; zero means
	// 16384. A WebP holds at most 16383 a side, so a WebP output is held to
	// that.
	MaxEdge int
	// MaxArea bounds the pixel count, 256 to 100,000,000; zero means
	// 100,000,000.
	MaxArea int
	// Animate writes every selected frame into one animated file instead of
	// one file per frame. The Format must be WebP when more than one frame is
	// selected. Several images of a still cannot be animated.
	Animate bool
	// Opaque flattens alpha onto white, so the output has no alpha channel.
	// A JPEG is always opaque.
	Opaque bool
}

// Image describes one output file an Encode call wrote.
type Image struct {
	// Name is the file's name within the output directory.
	Name string
	// Encoding is the index, among the call's encodings, of the one that
	// produced the file.
	Encoding      int
	Width, Height int
	// Frames are the frames of the source the file shows, in order: one for
	// a plain image, one per scene of an animated file.
	Frames []Frame
	// Region is the part of the frame the file shows, in upright pixels of
	// the source: the Region asked for, or the whole frame. The whole frame's
	// Region is the upright size of the source, also for a still whose Info
	// did not have it.
	Region Region
}

// Region is a rectangle of a frame, in pixels of the frame as shown: after a
// still's orientation and a video's display rotation, and on an animation's
// canvas. X, Y is its top-left corner. The zero Region is the whole frame; any
// other must have sides of at least 1 and lie inside the frame.
type Region struct{ X, Y, Width, Height int }

func (r Region) internal() region { return region{r.X, r.Y, r.Width, r.Height} }

const (
	defaultQuality = 90

	// The ranges of Encoding.MaxEdge and MaxArea; zero is the top of the
	// range.
	edgeFloor   = 16
	edgeCeiling = maxDimension
	areaFloor   = 256
	areaCeiling = 100_000_000

	// webpMaxEdge is the largest side the WebP format holds. JPEG (65535) and
	// PNG (2^31-1) hold more than edgeCeiling, the largest frame the library
	// decodes, so for them edgeCeiling is the limit.
	webpMaxEdge = 16383
)

// edgeLimit is the largest side an output of format f has.
func edgeLimit(f Format) int {
	if f == WebP {
		return webpMaxEdge
	}
	return edgeCeiling
}

// outputFormats maps each Format to the extension of its files.
var outputFormats = map[Format]string{
	JPEG: ".jpg",
	WebP: ".webp",
	PNG:  ".png",
}

// encoding is an Encoding with defaults filled in and every field checked.
type encoding struct {
	format                    Format
	ext                       string
	quality, maxEdge, maxArea int
	animate, opaque           bool
}

// region is a rectangle of a frame in pixels of the frame as shown: after a
// still's orientation, a video's display rotation, on an animation's canvas.
// x, y is its top-left corner. The zero region is the whole frame.
type region struct{ x, y, width, height int }

func (r region) zero() bool { return r == region{} }

// shape returns an error unless r is the zero region or has a corner at or
// after 0,0 and a side of at least one pixel.
func (r region) shape() error {
	switch {
	case r.zero():
		return nil
	case r.x < 0 || r.y < 0:
		return invalid("region corner %d,%d is negative", r.x, r.y)
	case r.width < 1 || r.height < 1:
		return invalid("region %dx%d has a side below 1 pixel", r.width, r.height)
	}
	return nil
}

// check returns an error unless r is the zero region or has a valid shape and
// lies inside a frame of w x h.
func (r region) check(w, h int) error {
	if err := r.shape(); err != nil || r.zero() {
		return err
	}
	if r.width > w || r.x > w-r.width || r.height > h || r.y > h-r.height {
		return invalid("region %dx%d at %d,%d is outside the %dx%d frame", r.width, r.height, r.x, r.y, w, h)
	}
	return nil
}

// subject returns the size the output is fitted to: the region's, or w x h,
// the size of the frame, when r is the zero region.
func (r region) subject(w, h int) (int, int) {
	if r.zero() {
		return w, h
	}
	return r.width, r.height
}

func (e Encoding) resolve() (encoding, error) {
	out := encoding{
		format: e.Format, quality: e.Quality, maxEdge: e.MaxEdge, maxArea: e.MaxArea, animate: e.Animate, opaque: e.Opaque,
	}
	if out.format == "" {
		out.format = JPEG
	}
	ext, ok := outputFormats[out.format]
	if !ok {
		return encoding{}, invalid("unknown format %q", e.Format)
	}
	out.ext = ext
	var err error
	if out.quality, err = resolveInt("quality", e.Quality, defaultQuality, 1, 100); err != nil {
		return encoding{}, err
	}
	if out.maxEdge, err = resolveInt("max edge", e.MaxEdge, edgeCeiling, edgeFloor, edgeCeiling); err != nil {
		return encoding{}, err
	}
	out.maxEdge = min(out.maxEdge, edgeLimit(out.format))
	if out.maxArea, err = resolveInt("max area", e.MaxArea, areaCeiling, areaFloor, areaCeiling); err != nil {
		return encoding{}, err
	}
	return out, nil
}

// resolveInt returns def for zero and v when it lies within lo..hi.
func resolveInt(name string, v, def, lo, hi int) (int, error) {
	if v == 0 {
		return def, nil
	}
	if v < lo || v > hi {
		return 0, invalid("%s %d is outside %d..%d", name, v, lo, hi)
	}
	return v, nil
}

// decoder pins how an input is read, so that a type never depends on the
// tool sniffing the bytes.
type decoder struct {
	tool string // "magick" or "ffmpeg"
	// name is the ImageMagick coder or the ffmpeg demuxer.
	name string
}

// inDir runs fn, which writes into dir, and empties dir again if it fails.
func inDir(dir string, fn func() ([]Image, error)) ([]Image, error) {
	images, err := fn()
	if err != nil {
		clearDir(dir)
		return nil, err
	}
	return images, nil
}

func requireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%w: output directory: %w", ErrInvalidRequest, withoutPath(err))
	}
	if len(entries) > 0 {
		return invalid("output directory is not empty")
	}
	return nil
}

// clearDir removes the entries of dir, which the operation found empty.
func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
