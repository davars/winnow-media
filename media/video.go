package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// maxVideoSampleFrames is the number of frames GenerateVideoThumbnails
// samples; it is tied to the animation's playback rate.
const maxVideoSampleFrames = 15

// videoFrameDelayCS is how long each sample is displayed in an animated
// output, in centiseconds: two frames a second.
const videoFrameDelayCS = 50

// GenerateVideoThumbnails samples fifteen frames of a video at the centres of
// equal intervals of its duration and writes them as two animated WebPs whose
// longest sides are at most sizeSmall and sizeLarge, at the given quality,
// each frame shown for half a second. Frames are sampled, tone-mapped, sized
// and stripped as Processor.VideoFrames does, and a frame identical to the one
// before it once resized is dropped.
//
// It requires smallPath and largePath to end in .webp and their directory to
// be writable, where it stages samples in a subdirectory it removes. The video
// stream is ffprobe's choice; streamIndex and frames are ignored. A stream
// over the package's caps is refused with ErrTooLarge before any frame is
// decoded.
func (e Env) GenerateVideoThumbnails(ctx context.Context, inputPath, smallPath, largePath string, streamIndex, frames, sizeSmall, sizeLarge, quality int) error {
	outputs := []videoOutput{
		{thumbnailEncoding(sizeSmall, quality), smallPath},
		{thumbnailEncoding(sizeLarge, quality), largePath},
	}
	dec := sniffedVideoDecoder(inputPath)
	decode := outputs[0].enc
	if outputs[1].enc.maxEdge > decode.maxEdge {
		decode = outputs[1].enc
	}
	plan, err := e.planVideo(ctx, dec, inputPath, decode)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(smallPath), ".video-thumbs-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	samples, err := e.sampleVideo(ctx, dec, inputPath, dir, plan, sampleTimesMS(plan.info, maxVideoSampleFrames))
	if err != nil {
		return err
	}
	return e.encodeVideoAnimations(ctx, dir, plan, samples, outputs)
}

// thumbnailEncoding is the encoding of an animated WebP thumbnail whose longest
// side is at most edge.
func thumbnailEncoding(edge, quality int) encoding {
	return encoding{format: WebP, ext: ".webp", quality: quality, maxEdge: edge, maxArea: edge * edge}
}

// videoOutput is one animated file an encode of samples writes.
type videoOutput struct {
	enc  encoding
	path string
}

// encodeVideoAnimations writes each output as one animated file of samples,
// after dropping each frame identical to the one before it once resized: a
// first run reports the signature of every frame as it will be encoded, since
// the muxer would otherwise collapse the repeats and leave an animation of
// fewer frames than the caller was told. A single sample is encoded without
// the first run. The scene count of what was written is checked.
func (e Env) encodeVideoAnimations(ctx context.Context, dir string, plan videoPlan, samples []sampledFrame, outputs []videoOutput) error {
	if len(samples) == 0 {
		return errors.New("no frame could be sampled")
	}
	for _, out := range outputs {
		src := plan.source(out.enc)
		bound := []encodeBound{boundOf(out.enc, src)}
		framesOf := func(positions []int) []encodeFrame {
			frames := make([]encodeFrame, len(positions))
			for i, position := range positions {
				frames[i] = encodeFrame{input: "pam:" + samples[position].path, delayCS: videoFrameDelayCS, fits: plan.fits(out.enc)}
			}
			return frames
		}
		kept := []int{0}
		if len(samples) > 1 {
			all := make([]int, len(samples))
			for i := range all {
				all[i] = i
			}
			probed, err := e.runEncode(ctx, encodeArgs(framesOf(all), src, out.enc, encodeOutput{kind: outputProbe}),
				encodeCheck{frames: len(all), bounds: bound})
			if err != nil {
				return fmt.Errorf("probing sampled frames: %w", err)
			}
			kept = kept[:0]
			for i, f := range probed {
				if i == 0 || f.signature != probed[i-1].signature {
					kept = append(kept, i)
				}
			}
		}
		if _, err := e.runEncode(ctx, encodeArgs(framesOf(kept), src, out.enc, encodeOutput{kind: outputAnimation, path: out.path}),
			encodeCheck{frames: len(kept), bounds: bound, outputs: []string{out.path}}); err != nil {
			return fmt.Errorf("encoding animation: %w", err)
		}
		scenes, _, _, err := e.IdentifyArtifact(ctx, out.path)
		if err != nil {
			return err
		}
		if scenes != len(kept) {
			return fmt.Errorf("encoded artifact holds %d scenes, want %d", scenes, len(kept))
		}
	}
	return nil
}
