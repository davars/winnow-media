package media

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	stillName  = "still"
	framesName = "frames"
)

//go:embed profiles/sRGB2014.icc.base64
var srgbProfileBase64 string

// srgbProfile is the sRGB destination profile of the image recipes.
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

// imageFormats maps the `format` parameter to the output extension and
// media type.
var imageFormats = map[string]struct{ ext, mediaType string }{
	"jpeg": {".jpg", "image/jpeg"},
	"webp": {".webp", "image/webp"},
	"png":  {".png", "image/png"},
}

func sizingParams() []Param {
	return []Param{
		{Name: "max_edge", Kind: KindInt, Min: 16, Max: 16384, Default: "2576"},
		{Name: "max_area", Kind: KindInt, Min: 256, Max: 100_000_000, Default: "3750000"},
		{Name: "format", Kind: KindEnum, Values: []string{"jpeg", "webp", "png"}, Default: "jpeg"},
		{Name: "quality", Kind: KindInt, Min: 1, Max: 100, Default: "90"},
	}
}

var imageOutputTypes = []string{"image/jpeg", "image/webp", "image/png"}

func stillRecipe() Recipe {
	return Recipe{
		Name:     stillName,
		Revision: 1,
		Inputs: []Input{
			{MediaType: "image/heic", Decoder: Decoder{Tool: "magick", Name: "heic"}},
			{MediaType: "image/heif", Decoder: Decoder{Tool: "magick", Name: "heic"}},
		},
		Params: sizingParams(),
		Outputs: Output{
			MediaTypes: slicesClone(imageOutputTypes),
			Fields:     []Field{FieldSourceFrame, FieldSourceTimeMS, FieldWidth, FieldHeight},
		},
		Engines: []string{"magick"},
	}
}

func framesRecipe() Recipe {
	return Recipe{
		Name:     framesName,
		Revision: 1,
		Inputs: []Input{
			{MediaType: "image/gif", Decoder: Decoder{Tool: "magick", Name: "gif"}},
			{MediaType: "image/webp", Decoder: Decoder{Tool: "magick", Name: "webp"}},
		},
		Params: append([]Param{
			{Name: "max_frames", Kind: KindInt, Min: 1, Max: 64, Default: "16"},
		}, sizingParams()...),
		Outputs: Output{
			MediaTypes: slicesClone(imageOutputTypes),
			Fields:     []Field{FieldSourceFrame, FieldSourceTimeMS, FieldWidth, FieldHeight},
		},
		Engines: []string{"magick"},
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

// sceneHeader is what one scene's header says, before any pixels decode.
type sceneHeader struct {
	delayCS       int
	hasICC        bool
	width, height int
	canvasW       int
	canvasH       int
	orientation   string
}

// identifyScenes reads the headers of the scenes selected by selector ("" for
// all, "[0]" for the first) of path, decoded with decoder.
func identifyScenes(ctx context.Context, env Env, decoder Decoder, path, selector string) ([]sceneHeader, error) {
	args := append([]string{"identify"}, LimitArgs()...)
	args = append(args, "-format", "%T|%[profiles]|%w|%h|%W|%H|%[orientation]|END\n", decoder.Name+":"+path+selector)
	res, err := env.Run(ctx, "magick", args...)
	if err != nil {
		return nil, fmt.Errorf("reading headers: %w", err)
	}
	trimmed := strings.TrimRight(string(res.Stdout), "\n")
	if trimmed == "" {
		return nil, fmt.Errorf("identify returned no scenes")
	}
	var scenes []sceneHeader
	for _, line := range strings.Split(trimmed, "\n") {
		f := strings.Split(line, "|")
		if len(f) != 8 || f[7] != "END" {
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
			orientation: f[6],
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

// imagePipeline describes the decode and pixel stages the image recipes
// share.
type imagePipeline struct {
	decoder     Decoder
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
	args = append(args, p.decoder.Name+":"+p.inputPath+p.selector)
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
// output directory's ownership and is gone before the recipe returns.
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
	args = append(args, "-format", "%w %h\n")
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

type imageParams struct {
	maxEdge, maxArea, quality int
	format                    string
}

func readImageParams(p paramValues) imageParams {
	return imageParams{
		maxEdge: p.int("max_edge"), maxArea: p.int("max_area"),
		quality: p.int("quality"), format: p.str("format"),
	}
}

func runStill(ctx context.Context, env Env, in runInput) ([]OutputFile, error) {
	ip := readImageParams(in.params)
	format := imageFormats[ip.format]

	scenes, err := identifyScenes(ctx, env, in.decoder, in.inputPath, "[0]")
	if err != nil {
		return nil, err
	}
	head := scenes[0]
	w, h := uprightSize(head.width, head.height, head.orientation)
	if w < 1 || h < 1 {
		return nil, fmt.Errorf("input reports size %dx%d", w, h)
	}
	tw, th, resize := fitSize(w, h, ip.maxEdge, ip.maxArea)

	name := "still" + format.ext
	err = withProfile(in.outputDir, head.hasICC, func(profilePath string) error {
		p := imagePipeline{
			decoder: in.decoder, inputPath: in.inputPath, selector: "[0]",
			profilePath: profilePath, profiled: head.hasICC,
			flatten: ip.format == "jpeg",
		}
		if resize {
			p.resizeTo = &[2]int{tw, th}
		}
		args := append(p.args(), "-quality", strconv.Itoa(ip.quality), filepath.Join(in.outputDir, name))
		_, err := env.Run(ctx, "magick", args...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("encoding still: %w", err)
	}
	sizes, err := identifySizes(ctx, env, []string{filepath.Join(in.outputDir, name)})
	if err != nil {
		return nil, err
	}
	return []OutputFile{{
		Name: name, MediaType: format.mediaType,
		Width: sizes[0][0], Height: sizes[0][1],
	}}, nil
}

func runFrames(ctx context.Context, env Env, in runInput) ([]OutputFile, error) {
	ip := readImageParams(in.params)
	format := imageFormats[ip.format]

	scenes, err := identifyScenes(ctx, env, in.decoder, in.inputPath, "")
	if err != nil {
		return nil, err
	}
	infos := make([]FrameInfo, len(scenes))
	var cw, ch int
	for i, s := range scenes {
		infos[i] = FrameInfo{delayCS: s.delayCS, hasICC: s.hasICC}
		w, h := s.canvasW, s.canvasH
		if w < 1 || h < 1 {
			w, h = s.width, s.height
		}
		cw, ch = max(cw, w), max(ch, h)
	}
	cw, ch = uprightSize(cw, ch, scenes[0].orientation)
	if cw < 1 || ch < 1 {
		return nil, fmt.Errorf("input reports size %dx%d", cw, ch)
	}
	tw, th, resize := fitSize(cw, ch, ip.maxEdge, ip.maxArea)

	sourceFrames := len(infos)
	selected := selectFrames(frameCount(in.params.int("max_frames"), sourceFrames, true), sourceFrames)
	profiled, err := selectedProfiled(infos, selected)
	if err != nil {
		return nil, err
	}

	var kept []int
	err = withProfile(in.outputDir, profiled, func(profilePath string) error {
		p := imagePipeline{
			decoder: in.decoder, inputPath: in.inputPath, coalesce: true,
			deleteExpr:  DeleteExpr(selected, sourceFrames),
			profilePath: profilePath, profiled: profiled,
			flatten: ip.format == "jpeg",
		}
		if resize {
			p.resizeTo = &[2]int{tw, th}
		}
		probeOut, err := env.Run(ctx, "magick", append(p.args(), "-format", "%s %#\n", "info:")...)
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
		pattern := strings.ReplaceAll(filepath.Join(in.outputDir, "frame-"), "%", "%%") + "%03d" + format.ext
		args := append(p.args(), "-scene", "0", "+adjoin", "-quality", strconv.Itoa(ip.quality), pattern)
		if _, err := env.Run(ctx, "magick", args...); err != nil {
			return fmt.Errorf("encoding frames: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	names, err := frameFileNames(in.outputDir, format.ext)
	if err != nil {
		return nil, err
	}
	if len(names) != len(kept) {
		return nil, fmt.Errorf("wrote %d files, kept %d frames", len(names), len(kept))
	}
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(in.outputDir, n)
	}
	sizes, err := identifySizes(ctx, env, paths)
	if err != nil {
		return nil, err
	}
	times := timestamps(frameDelays(infos))
	files := make([]OutputFile, len(names))
	for i, n := range names {
		src := kept[i]
		if src < 0 || src >= len(times) {
			return nil, fmt.Errorf("probe reported source frame %d outside the %d-frame source", src, len(times))
		}
		files[i] = OutputFile{
			Name: n, MediaType: format.mediaType,
			Width: sizes[i][0], Height: sizes[i][1],
			SourceFrame: src, SourceTimeMS: times[src],
		}
	}
	return files, nil
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
