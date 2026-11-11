package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// ErrUnsupportedType is returned, wrapped with the media type found in the
// input's bytes, when an operation does not accept that type or the bytes are
// of no type the package recognizes. Test for it with errors.Is.
var ErrUnsupportedType = errors.New("unsupported media type")

// Format is the encoding of an output image.
type Format string

// The output formats. The zero Format is JPEG.
const (
	JPEG Format = "jpeg"
	WebP Format = "webp"
	PNG  Format = "png"
)

// Encoding is how an operation sizes and encodes each output image. The
// output satisfies both size bounds with one resample, each side rounded down
// (a 4000x3001 image at MaxEdge 2048 is 2048x1536), and is never upscaled; it
// is upright, converted to sRGB when the source carries an ICC profile (an
// image without one is left as it is), has transparency flattened onto white
// for JPEG, carries no metadata (the converted profile is not embedded) and is
// written at 8 bits per channel whatever the depth of the source. The sRGB
// profile is embedded in the library and exists as a temporary dot-file in
// the output directory only while a tool runs. Zero fields take defaults.
//
// The size is the largest within both bounds with the source's aspect ratio,
// and is the size of the picture actually produced whatever orientation the
// file's header does or does not report. The resize filter follows what the
// source frame declares: Mitchell when it is palette-based (GIF, PNG8) or
// declares alpha, Lanczos for opaque truecolour and video, whichever way the
// pixels reached the encode.
type Encoding struct {
	// Format defaults to JPEG.
	Format Format
	// Quality is the encoder quality, 1 to 100; the default is 90.
	Quality int
	// MaxEdge bounds the longest side in pixels, 16 to 16384; the default
	// is 2576.
	MaxEdge int
	// MaxArea bounds the pixel count, 256 to 100000000; the default is
	// 3750000.
	MaxArea int
}

// Image describes one output file an operation wrote.
type Image struct {
	// Name is the file's name relative to the output directory.
	Name          string
	Width, Height int
	// Frame is the index of the animation frame the image shows; 0 for other
	// operations, a video among them.
	Frame int
	// TimeMS is the position of the image in the source, in milliseconds: the
	// time a video was sampled at, or the sum of the display delays of the
	// frames before it in an animation (a stored delay below 20 ms counts as
	// 100 ms, as browsers display it). It is 0 for other operations.
	TimeMS int64
}

const (
	defaultQuality   = 90
	defaultMaxEdge   = 2576
	defaultMaxArea   = 3_750_000
	defaultMaxFrames = 16
	maxMaxFrames     = 64
)

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
	// region is the part of each frame the output shows, cut before the
	// output is sized. It belongs to the encode call, not to a target:
	// Source.encode gives every target the first one's.
	region region
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
		return fmt.Errorf("region corner %d,%d is negative", r.x, r.y)
	case r.width < 1 || r.height < 1:
		return fmt.Errorf("region %dx%d has a side below 1 pixel", r.width, r.height)
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
		return fmt.Errorf("region %dx%d at %d,%d is outside the %dx%d frame", r.width, r.height, r.x, r.y, w, h)
	}
	return nil
}

// subject returns the size the output is fitted to: the region's, or w x h,
// the size of the frame, when there is none.
func (e encoding) subject(w, h int) (int, int) {
	if e.region.zero() {
		return w, h
	}
	return e.region.width, e.region.height
}

// anyRegion reports whether any of encs has a region.
func anyRegion(encs []encoding) bool {
	return slices.ContainsFunc(encs, func(e encoding) bool { return !e.region.zero() })
}

func (e Encoding) resolve() (encoding, error) {
	out := encoding{
		format: e.Format, quality: e.Quality, maxEdge: e.MaxEdge, maxArea: e.MaxArea,
	}
	if out.format == "" {
		out.format = JPEG
	}
	ext, ok := outputFormats[out.format]
	if !ok {
		return encoding{}, fmt.Errorf("unknown format %q", e.Format)
	}
	out.ext = ext
	var err error
	if out.quality, err = resolveInt("quality", e.Quality, defaultQuality, 1, 100); err != nil {
		return encoding{}, err
	}
	if out.maxEdge, err = resolveInt("max edge", e.MaxEdge, defaultMaxEdge, 16, 16384); err != nil {
		return encoding{}, err
	}
	if out.maxArea, err = resolveInt("max area", e.MaxArea, defaultMaxArea, 256, 100_000_000); err != nil {
		return encoding{}, err
	}
	return out, nil
}

// resolveFrames returns maxFrames with the default applied, or an error when
// it is outside 1..64.
func resolveFrames(maxFrames int) (int, error) {
	return resolveInt("max frames", maxFrames, defaultMaxFrames, 1, maxMaxFrames)
}

// resolveInt returns def for zero and v when it lies within lo..hi.
func resolveInt(name string, v, def, lo, hi int) (int, error) {
	if v == 0 {
		return def, nil
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%s %d is outside %d..%d", name, v, lo, hi)
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

// spec is the internal description of one operation: the media types it
// accepts, the decoder each is read with, the tools it runs and the
// ImageMagick coders it uses besides its input decoders.
type spec struct {
	inputs []acceptedInput
	tools  []string
	// reads and writes name the ImageMagick coders the operation reads and
	// writes itself besides its input decoders: its output formats, the
	// "info" coder its encode reports through, and the intermediates it passes
	// between tools.
	reads, writes []string
}

type acceptedInput struct {
	mediaType string
	decoder   decoder
}

func (s spec) decoderFor(mediaType string) (decoder, error) {
	for _, in := range s.inputs {
		if in.mediaType == mediaType {
			return in.decoder, nil
		}
	}
	return decoder{}, fmt.Errorf("%w: %s", ErrUnsupportedType, mediaType)
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
		return fmt.Errorf("output directory: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("output directory %s is not empty", dir)
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
