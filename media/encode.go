package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Limits are the bounds that shape the encode ladder.
//
// Deprecated: the ladder belongs to the caller, over Even.
type Limits struct {
	// Ceilings is the shrink-only dimension ladder, longest side first. The
	// first entry is the nominal ceiling; later entries are reached only
	// after the frame budget has been spent down to one.
	Ceilings []int
	// MaxFrames is the first rung's sampled-frame cap; the ladder halves it.
	MaxFrames int
}

// Frame is one frame of an encoded output and the frame of the ORIGINAL media
// it came from. The slice index of a Frame is its position in the encoded
// sequence.
type Frame struct {
	// Index is the frame's index in the original media: 0 for a still and
	// for a video, whose samples have no frame index.
	Index int
	// TimeMS is the time the frame is shown in the original, in
	// milliseconds: the sum of the display delays of the frames before it in
	// an animation (a stored delay below 20 ms counts as 100 ms, as browsers
	// display it), or the time a video was sampled at; 0 for a still.
	TimeMS int64
}

// Rung is one step of the fallback ladder: an encode at a dimension ceiling
// (longest side, in pixels) sampling at most MaxFrames frames of an
// animation.
//
// Deprecated: the ladder belongs to the caller, over Even.
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
//
// Deprecated: the ladder belongs to the caller, over Even.
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
// holds. It is Open, EnvSource.Encode of the rung's frames and Close.
// sourceFrames is ProbeFrames' result for inputPath, used only to check an
// animation's frame count; mimeType keys the coder fallback ("" disables it);
// quality is the WebP quality; profilePath, when not empty, is the sRGB
// destination profile in place of the package's own; orient, when set, is a
// magick orientation name that replaces the orientation the file carries.
//
// animated says what the caller treats the file as. A still (animated false)
// is the first image of the file, as Hints.Still; an animation of more than
// one frame must be a GIF or a WebP, else the error wraps ErrUnsupportedType.
// Everything else, the sizing, the caps and the staging, is as Open and
// EnvSource.Encode say. Env.Scratch is required.
//
// The caller owns the artifact via cleanupFn and decides, from its size,
// whether to publish it or drop it and try the next rung. On error the staged
// file is already removed.
//
// Deprecated: use Processor.Open and Source.Encode.
func (e Env) EncodeRung(ctx context.Context, inputPath, mimeType string,
	animated bool, rung Rung, sourceFrames []FrameInfo, profilePath, orient string, quality int,
) (outputPath string, cleanupFn func(), width, height int, frames []Frame, err error) {
	orientation, err := orientationNumber(orient)
	if err != nil {
		return "", nil, 0, 0, nil, err
	}
	budget := max(1, rung.MaxFrames)
	hints := Hints{MediaType: mimeType, Orientation: orientation, Still: !animated}
	if animated {
		hints.Frames = budget
	}
	src, err := e.Open(ctx, inputPath, hints)
	if err != nil {
		return "", nil, 0, 0, nil, err
	}
	defer src.Close()
	if n := len(src.layout.infos); animated && len(sourceFrames) != n {
		return "", nil, 0, 0, nil, fmt.Errorf("source frames (%d) differ from the headers of the file (%d); they must come from ProbeFrames of the same file",
			len(sourceFrames), n)
	}
	src.destination = profilePath
	enc := encoding{
		format: WebP, ext: ".webp", quality: quality,
		maxEdge: rung.CeilingPx, maxArea: rung.CeilingPx * rung.CeilingPx,
	}
	out, err := src.encode(ctx, region{}, budget, []encoding{enc})
	if err != nil {
		return "", nil, 0, 0, nil, err
	}
	a := out[0]
	return a.Path, a.Cleanup, a.Width, a.Height, a.Frames, nil
}

// FrameInfo is the header-only metadata needed before a rung decodes
// pixels. ICC presence is per scene so an animation never assigns the sRGB
// destination profile to an unprofiled frame merely because another frame was
// tagged.
//
// Deprecated: use Info.
type FrameInfo struct {
	delayCS int
	hasICC  bool
}

// ProbeFrames reads each scene's delay in centiseconds and whether it carries
// an ICC profile. The returned length is the source frame count.
//
// Scenes are read from their headers, without regard to their size, except
// that a file of more scenes than the list-length ceiling is refused with
// ErrTooLarge; EncodeRung applies the caps. The profile of a
// single-frame WebP comes from its chunk list when that settles it and
// otherwise from one decode of the file; that of a single-frame JXL always
// comes from one decode, since ImageMagick does not report it from the headers
// and libjxl gives every JXL a profile. Those decodes are bounded by the
// resource limits only, not by the frame caps. A failed read is retried once
// with the coder CoderForMIME names for mimeType ("" disables the retry),
// because magick cannot sniff some formats from an extensionless path.
//
// Deprecated: use Processor.Open and Source.Info.
func (e Env) ProbeFrames(ctx context.Context, inputPath, mimeType string) ([]FrameInfo, error) {
	scenes, ref, err := identifyWithFallback(ctx, e, inputPath, "", mimeType)
	if err != nil {
		return nil, err
	}
	if err := completeProfile(ctx, e, ref, input{path: inputPath}, scenes); err != nil {
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
	if err == nil || errors.Is(err, ErrTooLarge) || errors.Is(err, ErrTimeLimit) {
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

// AnyProfiled reports whether any frame carries an ICC profile.
//
// Deprecated: use Processor.Open and Source.Info.
func AnyProfiled(frames []FrameInfo) bool {
	for _, frame := range frames {
		if frame.hasICC {
			return true
		}
	}
	return false
}
