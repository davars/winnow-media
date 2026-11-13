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

// Kind is how a Source's frames are read.
type Kind string

// The kinds of Source.
const (
	// Still is an image file, possibly of several images (TIFF pages, icon
	// sizes).
	Still Kind = "still"
	// Animation is a GIF or animated WebP.
	Animation Kind = "animation"
	// Video is a video file.
	Video Kind = "video"
)

// OpenOptions are what Open cannot learn from the file's bytes.
type OpenOptions struct {
	// Type, when set, is the media type of the input: it must be in
	// Config.Types, and the decoder is pinned to it, replacing the type the
	// bytes and ImageMagick's detection name. It is how a file with no
	// signature (TGA, ICO and CUR, G3) or one detection names differently
	// (DNG) is read.
	Type string
	// Orientation, 1 to 8 as in EXIF, is the orientation of a still or
	// animation whose header read reports none; 0 leaves the file's own. It
	// is an error for a video.
	Orientation int
	// KeepDecode keeps a full-size decode of a still between Encode calls, in
	// the Source's private scratch, up to half of Resources.Disk for each
	// Source (the other half is room for the copy a later Encode reads). Only
	// the images the latest Encode selected stay kept. A later Encode of a
	// kept image reads the region it needs from the decode and writes the
	// bytes an Encode from a full-size decode would, which are not those of
	// an Encode without KeepDecode that decodes a JPEG or a still WebP reduced.
	// The Source never decodes a KeepDecode still reduced, so its frame and pixel
	// caps judge the full decode; an image over them is encoded as without
	// KeepDecode, as is a type or image whose kept decode would not be exact (more
	// than 16 bits a channel, floating-point samples, a colorspace a PAM
	// does not carry) or that does not fit. Animations and videos ignore
	// KeepDecode. Source.KeptBytes reports what is held.
	KeepDecode bool
}

// Info describes a Source. It is a snapshot: a later Encode can settle a size
// it left open.
type Info struct {
	// Type is the media type the input was read as.
	Type string
	Kind Kind
	// Width and Height are the size of the first frame or image as shown:
	// upright when Upright, else as stored.
	Width, Height int
	// Upright says Width and Height are upright; otherwise they are the size
	// as stored, and an Encode of that image settles them.
	Upright bool
	// Orientation is the EXIF orientation 1 to 8 the header read reported or
	// OpenOptions gave; 0 when neither did.
	Orientation int
	// OrientationGiven is set when Orientation is the one OpenOptions gave,
	// the file's header having reported none; it is false when the file's own
	// orientation applies or neither exists.
	OrientationGiven bool
	// Images describes each image of a Still, in the order Indices selects
	// them; its length is the number of images. It is nil for an animation
	// or a video.
	Images []ImageInfo
	// Animation is set when Kind is Animation, and nil otherwise.
	Animation *AnimationInfo
	// Video is set when Kind is Video, and nil otherwise.
	Video *VideoInfo
}

// AnimationInfo describes an animation.
type AnimationInfo struct {
	// Frames is the number of frames.
	Frames int
	// Delays is the display delay of each frame, a delay below 20 ms counting
	// as 100 ms, as browsers display it.
	Delays []time.Duration
	// Duration is the sum of Delays.
	Duration time.Duration
}

// VideoInfo describes a video.
type VideoInfo struct {
	// Duration is the video's duration, 0 when unknown.
	Duration time.Duration
	// FrameRate is the base frame rate, 0 when unknown.
	FrameRate float64
	// DeclaredFrames is the frame count the container declares, 0 when it
	// declares none.
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

// ImageInfo describes one image of a still. Width and Height are as for
// Info.
type ImageInfo struct {
	Width, Height int
	// Auxiliary is set for an image the decoder reports as a reduced-resolution
	// preview or a transparency mask of another, not a page of its own.
	Auxiliary bool
}

func (i Info) clone() Info {
	i.Images = slices.Clone(i.Images)
	if i.Animation != nil {
		a := *i.Animation
		a.Delays = slices.Clone(a.Delays)
		i.Animation = &a
	}
	if i.Video != nil {
		v := *i.Video
		i.Video = &v
	}
	return i
}

// Source is an input file the Processor opened, from which Encode writes
// images. It holds no process between calls, and the input file open until
// Close. Calls on one Source are serialised; Sources run in parallel. A Source
// is safe for concurrent use.
type Source struct {
	p *Processor
	// mu serialises calls and guards every field below up to imu.
	mu     sync.Mutex
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
	// read without mu.
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

// Open admits the file at path and reads what Encode needs to know of it,
// without decoding any pixel: the header of a still or animation, or one
// ffprobe of a video. The gate and the caps of the Processor apply: an input
// that is not a regular file within Caps.MaxInputBytes, of a type outside
// Config.Types (or opts.Type), or over a cap is refused with an error wrapping
// ErrUnsupportedType or ErrTooLarge. The Usage covers the tool runs of the
// call, also when it fails.
//
// Open opens the file once. The checks and Go's reads use that descriptor; on
// Linux so do the tools', so a file put at path afterwards is not read.
// Elsewhere a tool reads through a link to the path, and only the per-Encode
// identity check guards it.
//
// The caller must Close the Source, or close the Processor.
func (p *Processor) Open(ctx context.Context, path string, opts OpenOptions) (*Source, Usage, error) {
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
		p: p, file: in.file, path: in.abs, ref: in.ref, dir: in.dir, release: in.release, id: in.id,
		typ: in.typ, override: override, dec: in.decoder,
	}
	src.probed = in.probe
	src.reuse = opts.KeepDecode
	env := p.env(&m, src.file)
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
		Animation: &AnimationInfo{Frames: count, Delays: delays, Duration: total},
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
		Video: &VideoInfo{
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

// KeptBytes returns the bytes on disk the Source holds in the decodes
// OpenOptions.KeepDecode keeps: their PAM files and ICC profile files, the figure
// the disk budget counts. It is 0 for a Source opened without KeepDecode, before
// the first Encode, once a kept decode is dropped and after Close. It is safe
// to call at any time and does not wait for an Encode that is running: it
// reports the total as of the last completed change to what is kept.
func (s *Source) KeptBytes() int64 {
	return s.keptTotal.Load()
}

// Close closes the input and removes the Source's scratch. It is idempotent,
// and waits for a call that is running. Images already written are not
// affected.
func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// Encode reads the frames and region sel selects and writes one file per
// frame into dir for each of encodings, or with an Animate encoding one
// animated file; a frame identical to the one before it after resizing is
// dropped from an encoding's files. The files are named NNN.<ext>, numbered
// across the call in encoding order, then frame order; the Images are returned in the same order.
// Each output is upright, converted to sRGB when its source carries a profile,
// and carries no metadata, as Encoding says.
//
// dir must exist and be empty; on failure it is emptied again. The Source is
// checked against the file it was opened on first: if that file changed, the
// call is refused. A rename over the path, an unlink or a new link changes the
// file's status-change time, so it counts as a change. A Select that does not apply to the Source's kind, or a
// region outside a frame whose size is settled, is refused before any tool
// runs. The Usage covers the tool runs of this call alone, also when it fails.
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireEmptyDir(dir); err != nil {
		return nil, m.usage, err
	}
	if s.closed {
		return nil, m.usage, fmt.Errorf("source: %w", ErrClosed)
	}
	if err := s.checkIdentity(); err != nil {
		return nil, m.usage, err
	}
	if s.srgb == "" {
		if s.srgb, err = placeSRGB(s.dir); err != nil {
			return nil, m.usage, err
		}
	}
	env := s.p.env(&m, s.file)
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
	s.dropKeptExcept(idx)
	srcs := make([]*frameSource, len(idx))
	for f, k := range idx {
		if srcs[f], err = s.stillAt(ctx, env, k); err != nil {
			return nil, err
		}
		if err := srcs[f].checkRegion(r); err != nil {
			return nil, err
		}
	}
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
	if err := src.decodeVideo(ctx, r, encs); err != nil {
		return nil, err
	}
	return encodeSamples(ctx, src, dir, r, encs, name)
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
