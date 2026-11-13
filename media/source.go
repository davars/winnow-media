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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind is how a Source's frames are read; the package documentation's Kinds
// section describes each. Later versions may add kinds, so a switch over a
// Kind needs a default case.
type Kind string

// The kinds of Source.
const (
	// Still is an image file, possibly of several images (TIFF pages, icon
	// sizes).
	Still Kind = "still"
	// Animation is a GIF, whatever its frame count, or an animated WebP.
	Animation Kind = "animation"
	// Video is a video file.
	Video Kind = "video"
)

// OpenOptions are what Open cannot learn from the file's bytes, and what the
// Source is to keep.
type OpenOptions struct {
	// Type, when set, is the media type of the input, in place of the one
	// its bytes name. It must be in Config.Types. It is how a file whose
	// bytes identify no type (see Types) is read.
	Type string
	// Orientation, 1 to 8 as in EXIF, is the orientation of a still or
	// animation whose header reports none; 0 leaves the file's own. It is
	// refused for a video.
	Orientation int
	// KeepDecode keeps a full-size decode of a still on disk, so that later
	// Encodes of the same image skip the decode: set it for a still that will
	// be encoded more than once, such as an overview and then regions of it.
	// Animations and videos ignore it, so it may be set whatever the file.
	//
	// A kept decode takes up to 10 bytes a pixel under Config.Dir, and one
	// Source keeps at most half of Resources.Disk; Source.KeptBytes reports
	// the total. Only the images the latest Encode selected stay kept. An
	// image that cannot be kept, because its format has no exact kept form,
	// it does not fit, or its full-size decode is over the caps, is encoded
	// as it would be without KeepDecode. With KeepDecode, the output bytes of
	// a JPEG or still WebP can differ from those without it.
	KeepDecode bool
}

// Info describes a Source. It is a snapshot: an Encode can settle a still's
// size (see Upright).
type Info struct {
	// Type is the media type the input was read as.
	Type string
	// Kind says which of Images, Animation and Video describes the Source.
	Kind Kind
	// Width and Height are the size of the first frame or image: as shown
	// when Upright is set, else as stored.
	Width, Height int
	// Upright says that Width and Height are the size as shown, after the
	// orientation or rotation. It is always set for an animation or video. A
	// still can leave it unset: when neither its header nor OpenOptions gives
	// an orientation, only a decode shows whether the picture is turned, and
	// Upright is set once an Encode has decoded the image. A Region is in
	// upright pixels: to choose one for such a still, encode the whole image
	// first and read Image.Region.
	Upright bool
	// Orientation is the EXIF orientation, 1 to 8, that the header reported
	// or OpenOptions gave; 0 when neither did.
	Orientation int
	// OrientationGiven is set when Orientation is the one OpenOptions gave,
	// the file's header having reported none.
	OrientationGiven bool
	// Images describes each image of a Still, in the order Indices numbers
	// them. It is nil unless Kind is Still.
	Images []ImageInfo
	// Animation describes an Animation; it is the zero value for any other
	// Kind.
	Animation AnimationInfo
	// Video describes a Video; it is the zero value for any other Kind.
	Video VideoInfo
}

// AnimationInfo describes an animation.
type AnimationInfo struct {
	// Frames is the number of frames.
	Frames int
	// Delays is how long each frame is displayed. A stored delay below 20 ms
	// counts as 100 ms, as browsers display it.
	Delays []time.Duration
	// Duration is the sum of Delays.
	Duration time.Duration
}

// VideoInfo describes a video.
type VideoInfo struct {
	// Duration is the video's duration, or 0 when unknown. A video of
	// unknown duration can be read only from time 0.
	Duration time.Duration
	// FrameRate is the base frame rate, or 0 when unknown.
	FrameRate float64
	// DeclaredFrames is the frame count the container declares, or 0 when it
	// declares none. It is not verified: choose samples by time.
	DeclaredFrames int
	// Rotation is the display rotation: 0, 90, 180 or 270 degrees.
	Rotation int
	// HDR is set when the transfer characteristic is HLG or PQ.
	HDR bool
	// Codec is the codec name as ffprobe reports it.
	Codec string
	// HasAudio is set when the container holds an audio stream.
	HasAudio bool
}

// ImageInfo describes one image of a still. Width and Height are as for Info:
// upright once an Encode has decoded the image, else as stored.
type ImageInfo struct {
	Width, Height int
	// Auxiliary is set for an image the decoder reports as a reduced-resolution
	// preview or a transparency mask of another, not a page of its own.
	Auxiliary bool
}

func (i Info) clone() Info {
	i.Images = slices.Clone(i.Images)
	i.Animation.Delays = slices.Clone(i.Animation.Delays)
	return i
}

// Source is one input file a Processor admitted, from which Encode writes
// images. It is safe for concurrent use; its Encode calls run one at a time.
// Until Close it holds the input file open and a scratch directory, and
// between calls no process.
type Source struct {
	p *Processor
	// gate serialises calls and guards every field below up to imu: a call
	// holds it by having sent into it, and a call waiting for it can give up.
	gate   chan struct{}
	closed bool
	// file is the input, opened once by Open and read through for the
	// Source's life; ref is what the tools call it.
	file     *os.File
	path     string
	ref      string
	dir      string
	release  func()
	id       fileID
	typ      string
	kind     Kind
	override string
	srgb     string

	// A still: its header read and its decodes, one per image read so far.
	hdr    headerRead
	req    openRequest
	stills map[int]*frameSource
	// reuse is OpenOptions.KeepDecode; kept holds the decodes it keeps, by image.
	reuse bool
	kept  map[int]*keptDecode
	// keptTotal is keptBytes, stored at each change to kept for KeptBytes to
	// read without the gate.
	keptTotal atomic.Int64
	// noKeep holds the images no later call could keep.
	noKeep map[int]bool
	// profiled are the images whose profile has been read in full.
	profiled map[int]bool

	// An animation: its header read, the frame compositor's request and the
	// composited frames of the most recent call.
	layout      animationLayout
	orientation string
	upright     [2]int
	compose     composeRequest
	frames      map[int]srcFrame

	// A video: its probe and decoder.
	probe videoInfo
	// probed is the probe that named the type at the gate, if one did; Open
	// uses it instead of probing again.
	probed *videoInfo
	dec    decoder

	imu  sync.Mutex
	info Info
}

// Open admits the file at path, which may be relative, and returns a Source
// for it. It reads only what Encode needs to know: the headers of a still or
// animation, or one probe of a video. No pixel is decoded. The package
// documentation's Admission section says what is checked.
//
// The Usage covers the tool runs of the call, also when it fails. On an error
// no Source is returned and nothing needs closing; otherwise the caller must
// Close the Source or the Processor.
//
// Errors wrap ErrUnsupportedType or ErrTooLarge for a file the Processor does
// not accept, ErrInvalidRequest for a path that is not a regular file or an
// opts.Orientation that is out of range or given for a video, ErrInputChanged
// for a file replaced during the call, ErrTimeLimit, ErrClosed for a closed
// Processor, or ctx's error; or they are a *ToolError. A path that cannot be
// opened gives the operating system's error, which wraps none of these;
// errors.Is(err, fs.ErrNotExist) holds for a missing file. An Open in
// progress when the Processor is closed returns ErrClosed or a Source that
// Close then closes.
func (p *Processor) Open(ctx context.Context, path string, opts OpenOptions) (*Source, Usage, error) {
	if !p.beginOpen() {
		return nil, Usage{}, fmt.Errorf("open: processor: %w", ErrClosed)
	}
	defer p.opening.Done()
	src, usage, err := p.open(ctx, path, opts)
	if err != nil {
		return nil, usage, fmt.Errorf("open: %w", err)
	}
	return src, usage, nil
}

func (p *Processor) open(ctx context.Context, path string, opts OpenOptions) (*Source, Usage, error) {
	var m meter
	override, err := orientationName(opts.Orientation)
	if err != nil {
		return nil, m.usage, err
	}
	in, err := p.admitInput(ctx, &m, opts.Type, path)
	if err != nil {
		return nil, m.usage, err
	}
	src := &Source{
		gate: make(chan struct{}, 1), p: p, file: in.file, path: in.abs, ref: in.ref, dir: in.dir, release: in.release, id: in.id,
		typ: in.typ, override: override, dec: in.decoder,
	}
	src.probed = in.probe
	src.reuse = opts.KeepDecode
	env := p.env(&m, src.file).pinnedTo(src.file, src.id, "while it was opened")
	if in.decoder.tool == "ffmpeg" {
		err = src.openVideo(ctx, env, opts)
	} else {
		err = src.openImage(ctx, env, in)
	}
	if err == nil {
		err = src.checkIdentity()
	}
	if err == nil && !p.register(src) {
		err = fmt.Errorf("processor: %w", ErrClosed)
	}
	if err != nil {
		in.release()
		_ = in.file.Close()
		return nil, m.usage, err
	}
	return src, m.usage, nil
}

// orientationNone reports whether a header's orientation says none.
func orientationNone(tag string) bool { return tag == "" || tag == "Undefined" }

// applied returns the orientation name the caller's override gives a scene
// whose header reports tag: the override when the header reports none.
func (s *Source) applied(tag string) string {
	if orientationNone(tag) {
		return s.override
	}
	return ""
}

func (s *Source) openImage(ctx context.Context, env toolEnv, in admitted) error {
	s.req = openRequest{
		path: in.ref, file: in.file, dec: in.decoder, scenes: in.scenes,
	}
	hdr, err := env.readSource(ctx, s.req)
	if err != nil {
		return err
	}
	s.hdr = hdr
	if hdr.route == routeStill {
		return s.openStill()
	}
	return s.openAnimation(env)
}

func (s *Source) openStill() error {
	s.kind = Still
	s.stills = map[int]*frameSource{}
	s.kept = map[int]*keptDecode{}
	s.noKeep = map[int]bool{}
	s.profiled = map[int]bool{0: true}
	info := Info{Type: s.typ, Kind: Still}
	for i, sc := range s.hdr.scenes {
		ov := s.applied(sc.orientation)
		es := &frameSource{route: routeStill, orientation: ov, dir: s.dir}
		if err := es.initStill(s.req, s.hdr, i, ov); err != nil {
			return err
		}
		s.stills[i] = es
		info.Images = append(info.Images, ImageInfo{
			Width: es.shownSize()[0], Height: es.shownSize()[1], Auxiliary: auxiliarySubfile(sc.subfile),
		})
		if i == 0 {
			info.Width, info.Height, info.Upright = es.shownSize()[0], es.shownSize()[1], !es.unsettled
			info.Orientation, _ = orientationNumber(cmp.Or(ov, sc.orientation))
			info.OrientationGiven = ov != ""
		}
	}
	s.info = info
	return nil
}

// auxiliarySubfile reports whether a TIFF subfile type, as ImageMagick prints
// %[tiff:subfiletype], marks a reduced-resolution preview or a transparency
// mask.
func auxiliarySubfile(subfile string) bool {
	return subfile == "REDUCEDIMAGE" || subfile == "MASK"
}

// shownSize is the size of the image as the header read gives it: upright when
// the source is settled, else as stored.
func (s *frameSource) shownSize() [2]int {
	if s.unsettled {
		return s.stored
	}
	return s.upright
}

func (s *Source) openAnimation(env toolEnv) error {
	s.kind = Animation
	hdr := s.hdr
	first := hdr.scenes[0]
	count := len(hdr.layout.infos)
	s.layout = hdr.layout
	s.orientation = cmp.Or(s.applied(first.orientation), first.orientation)
	s.layout.uw, s.layout.uh = uprightSize(s.layout.cw, s.layout.ch, s.orientation)
	s.upright = [2]int{s.layout.uw, s.layout.uh}
	s.frames = map[int]srcFrame{}
	s.compose = composeRequest{
		path: s.ref, dir: s.dir, frames: count, width: s.layout.cw, height: s.layout.ch,
	}
	if hdr.route == routeWebP {
		s.compose.external = webpFramesTool
	} else {
		s.compose.own = make([]bool, count)
		for i, f := range hdr.layout.infos {
			s.compose.own[i] = f.hasICC
		}
	}
	delays := make([]time.Duration, count)
	var total time.Duration
	for i, f := range hdr.layout.infos {
		delays[i] = time.Duration(displayDelayCS(f.delayCS)) * 10 * time.Millisecond
		total += delays[i]
	}
	orientation, _ := orientationNumber(s.orientation)
	given := s.applied(first.orientation) != ""
	s.info = Info{
		Type: s.typ, Kind: Animation, Width: s.upright[0], Height: s.upright[1], Upright: true,
		Orientation: orientation, OrientationGiven: given,
		Animation: AnimationInfo{Frames: count, Delays: delays, Duration: total},
	}
	return nil
}

func (s *Source) openVideo(ctx context.Context, env toolEnv, opts OpenOptions) error {
	if opts.Orientation != 0 {
		return invalid("orientation does not apply to a video")
	}
	var info videoInfo
	if s.probed != nil {
		info = *s.probed
	} else {
		var err error
		if info, err = env.probeChecked(ctx, s.dec, s.ref); err != nil {
			return err
		}
	}
	s.kind, s.probe = Video, info
	w, h := info.upright()
	s.info = Info{
		Type: s.typ, Kind: Video, Width: w, Height: h, Upright: true,
		Video: VideoInfo{
			Duration: time.Duration(info.durationMS) * time.Millisecond, FrameRate: info.rate,
			DeclaredFrames: info.declared, Rotation: info.rotation,
			HDR: isHDRTransfer(info.transfer), Codec: info.codec, HasAudio: info.hasAudio,
		},
	}
	return nil
}

// Info returns a snapshot of what is known of the Source. It is safe to call
// at any time, also while an Encode runs.
func (s *Source) Info() Info {
	s.imu.Lock()
	defer s.imu.Unlock()
	return s.info.clone()
}

// KeptBytes returns the bytes on disk the Source holds for
// OpenOptions.KeepDecode. It is 0 for a Source opened without KeepDecode,
// before the first Encode, when nothing could be kept and after Close. It is
// safe to call at any time and does not wait for a running Encode.
func (s *Source) KeptBytes() int64 {
	return s.keptTotal.Load()
}

// lock acquires the gate, or returns ctx's error if ctx ends first. A context
// that has ended never acquires it.
func (s *Source) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Source) unlock() { <-s.gate }

// Close closes the input and removes the Source's scratch; files Encode wrote
// stay. It waits for a running call, which it does not interrupt: cancel that
// call's context first to end it sooner. An Encode that was waiting its turn
// returns ErrClosed. Close is safe to call more than once.
func (s *Source) Close() error {
	s.gate <- struct{}{}
	defer s.unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.release()
	s.p.forget(s)
	s.stills, s.frames, s.kept = nil, nil, nil
	s.syncKept()
	return s.file.Close()
}

// checkIdentity returns an error unless the open input is as it was when the
// Source was opened. It asks the descriptor, so a file put in place of the
// path is not the question; a rewrite of the file itself is.
func (s *Source) checkIdentity() error {
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("source: %w", withoutPath(err))
	}
	if idOf(info) != s.id {
		return fmt.Errorf("source: %w since it was opened", ErrInputChanged)
	}
	return nil
}

// Encode reads the frames and region sel selects and writes them into dir:
// for each of encodings, one file per frame, or one animated file when the
// Encoding sets Animate. Encoding says what every output has in common.
//
// dir, which may be relative, must exist and be empty, so every call needs a
// directory of its own. The files are named NNN.jpg, NNN.webp or NNN.png,
// numbered from 000 across the call in encoding order, then frame order, and
// the Images are returned in the same order. A call can write fewer files
// than it selected frames: a frame of an animation or video that is identical,
// once resized, to the one before it is dropped, and a time past a video's
// last frame gives none. Image.Frames says what each file shows.
//
// The Usage covers the tool runs of this call alone, also when it fails.
//
// Calls on one Source run one at a time. A call waiting for another returns
// ctx's error, without running, if ctx ends first. Cancelling ctx during a
// run kills its tools and returns an error wrapping ctx's. A call that fails
// or is cancelled leaves dir as it found it and the Source usable, though the
// Source may have dropped what it keeps between calls.
//
// Errors wrap ErrInvalidRequest, ErrInputChanged, ErrClosed, ErrTooLarge,
// ErrTimeLimit or ctx's error, or are a *ToolError. An Encoding or
// FrameSelection that cannot be honoured is refused before any tool runs, and
// a video call whose sampled frames would exceed Resources.Disk is refused,
// with ErrTooLarge, before any frame is decoded.
func (s *Source) Encode(ctx context.Context, dir string, sel Select, encodings ...Encoding) ([]Image, Usage, error) {
	images, usage, err := s.encode(ctx, dir, sel, encodings, numbered)
	if err != nil {
		return nil, usage, fmt.Errorf("encode: %w", err)
	}
	return images, usage, nil
}

// numbered names the n-th file of an Encode call.
func numbered(n int, ext string) string { return fmt.Sprintf("%03d%s", n, ext) }

// encode is Encode naming its files name(n, ext).
func (s *Source) encode(ctx context.Context, dir string, sel Select, encodings []Encoding, name func(n int, ext string) string) ([]Image, Usage, error) {
	var m meter
	if len(encodings) == 0 {
		return nil, m.usage, invalid("no encodings")
	}
	encs := make([]encoding, len(encodings))
	for i, t := range encodings {
		var err error
		if encs[i], err = t.resolve(); err != nil {
			return nil, m.usage, err
		}
	}
	r := sel.Region.internal()
	if err := r.shape(); err != nil {
		return nil, m.usage, err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, m.usage, err
	}
	if err := s.lock(ctx); err != nil {
		return nil, m.usage, err
	}
	defer s.unlock()
	if s.closed {
		return nil, m.usage, fmt.Errorf("source: %w", ErrClosed)
	}
	if err := requireEmptyDir(dir); err != nil {
		return nil, m.usage, err
	}
	if err := s.checkIdentity(); err != nil {
		return nil, m.usage, err
	}
	if s.srgb == "" {
		if s.srgb, err = placeSRGB(s.dir); err != nil {
			return nil, m.usage, err
		}
	}
	env := s.p.env(&m, s.file).pinnedTo(s.file, s.id, "since it was opened")
	images, err := inDir(dir, func() ([]Image, error) {
		switch s.kind {
		case Still:
			return s.encodeStills(ctx, env, dir, sel.Frames, r, encs, name)
		case Animation:
			return s.encodeAnimation(ctx, env, dir, sel.Frames, r, encs, name)
		}
		return s.encodeVideo(ctx, env, dir, sel.Frames, r, encs, name)
	})
	s.refreshInfo()
	if err != nil {
		return nil, m.usage, err
	}
	return images, m.usage, nil
}

// refreshInfo records the sizes a call settled.
func (s *Source) refreshInfo() {
	if s.kind != Still {
		return
	}
	s.imu.Lock()
	defer s.imu.Unlock()
	info := s.info.clone()
	for i, es := range s.stills {
		size := es.shownSize()
		info.Images[i].Width, info.Images[i].Height = size[0], size[1]
		if i == 0 {
			info.Width, info.Height, info.Upright = size[0], size[1], !es.unsettled
		}
	}
	s.info = info
}

// stillAt returns the decode of image i, bound to env, with the profile of the
// image read if its coder cannot report it from the header.
func (s *Source) stillAt(ctx context.Context, env toolEnv, i int) (*frameSource, error) {
	es := s.stills[i]
	if !s.profiled[i] {
		scenes := s.hdr.scenes[i : i+1]
		if err := completeProfile(ctx, env, s.hdr.ref+"["+strconv.Itoa(i)+"]", s.req.input(), scenes); err != nil {
			return nil, err
		}
		s.profiled[i] = true
		if err := es.initStill(s.req, s.hdr, i, es.orientation); err != nil {
			return nil, err
		}
	}
	es.env, es.destination = env, s.srgb
	return es, nil
}

func (s *Source) encodeStills(ctx context.Context, env toolEnv, dir string, fr FrameSelection, r region, encs []encoding, name func(int, string) string) ([]Image, error) {
	idx, err := fr.images(len(s.hdr.scenes))
	if err != nil {
		return nil, err
	}
	for _, enc := range encs {
		if enc.animate && len(idx) > 1 {
			return nil, invalid("images of a still cannot be written as one animation")
		}
	}
	srcs := make([]*frameSource, len(idx))
	for f, k := range idx {
		if srcs[f], err = s.stillAt(ctx, env, k); err != nil {
			return nil, err
		}
		if err := srcs[f].checkRegion(r); err != nil {
			return nil, err
		}
	}
	s.dropKeptExcept(idx)
	n := len(idx)
	images := make([]Image, len(encs)*n)
	for f, es := range srcs {
		paths := make([]string, len(encs))
		for t, enc := range encs {
			paths[t] = filepath.Join(dir, name(t*n+f, enc.ext))
		}
		var res []encodedFrame
		var err error
		used := false
		if s.kept[idx[f]] != nil {
			res, used, err = s.readKept(ctx, es, idx[f], r, encs, paths)
		}
		if !used && err == nil {
			res, err = s.encodeFresh(ctx, es, idx[f], r, encs, paths)
		}
		if err != nil {
			return nil, fmt.Errorf("encoding still: %w", err)
		}
		for t, enc := range encs {
			images[t*n+f] = Image{
				Name: name(t*n+f, enc.ext), Encoding: t, Width: res[t].width, Height: res[t].height,
				Frames: []Frame{{Index: idx[f]}}, Region: es.shown(r),
			}
		}
	}
	return images, nil
}

// encodeFresh decodes image k of the still, keeping the decode when the Source
// reuses decodes. If the run that writes the kept files fails, which includes
// a full-size decode that exceeds the caps, the encode is tried once without
// them, and the image is then remembered as one that cannot be kept.
func (s *Source) encodeFresh(ctx context.Context, es *frameSource, k int, r region, encs []encoding, paths []string) ([]encodedFrame, error) {
	es.keep = s.planKeep(k, es)
	res, err := es.encodeStills(ctx, r, encs, paths)
	if es.keep == nil {
		return res, err
	}
	if err == nil {
		s.adoptKept(k, es)
		return res, nil
	}
	removeKeepFiles(es.keep)
	es.keep = nil
	if res, err = es.encodeStills(ctx, r, encs, paths); err == nil {
		s.noKeep[k] = true
	}
	return res, err
}

// animationSource returns the decode of the frames idx of the animation: the
// frames themselves are attached once composited.
func (s *Source) animationSource(env toolEnv, idx []int) *frameSource {
	layout := s.layout
	return &frameSource{
		env: env, route: s.hdr.route, layout: layout, orientation: s.orientation,
		selected: idx, dir: s.dir, destination: s.srgb, upright: s.upright,
	}
}

func (s *Source) encodeAnimation(ctx context.Context, env toolEnv, dir string, fr FrameSelection, r region, encs []encoding, name func(int, string) string) ([]Image, error) {
	starts := timestamps(frameDelays(s.layout.infos))
	idx, err := fr.animation(starts, s.info.Animation.Duration.Milliseconds())
	if err != nil {
		return nil, err
	}
	if err := checkAnimate(encs, len(idx)); err != nil {
		return nil, err
	}
	if err := checkReturnedPixels(len(idx), s.layout.cw, s.layout.ch, s.p.caps); err != nil {
		return nil, err
	}
	src := s.animationSource(env, idx)
	if err := src.checkRegion(r); err != nil {
		return nil, err
	}
	if need := int64(len(idx)) * int64(s.layout.cw) * int64(s.layout.ch) * 4; need > s.p.res.Disk {
		return s.streamAnimation(ctx, env, src, dir, idx, need, r, encs, name)
	}
	s.keepOnly(idx)
	if err := s.composite(ctx, env, idx); err != nil {
		return nil, fmt.Errorf("compositing selected frames: %w", err)
	}
	for _, k := range idx {
		src.frames = append(src.frames, s.frames[k])
	}
	return encodeSamples(ctx, src, dir, r, encs, name)
}

// streamAnimation encodes the frames idx of an animation whose composited
// frames would not fit in Resources.Disk, which is need bytes of scratch: the
// compositor waits for each batch to be encoded, so about one batch is on disk
// at a time and nothing is kept. Each encoding composites afresh, and an encoding
// that holds all the frames in one file cannot be written.
func (s *Source) streamAnimation(ctx context.Context, env toolEnv, src *frameSource, dir string, idx []int, need int64, r region, encs []encoding, name func(int, string) string) ([]Image, error) {
	for _, enc := range encs {
		if enc.animate {
			return nil, tooLarge("%d frames of %dx%d need %d bytes of scratch to write as one animation, Resources.Disk is %d",
				len(idx), s.layout.cw, s.layout.ch, need, s.p.res.Disk)
		}
	}
	s.keepOnly(nil)
	defer s.removeLeftovers()
	src.stream = true
	src.req = s.compose
	src.req.selected, src.req.consumeTimeout = idx, env.consumeTimeout()
	return encodeSamples(ctx, src, dir, r, encs, name)
}

// checkAnimate refuses an Animate encoding that is not WebP when n frames,
// more than one, go into it.
func checkAnimate(encs []encoding, n int) error {
	for _, enc := range encs {
		if enc.animate && n > 1 && enc.format != WebP {
			return invalid("%s output cannot hold %d frames", enc.format, n)
		}
	}
	return nil
}

// composite makes every frame of idx available in s.frames, composing only
// the missing ones in one run of the compositor.
func (s *Source) composite(ctx context.Context, env toolEnv, idx []int) error {
	var missing []int
	for _, k := range idx {
		if _, ok := s.frames[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	req := s.compose
	req.selected, req.inFlight, req.consumeTimeout = missing, len(missing), env.consumeTimeout()
	got := 0
	err := env.compose(ctx, req, func(f composedFrame) error {
		if got >= len(missing) {
			return errors.New("the compositor wrote more frames than selected")
		}
		k := missing[got]
		kept := filepath.Join(s.dir, fmt.Sprintf(".frame-%d.pam", k))
		if err := os.Rename(f.path, kept); err != nil {
			return err
		}
		s.frames[k] = srcFrame{path: kept, profile: f.profile, soft: f.soft, sourceFrame: k}
		got++
		return nil
	})
	if err == nil && got != len(missing) {
		err = fmt.Errorf("composited %d of %d frames", got, len(missing))
	}
	if err != nil {
		s.removeLeftovers()
	}
	return err
}

// removeLeftovers deletes the frames a compositor run wrote and did not hand
// over.
func (s *Source) removeLeftovers() {
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sample-") {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
}

// keepOnly drops every composited frame that is not in idx.
func (s *Source) keepOnly(idx []int) {
	for k, f := range s.frames {
		if !slices.Contains(idx, k) {
			_ = os.Remove(f.path)
			delete(s.frames, k)
		}
	}
}

func (s *Source) encodeVideo(ctx context.Context, env toolEnv, dir string, fr FrameSelection, r region, encs []encoding, name func(int, string) string) ([]Image, error) {
	times, run, err := fr.video(s.probe)
	if err != nil {
		return nil, err
	}
	asked := len(times)
	if run != nil {
		asked = run.count
	}
	if err := checkAnimate(encs, asked); err != nil {
		return nil, err
	}
	src, err := env.videoFromProbe(openRequest{
		path: s.ref, file: s.file, dec: s.dec, scratch: s.dir,
		video: &videoRequest{times: func(videoInfo) ([]int64, error) { return times, nil }, run: run},
	}, s.probe)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	src.destination = s.srgb
	if err := src.checkRegion(r); err != nil {
		return nil, err
	}
	if err := checkSampleScratch(src.videoPlanFor(r, encs), asked, s.p.res.Disk); err != nil {
		return nil, err
	}
	if err := src.decodeVideo(ctx, r, encs); err != nil {
		return nil, err
	}
	return encodeSamples(ctx, src, dir, r, encs, name)
}

// checkSampleScratch refuses a video call whose n samples, all of which are
// written to scratch before the first is encoded, would exceed disk.
func checkSampleScratch(plan videoPlan, n int, disk int64) error {
	if need := int64(n) * plan.sampleBytes(); need > disk {
		return tooLarge("%d samples of %dx%d are %d bytes of scratch, Resources.Disk is %d",
			n, plan.fw, plan.fh, need, disk)
	}
	return nil
}

// encodeSamples writes the frames src holds, composited or sampled, for each
// of encs.
func encodeSamples(ctx context.Context, src *frameSource, dir string, r region, encs []encoding, name func(int, string) string) ([]Image, error) {
	var images []Image
	for t, enc := range encs {
		base := len(images)
		if enc.animate {
			pos := src.positions()
			file := name(base, enc.ext)
			w, h, kept, err := src.encodeAnimation(ctx, r, enc, pos, filepath.Join(dir, file))
			if err != nil {
				return nil, err
			}
			indices := make([]int, len(kept))
			for j, p := range kept {
				indices[j] = src.frames[p].sourceFrame
			}
			images = append(images, Image{
				Name: file, Encoding: t, Width: w, Height: h, Frames: src.mapping(indices), Region: src.shown(r),
			})
			continue
		}
		files, err := src.encodeFiles(ctx, r, enc, dir, func(n int) string { return name(base+n, enc.ext) })
		if err != nil {
			return nil, err
		}
		for _, img := range files {
			img.Encoding = t
			images = append(images, img)
		}
	}
	return images, nil
}
