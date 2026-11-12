package media

import (
	"context"
)

// The helpers below make one Open, one Encode of a single target and Close,
// and add nothing of their own: every check and every error is the library's.
// The files are named still.<ext> and frame-NNN.<ext>, which the golden argv
// files record.

// encodeSel opens path, makes one Encode of sel into dir and closes the
// Source, naming the files as name says. The Usage covers both calls, also when
// one fails.
func (p *Processor) encodeSel(ctx context.Context, path string, sel Select, enc Encoding, dir string, name func(n int, ext string) string) ([]Image, Usage, error) {
	src, usage, err := p.Open(ctx, path, OpenOptions{})
	if err != nil {
		return nil, usage, err
	}
	defer func() { _ = src.Close() }()
	images, used, err := src.encode(ctx, dir, sel, []Encoding{enc}, name)
	usage.CPU += used.CPU
	usage.MaxRSS = max(usage.MaxRSS, used.MaxRSS)
	return images, usage, err
}

// encodeFirst encodes the first image or frame of path.
func (p *Processor) encodeFirst(ctx context.Context, path string, enc Encoding, dir string) (Image, Usage, error) {
	images, usage, err := p.encodeSel(ctx, path, Select{}, enc, dir, func(_ int, ext string) string { return "still" + ext })
	if err != nil {
		return Image{}, usage, err
	}
	return images[0], usage, nil
}

// encodeEven encodes n evenly spaced frames of path.
func (p *Processor) encodeEven(ctx context.Context, path string, enc Encoding, n int, dir string) ([]Image, Usage, error) {
	return p.encodeSel(ctx, path, Select{Frames: Even(n)}, enc, dir, frameName)
}

// encodeTimes encodes the frames of path at timesMS.
func (p *Processor) encodeTimes(ctx context.Context, path string, enc Encoding, timesMS []int64, dir string) ([]Image, Usage, error) {
	return p.encodeSel(ctx, path, Select{Frames: Times(timesMS...)}, enc, dir, frameName)
}

// encodeRun encodes count consecutive frames of path from startMS.
func (p *Processor) encodeRun(ctx context.Context, path string, enc Encoding, startMS int64, count int, dir string) ([]Image, Usage, error) {
	return p.encodeSel(ctx, path, Select{Frames: Run(startMS, count)}, enc, dir, frameName)
}

// encodeIndices encodes the frames of path at indices.
func (p *Processor) encodeIndices(ctx context.Context, path string, enc Encoding, indices []int, dir string) ([]Image, Usage, error) {
	return p.encodeSel(ctx, path, Select{Frames: Indices(indices...)}, enc, dir, frameName)
}

// infoOf opens path and returns its Info.
func (p *Processor) infoOf(ctx context.Context, path string) (Info, Usage, error) {
	src, usage, err := p.Open(ctx, path, OpenOptions{})
	if err != nil {
		return Info{}, usage, err
	}
	defer func() { _ = src.Close() }()
	return src.Info(), usage, nil
}
