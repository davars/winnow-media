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
	reads:  []string{"icc"},
	writes: append([]string{"info"}, outputCoders...),
}

var animationSpec = spec{
	inputs: []acceptedInput{
		{typeGIF, decoder{"magick", "gif"}},
		{typeWebP, decoder{"magick", "webp"}},
	},
	tools:  []string{"magick", webpFramesTool},
	reads:  []string{"icc", "pam"},
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
// headers were read from, path the file it names. A WebP is answered by its
// chunk list when the layout allows (see webpICC) and decoded otherwise; a
// JXL is always decoded, as libjxl gives every JXL a profile. The decode is
// bounded only by whatever caps the caller applied to the headers first. A
// file of more than one scene is left as read.
func completeProfile(ctx context.Context, env Env, ref, path string, scenes []sceneHeader) error {
	if len(scenes) != 1 {
		return nil
	}
	switch scenes[0].format {
	case "WEBP":
		if hasICC, known := webpICC(path); known {
			scenes[0].hasICC = hasICC
			return nil
		}
	case "JXL":
	default:
		return nil
	}
	full, err := identifyHeaders(ctx, env, ref, false)
	if err != nil {
		return err
	}
	if len(full) != 1 {
		return fmt.Errorf("reading headers: 1 scene, then %d", len(full))
	}
	scenes[0].hasICC = full[0].hasICC
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
// most maxArea, each side rounded down. resize is false when w x h already
// satisfies both, so the image is never upscaled.
//
// It is the specification of fitExpr, which does the same arithmetic in
// ImageMagick. A side of at most one pixel is raised to 1, and the other then
// limited to what the area allows; for any other shape the rounding down
// already satisfies the area.
func fitSize(w, h, maxEdge, maxArea int) (tw, th int, resize bool) {
	scale := math.Min(1, math.Min(float64(maxEdge)/float64(max(w, h)), math.Sqrt(float64(maxArea)/(float64(w)*float64(h)))))
	if scale >= 1 {
		return w, h, false
	}
	// eps keeps a product such as (E/m)*m that lands a hair below the
	// integer E from flooring to E-1; fitExpr uses the same value.
	const eps = 1e-9
	tw = max(1, int(math.Floor(float64(w)*scale+eps)))
	th = max(1, int(math.Floor(float64(h)*scale+eps)))
	tw = max(1, min(tw, maxArea/th))
	th = max(1, min(th, maxArea/tw))
	return tw, th, true
}

// placeSRGB writes the package's sRGB profile to a new file in dir and
// returns its path. The file is left for the caller to remove with dir.
func placeSRGB(dir string) (string, error) {
	path := filepath.Join(dir, profileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(srgbProfile)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return "", werr
	}
	return path, nil
}

// withProfile makes the sRGB profile available as a file for the duration of
// fn when profiled is set. The file lives in outDir, so it follows the
// output directory's ownership and is gone before the operation returns.
func withProfile(outDir string, profiled bool, fn func(profilePath string) error) error {
	if !profiled {
		return fn("")
	}
	path, err := placeSRGB(outDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	return fn(path)
}

// runEncode runs one encode and returns what it reported, one entry per frame.
func (e Env) runEncode(ctx context.Context, args []string) ([]encodedFrame, error) {
	res, err := e.Run(ctx, "magick", args...)
	if err != nil {
		return nil, err
	}
	return parseEncoded(res.Stdout)
}

func (e Env) still(ctx context.Context, dec decoder, path string, enc encoding, dir string) (Image, error) {
	scenes, err := readHeaders(ctx, e, dec, path, "[0]", false)
	if err != nil {
		return Image{}, err
	}
	if head := scenes[0]; head.width < 1 || head.height < 1 {
		return Image{}, fmt.Errorf("input reports size %dx%d", head.width, head.height)
	}

	name := "still" + enc.ext
	var encoded []encodedFrame
	err = withProfile(dir, scenes[0].hasICC, func(profilePath string) error {
		var err error
		encoded, err = e.runEncode(ctx, encodeArgs(
			[]encodeFrame{{input: dec.name + ":" + path + "[0]", convertTo: profilePath}},
			encodeSource{}, enc, encodeOutput{kind: outputStill, path: filepath.Join(dir, name)}))
		return err
	})
	if err != nil {
		return Image{}, fmt.Errorf("encoding still: %w", err)
	}
	if len(encoded) != 1 {
		return Image{}, fmt.Errorf("encoding still: %d results", len(encoded))
	}
	return Image{Name: name, Width: encoded[0].width, Height: encoded[0].height}, nil
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
	return layoutOf(scenes)
}

// layoutOf is the layout of the scenes a header read found.
func layoutOf(scenes []sceneHeader) (animationLayout, error) {
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
			sourceFrames: len(infos), cw: cw, ch: ch,
			own: own, orientation: layout.orientation,
		})
	}
	if dec.name == "webp" && len(infos) > 1 {
		return e.compositedFrames(ctx, path, enc, dir, selected, compositeGeometry{
			sourceFrames: len(infos), cw: cw, ch: ch,
			external: webpFramesTool, orientation: layout.orientation,
		})
	}
	profiled, err := selectedProfiled(infos, selected)
	if err != nil {
		return nil, err
	}
	if len(selected) != 1 {
		return nil, fmt.Errorf("selected %d frames of a one-frame file", len(selected))
	}

	// ImageMagick reads the one frame whole and composes it onto its canvas.
	var images []Image
	err = withProfile(dir, profiled, func(profilePath string) error {
		encoder := &sampleEncoder{
			env: e, src: encodeSource{},
			enc: enc, dir: dir, framePixels: cw * ch,
			keep: func(i, n, w, h int) {
				images = append(images, Image{Name: frameName(n, enc.ext), Width: w, Height: h, Frame: selected[i]})
			},
		}
		return encoder.encode(ctx, []encodeFrame{{
			input: dec.name + ":" + path, prelude: []string{"-coalesce"}, convertTo: profilePath,
		}})
	})
	if err != nil {
		return nil, err
	}
	return images, nil
}

// webpFramesTool is the program that composites the frames of an animated
// WebP.
const webpFramesTool = "webp-frames"

// compositeGeometry is what the header read decided about an animation
// composited outside ImageMagick.
type compositeGeometry struct {
	sourceFrames int
	cw, ch       int // canvas as stored, before any rotation
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
// sRGB; one without is left as it is.
func (e Env) compositedFrames(ctx context.Context, path string, enc encoding, dir string, selected []int, g compositeGeometry) ([]Image, error) {
	var images []Image
	err := withProfile(dir, slices.Contains(g.own, true) || g.external != "", func(srgbPath string) error {
		usedProfiles := map[int]bool{}
		defer func() {
			for id := range usedProfiles {
				_ = os.Remove(filepath.Join(dir, composedProfileName(id)))
			}
		}()
		encoder := &sampleEncoder{
			env: e, src: encodeSource{orientation: g.orientation},
			enc: enc, dir: dir, framePixels: g.cw * g.ch,
			keep: func(i, n, w, h int) {
				images = append(images, Image{Name: frameName(n, enc.ext), Width: w, Height: h, Frame: selected[i]})
			},
		}
		var pending []encodeFrame
		var pendingPaths []string
		flush := func() error {
			defer removeFiles(pendingPaths)
			err := encoder.encode(ctx, pending)
			pending, pendingPaths = nil, nil
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
			f := encodeFrame{input: "pam:" + p}
			if profile > 0 {
				usedProfiles[profile] = true
				f.convertTo = srgbPath
				f.assign = filepath.Join(dir, composedProfileName(profile))
			}
			pending = append(pending, f)
			pendingPaths = append(pendingPaths, p)
			if len(pending) >= encoder.batchSize() {
				return flush()
			}
			return nil
		})
		if err != nil {
			removeFiles(pendingPaths)
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
