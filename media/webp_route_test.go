package media

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// TestWebPFramesMatchCoalesce compares the webp-frames tool with ImageMagick's
// -coalesce over the combinations of codec, alpha, blending, disposal and
// frame geometry.
//
// Without alpha the two agree exactly, except on canvas no frame covers when
// the file does not declare alpha: libwebp leaves it transparent, as browsers
// do, and ImageMagick makes it opaque black. With alpha they agree exactly
// unless frames are blended over earlier content (blending, no disposal),
// where ImageMagick's own blend arithmetic differs from libwebp's: measured
// bounds are 1 level of alpha, 1 level of colour where both come out opaque
// and 2 levels of colour where both are partly transparent.
func TestWebPFramesMatchCoalesce(t *testing.T) {
	requireWebPBuilders(t)
	env := requireMagick(t)
	for _, codec := range []string{"lossy", "lossless", "mixed"} {
		for _, alpha := range []bool{false, true} {
			for _, blend := range []bool{true, false} {
				for _, dispose := range []int{0, 1} {
					for _, sub := range []bool{false, true} {
						c := webpCase{codec: codec, alpha: alpha, blend: blend, dispose: dispose, sub: sub}
						t.Run(c.String(), func(t *testing.T) {
							path := buildWebP(t, c)
							data, err := os.ReadFile(path)
							if err != nil {
								t.Fatal(err)
							}
							w, h, n := webpHeader(t, data)
							got := runWebPFrames(t, path, w, h, n)
							want := magickCoalesced(t, env, path, w, h, n)
							var d pixelDiff
							for i := range got {
								d.merge(diffImages(got[i], want[i]))
							}
							wantDiff := pixelDiff{}
							if alpha && blend && dispose == 0 {
								wantDiff = pixelDiff{alpha: 1, opaque: 0, partial: 2}
								if sub {
									wantDiff.opaque = 1
								}
							}
							if !alpha && sub {
								if d.uncovered == 0 {
									t.Error("no uncovered canvas found, the case no longer exercises it")
								}
							} else if d.uncovered != 0 {
								t.Errorf("%d uncovered pixels in a case that has none", d.uncovered)
							}
							d.uncovered = 0
							if d != wantDiff {
								t.Errorf("differences %+v, want %+v", d, wantDiff)
							}
						})
					}
				}
			}
		}
	}
}

// referenceFrames runs the decode that animated WebPs took before webp-frames:
// ImageMagick reads and coalesces every frame, then deletes the unselected.
// It writes the selected frames of path to dir as PNG and returns their
// source frame indices.
//
// ImageMagick reads neither the ICC profile nor the EXIF of an animated WebP.
// assign, when not nil, is a profile to assign to the frames and convert to
// sRGB, which is what webp-frames does with the file's own.
func referenceFrames(t *testing.T, env Env, path string, enc Encoding, maxFrames int, dir string, assign []byte) []int {
	t.Helper()
	ec, err := enc.resolve()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scenes, err := identifyHeaders(ctx, env, "webp:"+path, true)
	if err != nil {
		t.Fatal(err)
	}
	full, err := identifyHeaders(ctx, env, "webp:"+path, false)
	if err != nil {
		t.Fatal(err)
	}
	var cw, ch int
	for i, s := range scenes {
		w, h := s.canvas()
		cw, ch = max(cw, w), max(ch, h)
		scenes[i].hasICC = full[i].hasICC
	}
	cw, ch = uprightSize(cw, ch, scenes[0].orientation)
	tw, th, resize := fitSize(cw, ch, ec.maxEdge, ec.maxArea)
	selected := selectFrames(frameCount(resolvedFrames(t, maxFrames), len(scenes), true), len(scenes))
	infos := make([]FrameInfo, len(scenes))
	for i, s := range scenes {
		infos[i] = FrameInfo{hasICC: s.hasICC}
	}
	profiled, err := selectedProfiled(infos, selected)
	if err != nil {
		t.Fatal(err)
	}
	assignPath := filepath.Join(dir, ".assigned.icc")
	if assign != nil {
		if err := os.WriteFile(assignPath, assign, 0o600); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(assignPath) }()
	}
	err = withProfile(dir, profiled || assign != nil, func(profilePath string) error {
		args := append(LimitArgs(), "webp:"+path, "-coalesce")
		if del := DeleteExpr(selected, len(scenes)); del != "" {
			args = append(args, "-delete", del)
		}
		args = append(args, "-auto-orient")
		if assign != nil {
			args = append(args, "-profile", assignPath, "-profile", profilePath)
		} else if profiled {
			args = append(args, "-profile", profilePath)
		}
		if ec.format == JPEG {
			args = append(args, "-background", "white", "-alpha", "remove", "-alpha", "off")
		}
		if resize {
			args = append(args, "-filter", "Lanczos", "-resize", fmt.Sprintf("%dx%d!", tw, th))
		}
		pattern := filepath.Join(dir, "frame-") + "%03d" + ec.ext
		args = append(args, "-strip", "-scene", "0", "+adjoin", "-quality", strconv.Itoa(ec.quality), pattern)
		_, err := env.Run(ctx, "magick", args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

func resolvedFrames(t *testing.T, maxFrames int) int {
	t.Helper()
	n, err := resolveFrames(maxFrames)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAnimatedWebPFramesMatchTheImageMagickPath compares AnimationFrames with
// the decode it replaces, on animations with and without an ICC profile and
// EXIF orientation, resized and not. JPEG output is byte-identical. PNG output
// is pixel-identical: ImageMagick writes an opaque animation's frames with or
// without an alpha channel depending on the frame and the disposal method.
//
// ImageMagick reads neither the EXIF orientation nor the ICC profile of an
// animated WebP. The expected output of an oriented one is therefore upright as
// stored, and that of a profiled one is the stored pixels with the profile
// assigned and converted to sRGB, which the old path never did.
func TestAnimatedWebPFramesMatchTheImageMagickPath(t *testing.T) {
	requireWebPBuilders(t)
	env := requireMagick(t)
	p := requireProcessor(t)
	adobe := mustAdobeProfile(t)

	opaque := webpCase{codec: "mixed", blend: true, dispose: 1, frames: 20}
	alpha := webpCase{codec: "lossless", alpha: true, blend: true, frames: 20}
	png := Encoding{Format: PNG}
	for _, c := range []struct {
		name   string
		path   func(t *testing.T) string
		assign []byte // the profile the file carries
		enc    Encoding
		exact  bool
		bound  pixelDiff // for inexact cases
	}{
		{name: "opaque jpeg", path: func(t *testing.T) string { return buildWebP(t, opaque) }, exact: true},
		{name: "opaque png", path: func(t *testing.T) string { return buildWebP(t, opaque) }, enc: png, exact: true},
		{name: "opaque no disposal", path: func(t *testing.T) string {
			return buildWebP(t, webpCase{codec: "lossy", blend: false, frames: 20})
		}, enc: png, exact: true},
		{name: "opaque no disposal resized", path: func(t *testing.T) string {
			return buildWebP(t, webpCase{codec: "lossy", blend: true, frames: 20})
		}, enc: Encoding{Format: PNG, MaxEdge: 20}, exact: true},
		{name: "profiled jpeg", path: func(t *testing.T) string {
			return withWebPChunk(t, buildWebP(t, opaque), "icc", adobe)
		}, assign: adobe, exact: true},
		{name: "profiled png", path: func(t *testing.T) string {
			return withWebPChunk(t, buildWebP(t, opaque), "icc", adobe)
		}, assign: adobe, enc: png, exact: true},
		{name: "oriented jpeg", path: func(t *testing.T) string {
			return withWebPChunk(t, buildWebP(t, opaque), "exif", exifOrientation(6))
		}, exact: true},
		{name: "oriented and profiled jpeg", path: func(t *testing.T) string {
			return withWebPChunk(t, withWebPChunk(t, buildWebP(t, opaque), "icc", adobe), "exif", exifOrientation(3))
		}, assign: adobe, exact: true},
		{
			name: "alpha", path: func(t *testing.T) string { return buildWebP(t, alpha) }, enc: png,
			bound: pixelDiff{alpha: 1, opaque: 2, partial: 2},
		},
		{name: "alpha without blending", path: func(t *testing.T) string {
			c := alpha
			c.blend = false
			return buildWebP(t, c)
		}, enc: png, exact: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := c.path(t)
			refDir := t.TempDir()
			selected := referenceFrames(t, env, path, c.enc, 6, refDir, c.assign)
			images, out := framesInto(t, p, stage(t, path), c.enc, 6)
			if len(images) != len(selected) {
				t.Fatalf("%d frames, want %d", len(images), len(selected))
			}
			var d pixelDiff
			for i, im := range images {
				if im.Frame != selected[i] {
					t.Errorf("image %d shows frame %d, want %d", i, im.Frame, selected[i])
				}
				ref, got := filepath.Join(refDir, im.Name), filepath.Join(out, im.Name)
				if c.enc.Format == "" {
					want, err := os.ReadFile(ref)
					if err != nil {
						t.Fatal(err)
					}
					have, err := os.ReadFile(got)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(have, want) {
						t.Errorf("%s differs from the ImageMagick path", im.Name)
					}
					continue
				}
				d.merge(diffImages(toPAM(readNRGBA(t, got)), toPAM(readNRGBA(t, ref))))
			}
			if c.enc.Format != "" {
				t.Logf("%+v", d)
				want := c.bound
				if c.exact {
					want = pixelDiff{}
				}
				if d != want {
					t.Errorf("differences %+v, want %+v", d, want)
				}
			}
			if got := dirNames(t, out); len(got) != len(images) {
				t.Errorf("output dir holds %v", got)
			}
		})
	}
}

// TestHeaderOrientationIsAppliedToAnimatedWebP injects an orientation into the
// header read, which ImageMagick never reports for an animated WebP, and
// checks that the frames, whose pixels carry no EXIF, come out rotated and
// sized to match.
func TestHeaderOrientationIsAppliedToAnimatedWebP(t *testing.T) {
	requireWebPBuilders(t)
	p := requireProcessor(t)
	inner := p.run
	p.run = func(ctx context.Context, name string, args ...string) (Result, error) {
		res, err := inner(ctx, name, args...)
		if slices.Contains(args, "identify") && slices.Contains(args, "-ping") {
			res.Stdout = bytes.ReplaceAll(res.Stdout, []byte("|Undefined|END"), []byte("|RightTop|END"))
		}
		return res, err
	}
	path := buildWebP(t, webpCase{codec: "lossless", blend: true, frames: 4})
	images, out := framesInto(t, p, stage(t, path), Encoding{Format: PNG}, 0)
	if len(images) != 4 {
		t.Fatalf("%d frames", len(images))
	}
	for _, im := range images {
		if im.Width != webpCanvasH || im.Height != webpCanvasW {
			t.Errorf("%s is %dx%d, want %dx%d", im.Name, im.Width, im.Height, webpCanvasH, webpCanvasW)
		}
		got := readNRGBA(t, filepath.Join(out, im.Name))
		// RightTop rotates the stored image 90 degrees clockwise.
		src := webpFrameImage(im.Frame, webpCanvasW, webpCanvasH, false)
		for y := range webpCanvasW {
			for x := range webpCanvasH {
				if got.NRGBAAt(x, y) != src.NRGBAAt(y, webpCanvasH-1-x) {
					t.Fatalf("%s: pixel %d,%d is not the rotated source", im.Name, x, y)
				}
			}
		}
	}
}
