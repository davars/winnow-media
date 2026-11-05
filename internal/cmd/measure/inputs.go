package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/davars/winnow-media/tool"
)

// inputBudget is the size generated files are filled to, just under the
// input cap.
const inputBudget = 24 << 20

// inputCap is the largest input file the operations accept.
const inputCap = 25 << 20

// iccProfile is the 580-byte public-domain compatibleWithAdobeRGB1998
// profile distributed by lcms2. It stands in for a camera's wide-gamut
// profile: the operations' cost depends on a profile being present, not on
// which one.
const iccProfile = "" +
	"AAACRGFyZ2wCIAAAbW50clJHQiBYWVogB9YABwAIAAMAHAAvYWNzcE1TRlQAAAAAbm9uZQAAAAAA" +
	"AAAAAAAAAAAAAAAAAPbWAAEAAAAA0y1hcmdsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAKZGVzYwAAAPwAAAB7Y3BydAAAAXgAAAA1d3RwdAAAAbAAAAAUYmtw" +
	"dAAAAcQAAAAUclhZWgAAAdgAAAAUZ1hZWgAAAewAAAAUYlhZWgAAAgAAAAAUclRSQwAAAhQAAAAO" +
	"Z1RSQwAAAiQAAAAOYlRSQwAAAjQAAAAOZGVzYwAAAAAAAAAhQ29tcGF0aWJsZSB3aXRoIEFkb2Jl" +
	"IFJHQiAoMTk5OCkAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAHRleHQAAAAAUHVibGljIERvbWFpbi4g" +
	"Tm8gV2FycmFudHksIFVzZSBhdCBvd24gcmlzay4AAAAAWFlaIAAAAAAAAPNRAAEAAAABFsxYWVog" +
	"AAAAAAAAAAAAAAAAAAAAAFhZWiAAAAAAAACcGAAAT6UAAAT8WFlaIAAAAAAAADSNAACgLAAAD5VY" +
	"WVogAAAAAAAAJjEAABAvAAC+nGN1cnYAAAAAAAAAAQIzAABjdXJ2AAAAAAAAAAECMwAAY3VydgAA" +
	"AAAAAAABAjMAAA=="

// inputs returns the measured inputs, then the extra animations.
func inputs(extra string) ([]inputSpec, error) {
	specs := []inputSpec{
		videoSpec("video-hevc-hlg-4k60", "hevc", "arib-std-b67"),
		videoSpec("video-hevc-pq-4k60", "hevc", "smpte2084"),
		videoSpec("video-h264-4k60", "h264", ""),
		longVideoSpec(),
		stillSpec("heic-48mp", 8064, 6048),
		stillSpec("heic-24mp", 5712, 4284),
		animSpec("gif", 2000, 707, 707),
		animSpec("gif", 1000, 1000, 1000),
		animSpec("gif", 2000, 64, 48),
		animSpec("webp", 2000, 707, 707),
		animSpec("webp", 1000, 1000, 1000),
	}
	for _, e := range strings.FieldsFunc(extra, func(r rune) bool { return r == ',' }) {
		parts := strings.Split(e, ":")
		var frames, w, h int
		if len(parts) != 3 {
			return nil, fmt.Errorf("extra %q is not kind:frames:WxH", e)
		}
		frames, err1 := strconv.Atoi(parts[1])
		_, err2 := fmt.Sscanf(parts[2], "%dx%d", &w, &h)
		if err1 != nil || err2 != nil || (parts[0] != "gif" && parts[0] != "gifu" && parts[0] != "webp") {
			return nil, fmt.Errorf("extra %q is not kind:frames:WxH", e)
		}
		specs = append(specs, animSpec(parts[0], frames, w, h))
	}
	return specs, nil
}

func runTool(ctx context.Context, name string, args ...string) error {
	path, err := tool.Resolve(name)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(tail))
	}
	return nil
}

func videoSpec(name, codec, transfer string) inputSpec {
	return inputSpec{
		name: name, file: name + ".mp4", op: "video",
		gen: func(ctx context.Context, path, work string) error {
			const seconds = 8
			// Bitrate for the video track, in kbit/s; corrected by the ratio
			// of the budget to the size produced.
			kbps := float64(inputBudget-(256<<10)) * 8 / seconds / 1000
			for attempt := 0; attempt < 4; attempt++ {
				if err := encodeVideo(ctx, path, codec, transfer, seconds, int(kbps)); err != nil {
					return err
				}
				info, err := os.Stat(path)
				if err != nil {
					return err
				}
				if info.Size() <= inputCap && info.Size() >= inputBudget*9/10 {
					return nil
				}
				kbps *= float64(inputBudget) / float64(info.Size())
			}
			return fmt.Errorf("could not fill the budget; last bitrate %.0f kbit/s", kbps)
		},
	}
}

// longVideoSpec is a video at the duration cap: six hours of a static
// picture, which a few megabytes hold.
func longVideoSpec() inputSpec {
	return inputSpec{
		name: "video-h264-6h", file: "video-h264-6h.mp4", op: "video",
		gen: func(ctx context.Context, path, _ string) error {
			return runTool(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-v", "error", "-y",
				"-f", "lavfi", "-i", "color=c=0x406080:size=1280x720:rate=5:duration=21600",
				"-vf", "drawgrid=w=80:h=80:t=2:c=white,format=yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "30", "-g", "250",
				"-movflags", "+faststart", "-f", "mp4", path)
		},
	}
}

func encodeVideo(ctx context.Context, path, codec, transfer string, seconds, kbps int) error {
	src := fmt.Sprintf("testsrc2=size=3840x2160:rate=60:duration=%d", seconds)
	pixFmt := "yuv420p"
	args := []string{"-hide_banner", "-nostdin", "-v", "error", "-y"}
	var codecArgs []string
	switch codec {
	case "hevc":
		pixFmt = "yuv420p10le"
		params := fmt.Sprintf("vbv-maxrate=%d:vbv-bufsize=%d:log-level=error", kbps*3/2, kbps*3)
		codecArgs = []string{"-c:v", "libx265", "-preset", "fast", "-b:v", strconv.Itoa(kbps) + "k", "-tag:v", "hvc1"}
		if transfer != "" {
			params += ":colorprim=bt2020:transfer=" + transfer + ":colormatrix=bt2020nc"
			codecArgs = append(codecArgs, "-color_primaries", "bt2020", "-color_trc", transfer, "-colorspace", "bt2020nc")
		}
		codecArgs = append(codecArgs, "-x265-params", params)
	case "h264":
		codecArgs = []string{
			"-c:v", "libx264", "-profile:v", "high", "-preset", "fast",
			"-b:v", strconv.Itoa(kbps) + "k", "-maxrate", strconv.Itoa(kbps*3/2) + "k", "-bufsize", strconv.Itoa(kbps*3) + "k",
		}
	default:
		return fmt.Errorf("unknown codec %q", codec)
	}
	args = append(args, "-f", "lavfi", "-i", src,
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-vf", "noise=alls=12:allf=t,format="+pixFmt)
	args = append(args, codecArgs...)
	args = append(args, "-c:a", "aac", "-b:a", "128k", "-shortest", "-movflags", "+faststart", "-f", "mp4", path)
	return runTool(ctx, "ffmpeg", args...)
}

func stillSpec(name string, w, h int) inputSpec {
	return inputSpec{
		name: name, file: name + ".heic", op: "still",
		gen: func(ctx context.Context, path, work string) error {
			icc := filepath.Join(work, "profile.icc")
			raw, err := base64.StdEncoding.DecodeString(iccProfile)
			if err != nil {
				return err
			}
			if err := os.WriteFile(icc, raw, 0o600); err != nil {
				return err
			}
			// Fractal detail plus mild noise keeps the encoder's output in
			// the size class of a phone photograph.
			for _, quality := range []int{85, 70, 55} {
				err := runTool(ctx, "magick", "-size", fmt.Sprintf("%dx%d", w, h), "plasma:fractal",
					"-attenuate", "0.25", "+noise", "Gaussian", "-profile", icc,
					"-quality", strconv.Itoa(quality), "heic:"+path)
				if err != nil {
					return err
				}
				info, err := os.Stat(path)
				if err != nil {
					return err
				}
				if info.Size() <= inputBudget {
					return nil
				}
				fmt.Fprintf(os.Stderr, "%s: %.1f MiB at quality %d, retrying lower\n", name, float64(info.Size())/(1<<20), quality)
			}
			return fmt.Errorf("still does not fit the budget")
		},
	}
}

func animSpec(kind string, frames, w, h int) inputSpec {
	name := fmt.Sprintf("%s-%dx%dx%d", kind, frames, w, h)
	spec := inputSpec{name: name, file: name + "." + kind, op: "animation"}
	switch kind {
	case "gif":
		spec.gen = func(_ context.Context, path, _ string) error { return writeGIF(path, frames, w, h, false) }
	case "gifu":
		spec.file = name + ".gif"
		spec.gen = func(_ context.Context, path, _ string) error { return writeGIF(path, frames, w, h, true) }
	default:
		spec.gen = func(ctx context.Context, path, work string) error { return writeWebP(ctx, path, work, frames, w, h) }
	}
	return spec
}

// patch is the edge of the square that changes from frame to frame; the rest
// of the canvas is static, as in an optimized animation.
const patch = 64

// writeGIF writes a GIF whose first frame fills the canvas and whose later
// frames each replace a patch-sized square of it, the way GIF optimizers
// store motion. With full set every frame fills the canvas instead, as an
// unoptimized GIF does: a flat background with a square of noise.
func writeGIF(path string, frames, w, h int, full bool) error {
	pal := make(color.Palette, 256)
	for i := range pal {
		pal[i] = color.RGBA{uint8(i), uint8(i * 5), uint8(255 - i), 255}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	out := &gif.GIF{Config: image.Config{Width: w, Height: h, ColorModel: pal}}
	for i := range frames {
		bounds := image.Rect(0, 0, w, h)
		if i > 0 && !full {
			pw, ph := min(patch, w), min(patch, h)
			x := rng.IntN(w - pw + 1)
			y := rng.IntN(h - ph + 1)
			bounds = image.Rect(x, y, x+pw, y+ph)
		}
		img := image.NewPaletted(bounds, pal)
		px, py := rng.IntN(max(1, w-patch+1)), rng.IntN(max(1, h-patch+1))
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			row := img.Pix[img.PixOffset(bounds.Min.X, y):][:bounds.Dx()]
			for x := range row {
				switch {
				case full && i > 0 && (x < px || x >= px+patch || y < py || y >= py+patch):
					row[x] = 10
				case i == 0 && !full:
					row[x] = uint8((x+y)/4) + uint8(rng.IntN(24))
				case i == 0:
					row[x] = 10
				default:
					row[x] = uint8(rng.IntN(256))
				}
			}
		}
		out.Image = append(out.Image, img)
		out.Delay = append(out.Delay, 4)
		out.Disposal = append(out.Disposal, gif.DisposalNone)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := gif.EncodeAll(f, out); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return checkBudget(path)
}

// writeWebP writes an animated WebP of a static textured canvas with a
// moving square, which libwebp's animation encoder stores as sub-frames.
func writeWebP(ctx context.Context, path, work string, frames, w, h int) error {
	still := filepath.Join(work, fmt.Sprintf("still-%dx%d.png", w, h))
	if err := runTool(ctx, "magick", "-size", fmt.Sprintf("%dx%d", w, h), "plasma:fractal", "-attenuate", "0.25", "+noise", "Gaussian", "png:"+still); err != nil {
		return err
	}
	overlay := fmt.Sprintf("[0:v][1:v]overlay=x='mod(n*9\\,%d)':y='mod(n*5\\,%d)':eval=frame,format=yuv420p",
		max(1, w-patch), max(1, h-patch))
	err := runTool(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-v", "error", "-y",
		"-loop", "1", "-framerate", "25", "-i", still,
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=red:size=%dx%d:rate=25", min(patch, w), min(patch, h)),
		"-filter_complex", overlay, "-frames:v", strconv.Itoa(frames),
		"-c:v", "libwebp_anim", "-lossless", "0", "-q:v", "70", "-loop", "0", "-f", "webp", path)
	if err != nil {
		return err
	}
	return checkBudget(path)
}

func checkBudget(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > inputCap {
		return fmt.Errorf("%s is %d bytes, over the input cap", filepath.Base(path), info.Size())
	}
	return nil
}
