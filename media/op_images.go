package media

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

//go:embed profiles/sRGB2014.icc.base64
var srgbProfileBase64 string

// srgbProfile is the sRGB destination profile of the image operations.
var srgbProfile = func() []byte {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(srgbProfileBase64))
	if err != nil {
		panic("media: embedded sRGB profile: " + err.Error())
	}
	return b
}()

// profileName is the scratch file the sRGB profile is written to inside the
// output directory for the duration of a run. The directory is empty when a
// run starts, so the name cannot collide.
const profileName = ".srgb2014.icc"

// outputCoders are the coders of the output formats; ImageMagick selects the
// coder from the file extension, so JPEG files are handled by "jpg".
var outputCoders = []string{"jpeg", "jpg", "png", "webp"}

var stillSpec = spec{
	inputs: []acceptedInput{
		{typeHEIC, decoder{"magick", "heic"}},
		{typeHEIF, decoder{"magick", "heic"}},
	},
	tools:  []string{"magick"},
	reads:  append([]string{"icc"}, outputCoders...),
	writes: outputCoders,
}

var animationSpec = spec{
	inputs: []acceptedInput{
		{typeGIF, decoder{"magick", "gif"}},
		{typeWebP, decoder{"magick", "webp"}},
	},
	tools:  []string{"magick", webpFramesTool},
	reads:  append([]string{"icc", "pam"}, outputCoders...),
	writes: append([]string{"info"}, outputCoders...),
}

// listLengthRefusal is what ImageMagick reports when an image list exceeds the
// policy's list-length ceiling.
const listLengthRefusal = "list length exceeds limit"

// sceneHeader is what one scene's header says, before any pixels decode.
type sceneHeader struct {
	delayCS       int
	hasICC        bool
	width, height int
	canvasW       int
	canvasH       int
	orientation   string
	format        string // ImageMagick's coder name for the file, as %m
}

// readHeaders reads the headers of the scenes selected by selector ("" for
// all, "[0]" for the first) of path, decoded with decoder, and refuses ones
// that exceed the caps before any pixel is decoded.
func readHeaders(ctx context.Context, env Env, dec decoder, path, selector string, animated bool) ([]sceneHeader, error) {
	ref := dec.name + ":" + path + selector
	scenes, err := identifyHeaders(ctx, env, ref, true)
	if err != nil {
		return nil, err
	}
	if err := checkSceneHeaders(scenes, animated); err != nil {
		return nil, err
	}
	if err := completeProfile(ctx, env, ref, path, scenes); err != nil {
		return nil, err
	}
	return scenes, nil
}

// completeProfile fills in hasICC for the one scene of a file whose coder
// does not report its profile under -ping. ref is the identify input the
// headers were read from, path the file it names. A WebP still is answered by
// its chunk list. A JXL still is decoded, which the callers' caps already
// allow; a multi-scene JXL is not, since the operations refuse it. An
// animated WebP is not examined: webp-frames reports its frames' profiles.
func completeProfile(ctx context.Context, env Env, ref, path string, scenes []sceneHeader) error {
	if len(scenes) != 1 {
		return nil
	}
	switch scenes[0].format {
	case "WEBP":
		hasICC, err := webpHasICC(path)
		if err != nil {
			return fmt.Errorf("reading headers: %w", err)
		}
		scenes[0].hasICC = hasICC
	case "JXL":
		full, err := identifyHeaders(ctx, env, ref, false)
		if err != nil {
			return err
		}
		if len(full) != 1 {
			return fmt.Errorf("reading headers: 1 scene, then %d", len(full))
		}
		scenes[0].hasICC = full[0].hasICC
	}
	return nil
}

// identifyHeaders runs one identify over inputRef, a path optionally prefixed
// by a coder and suffixed by a scene selector, and parses each scene's
// header. ping reads headers only. A file with more scenes than the list
// length ceiling of LimitArgs is refused with an error wrapping ErrTooLarge,
// before any frame is held.
func identifyHeaders(ctx context.Context, env Env, inputRef string, ping bool) ([]sceneHeader, error) {
	args := append([]string{"identify"}, LimitArgs()...)
	if ping {
		args = append(args, "-ping")
	}
	args = append(args, "-format", sceneHeaderFormat, inputRef)
	res, err := env.Run(ctx, "magick", args...)
	if err != nil {
		if strings.Contains(err.Error(), listLengthRefusal) {
			return nil, tooLarge("file holds more than %s scenes, the limit is %d", policyListLength, maxAnimationFrames)
		}
		return nil, fmt.Errorf("reading headers: %w", err)
	}
	return parseSceneHeaders(res.Stdout)
}

// sceneHeaderFormat is the identify -format that parseSceneHeaders reads. %m
// is the coder that read the file, which -ping reports even where it cannot
// report the profiles.
const sceneHeaderFormat = "%T|%[profiles]|%w|%h|%W|%H|%m|%[orientation]|END\n"

// parseSceneHeaders reads the output of identify -format sceneHeaderFormat.
func parseSceneHeaders(out []byte) ([]sceneHeader, error) {
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil, fmt.Errorf("identify returned no scenes")
	}
	var scenes []sceneHeader
	for _, line := range strings.Split(trimmed, "\n") {
		f := strings.Split(line, "|")
		if len(f) != 9 || f[8] != "END" {
			return nil, fmt.Errorf("identify line %q missing fields", line)
		}
		var nums [5]int
		for i, idx := range []int{0, 2, 3, 4, 5} {
			n, err := strconv.Atoi(f[idx])
			if err != nil {
				return nil, fmt.Errorf("parsing %q in %q: %w", f[idx], line, err)
			}
			nums[i] = n
		}
		scenes = append(scenes, sceneHeader{
			delayCS: nums[0], hasICC: profileListHasICC(f[1]),
			width: nums[1], height: nums[2], canvasW: nums[3], canvasH: nums[4],
			format: f[6], orientation: f[7],
		})
	}
	return scenes, nil
}

// uprightSize returns the dimensions w x h has after -auto-orient applied to
// an image tagged with orientation.
func uprightSize(w, h int, orientation string) (int, int) {
	switch orientation {
	case "LeftTop", "RightTop", "RightBottom", "LeftBottom":
		return h, w
	}
	return w, h
}

// fitSize returns the largest size at most w x h, with the same aspect
// ratio, whose longest side is at most maxEdge and whose pixel count is at
// most maxArea. resize is false when w x h already satisfies both, so the
// image is never upscaled.
func fitSize(w, h, maxEdge, maxArea int) (tw, th int, resize bool) {
	scale := math.Min(1, float64(maxEdge)/float64(max(w, h)))
	scale = math.Min(scale, math.Sqrt(float64(maxArea)/(float64(w)*float64(h))))
	if scale >= 1 {
		return w, h, false
	}
	const eps = 1e-9
	tw = max(1, int(math.Floor(float64(w)*scale+eps)))
	th = max(1, int(math.Floor(float64(h)*scale+eps)))
	for tw*th > maxArea && (tw > 1 || th > 1) {
		if tw >= th && tw > 1 {
			tw--
		} else {
			th--
		}
	}
	return tw, th, true
}

// imagePipeline describes the decode and pixel stages the image operations
// share.
type imagePipeline struct {
	decoder     decoder
	inputPath   string
	selector    string // appended to the input path, "[0]" for a still
	coalesce    bool
	deleteExpr  string
	profilePath string
	profiled    bool
	flatten     bool
	resizeTo    *[2]int
}

// args builds the magick arguments up to, not including, the output stage.
// Transparency is flattened before the resample so the white does not bleed
// through the filter, and metadata is stripped last, after the profile
// conversion that needed it.
func (p imagePipeline) args() []string {
	args := append([]string{}, LimitArgs()...)
	args = append(args, p.decoder.name+":"+p.inputPath+p.selector)
	if p.coalesce {
		args = append(args, "-coalesce")
		if p.deleteExpr != "" {
			args = append(args, "-delete", p.deleteExpr)
		}
	}
	args = append(args, "-auto-orient")
	args = append(args, colorArgs(p.profilePath, p.profiled)...)
	if p.flatten {
		args = append(args, "-background", "white", "-alpha", "remove", "-alpha", "off")
	}
	if p.resizeTo != nil {
		args = append(args, "-resize", fmt.Sprintf("%dx%d!", p.resizeTo[0], p.resizeTo[1]))
	}
	return append(args, "-strip")
}

// withProfile makes the sRGB profile available as a file for the duration of
// fn when profiled is set. The file lives in outDir, so it follows the
// output directory's ownership and is gone before the operation returns.
func withProfile(outDir string, profiled bool, fn func(profilePath string) error) error {
	if !profiled {
		return fn("")
	}
	path := filepath.Join(outDir, profileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	_, werr := f.Write(srgbProfile)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return fn(path)
}

// identifySizes reports the width and height of each of paths, which must
// each hold exactly one scene.
func identifySizes(ctx context.Context, env Env, paths []string) ([][2]int, error) {
	args := append([]string{"identify"}, LimitArgs()...)
	args = append(args, "-ping", "-format", "%w %h\n")
	args = append(args, paths...)
	res, err := env.Run(ctx, "magick", args...)
	if err != nil {
		return nil, fmt.Errorf("identifying outputs: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("outputs hold %d scenes, want %d", len(lines), len(paths))
	}
	sizes := make([][2]int, len(lines))
	for i, line := range lines {
		var w, h int
		if _, err := fmt.Sscanf(line, "%d %d", &w, &h); err != nil {
			return nil, fmt.Errorf("parsing %q: %w", line, err)
		}
		sizes[i] = [2]int{w, h}
	}
	return sizes, nil
}

func (e Env) still(ctx context.Context, dec decoder, path string, enc encoding, dir string) (Image, error) {
	scenes, err := readHeaders(ctx, e, dec, path, "[0]", false)
	if err != nil {
		return Image{}, err
	}
	head := scenes[0]
	w, h := uprightSize(head.width, head.height, head.orientation)
	if w < 1 || h < 1 {
		return Image{}, fmt.Errorf("input reports size %dx%d", w, h)
	}
	tw, th, resize := fitSize(w, h, enc.maxEdge, enc.maxArea)

	name := "still" + enc.ext
	err = withProfile(dir, head.hasICC, func(profilePath string) error {
		p := imagePipeline{
			decoder: dec, inputPath: path, selector: "[0]",
			profilePath: profilePath, profiled: head.hasICC,
			flatten: enc.format == JPEG,
		}
		if resize {
			p.resizeTo = &[2]int{tw, th}
		}
		args := append(p.args(), "-quality", strconv.Itoa(enc.quality), filepath.Join(dir, name))
		_, err := e.Run(ctx, "magick", args...)
		return err
	})
	if err != nil {
		return Image{}, fmt.Errorf("encoding still: %w", err)
	}
	sizes, err := identifySizes(ctx, e, []string{filepath.Join(dir, name)})
	if err != nil {
		return Image{}, err
	}
	return Image{Name: name, Width: sizes[0][0], Height: sizes[0][1]}, nil
}

// animationLayout is what the header read of an animation decided: the
// frames, the canvas and the upright size of the canvas.
type animationLayout struct {
	infos       []FrameInfo
	cw, ch      int // canvas as stored, before any rotation
	uw, uh      int // canvas after rotation
	orientation string
}

// readAnimation reads the headers of every frame of path and refuses the file
// if it exceeds a cap. It is the first step of every animation operation.
func (e Env) readAnimation(ctx context.Context, dec decoder, path string) (animationLayout, error) {
	scenes, err := readHeaders(ctx, e, dec, path, "", true)
	if err != nil {
		return animationLayout{}, err
	}
	l := animationLayout{infos: make([]FrameInfo, len(scenes)), orientation: scenes[0].orientation}
	for i, s := range scenes {
		l.infos[i] = FrameInfo{delayCS: s.delayCS, hasICC: s.hasICC}
		w, h := s.canvas()
		l.cw, l.ch = max(l.cw, w), max(l.ch, h)
	}
	if l.cw < 1 || l.ch < 1 {
		return animationLayout{}, fmt.Errorf("input reports size %dx%d", l.cw, l.ch)
	}
	l.uw, l.uh = uprightSize(l.cw, l.ch, l.orientation)
	return l, nil
}

// animationFrames writes the frames pick chooses, given the number of source
// frames, which must be indices in ascending order without repeats.
func (e Env) animationFrames(ctx context.Context, dec decoder, path string, enc encoding, pick func(sourceFrames int) ([]int, error), dir string) ([]Image, error) {
	layout, err := e.readAnimation(ctx, dec, path)
	if err != nil {
		return nil, err
	}
	infos, cw, ch := layout.infos, layout.cw, layout.ch
	orientation := layout.orientation
	tw, th, resize := fitSize(layout.uw, layout.uh, enc.maxEdge, enc.maxArea)

	sourceFrames := len(infos)
	selected, err := pick(sourceFrames)
	if err != nil {
		return nil, err
	}
	if err := checkReturnedPixels(len(selected), cw, ch); err != nil {
		return nil, err
	}
	if dec.name == "gif" {
		own := make([]bool, len(infos))
		for i, f := range infos {
			own[i] = f.hasICC
		}
		return e.compositedFrames(ctx, path, enc, dir, selected, compositeGeometry{
			sourceFrames: len(infos), cw: cw, ch: ch, tw: tw, th: th, resize: resize, own: own,
		})
	}
	if dec.name == "webp" && len(infos) > 1 {
		return e.compositedFrames(ctx, path, enc, dir, selected, compositeGeometry{
			sourceFrames: len(infos), cw: cw, ch: ch, tw: tw, th: th, resize: resize,
			external: webpFramesTool, orientation: orientation,
		})
	}
	profiled, err := selectedProfiled(infos, selected)
	if err != nil {
		return nil, err
	}

	var kept []int
	err = withProfile(dir, profiled, func(profilePath string) error {
		p := imagePipeline{
			decoder: dec, inputPath: path, coalesce: true,
			deleteExpr:  DeleteExpr(selected, sourceFrames),
			profilePath: profilePath, profiled: profiled,
			flatten: enc.format == JPEG,
		}
		if resize {
			p.resizeTo = &[2]int{tw, th}
		}
		probeOut, err := e.Run(ctx, "magick", append(p.args(), "-format", "%s %#\n", "info:")...)
		if err != nil {
			return fmt.Errorf("probing selected frames: %w", err)
		}
		probed, err := parseProbe(probeOut.Stdout)
		if err != nil {
			return err
		}
		if len(probed) == 0 {
			return fmt.Errorf("frame selection probe returned no frames")
		}
		lastSig := ""
		for i, f := range probed {
			if i > 0 && f.signature == lastSig {
				continue
			}
			kept = append(kept, f.sourceFrame)
			lastSig = f.signature
		}

		p.deleteExpr = DeleteExpr(kept, sourceFrames)
		pattern := strings.ReplaceAll(filepath.Join(dir, "frame-"), "%", "%%") + "%03d" + enc.ext
		args := append(p.args(), "-scene", "0", "+adjoin", "-quality", strconv.Itoa(enc.quality), pattern)
		if _, err := e.Run(ctx, "magick", args...); err != nil {
			return fmt.Errorf("encoding frames: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	names, err := frameFileNames(dir, enc.ext)
	if err != nil {
		return nil, err
	}
	if len(names) != len(kept) {
		return nil, fmt.Errorf("wrote %d files, kept %d frames", len(names), len(kept))
	}
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(dir, n)
	}
	sizes, err := identifySizes(ctx, e, paths)
	if err != nil {
		return nil, err
	}
	images := make([]Image, len(names))
	for i, n := range names {
		images[i] = Image{Name: n, Width: sizes[i][0], Height: sizes[i][1], Frame: kept[i]}
	}
	return images, nil
}

// webpFramesTool is the program that composites the frames of an animated
// WebP.
const webpFramesTool = "webp-frames"

// compositeGeometry is what the header read and the sizing decided about an
// animation composited outside ImageMagick.
type compositeGeometry struct {
	sourceFrames int
	cw, ch       int // canvas as stored, before any rotation
	tw, th       int // output size
	resize       bool
	// own[i] is set when the header read found an ICC profile on frame i. It
	// is nil for a WebP, whose tool reports the profile with each frame.
	own []bool
	// external names the tool that composites, empty for the GIF compositor.
	external string
	// orientation is the EXIF orientation the header read found. The
	// composited frames carry none, so it is applied explicitly.
	orientation string
}

// compositedFrames writes the selected frames of a GIF or animated WebP. The
// sandboxed compositor decodes the file and composites it one canvas at a
// time, writing each selected frame as a full-canvas PAM file; ImageMagick
// encodes them in batches as they arrive, so it never holds more than a
// batch.
//
// The compositor says which ICC profile, if any, each frame carries, as the
// frames do after -coalesce. A frame is assigned its profile and converted to
// sRGB; one without is left as it is. A batch holds frames of one profile.
func (e Env) compositedFrames(ctx context.Context, path string, enc encoding, dir string, selected []int, g compositeGeometry) ([]Image, error) {
	tail := []string{}
	if enc.format == JPEG {
		tail = append(tail, "-background", "white", "-alpha", "remove", "-alpha", "off")
	}
	if g.resize {
		tail = append(tail, "-resize", fmt.Sprintf("%dx%d!", g.tw, g.th))
	}
	tail = append(tail, "-strip")

	var images []Image
	err := withProfile(dir, slices.Contains(g.own, true) || g.external != "", func(srgbPath string) error {
		usedProfiles := map[int]bool{}
		defer func() {
			for id := range usedProfiles {
				_ = os.Remove(filepath.Join(dir, composedProfileName(id)))
			}
		}()
		encoder := &sampleEncoder{
			env: e, coder: "pam", enc: enc, dir: dir, framePixels: g.cw * g.ch,
			keep: func(i, n, w, h int) {
				images = append(images, Image{Name: frameName(n, enc.ext), Width: w, Height: h, Frame: selected[i]})
			},
		}
		var pending []string
		pendingProfile := 0
		flush := func() error {
			defer removeFiles(pending)
			encoder.middle = nil
			if g.orientation != "" && g.orientation != "Undefined" {
				encoder.middle = append(encoder.middle, "-orient", g.orientation)
			}
			encoder.middle = append(encoder.middle, "-auto-orient")
			if pendingProfile > 0 {
				encoder.middle = append(encoder.middle, colorArgs(filepath.Join(dir, composedProfileName(pendingProfile)), true)...)
				encoder.middle = append(encoder.middle, colorArgs(srgbPath, true)...)
			}
			encoder.middle = append(encoder.middle, tail...)
			err := encoder.encode(ctx, pending)
			pending = nil
			return err
		}
		ordinal := 0
		req := composeRequest{
			path: path, dir: dir, selected: selected, inFlight: encoder.batchSize(),
			frames: g.sourceFrames, width: g.cw, height: g.ch, own: g.own, external: g.external, consumeTimeout: composeTimeout,
		}
		err := e.compose(ctx, req, func(p string, profile int) error {
			if ordinal >= len(selected) {
				return errors.New("the compositor wrote more frames than selected")
			}
			ordinal++
			if len(pending) > 0 && profile != pendingProfile {
				if err := flush(); err != nil {
					return err
				}
			}
			pendingProfile = profile
			if profile > 0 {
				usedProfiles[profile] = true
			}
			pending = append(pending, p)
			if len(pending) >= encoder.batchSize() {
				return flush()
			}
			return nil
		})
		if err != nil {
			removeFiles(pending)
			return err
		}
		if err := flush(); err != nil {
			return err
		}
		if encoder.seen != len(selected) {
			return fmt.Errorf("composited %d of %d frames", encoder.seen, len(selected))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return images, nil
}

// removeFiles deletes paths, ignoring errors.
func removeFiles(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// frameFileNames lists the numbered frame files in dir, in order, and
// requires the numbering to be consecutive from zero.
func frameFileNames(dir, ext string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "frame-") {
			names = append(names, e.Name())
		}
	}
	for i, n := range names {
		if want := fmt.Sprintf("frame-%03d%s", i, ext); n != want {
			return nil, fmt.Errorf("frame file %q found where %q was expected", n, want)
		}
	}
	return names, nil
}
