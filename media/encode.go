package media

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
		elapsedCS += int64(displayDelayCS(delay))
	}
	return out
}

// displayDelayCS is the delay a frame is displayed for: a stored delay below
// 2 centiseconds is shown as 10.
func displayDelayCS(delay int) int {
	if delay < 2 {
		return 10
	}
	return delay
}

// EncodeRung encodes inputPath as a WebP at rung and returns the staged
// artifact, its canvas size and the frame mapping the artifact actually
// holds. sourceFrames is ProbeFrames' result for inputPath; mimeType keys the
// coder fallback ("" disables it); quality is the WebP quality; profilePath
// is the sRGB destination profile, with "" meaning the package's own, and is
// read only when a frame carries an ICC profile; orient, when set, is a
// magick orientation name that replaces the orientation the file carries, in
// a still or an animation alike.
//
// The output is upright, sized by fitSize to the rung's ceiling on its longest
// side and never upscaled, converted to sRGB when its source carries an ICC
// profile, and free of metadata.
//
// An animation is read from its headers first, as AnimationFrames reads one,
// and refused with an error wrapping ErrTooLarge, before anything is decoded,
// when its frame count, a frame's size or the pixels to decode exceed the
// caps; sourceFrames must describe those headers. An animation of more than
// one frame must be a GIF or a WebP, and its selected frames are composited
// outside ImageMagick, which receives only those frames, one full canvas
// each: the scratch space is about four bytes per pixel of the canvas for each
// selected frame, in the directory of the staged artifact, and the compositing
// of a GIF runs in this process. A frame's embedded ICC profile is honoured, a
// WebP is composited by the webp-frames tool, which Env.Run must be able to
// run, and the encode keeps each frame's stored delay. A still, or a one-frame
// file that is neither a GIF nor a WebP, is not refused for its size.
//
// The artifact is created through Env.Stage. The caller owns it via
// cleanupFn and decides, from its size, whether to publish it or drop it and
// try the next rung. On error the staged file is already removed.
func (e Env) EncodeRung(ctx context.Context, inputPath, mimeType string,
	animated bool, rung Rung, sourceFrames []FrameInfo, profilePath, orient string, quality int,
) (outputPath string, cleanupFn func(), width, height int, frames []Frame, err error) {
	enc := encoding{
		format: WebP, ext: ".webp", quality: quality,
		maxEdge: rung.CeilingPx, maxArea: rung.CeilingPx * rung.CeilingPx,
	}
	if !animated {
		profiled := len(sourceFrames) > 0 && sourceFrames[0].hasICC
		outputPath, cleanupFn, width, height, err = e.encodeStill(
			ctx, inputPath, mimeType, enc, profilePath, profiled, orient)
		if err != nil {
			return "", nil, 0, 0, nil, fmt.Errorf("encoding still: %w", err)
		}
		return outputPath, cleanupFn, width, height, []Frame{{SourceFrame: 0, SourceTimeMS: 0}}, nil
	}
	kind, _ := sniffFile(inputPath)
	var layout animationLayout
	switch kind {
	case typeGIF, typeWebP:
		dec := decoder{"magick", "gif"}
		if kind == typeWebP {
			dec.name = "webp"
		}
		if layout, err = e.readAnimation(ctx, dec, inputPath); err != nil {
			return "", nil, 0, 0, nil, err
		}
		if err := checkFramesAgree(sourceFrames, layout.infos); err != nil {
			return "", nil, 0, 0, nil, err
		}
	}
	return e.encodeAnimation(ctx, inputPath, mimeType, kind, layout, rung, enc, sourceFrames, profilePath, orient)
}

// checkFramesAgree requires the frames the caller passed to be those the
// header read found.
func checkFramesAgree(given, read []FrameInfo) error {
	if !slices.Equal(given, read) {
		return fmt.Errorf("source frames (%d) differ from the headers of the file (%d); they must come from ProbeFrames of the same file",
			len(given), len(read))
	}
	return nil
}

// encodeAnimation samples, deduplicates and encodes one animation rung.
//
// Two magick passes over the same frames. The first reports, per selected
// frame, the signature of the pixels that will actually be encoded (taken
// after the resize, since two frames differing only in fine detail can become
// identical once downscaled). The second encodes. A one-frame selection has
// nothing to deduplicate and skips the first.
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
// The mapping is read off the probe's rows, one per selected frame in order,
// rather than recomputed from the selection, so it records the frames the
// encode keeps rather than the frames the selection meant it to keep.
//
// A one-frame animation is read whole by ImageMagick, which holds that one
// frame either way. Any other is composited by composeSelected.
func (e Env) encodeAnimation(ctx context.Context, inputPath, mimeType, kind string, layout animationLayout,
	rung Rung, enc encoding, sourceFrames []FrameInfo, profilePath, orient string,
) (outputPath string, cleanupFn func(), width, height int, frames []Frame, err error) {
	sourceFrameCount := len(sourceFrames)
	n := frameCount(rung.MaxFrames, sourceFrameCount, true)
	selected := selectFrames(n, sourceFrameCount)
	profiled, err := selectedProfiled(sourceFrames, selected)
	if err != nil {
		return "", nil, 0, 0, nil, err
	}
	outputPath, cleanupFn, err = e.Stage(".webp")
	if err != nil {
		return "", nil, 0, 0, nil, err
	}
	fail := func(err error) (string, func(), int, int, []Frame, error) {
		cleanupFn()
		return "", nil, 0, 0, nil, err
	}
	scratch, err := os.MkdirTemp(filepath.Dir(outputPath), ".rung-frames-*")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	// The compositors write frames without a tag, so a composited animation
	// carries the header's orientation; a frame read whole keeps its own tag.
	src := encodeSource{orientation: orient}
	if sourceFrameCount > 1 {
		src.orientation = cmp.Or(orient, layout.orientation)
	}

	// destination is the sRGB profile the frames that carry one convert to.
	var destination string
	needsDestination := profiled
	var composed []composedFrame
	runMIME := mimeType
	if sourceFrameCount > 1 {
		composed, err = e.composeSelected(ctx, kind, inputPath, scratch, layout, selected)
		if err != nil {
			return fail(fmt.Errorf("compositing selected frames: %w", err))
		}
		needsDestination = slices.ContainsFunc(composed, func(f composedFrame) bool { return f.profile > 0 })
		// The frames are read by an explicit coder, so there is no sniffing to
		// fall back from.
		runMIME = ""
	}
	if destination, err = rungDestination(scratch, profilePath, needsDestination); err != nil {
		return fail(err)
	}

	// framesAt returns the frames at positions of the selection.
	framesAt := func(coder string, positions []int) []encodeFrame {
		if composed == nil {
			input := inputPath
			if coder != "" {
				input = coder + ":" + input
			}
			f := encodeFrame{input: input, prelude: []string{"-coalesce"}, delayCS: -1}
			if profiled {
				f.convertTo = destination
			}
			return []encodeFrame{f}
		}
		fs := make([]encodeFrame, len(positions))
		for i, position := range positions {
			c := composed[position]
			fs[i] = encodeFrame{input: "pam:" + c.path, delayCS: sourceFrames[selected[position]].delayCS}
			if c.profile > 0 {
				fs[i].convertTo = destination
				fs[i].assign = filepath.Join(scratch, composedProfileName(c.profile))
			}
		}
		return fs
	}

	kept := []int{0}
	if len(selected) > 1 {
		all := make([]int, len(selected))
		for i := range all {
			all[i] = i
		}
		out, err := e.RunMagickWithCoderFallback(ctx, runMIME, func(coder string) []string {
			return encodeArgs(framesAt(coder, all), src, enc, encodeOutput{kind: outputProbe})
		})
		if err != nil {
			return fail(fmt.Errorf("probing selected frames: %w", err))
		}
		probed, err := parseEncoded(out)
		if err != nil {
			return fail(err)
		}
		if len(probed) != len(selected) {
			return fail(fmt.Errorf("frame selection probe returned %d frames, want %d", len(probed), len(selected)))
		}
		kept = make([]int, 0, len(probed))
		lastSig := ""
		for i, frame := range probed {
			if i > 0 && frame.signature == lastSig {
				continue
			}
			kept = append(kept, i)
			lastSig = frame.signature
		}
	}

	if _, err := e.RunMagickWithCoderFallback(ctx, runMIME, func(coder string) []string {
		return encodeArgs(framesAt(coder, kept), src, enc, encodeOutput{kind: outputAnimation, path: outputPath})
	}); err != nil {
		return fail(fmt.Errorf("encoding animation: %w", err))
	}

	scenes, w, h, err := e.IdentifyArtifact(ctx, outputPath)
	if err != nil {
		return fail(err)
	}
	if scenes != len(kept) {
		return fail(fmt.Errorf("encoded artifact holds %d scenes, mapping describes %d", scenes, len(kept)))
	}

	timestamps := timestamps(frameDelays(sourceFrames))
	frames = make([]Frame, 0, len(kept))
	for _, position := range kept {
		sourceFrame := selected[position]
		if sourceFrame < 0 || sourceFrame >= len(timestamps) {
			return fail(fmt.Errorf("probe reported source frame %d outside the %d-frame source", sourceFrame, len(timestamps)))
		}
		frames = append(frames, Frame{SourceFrame: sourceFrame, SourceTimeMS: timestamps[sourceFrame]})
	}
	return outputPath, cleanupFn, w, h, frames, nil
}

// rungDestination returns the sRGB profile a rung converts to: profilePath
// when given, otherwise the package's own, written into scratch. It returns
// "" when no frame needs one.
func rungDestination(scratch, profilePath string, needed bool) (string, error) {
	if !needed {
		return "", nil
	}
	if profilePath != "" {
		return profilePath, nil
	}
	return placeSRGB(scratch)
}

// composedFrame is a selected frame written as a full-canvas PAM file, and
// the id of the ICC profile it carries, 0 for none.
type composedFrame struct {
	path    string
	profile int
}

// composeSelected composites the selected frames of the GIF or WebP at
// inputPath, as -coalesce would, and writes each as a PAM file in dir. The
// compositing of a GIF is done in this process, which a failure in cannot
// bring down; that of a WebP by the webp-frames tool. Both refuse a file whose
// frames or canvas differ from layout, the header read.
func (e Env) composeSelected(ctx context.Context, kind, inputPath, dir string, layout animationLayout, selected []int) (composed []composedFrame, err error) {
	cw, ch := layout.cw, layout.ch
	own := make([]bool, len(layout.infos))
	for i, f := range layout.infos {
		own[i] = f.hasICC
	}
	sourceFrames := layout.infos
	switch kind {
	case typeGIF:
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("gif compositor panic: %v", r)
			}
		}()
		job := gifJob{
			path: inputPath, dir: dir, frames: len(sourceFrames), width: cw, height: ch,
			mask: ownMask(own), selected: selected,
		}
		err = job.run(ctx, nil, func(ordinal, profile int) error {
			composed = append(composed, composedFrame{path: filepath.Join(dir, gifSampleName(ordinal)), profile: profile})
			return nil
		})
	case typeWebP:
		req := composeRequest{
			path: inputPath, dir: dir, selected: selected, inFlight: len(selected),
			frames: len(sourceFrames), width: cw, height: ch, external: webpFramesTool, consumeTimeout: composeTimeout,
		}
		var res Result
		if res, err = e.Run(ctx, webpFramesTool, req.externalArgs()...); err != nil {
			return nil, err
		}
		lines := &frameLines{dir: dir, onFrame: func(path string, profile int) error {
			composed = append(composed, composedFrame{path: path, profile: profile})
			return nil
		}}
		_, err = lines.Write(res.Stdout)
	default:
		return nil, fmt.Errorf("cannot composite an animation of type %q", kind)
	}
	if err != nil {
		return nil, err
	}
	if len(composed) != len(selected) {
		return nil, fmt.Errorf("composited %d of %d frames", len(composed), len(selected))
	}
	return composed, nil
}

// encodeStill encodes the first frame of inputPath as one WebP at enc in one
// magick run that decodes, orients, converts, resizes and writes it and
// reports the size it wrote. The decode falls back to the coder CoderForMIME
// names for mimeType when the bare read fails.
//
// The caller owns the returned staging file via cleanupFn; on error the
// staging file is already removed.
func (e Env) encodeStill(ctx context.Context, inputPath, mimeType string, enc encoding, profilePath string, profiled bool, orient string) (outputPath string, cleanupFn func(), width, height int, err error) {
	outputPath, cleanupFn, err = e.Stage(".webp")
	if err != nil {
		return "", nil, 0, 0, err
	}
	fail := func(err error) (string, func(), int, int, error) {
		cleanupFn()
		return "", nil, 0, 0, err
	}
	destination := ""
	if profiled {
		if destination = profilePath; destination == "" {
			scratch, err := os.MkdirTemp(filepath.Dir(outputPath), ".rung-still-*")
			if err != nil {
				return fail(err)
			}
			defer func() { _ = os.RemoveAll(scratch) }()
			if destination, err = placeSRGB(scratch); err != nil {
				return fail(err)
			}
		}
	}

	out, err := e.RunMagickWithCoderFallback(ctx, mimeType, func(coder string) []string {
		input := inputPath + "[0]"
		if coder != "" {
			input = coder + ":" + input
		}
		return encodeArgs([]encodeFrame{{input: input, convertTo: destination}},
			encodeSource{orientation: orient}, enc, encodeOutput{kind: outputStill, path: outputPath})
	})
	if err != nil {
		return fail(err)
	}
	encoded, err := parseEncoded(out)
	if err != nil {
		return fail(err)
	}
	if len(encoded) != 1 {
		return fail(fmt.Errorf("still encode produced %d scenes, want 1", len(encoded)))
	}
	return outputPath, cleanupFn, encoded[0].width, encoded[0].height, nil
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
// an ICC profile. The returned length is the source frame count.
//
// Scenes are read from their headers, without regard to their size, except
// that a file of more scenes than the list-length ceiling of LimitArgs is
// refused with ErrTooLarge; EncodeRung applies the caps. The profile of a
// single-frame WebP comes from its chunk list when that settles it and
// otherwise from one decode of the file; that of a single-frame JXL always
// comes from one decode, since ImageMagick does not report it from the headers
// and libjxl gives every JXL a profile. Those decodes are bounded by LimitArgs
// only, not by the frame caps. A failed read is retried once
// with the coder CoderForMIME names for mimeType ("" disables the retry),
// because magick cannot sniff some formats from an extensionless path.
func (e Env) ProbeFrames(ctx context.Context, inputPath, mimeType string) ([]FrameInfo, error) {
	scenes, ref, err := identifyWithFallback(ctx, e, inputPath, "", mimeType)
	if err != nil {
		return nil, err
	}
	if err := completeProfile(ctx, e, ref, inputPath, scenes); err != nil {
		return nil, err
	}
	frames := make([]FrameInfo, len(scenes))
	for i, s := range scenes {
		frames[i] = FrameInfo{delayCS: s.delayCS, hasICC: s.hasICC}
	}
	return frames, nil
}

// identifyWithFallback reads the headers of the scenes of path selected by
// selector ("" for all). A failed read is retried once with the coder
// CoderForMIME names for mimeType ("" disables the retry). ref is the input
// reference the headers were read from.
func identifyWithFallback(ctx context.Context, env Env, path, selector, mimeType string) (scenes []sceneHeader, ref string, err error) {
	ref = path + selector
	scenes, err = identifyHeaders(ctx, env, ref, true)
	if err == nil || errors.Is(err, ErrTooLarge) {
		return scenes, ref, err
	}
	coder, ok := CoderForMIME(mimeType)
	if !ok {
		return nil, "", err
	}
	ref = coder + ":" + path + selector
	scenes, retryErr := identifyHeaders(ctx, env, ref, true)
	if retryErr != nil {
		return nil, "", fmt.Errorf("%w; retried with coder %q, which also failed: %w", err, coder, retryErr)
	}
	return scenes, ref, nil
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
