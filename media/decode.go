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
	// carries; 0 leaves the file's own.
	Orientation int
	// Frames is the number of frames selected from an animation, evenly
	// spaced from the first; 0 selects every frame. It must not be negative.
	Frames int
	// Still selects the first image of the file alone, and excludes Frames.
	// Without it a file of several images that is neither a GIF nor a WebP is
	// refused with ErrUnsupportedType.
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

// jpegSizeMargin is how many times the largest target a reduced JPEG decode
// covers in each dimension.
const jpegSizeMargin = 2

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
)

// openRequest is what Env.Open and the Processor's operations share.
type openRequest struct {
	path  string
	hints Hints
	// dec pins the coder, and so the type, of the input when pinned is set;
	// otherwise the type comes from the bytes.
	dec    decoder
	pinned bool
	// caps applies the caps on stills and on the frames returned.
	caps bool
	// pick chooses the frames of an animation from their count, as indices in
	// ascending order without repeats; nil takes them from hints.
	pick func(sourceFrames int) ([]int, error)
	// scratch is the directory for decoded frames; "" takes Env.Scratch.
	scratch string
	// stream leaves the frames undecoded until EncodeFrames, which decodes
	// and encodes them in batches so that no more than a batch is on disk.
	stream bool
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
			case typeMP4, typeMOV, typeWebM, typeMKV:
				return headerRead{}, fmt.Errorf("%w: %s", ErrUnsupportedType, kind)
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
	animated := h.route != routeStill
	switch {
	case animated:
		err = checkSceneHeaders(h.scenes, true)
	case req.caps:
		err = checkSceneHeaders(h.scenes[:1], false)
	}
	if err != nil {
		return headerRead{}, err
	}
	if err := completeProfile(ctx, e, h.ref+selector, req.path, h.scenes); err != nil {
		return headerRead{}, err
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
// image of a still, or the frames of a GIF or animated WebP composited onto
// their canvas. It is not safe for concurrent use.
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
	// reduce allows a reduced decode of a JPEG.
	reduce bool
	// softRef is the identify input whose first scene decides soft by a full
	// read, which is made when an encode first resizes; "" when the -ping
	// read decided it.
	softRef string
}

// Open reads the headers of the file at path, applies the caps and selects
// its frames, decoding those of a GIF or animated WebP onto a full canvas in
// the directory Env.Scratch provides. The type comes from the file's bytes: a
// GIF is composited by the package and a WebP with the animation flag by the
// webp-frames tool, whatever their frame count; any other image is decoded by
// ImageMagick when it is encoded. A file of several images that is neither a
// GIF nor a WebP is refused with ErrUnsupportedType unless h.Still is set, and
// so is a video file, whatever h says. An animation over the caps is refused
// with ErrTooLarge before any frame is decoded.
//
// The caller must Close the Source.
func (e Env) Open(ctx context.Context, path string, h Hints) (*Source, error) {
	return e.open(ctx, openRequest{path: path, hints: h})
}

func (e Env) open(ctx context.Context, req openRequest) (*Source, error) {
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
	hdr, err := e.readSource(ctx, req)
	if err != nil {
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
		s.layout.uw, s.layout.uh = uprightSize(first.width, first.height, cmp.Or(override, first.orientation))
		s.ref, s.soft, s.jpeg = hdr.ref+"[0]", first.soft, first.format == "JPEG"
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

	if req.scratch != "" {
		s.dir = req.scratch
	} else {
		if e.Scratch == nil {
			return nil, errors.New("Env.Scratch is required to open a source")
		}
		if s.dir, s.cleanup, err = e.Scratch(); err != nil {
			return nil, err
		}
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
// original, in order.
func (s *Source) Frames() []Frame {
	return mappingOf(s.selected, s.layout.infos)
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

// reduction decides whether a JPEG still is decoded at a reduced size for
// encodes at encs and, if so, returns the read option and the exact size of
// each output. The decoded size is the smallest eighth of the stored size
// that covers jpegSizeMargin times every output in both dimensions; the exact
// sizes are computed here, from the size and orientation the header gives,
// because ImageMagick would round each side of the reduced frame separately.
func (s *Source) reduction(encs []encoding) (readOps []string, sizes [][2]int) {
	if !s.jpeg || !s.reduce {
		return nil, nil
	}
	n := 1
	sizes = make([][2]int, len(encs))
	sw, sh := s.stored[0], s.stored[1]
	swapped := s.upright[0] != sw || s.upright[1] != sh
	for i, enc := range encs {
		tw, th, resize := fitSize(s.upright[0], s.upright[1], enc.maxEdge, enc.maxArea)
		if !resize {
			return nil, nil
		}
		sizes[i] = [2]int{tw, th}
		if swapped {
			tw, th = th, tw
		}
		n = max(n, ceilDiv(8*jpegSizeMargin*tw, sw), ceilDiv(8*jpegSizeMargin*th, sh))
	}
	if n >= 8 {
		return nil, nil
	}
	hint := fmt.Sprintf("%dx%d", ceilDiv(sw*n, 8), ceilDiv(sh*n, 8))
	return []string{"-define", "jpeg:size=" + hint}, sizes
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// encodeStills runs the fused still encode for the targets and returns what
// each reported.
func (s *Source) encodeStills(ctx context.Context, encs []encoding, paths []string) ([]encodedFrame, error) {
	if s.softRef != "" && slices.ContainsFunc(encs, func(e encoding) bool { return !s.fits(e) }) {
		soft, err := fullSoft(ctx, s.env, s.softRef)
		if err != nil {
			return nil, err
		}
		s.soft, s.softRef = soft, ""
	}
	readOps, sizes := s.reduction(encs)
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
	src := encodeSource{orientation: s.orientation}
	chk := encodeCheck{frames: len(encs), outputs: paths}
	for _, t := range targets {
		chk.bounds = append(chk.bounds, boundOf(t.enc, t.source(src)))
	}
	res, err := s.env.runEncode(ctx, fusedArgs(readOps, src, targets), chk)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Encode writes one file per target through Env.Stage and returns them in the
// order of targets. Each holds the frames a budget of maxFrames selects of the
// frames Open selected, evenly spaced from the first (all of them for 0), as
// one animated file; a still, or one selected frame, makes a plain image, and
// a target's Format must be WebP when there are several. A frame identical to
// the one before it at the target's size is dropped, so Artifact.Frames says
// which frames the file holds. A still is decoded once for all targets. A run
// that hits ImageMagick's time limit fails with ErrTimeLimit, and one that
// reports a frame above the bounds of its target fails too; neither returns
// an artifact.
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
		out[i].Frames = mappingOf(indices, s.layout.infos)
	}
	return out, nil
}

// encodeAnimation encodes the frames at pos to one file at path and returns
// its size and the positions it keeps.
//
// With several frames there are two magick runs over the same frames. The
// first reports the signature of the pixels that will actually be encoded,
// taken after the resize, since two frames differing only in fine detail can
// become identical once downscaled. The second encodes the frames that
// differ from the one before. The dedup is needed because the WebP writer
// collapses runs of identical consecutive frames and no option prevents it:
// left alone, the mapping would promise frames the file does not hold. The
// scene count of the written file is the guarantee, and the dedup is what
// keeps the check from failing on the common case.
func (s *Source) encodeAnimation(ctx context.Context, enc encoding, pos []int, path string) (w, h int, kept []int, err error) {
	src := encodeSource{orientation: s.orientation}
	frames, err := s.framesAt(pos, enc, true)
	if err != nil {
		return 0, 0, nil, err
	}
	keepAt := []int{0}
	if len(pos) > 1 {
		probed, err := s.env.runEncode(ctx, encodeArgs(frames, src, enc, encodeOutput{kind: outputProbe}),
			encodeCheck{frames: len(pos), bounds: []encodeBound{boundOf(enc, src)}})
		if err != nil {
			return 0, 0, nil, fmt.Errorf("probing selected frames: %w", err)
		}
		keepAt = keepAt[:0]
		for i, f := range probed {
			if i == 0 || f.signature != probed[i-1].signature {
				keepAt = append(keepAt, i)
			}
		}
	}
	keptFrames := make([]encodeFrame, len(keepAt))
	kept = make([]int, len(keepAt))
	for i, k := range keepAt {
		keptFrames[i], kept[i] = frames[k], pos[k]
	}
	kind := outputAnimation
	if len(kept) == 1 {
		kind = outputStill
	}
	res, err := s.env.runEncode(ctx, encodeArgs(keptFrames, src, enc, encodeOutput{kind: kind, path: path}),
		encodeCheck{frames: len(kept), bounds: []encodeBound{boundOf(enc, src)}})
	if err != nil {
		return 0, 0, nil, fmt.Errorf("encoding animation: %w", err)
	}
	if len(kept) == 1 {
		return res[0].width, res[0].height, kept, nil
	}
	scenes, w, h, err := s.env.IdentifyArtifact(ctx, path)
	if err != nil {
		return 0, 0, nil, err
	}
	if scenes != len(kept) {
		return 0, 0, nil, fmt.Errorf("encoded artifact holds %d scenes, mapping describes %d", scenes, len(kept))
	}
	return w, h, kept, nil
}

// EncodeFrames writes one file per frame of the source into dir, named
// frame-NNN with the extension of enc.Format and numbered from 0, and returns
// them in order; Image.Frame is the index of the source frame. A frame
// identical to the one before it after resizing is dropped. dir must exist
// and be empty. It fails as Encode does.
func (s *Source) EncodeFrames(ctx context.Context, enc Encoding, dir string) ([]Image, error) {
	ec, err := enc.resolve()
	if err != nil {
		return nil, err
	}
	return s.encodeFiles(ctx, ec, dir)
}

func (s *Source) encodeFiles(ctx context.Context, enc encoding, dir string) ([]Image, error) {
	if s.route == routeStill {
		name := frameName(0, enc.ext)
		res, err := s.encodeStills(ctx, []encoding{enc}, []string{filepath.Join(dir, name)})
		if err != nil {
			return nil, fmt.Errorf("encoding still: %w", err)
		}
		return []Image{{Name: name, Width: res[0].width, Height: res[0].height}}, nil
	}
	var images []Image
	encoder := &sampleEncoder{
		env: s.env, src: encodeSource{orientation: s.orientation},
		enc: enc, dir: dir, framePixels: s.layout.cw * s.layout.ch,
		keep: func(i, n, w, h int) {
			images = append(images, Image{Name: frameName(n, enc.ext), Width: w, Height: h, Frame: s.selected[i]})
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
