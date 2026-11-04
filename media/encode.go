package media

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Limits are the bounds that shape the encode ladder.
type Limits struct {
	// Ceilings is the shrink-only dimension ladder, longest side first. The
	// first entry is the nominal ceiling; later entries are reached only
	// after the frame budget has been spent down to one.
	Ceilings []int
	// MaxFrames is the first rung's sampled-frame cap; the ladder halves it.
	MaxFrames int
}

// Frame is one frame of an encoded artifact and the frame of the ORIGINAL
// media it came from. The slice index of a Frame is its position in the
// encoded sequence.
type Frame struct {
	SourceFrame  int   // frame in the original media; 0 for a still
	SourceTimeMS int64 // elapsed milliseconds before it in the original
}

// Rung is one step of the fallback ladder: an encode at a dimension ceiling
// (longest side, in pixels) sampling at most MaxFrames frames of an
// animation.
type Rung struct {
	CeilingPx int
	MaxFrames int
}

// Ladder returns the rungs in walk order: frames are spent first, halving
// from limits.MaxFrames down to one at the first ceiling, and only then does
// the ceiling step, without resetting the frame count. The sequence is
// monotonically decreasing in output size, so no rung revisits a size already
// known to fail. Limits{Ceilings: {2048, 1536, 1024}, MaxFrames: 16} and an
// animation give (2048,16) (2048,8) (2048,4) (2048,2) (2048,1) (1536,1)
// (1024,1). Frames are spent before resolution because frame coverage
// degrades gracefully where resolution does not.
//
// A still (animated false) has one rung per ceiling. Rungs that would encode
// identically are collapsed using sourceFrames, the source's frame count: a
// five-frame animation caps to five at both the 16- and 8-frame rungs.
// Ladder returns nil when limits has no ceilings.
func Ladder(limits Limits, animated bool, sourceFrames int) []Rung {
	if len(limits.Ceilings) == 0 {
		return nil
	}
	var caps []int
	if animated {
		for n := limits.MaxFrames; n > 1; n /= 2 {
			caps = append(caps, n)
		}
	}
	caps = append(caps, 1)

	var rungs []Rung
	appendRung := func(ceiling, maxFrames int) {
		effective := frameCount(maxFrames, sourceFrames, animated)
		if n := len(rungs); n > 0 {
			prev := rungs[n-1]
			if prev.CeilingPx == ceiling && frameCount(prev.MaxFrames, sourceFrames, animated) == effective {
				return
			}
		}
		rungs = append(rungs, Rung{CeilingPx: ceiling, MaxFrames: maxFrames})
	}
	for _, maxFrames := range caps {
		appendRung(limits.Ceilings[0], maxFrames)
	}
	for _, ceiling := range limits.Ceilings[1:] {
		appendRung(ceiling, 1)
	}
	return rungs
}

// frameCount is N: the number of frames a rung samples.
func frameCount(maxFrames, sourceFrames int, animated bool) int {
	if !animated {
		return 1
	}
	n := maxFrames
	if n > sourceFrames {
		n = sourceFrames
	}
	if n < 1 {
		n = 1
	}
	return n
}

// selectFrames returns the evenly spaced source-frame indices a
// rung samples: selected[i] = i*sourceFrames/N, integer division, so frame 0
// always appears. The last frame appears only when sourceFrames <= N — even
// spacing over the whole sequence is the rule, and pinning both endpoints
// would make the interior spacing uneven.
//
// This is videoSample's arithmetic; only the cap differs, and it is the
// caller's.
func selectFrames(n, sourceFrames int) []int {
	if n < 1 || sourceFrames < 1 {
		return []int{0}
	}
	selected := make([]int, 0, n)
	for i := range n {
		selected = append(selected, i*sourceFrames/n)
	}
	return selected
}

// timestamps returns, for each original frame index, the elapsed
// milliseconds before it in the ORIGINAL media — the sum runs over every
// preceding frame, not only the sampled ones.
//
// A stored delay below 2 centiseconds is rendered as 10 by the long-standing
// GIF display convention, and real GIFs do store 0 on every frame; an
// unclamped sum makes every timestamp 0, which locates no moment in the
// media. It is a display convention only: frames are identified by source
// frame index, not by timestamp, so a wrong clamp cannot change a mapping.
func timestamps(delaysCS []int) []int64 {
	out := make([]int64, len(delaysCS))
	var elapsedCS int64
	for i, delay := range delaysCS {
		out[i] = elapsedCS * 10
		effective := delay
		if effective < 2 {
			effective = 10
		}
		elapsedCS += int64(effective)
	}
	return out
}

// EncodeRung encodes inputPath as a WebP at rung and returns the staged
// artifact, its canvas size and the frame mapping the artifact actually
// holds. sourceFrames is ProbeFrames' result for inputPath; mimeType keys the
// coder fallback ("" disables it); quality is the WebP quality; profilePath
// is the sRGB destination profile and is read only when a frame carries an
// ICC profile; orient, when set, is a magick orientation name assigned to a
// still before auto-orientation.
//
// The artifact is created through Env.Stage. The caller owns it via
// cleanupFn and decides, from its size, whether to publish it or drop it and
// try the next rung. On error the staged file is already removed.
func (e Env) EncodeRung(ctx context.Context, inputPath, mimeType string,
	animated bool, rung Rung, sourceFrames []FrameInfo, profilePath, orient string, quality int,
) (outputPath string, cleanupFn func(), width, height int, frames []Frame, err error) {
	if !animated {
		profiled := len(sourceFrames) > 0 && sourceFrames[0].hasICC
		outputPath, cleanupFn, width, height, err = e.encodeStill(
			ctx, inputPath, mimeType, rung.CeilingPx, profilePath, profiled, orient, quality)
		if err != nil {
			return "", nil, 0, 0, nil, fmt.Errorf("encoding still: %w", err)
		}
		return outputPath, cleanupFn, width, height, []Frame{{SourceFrame: 0, SourceTimeMS: 0}}, nil
	}
	return e.encodeAnimation(ctx, inputPath, mimeType, rung, sourceFrames, profilePath, quality)
}

// encodeAnimation composites, samples, deduplicates and
// encodes one animation rung.
//
// Two magick passes over the same pipeline. The first reports, per surviving
// frame, the ORIGINAL scene index (%s survives -delete) and the signature of
// the pixels that will actually be encoded (%# taken after the resize, since
// two frames differing only in fine detail can become identical once
// downscaled). The second encodes.
//
// The dedup between them exists because magick's WebP writer collapses runs
// of identical consecutive frames and no -define prevents it: a four-frame
// GIF of one repeated image encodes to a one-scene WebP. Left alone that
// yields a mapping promising four frames against an artifact holding one, so
// an index into the artifact would resolve to a source frame the artifact
// does not hold. Non-consecutive duplicates survive the writer, so dropping
// only consecutive repeats is both sufficient and minimal.
//
// The scene-count check after the encode, not the dedup, is the guarantee —
// an unmodelled encoder behavior must never reach a persisted mapping. The
// dedup is what keeps the check from firing on the common case.
//
// The mapping is read off the probe rather than recomputed from the
// selection, so it records the frames magick kept rather than the frames the
// selection meant it to keep.
func (e Env) encodeAnimation(ctx context.Context, inputPath, mimeType string,
	rung Rung, sourceFrames []FrameInfo, profilePath string, quality int,
) (outputPath string, cleanupFn func(), width, height int, frames []Frame, err error) {
	sourceFrameCount := len(sourceFrames)
	n := frameCount(rung.MaxFrames, sourceFrameCount, true)
	selected := selectFrames(n, sourceFrameCount)
	profiled, err := selectedProfiled(sourceFrames, selected)
	if err != nil {
		return "", nil, 0, 0, nil, err
	}

	probed, err := e.probeSelection(ctx, inputPath, mimeType, rung.CeilingPx,
		DeleteExpr(selected, sourceFrameCount), profilePath, profiled)
	if err != nil {
		return "", nil, 0, 0, nil, fmt.Errorf("probing selected frames: %w", err)
	}
	if len(probed) == 0 {
		return "", nil, 0, 0, nil, fmt.Errorf("frame selection probe returned no frames")
	}

	kept := make([]int, 0, len(probed))
	lastSig := ""
	for i, frame := range probed {
		if i > 0 && frame.signature == lastSig {
			continue
		}
		kept = append(kept, frame.sourceFrame)
		lastSig = frame.signature
	}

	deleteExpr := DeleteExpr(kept, sourceFrameCount)
	outputPath, cleanupFn, err = e.Stage(".webp")
	if err != nil {
		return "", nil, 0, 0, nil, err
	}
	if _, err := e.RunMagickWithCoderFallback(ctx, mimeType, func(coder string) []string {
		args := animationPipeline(inputPath, rung.CeilingPx, deleteExpr, coder, profilePath, profiled)
		return append(args, "-quality", strconv.Itoa(quality), outputPath)
	}); err != nil {
		cleanupFn()
		return "", nil, 0, 0, nil, fmt.Errorf("encoding animation: %w", err)
	}

	scenes, w, h, err := e.IdentifyArtifact(ctx, outputPath)
	if err != nil {
		cleanupFn()
		return "", nil, 0, 0, nil, err
	}
	if scenes != len(kept) {
		cleanupFn()
		return "", nil, 0, 0, nil, fmt.Errorf(
			"encoded artifact holds %d scenes, mapping describes %d", scenes, len(kept))
	}

	timestamps := timestamps(frameDelays(sourceFrames))
	frames = make([]Frame, 0, len(kept))
	for _, sourceFrame := range kept {
		if sourceFrame < 0 || sourceFrame >= len(timestamps) {
			cleanupFn()
			return "", nil, 0, 0, nil, fmt.Errorf(
				"probe reported source frame %d outside the %d-frame source", sourceFrame, len(timestamps))
		}
		frames = append(frames, Frame{SourceFrame: sourceFrame, SourceTimeMS: timestamps[sourceFrame]})
	}
	return outputPath, cleanupFn, w, h, frames, nil
}

// probedFrame is one line of the selection probe.
type probedFrame struct {
	sourceFrame int
	signature   string
}

// probeSelection runs the decode/select/resize pipeline without
// encoding, reporting the original scene index and post-resize signature of
// every surviving frame.
func (e Env) probeSelection(ctx context.Context, inputPath, mimeType string, ceiling int, deleteExpr, profilePath string, profiled bool) ([]probedFrame, error) {
	out, err := e.RunMagickWithCoderFallback(ctx, mimeType, func(coder string) []string {
		args := animationPipeline(inputPath, ceiling, deleteExpr, coder, profilePath, profiled)
		return append(args, "-format", "%s %#\n", "info:")
	})
	if err != nil {
		return nil, err
	}
	return parseProbe(out)
}

// parseProbe reads the "%s %#" lines a selection probe prints.
func parseProbe(out []byte) ([]probedFrame, error) {
	var probed []probedFrame
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("probe line %q missing fields", line)
		}
		sourceFrame, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("parsing scene index from %q: %w", line, err)
		}
		probed = append(probed, probedFrame{sourceFrame: sourceFrame, signature: fields[1]})
	}
	return probed, nil
}

// animationPipeline is the decode/select/resize stage both the
// probe and the encode share, so the frames the probe reports on are exactly
// the frames the encode writes.
//
// -coalesce then -delete is the only correct form. Reading a frame subset
// directly (path[0,5,10]) yields the stored, partial, sub-canvas frames a
// GIF actually holds — verified — so an artifact built that way would carry
// fragments where it claims to carry moments.
//
// -layers optimize is deliberately absent: it is a GIF palette and disposal
// optimization that does not apply to WebP output.
func animationPipeline(inputPath string, ceiling int, deleteExpr, coder, profilePath string, profiled bool) []string {
	arg0 := inputPath
	if coder != "" {
		arg0 = coder + ":" + arg0
	}
	args := append([]string{}, LimitArgs()...)
	args = append(args, arg0, "-coalesce")
	if deleteExpr != "" {
		args = append(args, "-delete", deleteExpr)
	}
	args = append(args, "-auto-orient")
	args = append(args, colorArgs(profilePath, profiled)...)
	return append(args, "-thumbnail", fmt.Sprintf("%dx%d>", ceiling, ceiling))
}

// colorArgs is the one conditional color-normalization stage shared
// by stills and both animation pixel passes. ImageMagick's -profile transforms
// from an existing embedded source profile to this destination profile; it is
// deliberately omitted for unprofiled input, where it would assign rather
// than convert.
func colorArgs(profilePath string, sourceProfiled bool) []string {
	if !sourceProfiled {
		return nil
	}
	return []string{"-profile", profilePath}
}

// encodeStill runs one real magick encode of inputPath at the given ceiling:
// the [0] frame selector, -auto-orient, the shrink-only ceiling, -quality,
// then the post-encode identify check that confirms exactly one scene and
// returns the frame-0 canvas dimensions.
//
// The caller owns the returned staging file via cleanupFn; on error the
// staging file is already removed.
func (e Env) encodeStill(ctx context.Context, inputPath, mimeType string, ceiling int, profilePath string, profiled bool, orient string, quality int) (outputPath string, cleanupFn func(), width, height int, err error) {
	outputPath, cleanupFn, err = e.Stage(".webp")
	if err != nil {
		return "", nil, 0, 0, err
	}
	if err := e.generateStill(ctx, inputPath, outputPath, ceiling, mimeType, profilePath, profiled, orient, quality); err != nil {
		cleanupFn()
		return "", nil, 0, 0, err
	}
	scenes, w, h, err := e.IdentifyArtifact(ctx, outputPath)
	if err != nil {
		cleanupFn()
		return "", nil, 0, 0, err
	}
	if scenes != 1 {
		cleanupFn()
		return "", nil, 0, 0, fmt.Errorf("still encode produced %d scenes, want 1", scenes)
	}
	return outputPath, cleanupFn, w, h, nil
}

// generateStill invokes magick against inputPath through the
// shared coder-retry runner, so a still gets the same extensionless-sniffing
// fallback the probe and the animation encode do.
func (e Env) generateStill(ctx context.Context, inputPath, outputPath string, ceiling int, mimeType, profilePath string, profiled bool, orient string, quality int) error {
	_, err := e.RunMagickWithCoderFallback(ctx, mimeType, func(coder string) []string {
		return stillArgs(inputPath, outputPath, ceiling, coder, profilePath, orient, profiled, quality)
	})
	return err
}

// stillArgs builds one magick invocation for the still-image path: the
// frame-0 selector, the mandatory resource limits, an -auto-orient
// (preceded by -orient when orient is set, for an embedded image that lost
// its orientation tag) ahead of the shrink-only "<C>x<C>>" ceiling (the ">"
// flag is the whole no-upscale contract), and -quality.
func stillArgs(inputPath, outputPath string, ceiling int, coder, profilePath, orient string, profiled bool, quality int) []string {
	arg0 := inputPath + "[0]"
	if coder != "" {
		arg0 = coder + ":" + arg0
	}
	args := append([]string{}, LimitArgs()...)
	args = append(args, arg0)
	if orient != "" {
		args = append(args, "-orient", orient)
	}
	args = append(args, "-auto-orient")
	args = append(args, colorArgs(profilePath, profiled)...)
	args = append(args,
		"-thumbnail", fmt.Sprintf("%dx%d>", ceiling, ceiling),
		"-quality", strconv.Itoa(quality),
		outputPath,
	)
	return args
}

// FrameInfo is the header-only metadata needed before a rung decodes
// pixels. ICC presence is per scene so an animation never assigns the sRGB
// destination profile to an unprofiled frame merely because another frame was
// tagged.
type FrameInfo struct {
	delayCS int
	hasICC  bool
}

// ProbeFrames reads each scene's delay in centiseconds and whether it carries
// an ICC profile, in one magick identify pass over the headers. The returned
// length is the source frame count.
//
// It does not composite, so its cost does not grow with frame count the way
// -coalesce does: a caller can refuse an input by frame count before any rung
// decodes pixels. A failed identify is retried once with the coder
// CoderForMIME names for mimeType ("" disables the retry), because magick
// cannot sniff some formats from an extensionless path.
func (e Env) ProbeFrames(ctx context.Context, inputPath, mimeType string) ([]FrameInfo, error) {
	out, err := e.RunMagickWithCoderFallback(ctx, mimeType, func(coder string) []string {
		path := inputPath
		if coder != "" {
			path = coder + ":" + path
		}
		args := append([]string{"identify"}, LimitArgs()...)
		return append(args, "-format", "%s|%T|%[profiles]|END\n", path)
	})
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil, fmt.Errorf("identify returned no scenes")
	}
	lines := strings.Split(trimmed, "\n")
	frames := make([]FrameInfo, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, "|")
		if len(fields) != 4 || fields[3] != "END" {
			return nil, fmt.Errorf("identify line %q missing fields", line)
		}
		delay, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("parsing frame delay from %q: %w", line, err)
		}
		frames = append(frames, FrameInfo{delayCS: delay, hasICC: profileListHasICC(fields[2])})
	}
	return frames, nil
}

func profileListHasICC(profiles string) bool {
	for _, profile := range strings.FieldsFunc(profiles, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		if strings.EqualFold(profile, "icc") || strings.EqualFold(profile, "icm") {
			return true
		}
	}
	return false
}

func frameDelays(frames []FrameInfo) []int {
	delays := make([]int, len(frames))
	for i, frame := range frames {
		delays[i] = frame.delayCS
	}
	return delays
}

// AnyProfiled reports whether any frame carries an ICC profile, which is when
// EncodeRung may read profilePath.
func AnyProfiled(frames []FrameInfo) bool {
	for _, frame := range frames {
		if frame.hasICC {
			return true
		}
	}
	return false
}

// selectedProfiled returns the one color policy shared by every
// selected animation scene. A mixed selection cannot safely use one list-wide
// ImageMagick -profile operator: it would convert tagged scenes but assign the
// RGB destination profile to untagged scenes.
func selectedProfiled(frames []FrameInfo, selected []int) (bool, error) {
	profiled, unprofiled := false, false
	for _, sourceFrame := range selected {
		if sourceFrame < 0 || sourceFrame >= len(frames) {
			return false, fmt.Errorf("selected source frame %d outside the %d-frame source", sourceFrame, len(frames))
		}
		if frames[sourceFrame].hasICC {
			profiled = true
		} else {
			unprofiled = true
		}
	}
	if profiled && unprofiled {
		return false, fmt.Errorf("selected animation frames mix embedded ICC profiles with unprofiled pixels")
	}
	return profiled, nil
}
