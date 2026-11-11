package media

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Hints carry what Env.Open cannot learn from the bytes of a file.
type Hints struct {
	// MediaType keys the coder fallback for a file ImageMagick cannot
	// identify without an extension: "image/x-tga", "image/vnd.microsoft.icon",
	// "image/x-win-bitmap" or "image/g3fax". "" and any other type give no
	// fallback.
	MediaType string
	// Orientation, 1 to 8 as in EXIF, replaces the orientation the file
	// carries; 0 leaves the file's own. It is an error for a video.
	Orientation int
	// Frames is the number of frames selected from an animation, evenly
	// spaced from the first; 0 selects every frame. It must not be negative.
	// For a video it is the number of samples, taken at the centres of equal
	// intervals of the duration (fewer when the video has fewer frames); 0
	// takes 15, and it must be at most 64.
	Frames int
	// Still selects the first image of the file alone, and excludes Frames.
	// Without it a file of several images that is neither a GIF nor a WebP is
	// refused with ErrUnsupportedType. A video has no still: the hint is an
	// error for one.
	Still bool
}

// Artifact is one encoded output of a Source.
type Artifact struct {
	// Path is the staged file; Cleanup removes it and is the caller's to call
	// once.
	Path    string
	Cleanup func()
	// Width and Height are the size of the encoded image, as the encode
	// reported it.
	Width, Height int
	// Frames maps each frame of the artifact, by position, to the frame of
	// the source file it shows.
	Frames []Frame
}

// exifOrientations are the ImageMagick names of the EXIF orientations 1 to 8.
var exifOrientations = [...]string{
	"", "TopLeft", "TopRight", "BottomRight", "BottomLeft",
	"LeftTop", "RightTop", "RightBottom", "LeftBottom",
}

// orientationName returns the ImageMagick name of EXIF orientation n, "" for
// 0, and an error for any other number outside 1 to 8.
func orientationName(n int) (string, error) {
	if n < 0 || n >= len(exifOrientations) {
		return "", fmt.Errorf("orientation %d is outside 1..8", n)
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

// decodeMargin is how many times the largest target a reduced decode of a
// JPEG or a still WebP covers in each dimension.
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

// openRequest is what Env.Open and the Processor's operations share.
type openRequest struct {
	path  string
	hints Hints
	// dec pins the coder, and so the type, of the input when pinned is set;
	// otherwise the type comes from the bytes.
	dec    decoder
	pinned bool
	// caps applies the cap on the frames returned. The caps on a still apply
	// whatever caps is: at the header read when nothing can reduce its
	// decode, and otherwise when it is encoded, against the targets.
	caps bool
	// pick chooses the frames of an animation from their count, as indices in
	// ascending order without repeats; nil takes them from hints.
	pick func(sourceFrames int) ([]int, error)
	// scratch is the directory for decoded frames; "" takes Env.Scratch.
	scratch string
	// webpProfileFromTool leaves the profile of a still WebP unread: the
	// caller decodes it with webp-frames, which reports the profile.
	webpProfileFromTool bool
	// stream leaves the frames undecoded until EncodeFrames, which decodes
	// and encodes them in batches so that no more than a batch is on disk.
	stream bool
	// video, when set, makes the input a video, read with dec; for a request
	// that is not pinned, Env.open sets it from the bytes.
	video *videoRequest
}

// headerRead is what the header read of a file decided.
type headerRead struct {
	route  route
	ref    string // the ImageMagick input reference, without a scene
	scenes []sceneHeader
	layout animationLayout
}

// readSource reads the headers of the file of req, routes it and applies the
// caps, before any pixel is decoded.
func (e Env) readSource(ctx context.Context, req openRequest) (headerRead, error) {
	coder := ""
	if req.pinned {
		coder = req.dec.name
	} else {
		kind, err := sniffFile(req.path)
		switch {
		case err == nil:
			switch kind {
			case typeGIF:
				coder = "gif"
			case typeWebP:
				coder = "webp"
			}
		case !errors.Is(err, ErrUnsupportedType):
			return headerRead{}, err
		}
	}
	var h headerRead
	var err error
	switch {
	case coder == "gif":
		h.route = routeGIF
	case coder == "webp" && webpAnimated(req.path):
		h.route = routeWebP
	}
	// A still asked for alone reads the headers of its first image only.
	selector := ""
	if h.route == routeStill && req.hints.Still {
		selector = "[0]"
	}
	if coder != "" {
		h.ref = coder + ":" + req.path
		h.scenes, err = identifyHeaders(ctx, e, h.ref+selector, true)
	} else {
		var ref string
		h.scenes, ref, err = identifyWithFallback(ctx, e, req.path, selector, req.hints.MediaType)
		h.ref = strings.TrimSuffix(ref, selector)
	}
	if err != nil {
		return headerRead{}, err
	}
	if h.route == routeStill && len(h.scenes) > 1 && !req.hints.Still {
		return headerRead{}, fmt.Errorf("%w: %s file of %d images", ErrUnsupportedType, strings.ToLower(h.scenes[0].format), len(h.scenes))
	}
	if h.route != routeStill {
		// Frames of an animation are never decoded at a reduced size, so the
		// header sizes are the decoded sizes.
		if err := checkSceneHeaders(h.scenes, true); err != nil {
			return headerRead{}, err
		}
	} else if first := h.scenes[0]; !reducible(first.format) {
		// Nothing reduces this decode, so the cap is known now. It must run
		// before completeProfile, which can decode the whole image.
		stored := [2]int{first.width, first.height}
		if err := checkStillSize(stored, stored); err != nil {
			return headerRead{}, err
		}
	}
	// A still WebP's profile is reported by the tool that decodes it, so a
	// caller that decodes it does not need the whole-image read that finds it.
	if !req.webpProfileFromTool || h.route != routeStill || h.scenes[0].format != "WEBP" {
		if err := completeProfile(ctx, e, h.ref+selector, req.path, h.scenes); err != nil {
			return headerRead{}, err
		}
	}
	if h.layout, err = layoutOf(h.scenes); err != nil {
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

// Source is a file whose frames have been selected for encoding: the first
// image of a still, the frames of a GIF or animated WebP composited onto
// their canvas, or the samples of a video. It is not safe for concurrent use.
type Source struct {
	env    Env
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
	cleanup     func()
	placed      []string

	// frames are the decoded frames of a composited source; nil for a still,
	// and for a streaming source before EncodeFrames decodes it.
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
	// reduce allows a reduced decode of a JPEG or a still WebP.
	reduce bool
	// video is set for a video, whose frames are the samples decoded by the
	// first encode.
	video *videoSource
	// webp is set for a still WebP, which webp-frames decodes whatever the
	// targets. exif is the name of the orientation ImageMagick applies to it,
	// "" for none, once exifKnown; a hint replaces it. webp-frames applies
	// none.
	webp      bool
	exif      string
	exifKnown bool
	// softRef is the identify input whose first scene decides soft by a full
	// read, which is made when an encode first resizes; "" when the -ping
	// read decided it.
	softRef string
}

// Open reads the headers of the file at path, applies the caps and selects
// its frames, decoding those of a GIF or animated WebP onto a full canvas in
// the directory Env.Scratch provides. The type comes from the file's bytes: a
// GIF is composited by the package and a WebP by the webp-frames tool, whatever
// its frame count (a still WebP is decoded when it is encoded, at a size that
// covers its targets); any other image is decoded by ImageMagick when it is
// encoded. A file of several images that is neither a GIF nor a WebP is
// refused with ErrUnsupportedType unless h.Still is set. An animation over the
// caps is refused with ErrTooLarge before any frame is decoded. A still that
// no decode stage can reduce is refused here if its stored size is over the
// caps, a WebP included, as libwebp's scaling does not bound its memory; one
// that can be reduced is judged by Encode against its targets.
//
// A video is an MP4, QuickTime, WebM or Matroska file, which the bytes name,
// or any other container ffmpeg detects among AVI, ASF, FLV, MPEG program
// and transport streams and raw MPEG video. A file that leads one of those
// containers is probed as a video first, and ImageMagick never reads it
// unless the probe refuses it; any other file that sniff names no type for is
// read as an image first, and probed as a video if ImageMagick cannot read
// it. A file that is neither is refused with ErrUnsupportedType. Open probes
// a video, refuses one over the caps with ErrTooLarge, and chooses h.Frames
// sample times. The samples are decoded by the first encode, at the size that
// encode's targets need, so a video's Frames are final only after it (see
// Source.Frames). It takes h.Frames and no other hint: MediaType is ignored,
// and Still and Orientation are errors.
//
// The caller must Close the Source.
func (e Env) Open(ctx context.Context, path string, h Hints) (*Source, error) {
	return e.open(ctx, openRequest{path: path, hints: h})
}

func (e Env) open(ctx context.Context, req openRequest) (*Source, error) {
	// videoErr is why a file whose leading bytes look like a video container
	// was not opened as one.
	var videoErr error
	if req.video == nil && !req.pinned {
		route, dec := videoRouteOf(req.path)
		vreq := req
		vreq.dec, vreq.pinned, vreq.video = dec, true, &videoRequest{}
		switch route {
		case routeSniffed:
			return e.openVideo(ctx, vreq)
		case routeLooksLike:
			s, err := e.openVideo(ctx, vreq)
			if herr := (hintError{}); err == nil || errors.Is(err, ErrTooLarge) || errors.As(err, &herr) {
				return s, err
			}
			videoErr = err
		}
	}
	if req.video != nil {
		return e.openVideo(ctx, req)
	}
	h := req.hints
	if h.Frames < 0 {
		return nil, fmt.Errorf("frames %d is negative", h.Frames)
	}
	if h.Still && h.Frames > 0 {
		return nil, errors.New("hints Still and Frames exclude each other")
	}
	override, err := orientationName(h.Orientation)
	if err != nil {
		return nil, err
	}
	req.webpProfileFromTool = true
	hdr, err := e.readSource(ctx, req)
	if err != nil {
		if !req.pinned && notImage(req.path, err) {
			if videoErr == nil {
				videoErr = e.probeOtherVideo(ctx, req)
				if videoErr == nil {
					return e.openOtherVideo(ctx, req)
				}
			}
			if errors.Is(videoErr, ErrTooLarge) {
				return nil, videoErr
			}
			return nil, fmt.Errorf("%w: not an image (%v) and not a video (%v)", ErrUnsupportedType, err, videoErr)
		}
		return nil, err
	}
	s := &Source{
		env: e, route: hdr.route, layout: hdr.layout, orientation: override, reduce: true,
		stream: req.stream,
	}
	first := hdr.scenes[0]
	count := len(hdr.layout.infos)
	if hdr.route == routeStill {
		count = 1
		tag := first.orientation
		if first.format == "WEBP" {
			s.webp = true
			s.req = composeRequest{
				path: req.path, external: webpFramesTool, width: first.width, height: first.height,
			}
			// The orientation is read from the file's metadata when it is
			// decoded; until then only a hint says it.
			tag = ""
		}
		s.layout.uw, s.layout.uh = uprightSize(first.width, first.height, cmp.Or(override, tag))
		s.ref, s.soft, s.jpeg = hdr.ref+"[0]", first.soft, reducible(first.format)
		s.stored = [2]int{first.width, first.height}
		if fullReadFormats[first.format] {
			s.softRef = hdr.ref
		}
		if first.width < 1 || first.height < 1 {
			return nil, fmt.Errorf("input reports size %dx%d", first.width, first.height)
		}
	}
	s.upright = [2]int{s.layout.uw, s.layout.uh}
	if s.selected, err = selection(req, count); err != nil {
		return nil, err
	}
	if hdr.route == routeStill && !slices.Equal(s.selected, []int{0}) {
		return nil, fmt.Errorf("selected %d frames of a one-frame file", len(s.selected))
	}
	if req.caps && hdr.route != routeStill {
		if err := checkReturnedPixels(len(s.selected), s.layout.cw, s.layout.ch); err != nil {
			return nil, err
		}
	}

	if s.dir, s.cleanup, err = e.scratchFor(req); err != nil {
		return nil, err
	}
	if hdr.route == routeStill {
		return s, nil
	}

	s.orientation = cmp.Or(override, first.orientation)
	s.req = composeRequest{
		path: req.path, dir: s.dir, selected: s.selected, inFlight: len(s.selected),
		frames: count, width: s.layout.cw, height: s.layout.ch, consumeTimeout: composeTimeout,
	}
	if hdr.route == routeWebP {
		s.req.external = webpFramesTool
	} else {
		s.req.own = make([]bool, count)
		for i, f := range hdr.layout.infos {
			s.req.own[i] = f.hasICC
		}
	}
	if !req.stream {
		if err := s.decode(ctx); err != nil {
			s.Close()
			return nil, fmt.Errorf("compositing selected frames: %w", err)
		}
	}
	return s, nil
}

// scratchFor returns the directory a source decodes into: the request's, or a
// new one from Env.Scratch whose cleanup removes it.
func (e Env) scratchFor(req openRequest) (dir string, cleanup func(), err error) {
	if req.scratch != "" {
		return req.scratch, nil, nil
	}
	if e.Scratch == nil {
		return "", nil, errors.New("Env.Scratch is required to open a source")
	}
	return e.Scratch()
}

// notImage reports whether err, from reading the headers of the file at path,
// leaves open that the file is a video: its bytes name no type, and the
// failure is not a refusal or a cancelled run.
func notImage(path string, err error) bool {
	if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrUnsupportedType) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	_, serr := sniffFile(path)
	return errors.Is(serr, ErrUnsupportedType)
}

// probeOtherVideo returns nil if the file at req.path is a video of one of the
// demuxers ffmpeg detects under detectedFormats, and why it is not otherwise.
func (e Env) probeOtherVideo(ctx context.Context, req openRequest) error {
	_, err := e.probeChecked(ctx, unnamedVideoDecoder, req.path)
	return err
}

// openOtherVideo opens as a video a file probeOtherVideo accepted.
func (e Env) openOtherVideo(ctx context.Context, req openRequest) (*Source, error) {
	req.dec, req.pinned, req.video = unnamedVideoDecoder, true, &videoRequest{}
	return e.openVideo(ctx, req)
}

// selection returns the source frames a request selects of count frames.
func selection(req openRequest, count int) ([]int, error) {
	switch h := req.hints; {
	case req.pick != nil:
		return req.pick(count)
	case h.Still:
		return []int{0}, nil
	case h.Frames == 0:
		all := make([]int, count)
		for i := range all {
			all[i] = i
		}
		return all, nil
	default:
		return selectFrames(frameCount(h.Frames, count, true), count), nil
	}
}

// decode composites the selected frames into the scratch directory.
func (s *Source) decode(ctx context.Context) error {
	err := s.env.composeFrames(ctx, s.req, func(f composedFrame) error {
		if len(s.frames) >= len(s.selected) {
			return errors.New("the compositor wrote more frames than selected")
		}
		if f.profile > 0 {
			s.placed = append(s.placed, filepath.Join(s.dir, composedProfileName(f.profile)))
		}
		s.frames = append(s.frames, srcFrame{
			path: f.path, profile: f.profile, soft: f.soft, sourceFrame: s.selected[len(s.frames)],
		})
		return nil
	})
	if err == nil && len(s.frames) != len(s.selected) {
		err = fmt.Errorf("composited %d of %d frames", len(s.frames), len(s.selected))
	}
	return err
}

// composeFrames runs the compositor of req: the Processor's sandbox, else the
// webp-frames tool through Env.Run for a WebP, else the GIF compositor in
// this process.
func (e Env) composeFrames(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) (err error) {
	switch {
	case e.compose != nil:
		return e.compose(ctx, req, onFrame)
	case req.external != "":
		res, err := e.Run(ctx, req.external, req.externalArgs()...)
		if err != nil {
			return err
		}
		_, err = (&frameLines{dir: req.dir, onFrame: onFrame}).Write(res.Stdout)
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("gif compositor panic: %v", r)
		}
	}()
	return req.job().run(ctx, nil, func(ordinal, profile int) error {
		return onFrame(composedFrame{path: filepath.Join(req.dir, gifSampleName(ordinal)), profile: profile, soft: true})
	})
}

// Frames returns the frames the source holds and when each is shown in the
// original, in order. A video's frames have SourceFrame 0, since sampling
// knows no frame index, and the time each was asked for as SourceTimeMS. They
// are the sample times Open chose until the first encode decodes them, which
// may leave out a time that has no frame; Artifact.Frames is the final word.
func (s *Source) Frames() []Frame {
	return s.mapping(s.selected)
}

// mapping returns the Frame of each source frame index of s.
func (s *Source) mapping(indices []int) []Frame {
	if s.video == nil {
		return mappingOf(indices, s.layout.infos)
	}
	frames := make([]Frame, len(indices))
	for i, idx := range indices {
		frames[i].SourceTimeMS = s.video.times[idx]
	}
	return frames
}

// mappingOf returns the Frame of each source frame index, with the time of
// each given the delays of every frame of the file; infos is nil for a still.
func mappingOf(indices []int, infos []FrameInfo) []Frame {
	at := timestamps(frameDelays(infos))
	frames := make([]Frame, len(indices))
	for i, idx := range indices {
		frames[i] = Frame{SourceFrame: idx}
		if idx < len(at) {
			frames[i].SourceTimeMS = at[idx]
		}
	}
	return frames
}

// Close removes the source's decoded frames. The artifacts it encoded are not
// affected.
func (s *Source) Close() {
	removeFiles(s.placed)
	removeFiles(s.framePaths())
	if s.cleanup != nil {
		s.cleanup()
		s.cleanup = nil
	}
}

func (s *Source) framePaths() []string {
	paths := make([]string, len(s.frames))
	for i, f := range s.frames {
		paths[i] = f.path
	}
	return paths
}

// srgb returns the path of the sRGB profile frames convert to.
func (s *Source) srgb() (string, error) {
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
func (s *Source) framePixels() int {
	if s.video != nil {
		return s.video.plan.fw * s.video.plan.fh
	}
	return s.layout.cw * s.layout.ch
}

// sourceOf is what the encode of enc needs to know about the frames.
func (s *Source) sourceOf(enc encoding) encodeSource {
	if s.video != nil {
		return s.video.plan.source(enc)
	}
	return encodeSource{orientation: s.orientation}
}

// positions returns which of the source's frames a budget of maxFrames
// selects, evenly spaced from the first; all of them for 0.
func (s *Source) positions(maxFrames int) ([]int, error) {
	if maxFrames < 0 {
		return nil, fmt.Errorf("max frames %d is negative", maxFrames)
	}
	n := len(s.selected)
	if maxFrames == 0 || s.route == routeStill {
		return selectFrames(n, n), nil
	}
	return selectFrames(min(maxFrames, n), n), nil
}

// frameOf returns the encodeFrame of a decoded frame.
func (s *Source) frameOf(f srcFrame, enc encoding) (encodeFrame, error) {
	out := encodeFrame{input: "pam:" + f.path, delayCS: -1, filter: filterFor(f.soft), fits: s.fits(enc)}
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
func (s *Source) framesAt(positions []int, enc encoding, delays bool) ([]encodeFrame, error) {
	out := make([]encodeFrame, len(positions))
	for i, p := range positions {
		f, err := s.frameOf(s.frames[p], enc)
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
func (s *Source) stillFrame(enc encoding) (encodeFrame, error) {
	f := encodeFrame{input: s.ref, filter: filterFor(s.soft), fits: s.fits(enc)}
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
func (s *Source) fits(enc encoding) bool {
	_, _, resize := fitSize(s.upright[0], s.upright[1], enc.maxEdge, enc.maxArea)
	return !resize
}

// reducible reports whether a decode of the ImageMagick coder format may be
// reduced at decode time. It is the one predicate for the header check and for
// choosing a reduction; a format for which it holds is still confirmed with
// the decoder (see decodePlan).
//
// A WebP is reduced too, inside libwebp by webp-frames, but is not reducible
// here: libwebp's scaling bounds memory only for lossy images without alpha
// (a 268 MP lossless WebP of 82 KB peaks at 1 GB decoding at 1024; see
// docs/measurements.md), so a WebP is capped at its stored size at the header, like a format that cannot be
// reduced.
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
func (s *Source) reducedEighths(encs []encoding) (n int, sizes [][2]int) {
	if !s.jpeg || !s.reduce {
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
func (s *Source) decodedSize(encs []encoding) [2]int {
	n, _ := s.reducedEighths(encs)
	if n == 0 {
		return s.stored
	}
	return [2]int{ceilDiv(s.stored[0]*n, 8), ceilDiv(s.stored[1]*n, 8)}
}

// reduction returns the read option that makes the decode of a JPEG still
// the size decodedSize gives, and the exact size of each output; nil, nil
// when the decode is not reduced.
func (s *Source) reduction(encs []encoding) (readOps []string, sizes [][2]int) {
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
// the decoder really produces: a decoder may ignore the hint (a lossless JPEG
// does). When the answer is not decodedSize the reduction is dropped and the
// still is judged at its stored size.
func (s *Source) decodePlan(ctx context.Context, encs []encoding) (readOps []string, sizes [][2]int, decoded [2]int, err error) {
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
func (s *Source) pingDecoded(ctx context.Context, readOps []string) ([2]int, error) {
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
func (s *Source) outputSizes(encs []encoding) [][2]int {
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
func (s *Source) storedSizes(sizes [][2]int) [][2]int {
	if s.upright == s.stored {
		return sizes
	}
	swapped := make([][2]int, len(sizes))
	for i, t := range sizes {
		swapped[i] = [2]int{t[1], t[0]}
	}
	return swapped
}

// webpDecodeSize returns the size a still WebP is decoded at for encodes at
// encs: the stored size, or a reduction when every target needs a resize and
// twice the largest is at most 7/8 of the stored size on both sides, the
// smallest step a JPEG reduction takes (a smaller reduction is not worth the
// scaler's different upsampling). libwebp scales to any size, so a reduction
// is the stored aspect at decodeMargin times every output in both dimensions.
//
// The size does not depend on the file's orientation, which is read from the
// decoded file: fitSize is symmetric under swapping width and height, so an
// output's fit in the stored axes is the same either way.
func (s *Source) webpDecodeSize(encs []encoding) [2]int {
	sw, sh := s.stored[0], s.stored[1]
	if !s.reduce {
		return s.stored
	}
	var decoded [2]int
	for _, enc := range encs {
		tw, th, resize := fitSize(sw, sh, enc.maxEdge, enc.maxArea)
		if !resize {
			return s.stored
		}
		tw, th = decodeMargin*tw, decodeMargin*th
		// The side that needs the larger scale fixes it; the other follows.
		w, h := tw, ceilDiv(sh*tw, sw)
		if tw*sh < th*sw {
			w, h = ceilDiv(sw*th, sh), th
		}
		decoded = [2]int{max(decoded[0], w), max(decoded[1], h)}
	}
	if 8*decoded[0] > 7*sw || 8*decoded[1] > 7*sh {
		return s.stored
	}
	return decoded
}

// encodeStills runs the still encode for the targets and returns what each
// reported: one ImageMagick run for all of them, except for a WebP.
func (s *Source) encodeStills(ctx context.Context, encs []encoding, paths []string) ([]encodedFrame, error) {
	if s.webp {
		return s.encodeWebPStill(ctx, encs, paths)
	}
	readOps, sizes, decoded, err := s.decodePlan(ctx, encs)
	if err != nil {
		return nil, err
	}
	if err := checkStillSize(s.stored, decoded); err != nil {
		return nil, err
	}
	if s.softRef != "" && slices.ContainsFunc(encs, func(e encoding) bool { return !s.fits(e) }) {
		soft, err := fullSoft(ctx, s.env, s.softRef)
		if err != nil {
			return nil, err
		}
		s.soft, s.softRef = soft, ""
	}
	src := encodeSource{orientation: s.orientation}
	targets := make([]fusedTarget, len(encs))
	for i, enc := range encs {
		f, err := s.stillFrame(enc)
		if err != nil {
			return nil, err
		}
		targets[i] = fusedTarget{frame: f, enc: enc, path: paths[i]}
		if sizes != nil {
			targets[i].width, targets[i].height = sizes[i][0], sizes[i][1]
		}
	}
	chk := encodeCheck{frames: len(encs), outputs: paths}
	for _, t := range targets {
		chk.bounds = append(chk.bounds, boundOf(t.enc, t.source(src)))
	}
	return s.env.runEncode(ctx, fusedArgs(readOps, src, targets), chk)
}

// encodeWebPStill decodes the still WebP once with webp-frames, at the size
// webpDecodeSize gives, and encodes each target from that frame in a run of
// its own. The frame carries no orientation tag, so the file's own is given,
// read from the metadata the tool reports. The output size is exact when the
// decode was reduced and ImageMagick's computation on the oriented frame
// otherwise.
func (s *Source) encodeWebPStill(ctx context.Context, encs []encoding, paths []string) ([]encodedFrame, error) {
	decoded := s.webpDecodeSize(encs)
	frame, meta, cleanup, err := s.decodeWebP(ctx, decoded)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	orientation, err := s.webpOrientation(ctx, meta)
	if err != nil {
		return nil, err
	}
	s.upright[0], s.upright[1] = uprightSize(s.stored[0], s.stored[1], orientation)
	var sizes [][2]int
	if decoded != s.stored {
		sizes = s.outputSizes(encs)
	}
	res := make([]encodedFrame, len(encs))
	for i, enc := range encs {
		src := encodeSource{orientation: orientation}
		if sizes != nil {
			src.width, src.height = sizes[i][0], sizes[i][1]
		}
		f, err := s.frameOf(frame, enc)
		if err != nil {
			return nil, err
		}
		out, err := s.env.runEncode(ctx,
			encodeArgs([]encodeFrame{f}, src, enc, encodeOutput{kind: outputStill, path: paths[i]}),
			encodeCheck{frames: 1, bounds: []encodeBound{boundOf(enc, src)}, outputs: []string{paths[i]}})
		if err != nil {
			return nil, err
		}
		res[i] = out[0]
	}
	return res, nil
}

// webpOrientation returns the name of the orientation to give the frame of the
// still WebP that was decoded with the metadata meta: the hint if there is one,
// else what meta settles, else what ImageMagick's own read of the whole image
// reports, which is the one thing asked of ImageMagick for such a file (its
// header read under -ping misses a WebP's orientation). The answer is
// the same for every decode of the file and is kept.
func (s *Source) webpOrientation(ctx context.Context, meta stillMeta) (string, error) {
	if s.orientation != "" {
		return s.orientation, nil
	}
	if s.exifKnown {
		return s.exif, nil
	}
	n, ok := meta.orientation()
	if !ok {
		full, err := identifyHeaders(ctx, s.env, s.ref, false)
		if err != nil {
			return "", err
		}
		if n, err = orientationNumber(full[0].orientation); err != nil {
			n = 0 // an orientation ImageMagick does not recognise turns nothing
		}
	}
	s.exif, _ = orientationName(n)
	s.exifKnown = true
	return s.exif, nil
}

// decodeWebP has webp-frames decode the still WebP at size into the scratch
// directory and returns the frame and the metadata the tool read, with the
// function that removes what the decode left. size is at most the stored size,
// which the header read has held to the caps. A failed decode leaves nothing
// in the directory.
func (s *Source) decodeWebP(ctx context.Context, size [2]int) (srcFrame, stillMeta, func(), error) {
	req := s.req
	req.dir, req.scaled = s.dir, size
	var frame srcFrame
	got := 0
	leftovers := []string{
		filepath.Join(s.dir, gifSampleName(0)), filepath.Join(s.dir, stillEXIFName), filepath.Join(s.dir, stillXMPName),
	}
	cleanup := func() { removeFiles(leftovers) }
	err := s.env.composeFrames(ctx, req, func(f composedFrame) error {
		if got++; got > 1 {
			return errors.New("webp-frames wrote more than one image")
		}
		if f.profile > 0 {
			if p := filepath.Join(s.dir, composedProfileName(f.profile)); !slices.Contains(s.placed, p) {
				s.placed = append(s.placed, p)
			}
		}
		frame = srcFrame{path: f.path, profile: f.profile, soft: f.soft}
		return nil
	})
	if err == nil && got != 1 {
		err = fmt.Errorf("webp-frames wrote %d images, want 1", got)
	}
	var meta stillMeta
	if err == nil {
		meta, err = readStillMeta(s.dir)
	}
	if err != nil {
		cleanup()
		removeFiles([]string{filepath.Join(s.dir, composedProfileName(1))})
		return srcFrame{}, stillMeta{}, nil, fmt.Errorf("decoding at %dx%d: %w", size[0], size[1], err)
	}
	return frame, meta, cleanup, nil
}

// Encode writes one file per target through Env.Stage and returns them in the
// order of targets. Each holds the frames a budget of maxFrames selects of the
// frames Open selected, evenly spaced from the first (all of them for 0), as
// one animated file; a still, or one selected frame, makes a plain image, and
// a target's Format must be WebP when there are several. A frame identical or
// nearly so to the one before it is merged into its scene by the WebP writer,
// so Artifact.Frames says which frames the file shows, each at the time it
// starts. A still is decoded once for all targets. It is
// refused with ErrTooLarge, before any pixel is decoded, when the frame the
// decode yields for these targets has a side over 16384 or more than 52
// million pixels; a JPEG that the targets let the decode reduce is judged at
// the reduced size, any other still at its stored size. A run that hits
// ImageMagick's time limit fails with ErrTimeLimit, and one that reports a
// frame above the bounds of its target fails too; neither returns an
// artifact.
func (s *Source) Encode(ctx context.Context, maxFrames int, targets ...Encoding) ([]Artifact, error) {
	encs := make([]encoding, len(targets))
	for i, t := range targets {
		var err error
		if encs[i], err = t.resolve(); err != nil {
			return nil, err
		}
	}
	return s.encode(ctx, maxFrames, encs)
}

func (s *Source) encode(ctx context.Context, maxFrames int, encs []encoding) (out []Artifact, err error) {
	if len(encs) == 0 {
		return nil, errors.New("no targets")
	}
	if s.stream {
		return nil, errors.New("a streaming source encodes frames to files only")
	}
	if s.video != nil {
		if err := s.decodeVideo(ctx, encs); err != nil {
			return nil, err
		}
	}
	pos, err := s.positions(maxFrames)
	if err != nil {
		return nil, err
	}
	if len(pos) > 1 {
		for _, enc := range encs {
			if enc.format != WebP {
				return nil, fmt.Errorf("%s output cannot hold %d frames", enc.format, len(pos))
			}
		}
	}
	defer func() {
		if err != nil {
			for _, a := range out {
				a.Cleanup()
			}
			out = nil
		}
	}()
	paths := make([]string, len(encs))
	for i, enc := range encs {
		path, cleanup, serr := s.env.Stage(enc.ext)
		if serr != nil {
			return out, serr
		}
		paths[i] = path
		out = append(out, Artifact{Path: path, Cleanup: cleanup})
	}
	if s.route == routeStill {
		res, err := s.encodeStills(ctx, encs, paths)
		if err != nil {
			return out, fmt.Errorf("encoding still: %w", err)
		}
		for i := range out {
			out[i].Width, out[i].Height = res[i].width, res[i].height
			out[i].Frames = []Frame{{}}
		}
		return out, nil
	}
	for i, enc := range encs {
		w, h, kept, err := s.encodeAnimation(ctx, enc, pos, paths[i])
		if err != nil {
			return out, err
		}
		out[i].Width, out[i].Height = w, h
		indices := make([]int, len(kept))
		for j, p := range kept {
			indices[j] = s.frames[p].sourceFrame
		}
		out[i].Frames = s.mapping(indices)
	}
	return out, nil
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
func (s *Source) encodeAnimation(ctx context.Context, enc encoding, pos []int, path string) (w, h int, shown []int, err error) {
	src := s.sourceOf(enc)
	frames, err := s.framesAt(pos, enc, true)
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
		encodeCheck{frames: len(pos), bounds: []encodeBound{boundOf(enc, src)}, outputs: []string{path}})
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

// EncodeFrames writes one file per frame of the source into dir, named
// frame-NNN with the extension of enc.Format and numbered from 0, and returns
// them in order; Image.Frame is the index of the source frame, 0 for a video,
// and Image.TimeMS the time of that frame or sample, as Frame.SourceTimeMS
// gives it. A frame identical to the one before it after resizing is dropped.
// dir must exist and be empty. It fails as Encode does.
func (s *Source) EncodeFrames(ctx context.Context, enc Encoding, dir string) ([]Image, error) {
	ec, err := enc.resolve()
	if err != nil {
		return nil, err
	}
	return s.encodeFiles(ctx, ec, dir)
}

func (s *Source) encodeFiles(ctx context.Context, enc encoding, dir string) ([]Image, error) {
	if s.video != nil {
		if err := s.decodeVideo(ctx, []encoding{enc}); err != nil {
			return nil, err
		}
	}
	if s.route == routeStill {
		name := frameName(0, enc.ext)
		res, err := s.encodeStills(ctx, []encoding{enc}, []string{filepath.Join(dir, name)})
		if err != nil {
			return nil, fmt.Errorf("encoding still: %w", err)
		}
		return []Image{{Name: name, Width: res[0].width, Height: res[0].height}}, nil
	}
	var images []Image
	sel := s.mapping(s.selected)
	encoder := &sampleEncoder{
		env: s.env, src: s.sourceOf(enc),
		enc: enc, dir: dir, framePixels: s.framePixels(),
		keep: func(i, n, w, h int) {
			images = append(images, Image{
				Name: frameName(n, enc.ext), Width: w, Height: h,
				Frame: sel[i].SourceFrame, TimeMS: sel[i].SourceTimeMS,
			})
		},
	}
	if !s.stream {
		pos, _ := s.positions(0)
		frames, err := s.framesAt(pos, enc, false)
		if err != nil {
			return nil, err
		}
		if err := encoder.encodeSamples(ctx, frames); err != nil {
			return nil, err
		}
		return images, nil
	}

	// The compositor waits for each batch to be consumed, so the directory
	// holds about one batch.
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
	err := s.env.composeFrames(ctx, req, func(f composedFrame) error {
		if ordinal >= len(s.selected) {
			return errors.New("the compositor wrote more frames than selected")
		}
		sf := srcFrame{path: f.path, profile: f.profile, soft: f.soft, sourceFrame: s.selected[ordinal]}
		ordinal++
		if f.profile > 0 {
			s.placed = append(s.placed, filepath.Join(s.dir, composedProfileName(f.profile)))
		}
		ef, err := s.frameOf(sf, enc)
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

// stillImage writes the first image of a source of one frame to dir as
// still.<ext> and describes it.
func (s *Source) stillImage(ctx context.Context, enc encoding, dir string) (Image, error) {
	name := "still" + enc.ext
	res, err := s.encodeStills(ctx, []encoding{enc}, []string{filepath.Join(dir, name)})
	if err != nil {
		return Image{}, fmt.Errorf("encoding still: %w", err)
	}
	return Image{Name: name, Width: res[0].width, Height: res[0].height}, nil
}
