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
	// subfile is the TIFF subfile type, read only by the gate.
	subfile string
	// soft is set when the frame is palette-based or declares alpha, which
	// picks the resize filter.
	soft bool
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

// fullReadFormats are the coders whose -ping read leaves out the alpha
// channel (TIFF, JPEG 2000, OpenEXR) or the palette (PCX).
var fullReadFormats = map[string]bool{"TIFF": true, "JP2": true, "EXR": true, "PCX": true}

// fullSoftAt is fullSoft for scene i of the file.
func fullSoftAt(ctx context.Context, env Env, ref string, i int) (bool, error) {
	full, err := identifyHeaders(ctx, env, ref+"["+strconv.Itoa(i)+"]", false)
	if err != nil {
		return false, err
	}
	if len(full) != 1 {
		return false, fmt.Errorf("reading headers: 1 scene, then %d", len(full))
	}
	return full[0].soft, nil
}

// identifyHeaders runs one identify over inputRef, a path optionally prefixed
// by a coder and suffixed by a scene selector, and parses each scene's
// header. ping reads headers only. A file with more scenes than the list
// length ceiling is refused with an error wrapping ErrTooLarge,
// before any frame is held, and a read that reaches the time limit with one
// wrapping ErrTimeLimit.
func identifyHeaders(ctx context.Context, env Env, inputRef string, ping bool) ([]sceneHeader, error) {
	return identifyHeadersAs(ctx, env, inputRef, ping, sceneHeaderFormat)
}

// identifyHeadersAs is identifyHeaders reading format, which must be
// sceneHeaderFormat or gateHeaderFormat.
func identifyHeadersAs(ctx context.Context, env Env, inputRef string, ping bool, format string) ([]sceneHeader, error) {
	args := []string{"identify"}
	if ping {
		args = append(args, "-ping")
	}
	args = append(args, "-format", format, inputRef)
	res, err := env.runMagick(ctx, args...)
	if err != nil {
		if !errors.Is(err, ErrTimeLimit) && strings.Contains(err.Error(), listLengthRefusal) {
			return nil, tooLarge("file holds more than %s scenes, the limit is %d", policyListLength, maxAnimationFrames)
		}
		return nil, fmt.Errorf("reading headers: %w", err)
	}
	return parseSceneHeaders(res.Stdout)
}

// stillScenes is the scene selector of the header read of a still of any
// number of images: one past maxStillImages, so that a file of more is seen to
// be, and no read lists more. ImageMagick's list-length limit does not bound a
// -ping read.
const (
	maxStillImages = 2048
	stillScenes    = "[0-2048]"
)

// checkStillImages refuses a still of more images than maxStillImages.
func checkStillImages(scenes []sceneHeader) error {
	if len(scenes) > maxStillImages {
		return tooLarge("file holds more than %d images, the limit is %d", maxStillImages, maxStillImages)
	}
	return nil
}

// sceneHeaderFormat is the identify -format that parseSceneHeaders reads. %m
// is the coder that read the file, which -ping reports even where it cannot
// report the profiles; %r and %A are the image class and alpha the coder
// declares.
const sceneHeaderFormat = "%T|%[profiles]|%w|%h|%W|%H|%m|%[orientation]|%r|%A|END\n"

// gateHeaderFormat is sceneHeaderFormat with the TIFF subfile type, which
// the gate reads from the header run that names an unnamed file's coder. A
// coder without the property prints a warning and an empty field.
const gateHeaderFormat = "%T|%[profiles]|%w|%h|%W|%H|%m|%[orientation]|%r|%A|%[tiff:subfiletype]|END\n"

// declaresSoft reports whether a frame is palette-based or has alpha, from
// the image class %r and the alpha trait %A. Gray is never a palette:
// ImageMagick gives a grayscale JPEG or PSD a colormap its file does not
// declare.
func declaresSoft(class, alpha string) bool {
	if alpha != "Undefined" && alpha != "False" && alpha != "" {
		return true
	}
	return strings.HasPrefix(class, "PseudoClass") && !strings.Contains(class, "Gray")
}

// parseSceneHeaders reads the output of identify -format sceneHeaderFormat.
func parseSceneHeaders(out []byte) ([]sceneHeader, error) {
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil, fmt.Errorf("identify returned no scenes")
	}
	var scenes []sceneHeader
	for _, line := range strings.Split(trimmed, "\n") {
		f := strings.Split(line, "|")
		if (len(f) != 11 && len(f) != 12) || f[len(f)-1] != "END" {
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
		scene := sceneHeader{
			delayCS: nums[0], hasICC: profileListHasICC(f[1]),
			width: nums[1], height: nums[2], canvasW: nums[3], canvasH: nums[4],
			format: f[6], orientation: f[7], soft: declaresSoft(f[8], f[9]),
		}
		if len(f) == 12 {
			scene.subfile = f[10]
		}
		scenes = append(scenes, scene)
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

// runEncode runs one encode and returns what it reported, one entry per frame.
// A run that failed, hit ImageMagick's time limit (ErrTimeLimit) or reported
// a frame count or size outside chk is an error, and removes the outputs chk
// names.
func (e Env) runEncode(ctx context.Context, args []string, chk encodeCheck) ([]encodedFrame, error) {
	res, err := e.runMagick(ctx, args...)
	var frames []encodedFrame
	if err == nil {
		var report []byte
		var uprights [][2]int
		if report, uprights, err = splitUpright(res.Stdout); err == nil {
			if frames, err = parseEncoded(report, chk.signed); err == nil {
				if err = chk.checkUpright(uprights, len(frames)); err == nil {
					if err = chk.checkFrames(frames); err == nil && chk.uprightOut != nil && len(uprights) > 0 {
						*chk.uprightOut = uprights[0]
					}
				}
			}
		}
	}
	if err != nil {
		chk.removeOutputs()
		return nil, err
	}
	return frames, nil
}

// animationLayout is what the header read of an animation decided: the
// frames, the canvas and the upright size of the canvas.
type animationLayout struct {
	infos       []FrameInfo
	cw, ch      int // canvas as stored, before any rotation
	uw, uh      int // canvas after rotation
	orientation string
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

// webpFramesTool is the program that composites the frames of an animated
// WebP.
const webpFramesTool = "webp-frames"

// removeFiles deletes paths, ignoring errors.
func removeFiles(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}
