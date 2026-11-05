package media

import (
	"context"
	"fmt"
	"os"
)

// GIFFramesComposited is AnimationFrames for a GIF whose frames are
// composited by compose instead of ImageMagick. compose receives the indices
// of the selected frames and writes each as a full-canvas PNG at the paths
// it returns, in order. ImageMagick then encodes each PNG in a run of its
// own. It exists on the measurement branch only.
func (p *Processor) GIFFramesComposited(ctx context.Context, path string, enc Encoding, maxFrames int, dir string,
	compose func(selected []int, dir string) ([]string, error),
) ([]Image, Usage, error) {
	ec, err := enc.resolve()
	if err != nil {
		return nil, Usage{}, err
	}
	frames, err := resolveFrames(maxFrames)
	if err != nil {
		return nil, Usage{}, err
	}
	in, err := p.admit(animationSpec, path, dir)
	if err != nil {
		return nil, Usage{}, err
	}
	var m meter
	env := p.env(&m)
	images, err := inDir(in.dir, func() ([]Image, error) {
		scenes, err := readHeaders(ctx, env, in.decoder, in.path, "", true)
		if err != nil {
			return nil, err
		}
		var cw, ch int
		for _, s := range scenes {
			w, h := s.canvas()
			cw, ch = max(cw, w), max(ch, h)
		}
		tw, th, resize := fitSize(cw, ch, ec.maxEdge, ec.maxArea)
		selected := selectFrames(frameCount(frames, len(scenes), true), len(scenes))
		samples, err := compose(selected, in.dir)
		if err != nil {
			return nil, err
		}
		if len(samples) != len(selected) {
			return nil, fmt.Errorf("composited %d of %d frames", len(samples), len(selected))
		}
		defer removeFiles(samples)
		middle := []string{"-auto-orient"}
		if ec.format == JPEG {
			middle = append(middle, "-background", "white", "-alpha", "remove", "-alpha", "off")
		}
		if resize {
			middle = append(middle, "-resize", fmt.Sprintf("%dx%d!", tw, th))
		}
		middle = append(middle, "-strip")
		var images []Image
		err = encodeSamples(ctx, env, samples, middle, ec, in.dir, func(i, n, w, h int) {
			images = append(images, Image{Name: frameName(n, ec.ext), Width: w, Height: h, Frame: selected[i]})
		})
		return images, err
	})
	if err != nil {
		return nil, m.usage, err
	}
	return images, m.usage, nil
}

func removeFiles(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}
