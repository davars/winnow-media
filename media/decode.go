package media

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// exifOrientations are the ImageMagick names of the EXIF orientations 1 to 8.
var exifOrientations = [...]string{
	"", "TopLeft", "TopRight", "BottomRight", "BottomLeft",
	"LeftTop", "RightTop", "RightBottom", "LeftBottom",
}

// orientationName returns the ImageMagick name of EXIF orientation n, "" for
// 0, and an error for any other number outside 1 to 8.
func orientationName(n int) (string, error) {
	if n < 0 || n >= len(exifOrientations) {
		return "", invalid("orientation %d is outside 1..8", n)
	}
	return exifOrientations[n], nil
}

// orientationNumber is the inverse of orientationName; "" and "Undefined" are
// 0.
func orientationNumber(name string) (int, error) {
	if name == "" || name == "Undefined" {
		return 0, nil
	}
	if i := slices.Index(exifOrientations[:], name); i > 0 {
		return i, nil
	}
	return 0, fmt.Errorf("unknown orientation %q", name)
}

// decodeMargin is how many times the largest encoding a reduced decode of a
// JPEG covers in each dimension.
const decodeMargin = 2

// route is the decoder a file goes through.
type route int

const (
	// routeStill is ImageMagick, fused with the encode: nothing is
	// materialised.
	routeStill route = iota
	// routeGIF is the Go compositor.
	routeGIF
	// routeWebP is the webp-frames tool, for a WebP with the animation flag.
	routeWebP
	// routeVideo is ffprobe and ffmpeg, for a video.
	routeVideo
)

// openRequest is what a Source needs to read the headers of an input, or to
// decode the samples of a video.
type openRequest struct {
	// path names the input to the tools. file is the descriptor the Go side
	// reads the input from and the one the tools' path reopens.
	path string
	file *os.File
	// dec is the coder, or demuxer, the input is pinned to.
	dec decoder
	// scenes are the headers the gate already read, for a file it named by
	// ImageMagick's detection; readSource reads none then.
	scenes []sceneHeader
	// scratch is the directory for decoded frames.
	scratch string
	// video, when set, makes the input a video, read with dec.
	video *videoRequest
}

// input is where the Go side reads the request's input from.
func (r openRequest) input() input { return input{f: r.file} }

// headerRead is what the header read of a file decided.
type headerRead struct {
	route  route
	ref    string // the ImageMagick input reference, without a scene
	scenes []sceneHeader
	layout animationLayout
}

// readSource reads the headers of the image file of req, routes it and applies
// the caps, before any pixel is decoded. The headers of every image of a still
// are read, and the profile of the first only: the others are completed when
// they are encoded.
func (e toolEnv) readSource(ctx context.Context, req openRequest) (headerRead, error) {
	coder := req.dec.name
	var h headerRead
	switch {
	case coder == "gif":
		h.route = routeGIF
	case coder == "webp" && webpAnimated(req.input()):
		h.route = routeWebP
	}
	selector, format := "", sceneHeaderFormat
	if h.route == routeStill {
		selector, format = stillScenes, gateHeaderFormat
	}
	var err error
	h.ref = coder + ":" + req.path
	if req.scenes != nil {
		h.scenes = req.scenes
	} else if h.scenes, err = identifyHeadersAs(ctx, e, h.ref+selector, true, format); err != nil {
		return headerRead{}, err
	}
	if h.route == routeStill {
		if err := checkStillImages(h.scenes); err != nil {
			return headerRead{}, err
		}
	}
	if h.route != routeStill {
		// Frames of an animation are never decoded at a reduced size, so the
		// header sizes are the decoded sizes.
		if err := checkSceneHeaders(h.scenes, true, e.caps); err != nil {
			return headerRead{}, err
		}
	} else if first := h.scenes[0]; !reducible(first.format) {
		// Nothing reduces this decode, so the cap is known now. It must run
		// before completeProfile, which can decode the whole image.
		stored := [2]int{first.width, first.height}
		if err := checkStillSize(stored, stored, e.caps); err != nil {
			return headerRead{}, err
		}
	}
	profiled, profileRef := h.scenes, h.ref+selector
	if h.route == routeStill {
		profiled, profileRef = h.scenes[:1], h.ref+"[0]"
	}
	if err := completeProfile(ctx, e, profileRef, req.input(), profiled); err != nil {
		return headerRead{}, err
	}
	laid := h.scenes
	if h.route == routeStill {
		laid = laid[:1]
	}
	if h.layout, err = layoutOf(laid); err != nil {
		return headerRead{}, err
	}
	return h, nil
}

// srcFrame is a frame of a source that was decoded onto a full canvas.
type srcFrame struct {
	path        string
	profile     int
	soft        bool
	sourceFrame int
}

// frameSource is a file whose frames have been selected for encoding: the first
// image of a still, the frames of a GIF or animated WebP composited onto
// their canvas, or the samples of a video. It is not safe for concurrent use.
type frameSource struct {
	env    toolEnv
	route  route
	layout animationLayout
	// selected are the source frame indices the source holds.
	selected []int
	// orientation is the ImageMagick orientation name applied to every frame:
	// the override when there is one and, for composited frames, which carry
	// no tag of their own, the file's. "" leaves a still's own tag to
	// -auto-orient.
	orientation string
	// destination is the sRGB profile frames convert to; "" means the
	// package's own, written into dir when first needed.
	destination string
	dir         string
	placed      []string

	// frames are the decoded frames of a composited source; nil for a still,
	// and for a streaming source before its frames are encoded.
	frames []srcFrame
	req    composeRequest
	stream bool

	// ref is the decode reference of a still; soft is whether it is
	// palette-based or declares alpha, jpeg whether the coder is JPEG, and
	// stored its size as stored.
	ref    string
	soft   bool
	jpeg   bool
	stored [2]int
	// upright is the size of the canvas after orientation, from the headers.
	upright [2]int
	// unsettled is set for a still whose orientation the header read does not
	// settle: it reports none and the library does not turn the picture
	// itself. The upright size is then known only from the run that decodes
	// it.
	unsettled bool
	// video is set for a video, whose frames are the samples decoded by the
	// first encode.
	video *videoSource
	// softRef is the identify input whose scene softScene decides soft by a
	// full read, which is made when an encode first resizes; "" when the
	// -ping read decided it.
	softRef   string
	softScene int
	// keep, when set, makes the next still encode also write the upright,
	// full-size decode it reads (see keepPlan).
	keep *keepPlan
}

// initStill makes s the source of image i of the still hdr describes, which
// override, when not empty, gives the orientation of.
func (s *frameSource) initStill(req openRequest, hdr headerRead, i int, override string) error {
	sc := hdr.scenes[i]
	if sc.width < 1 || sc.height < 1 {
		return fmt.Errorf("input reports size %dx%d", sc.width, sc.height)
	}
	layout, err := layoutOf(hdr.scenes[i : i+1])
	if err != nil {
		return err
	}
	s.layout = layout
	tag := sc.orientation
	s.layout.uw, s.layout.uh = uprightSize(sc.width, sc.height, cmp.Or(override, tag))
	s.upright = [2]int{s.layout.uw, s.layout.uh}
	s.ref, s.soft, s.jpeg = hdr.ref+"["+strconv.Itoa(i)+"]", sc.soft, reducible(sc.format)
	s.stored = [2]int{sc.width, sc.height}
	s.unsettled = cmp.Or(override, tag) == "" || cmp.Or(override, tag) == "Undefined"
	s.unsettled = s.unsettled && !libraryTurnsPicture[sc.format]
	if fullReadFormats[sc.format] {
		s.softRef, s.softScene = hdr.ref, i
	}
	return nil
}

// mapping returns the Frame of each source frame index of s.
func (s *frameSource) mapping(indices []int) []Frame {
	if s.video == nil {
		return mappingOf(indices, s.layout.infos)
	}
	frames := make([]Frame, len(indices))
	for i, idx := range indices {
		frames[i].Time = time.Duration(s.video.times[idx]) * time.Millisecond
	}
	return frames
}

// mappingOf returns the Frame of each source frame index, with the time of
// each given the delays of every frame of the file; infos is nil for a still.
func mappingOf(indices []int, infos []frameInfo) []Frame {
	at := timestamps(frameDelays(infos))
	frames := make([]Frame, len(indices))
	for i, idx := range indices {
		frames[i] = Frame{Index: idx}
		if idx < len(at) {
			frames[i].Time = time.Duration(at[idx]) * time.Millisecond
		}
	}
	return frames
}

// Close removes the source's decoded frames. The images it encoded are not
// affected.
func (s *frameSource) Close() {
	removeFiles(s.placed)
	removeFiles(s.framePaths())
}

func (s *frameSource) framePaths() []string {
	paths := make([]string, len(s.frames))
	for i, f := range s.frames {
		paths[i] = f.path
	}
	return paths
}

// srgb returns the path of the sRGB profile frames convert to.
func (s *frameSource) srgb() (string, error) {
	if s.destination != "" {
		return s.destination, nil
	}
	path, err := placeSRGB(s.dir)
	if err != nil {
		return "", err
	}
	s.destination = path
	s.placed = append(s.placed, path)
	return path, nil
}

// framePixels is the pixel count of each decoded frame.
func (s *frameSource) framePixels() int {
	if s.video != nil {
		return s.video.plan.fw * s.video.plan.fh
	}
	return s.layout.cw * s.layout.ch
}

// sourceOf is what the encode of enc needs to know about the frames.
func (s *frameSource) sourceOf(r region, enc encoding) encodeSource {
	if s.video != nil {
		return s.video.plan.source(enc)
	}
	return s.cropped(encodeSource{orientation: s.orientation}, r, enc)
}

// libraryTurnsPicture are the ImageMagick coders whose decoder applies the
// file's orientation itself, so the header read already reports the upright
// size. Membership is by test: TestRegionEdgesAreThoseOfTheUprightFrame proves HEIC.
var libraryTurnsPicture = map[string]bool{"HEIC": true}

// exactSize is the size of an output of enc that shows the region r.
func exactSize(r region, enc encoding) (w, h int) {
	w, h, _ = fitSize(r.width, r.height, enc.maxEdge, enc.maxArea)
	return w, h
}

// cropped returns src made to cut the region r, and to size the cut exactly,
// unless r is the zero region.
func (s *frameSource) cropped(src encodeSource, r region, enc encoding) encodeSource {
	if r.zero() {
		return src
	}
	src.crop = r
	if !s.unsettled {
		src.upright = s.upright
	}
	src.width, src.height = exactSize(r, enc)
	return src
}

// checkRegion returns an error if r is not inside the frame the source shows.
// Where the header read does not settle the orientation only the shape is
// checked; the run that decodes the picture reports its size, and the region
// is checked against that.
func (s *frameSource) checkRegion(r region) error {
	if s.unsettled {
		return r.shape()
	}
	return r.check(s.upright[0], s.upright[1])
}

// shown returns the region of the frame an output of r shows: r, or the whole
// upright frame for the zero region.
func (s *frameSource) shown(r region) Region {
	if r.zero() {
		return Region{Width: s.upright[0], Height: s.upright[1]}
	}
	return Region{X: r.x, Y: r.y, Width: r.width, Height: r.height}
}

// positions returns every frame of the source, in order.
func (s *frameSource) positions() []int {
	n := len(s.selected)
	return selectFrames(n, n)
}

// frameOf returns the encodeFrame of a decoded frame.
func (s *frameSource) frameOf(f srcFrame, r region, enc encoding) (encodeFrame, error) {
	out := encodeFrame{input: "pam:" + f.path, delayCS: -1, filter: filterFor(f.soft), fits: s.fits(r, enc)}
	if f.profile > 0 {
		srgb, err := s.srgb()
		if err != nil {
			return encodeFrame{}, err
		}
		out.convertTo = srgb
		out.assign = filepath.Join(s.dir, composedProfileName(f.profile))
	}
	return out, nil
}

// framesAt returns the encodeFrames of the frames at positions, each with the
// delay the original gives it when delays is set.
func (s *frameSource) framesAt(positions []int, r region, enc encoding, delays bool) ([]encodeFrame, error) {
	out := make([]encodeFrame, len(positions))
	for i, p := range positions {
		f, err := s.frameOf(s.frames[p], r, enc)
		if err != nil {
			return nil, err
		}
		if delays {
			f.delayCS = s.layout.infos[s.frames[p].sourceFrame].delayCS
		}
		out[i] = f
	}
	return out, nil
}

// stillFrame returns the encodeFrame that reads the still.
func (s *frameSource) stillFrame(r region, enc encoding) (encodeFrame, error) {
	f := encodeFrame{input: s.ref, filter: filterFor(s.soft), fits: s.fits(r, enc)}
	if s.layout.infos[0].hasICC {
		srgb, err := s.srgb()
		if err != nil {
			return encodeFrame{}, err
		}
		f.convertTo = srgb
	}
	return f, nil
}

// fits reports whether the source is already within the bounds of enc. The
// answer does not depend on the orientation: both bounds are symmetric in
// the two sides.
func (s *frameSource) fits(r region, enc encoding) bool {
	w, h := r.subject(s.upright[0], s.upright[1])
	_, _, resize := fitSize(w, h, enc.maxEdge, enc.maxArea)
	return !resize
}

// reducible reports whether a decode of the ImageMagick coder format may be
// reduced at decode time. It is the one predicate for the header check and for
// choosing a reduction; a format for which it holds is still confirmed with
// the decoder (see decodePlan).
//
// A WebP is not reducible: ImageMagick decodes it whole, so it is capped at
// its stored size at the header, like any format that cannot be reduced.
func reducible(format string) bool { return format == "JPEG" }

// reducedEighths decides whether a still is decoded at a reduced size for
// encodes at encs. If so it returns n, 1 to 7, such that the decoded frame
// is n eighths of the stored size, and the exact size of each output; else 0
// and nil. It is the one place the reduction is chosen: the read option
// and the cap on the decoded size both come from it.
//
// The decoded size is the smallest eighth of the stored size that covers
// decodeMargin times every output in both dimensions. The exact output
// sizes are computed here, from the size and orientation the header gives,
// because ImageMagick would round each side of the reduced frame separately.
func (s *frameSource) reducedEighths(encs []encoding) (n int, sizes [][2]int) {
	if !s.jpeg {
		return 0, nil
	}
	sizes = s.outputSizes(encs)
	if sizes == nil {
		return 0, nil
	}
	n = 1
	sw, sh := s.stored[0], s.stored[1]
	for _, t := range s.storedSizes(sizes) {
		n = max(n, ceilDiv(8*decodeMargin*t[0], sw), ceilDiv(8*decodeMargin*t[1], sh))
	}
	if n >= 8 {
		return 0, nil
	}
	return n, sizes
}

// decodedSize is the size of the frame decoding the still yields for encodes
// at encs, in the stored orientation.
func (s *frameSource) decodedSize(encs []encoding) [2]int {
	n, _ := s.reducedEighths(encs)
	if n == 0 {
		return s.stored
	}
	return [2]int{ceilDiv(s.stored[0]*n, 8), ceilDiv(s.stored[1]*n, 8)}
}

// reduction returns the read option that makes the decode of a JPEG still
// the size decodedSize gives, and the exact size of each output; nil, nil
// when the decode is not reduced.
func (s *frameSource) reduction(encs []encoding) (readOps []string, sizes [][2]int) {
	n, sizes := s.reducedEighths(encs)
	if n == 0 {
		return nil, nil
	}
	d := s.decodedSize(encs)
	return []string{"-define", fmt.Sprintf("jpeg:size=%dx%d", d[0], d[1])}, sizes
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// decodePlan returns the read option, the exact output sizes and the size of
// the decoded frame for encodes at encs. The reduction reducedEighths chooses
// is confirmed by a header read made with the option, which reports the size
// the decoder really produces: a decoder may ignore the size option (a lossless JPEG
// does). When the answer is not decodedSize the reduction is dropped and the
// still is judged at its stored size.
func (s *frameSource) decodePlan(ctx context.Context, encs []encoding) (readOps []string, sizes [][2]int, decoded [2]int, err error) {
	readOps, sizes = s.reduction(encs)
	if readOps == nil {
		return nil, nil, s.stored, nil
	}
	got, err := s.pingDecoded(ctx, readOps)
	if err != nil {
		return nil, nil, [2]int{}, err
	}
	if got != s.decodedSize(encs) {
		return nil, nil, s.stored, nil
	}
	return readOps, sizes, got, nil
}

// pingDecoded returns the size, in the stored orientation, that reading the
// still with readOps decodes to, from the header alone.
func (s *frameSource) pingDecoded(ctx context.Context, readOps []string) ([2]int, error) {
	args := append([]string{"identify", "-ping"}, readOps...)
	args = append(args, "-format", "%w %h", s.ref)
	res, err := s.env.runMagick(ctx, args...)
	if err != nil {
		return [2]int{}, fmt.Errorf("reading the reduced size: %w", err)
	}
	var d [2]int
	if _, err := fmt.Sscanf(string(res.Stdout), "%d %d", &d[0], &d[1]); err != nil {
		return [2]int{}, fmt.Errorf("reading the reduced size from %q: %w", res.Stdout, err)
	}
	return d, nil
}

// outputSizes returns the exact size of each output of encs, or nil when any
// of them needs no resize, which no reduced decode serves.
func (s *frameSource) outputSizes(encs []encoding) [][2]int {
	sizes := make([][2]int, len(encs))
	for i, enc := range encs {
		tw, th, resize := fitSize(s.upright[0], s.upright[1], enc.maxEdge, enc.maxArea)
		if !resize {
			return nil
		}
		sizes[i] = [2]int{tw, th}
	}
	return sizes
}

// storedSizes turns upright output sizes into the sides of the stored image
// they bound.
func (s *frameSource) storedSizes(sizes [][2]int) [][2]int {
	if s.upright == s.stored {
		return sizes
	}
	swapped := make([][2]int, len(sizes))
	for i, t := range sizes {
		swapped[i] = [2]int{t[1], t[0]}
	}
	return swapped
}

// encodeStills runs the still encode for the encodings and returns what each
// reported: one ImageMagick run for all of them.
func (s *frameSource) encodeStills(ctx context.Context, r region, encs []encoding, paths []string) ([]encodedFrame, error) {
	// A frame is cut before it is sized, so no reduced decode serves a region.
	var readOps []string
	var sizes [][2]int
	decoded := s.stored
	if r.zero() && s.keep == nil {
		var err error
		if readOps, sizes, decoded, err = s.decodePlan(ctx, encs); err != nil {
			return nil, err
		}
	}
	if err := checkStillSize(s.stored, decoded, s.env.caps); err != nil {
		return nil, err
	}
	if s.softRef != "" && slices.ContainsFunc(encs, func(e encoding) bool { return !s.fits(r, e) }) {
		soft, err := fullSoftAt(ctx, s.env, s.softRef, s.softScene)
		if err != nil {
			return nil, err
		}
		s.soft, s.softRef = soft, ""
	}
	src := s.cropped(encodeSource{orientation: s.orientation}, r, encs[0])
	targets := make([]fusedTarget, len(encs))
	for i, enc := range encs {
		f, err := s.stillFrame(r, enc)
		if err != nil {
			return nil, err
		}
		targets[i] = fusedTarget{frame: f, enc: enc, path: paths[i]}
		switch {
		case !r.zero():
			targets[i].width, targets[i].height = exactSize(r, enc)
		case sizes != nil:
			targets[i].width, targets[i].height = sizes[i][0], sizes[i][1]
		}
	}
	if s.keep != nil {
		if targets[0].frame.convertTo == "" {
			s.keep.icc = ""
		}
		targets[0].frame.prelude = s.keep.prelude(s.orientation)
		src.orientation = ""
	}
	var observed [2]int
	src.reportUpright = s.unsettled
	chk := encodeCheck{
		frames: len(encs), outputs: paths, crop: src.crop, upright: src.upright,
		reportUpright: s.unsettled, uprightOut: &observed,
	}
	if s.keep != nil {
		chk.keepOut = &s.keep.line
	}
	for _, t := range targets {
		chk.bounds = append(chk.bounds, boundOf(t.enc, t.source(src)))
	}
	res, err := s.env.runEncode(ctx, fusedArgs(readOps, src, targets), chk)
	if err != nil {
		return nil, err
	}
	s.settle(observed, decoded)
	return res, nil
}

// settle makes the upright size a run reported the size of a still whose
// orientation the header read left open. decoded is the size the run decoded
// at, in the stored orientation: a reduced decode reports its own size, which
// says only whether the picture was turned.
func (s *frameSource) settle(observed, decoded [2]int) {
	if !s.unsettled || observed == ([2]int{}) {
		return
	}
	switch {
	case decoded == s.stored:
		s.upright = observed
	case observed == decoded:
		s.upright = s.stored
	case observed == [2]int{decoded[1], decoded[0]}:
		s.upright = [2]int{s.stored[1], s.stored[0]}
	default:
		return
	}
	s.layout.uw, s.layout.uh = s.upright[0], s.upright[1]
	s.unsettled = false
}

// encodeAnimation encodes the frames at pos to one file at path and returns
// its size and the positions of the frames its scenes show.
//
// Every frame is encoded, each for displayDelayCS of its stored delay. The
// WebP writer merges a frame into the scene before it when the two differ by
// less than its lossy tolerance, an identical frame included, and no option
// prevents it, so the file may hold fewer scenes than frames were given. The
// returned positions are those the scenes show, found by reading the scene
// delays back (see sceneStarts).
func (s *frameSource) encodeAnimation(ctx context.Context, r region, enc encoding, pos []int, path string) (w, h int, shown []int, err error) {
	src := s.sourceOf(r, enc)
	frames, err := s.framesAt(pos, r, enc, true)
	if err != nil {
		return 0, 0, nil, err
	}
	kind := outputAnimation
	if len(pos) == 1 {
		kind = outputStill
	}
	delays := make([]int, len(frames))
	for i := range frames {
		frames[i].delayCS = displayDelayCS(frames[i].delayCS)
		delays[i] = frames[i].delayCS
	}
	res, err := s.env.runEncode(ctx, encodeArgs(frames, src, enc, encodeOutput{kind: kind, path: path}),
		encodeCheck{frames: len(pos), bounds: []encodeBound{boundOf(enc, src)}, outputs: []string{path}, crop: src.crop, upright: src.upright})
	if err != nil {
		return 0, 0, nil, fmt.Errorf("encoding animation: %w", err)
	}
	if len(pos) == 1 {
		return res[0].width, res[0].height, pos, nil
	}
	written, w, h, err := s.env.identifyScenes(ctx, path)
	if err != nil {
		return 0, 0, nil, err
	}
	starts, err := sceneStarts(delays, written)
	if err != nil {
		return 0, 0, nil, err
	}
	shown = make([]int, len(starts))
	for i, k := range starts {
		shown[i] = pos[k]
	}
	return w, h, shown, nil
}

func totalCS(delays []int) int {
	var total int
	for _, d := range delays {
		total += d
	}
	return total
}

// sceneStarts returns, for each scene of a written animation, the index of
// the frame it shows: the first of the frames the writer merged into it.
// given are the delays asked for each frame, all positive, and written the
// delay of each scene as the file states it, both in centiseconds. A scene
// begins where a frame does and lasts the sum of the delays of the frames it
// holds, so it fails when a scene is empty, starts or ends between frames, or
// the file's total differs from the frames'. A file of one scene may be a
// still, which states no delay, or a one-frame animation, which states the
// total; either shows the first frame.
func sceneStarts(given, written []int) ([]int, error) {
	if len(written) == 1 {
		if total := totalCS(given); written[0] != 0 && written[0] != total {
			return nil, fmt.Errorf("encoded artifact scene lasts %d cs, the frames give %d", written[0], total)
		}
		return []int{0}, nil
	}
	starts := make([]int, 0, len(written))
	next := 0
	for i, d := range written {
		if d <= 0 {
			return nil, fmt.Errorf("encoded artifact scene %d lasts %d cs", i, d)
		}
		if next >= len(given) {
			return nil, fmt.Errorf("encoded artifact holds %d scenes for %d frames", len(written), len(given))
		}
		starts = append(starts, next)
		held := 0
		for next < len(given) && held < d {
			held += given[next]
			next++
		}
		if held != d {
			return nil, fmt.Errorf("encoded artifact scene %d lasts %d cs, the frames from %d give %d", i, d, starts[i], held)
		}
	}
	if next != len(given) {
		return nil, fmt.Errorf("encoded artifact ends before frame %d of %d", next, len(given))
	}
	return starts, nil
}

// encodeFiles writes one file per frame of the region r of each frame, in the
// format of enc. name is the name of the n-th file, numbered from 0.
func (s *frameSource) encodeFiles(ctx context.Context, r region, enc encoding, dir string, name func(n int) string) ([]Image, error) {
	if err := s.checkRegion(r); err != nil {
		return nil, err
	}
	if s.video != nil {
		if err := s.decodeVideo(ctx, r, []encoding{enc}); err != nil {
			return nil, err
		}
	}
	if s.route == routeStill {
		file := name(0)
		res, err := s.encodeStills(ctx, r, []encoding{enc}, []string{filepath.Join(dir, file)})
		if err != nil {
			return nil, fmt.Errorf("encoding still: %w", err)
		}
		return []Image{{Name: file, Width: res[0].width, Height: res[0].height, Frames: []Frame{{}}, Region: s.shown(r)}}, nil
	}
	var images []Image
	sel := s.mapping(s.selected)
	encoder := &sampleEncoder{
		env: s.env, src: s.sourceOf(r, enc),
		enc: enc, dir: dir, framePixels: s.framePixels(), name: name,
		keep: func(i, n, w, h int) {
			images = append(images, Image{
				Name: name(n), Width: w, Height: h,
				Frames: []Frame{sel[i]}, Region: s.shown(r),
			})
		},
	}
	if !s.stream {
		pos := s.positions()
		frames, err := s.framesAt(pos, r, enc, false)
		if err != nil {
			return nil, err
		}
		if err := encoder.encodeSamples(ctx, frames); err != nil {
			return nil, err
		}
		return images, nil
	}

	// The compositor writes each selected frame as an uncompressed PAM file
	// and waits for each batch to be consumed before writing more, so the
	// directory holds about one batch; Open, which keeps every frame until
	// Close, sets inFlight to all of them instead. ImageMagick encodes a batch
	// per run, of at most encodeBatchPixels input pixels. The GIF compositor
	// first checks the block structure (frame count, every frame inside the
	// logical screen), composites as -coalesce does and stops after the last
	// selected frame. The animated WebP compositor runs libwebp's animation
	// decoder, which composites one frame at a time; it is told the canvas
	// and frame count of the header read and refuses a file that differs.
	var pending []encodeFrame
	var pendingPaths []string
	flush := func() error {
		defer removeFiles(pendingPaths)
		err := encoder.encode(ctx, pending)
		pending, pendingPaths = nil, nil
		return err
	}
	req := s.req
	req.inFlight = encoder.batchSize()
	ordinal := 0
	err := s.env.compose(ctx, req, func(f composedFrame) error {
		if ordinal >= len(s.selected) {
			return errors.New("the compositor wrote more frames than selected")
		}
		sf := srcFrame{path: f.path, profile: f.profile, soft: f.soft, sourceFrame: s.selected[ordinal]}
		ordinal++
		if f.profile > 0 {
			s.placed = append(s.placed, filepath.Join(s.dir, composedProfileName(f.profile)))
		}
		ef, err := s.frameOf(sf, r, enc)
		if err != nil {
			return err
		}
		pending = append(pending, ef)
		pendingPaths = append(pendingPaths, f.path)
		if len(pending) >= encoder.batchSize() {
			return flush()
		}
		return nil
	})
	if err != nil {
		removeFiles(pendingPaths)
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if encoder.seen != len(s.selected) {
		return nil, fmt.Errorf("composited %d of %d frames", encoder.seen, len(s.selected))
	}
	return images, nil
}
