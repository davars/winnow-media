// Examples defer Close as a caller would, without reading its error.
//
//nolint:errcheck
package media_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/davars/winnow-media/media"
)

// Open reads headers only, so it is also the way to ask what a file is.
func ExampleProcessor_Open() {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()

	src, _, err := p.Open(ctx, "input", media.OpenOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()

	info := src.Info()
	fmt.Printf("%s, %dx%d\n", info.Type, info.Width, info.Height)
	// Kind says which of Images, Animation and Video is filled in.
	switch info.Kind {
	case media.Still:
		fmt.Printf("%d image(s)\n", len(info.Images))
	case media.Animation:
		fmt.Printf("%d frames over %s\n", info.Animation.Frames, info.Animation.Duration)
	case media.Video:
		fmt.Printf("%s, %s\n", info.Video.Codec, info.Video.Duration)
	default:
		// A later version of the library may add a kind.
	}
}

// One call decodes the still once and writes every Encoding from that decode.
func ExampleSource_Encode() {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()
	src, _, err := p.Open(ctx, "photo.jpg", media.OpenOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()

	dir, err := os.MkdirTemp("", "out") // each Encode needs an empty directory
	if err != nil {
		log.Fatal(err)
	}
	images, usage, err := src.Encode(ctx, dir, media.Select{},
		media.Encoding{Format: media.WebP, MaxEdge: 2048},
		media.Encoding{MaxEdge: 256, Quality: 80}, // a JPEG
	)
	if err != nil {
		log.Fatal(err)
	}
	for _, img := range images { // 000.webp, then 001.jpg
		fmt.Printf("encoding %d: %s %dx%d\n", img.Encoding, img.Name, img.Width, img.Height)
	}
	fmt.Printf("cpu %s, peak rss %d bytes\n", usage.CPU, usage.MaxRSS)
}

// Even samples a video at evenly spaced times; Image.Frames says which moment
// each file shows.
func ExampleSource_Encode_video() {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()
	src, _, err := p.Open(ctx, "clip.mp4", media.OpenOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()

	dir, err := os.MkdirTemp("", "samples")
	if err != nil {
		log.Fatal(err)
	}
	images, _, err := src.Encode(ctx, dir, media.Select{Frames: media.Even(8)},
		media.Encoding{Format: media.WebP, MaxEdge: 1024})
	if err != nil {
		log.Fatal(err)
	}
	// There may be fewer than 8 files: a short video has fewer frames, and a
	// sample identical to the one before it is dropped.
	for _, img := range images {
		fmt.Printf("%s shows the frame at %s\n", img.Name, img.Frames[0].Time)
	}
}

// All with Animate writes the frames of an animation into one animated WebP.
func ExampleSource_Encode_animation() {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()
	src, _, err := p.Open(ctx, "loop.gif", media.OpenOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()

	dir, err := os.MkdirTemp("", "preview")
	if err != nil {
		log.Fatal(err)
	}
	images, _, err := src.Encode(ctx, dir, media.Select{Frames: media.All()},
		media.Encoding{Format: media.WebP, MaxEdge: 480, Animate: true})
	if err != nil {
		log.Fatal(err)
	}
	anim := images[0] // the one file; Frames lists the source frame of each scene
	fmt.Printf("%s holds %d scenes\n", anim.Name, len(anim.Frames))
}

// KeepDecode pays for a still's decode once: an overview first, then regions
// of the full-size picture without decoding it again.
func ExampleSource_Encode_region() {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()
	// KeepDecode may be set whatever the file is: animations and videos
	// ignore it.
	src, _, err := p.Open(ctx, "scan.tiff", media.OpenOptions{KeepDecode: true})
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()

	overview, err := os.MkdirTemp("", "overview")
	if err != nil {
		log.Fatal(err)
	}
	images, _, err := src.Encode(ctx, overview, media.Select{}, media.Encoding{MaxEdge: 1024})
	if err != nil {
		log.Fatal(err)
	}
	// Image.Region of a whole-frame output is the upright frame, known for
	// every still once it has been encoded.
	full := images[0].Region
	fmt.Printf("upright size %dx%d, %d bytes kept\n", full.Width, full.Height, src.KeptBytes())

	detail, err := os.MkdirTemp("", "detail")
	if err != nil {
		log.Fatal(err)
	}
	quarter := media.Region{X: full.Width / 2, Y: full.Height / 2, Width: full.Width / 2, Height: full.Height / 2}
	if _, _, err := src.Encode(ctx, detail, media.Select{Region: quarter}, media.Encoding{MaxEdge: 2048}); err != nil {
		log.Fatal(err)
	}
}

// A configuration for input nobody vouches for. The numbers are examples:
// choose them from what the host can bear (see the package documentation's
// Sizing section).
func ExampleNewProcessor_hostileInput() {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{
		// Name the types. A nil list follows Types(), which a later version
		// of the library may extend.
		Types: []string{"image/jpeg", "image/png", "image/webp", "image/gif", "video/mp4"},
		Caps: media.Caps{
			MaxInputBytes:      64 << 20,    // the file on disk
			MaxFramePixels:     50_000_000,  // one decoded frame: memory per tool run
			MaxAnimationFrames: 1000,        // frames an animation may declare
			MaxDecodedPixels:   900_000_000, // every frame of an animation: time to decode it
			MaxReturnedPixels:  64_000_000,  // the frames one call asks for: time and scratch
		},
		Resources: media.Resources{
			Memory:    512 << 20,        // ImageMagick's pixel cache in memory
			Map:       1 << 30,          // and memory-mapped from scratch
			Disk:      2 << 30,          // and on disk; also bounds what a Source keeps
			Threads:   2,                // per tool run
			TimeLimit: 60 * time.Second, // ImageMagick's work on one image
			CPUTime:   90 * time.Second, // CPU of one tool run, over all its threads
		},
		Dir: "/var/lib/myapp/scratch", // on disk, with room for Disk per concurrent call
	})
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()

	// The library sets no deadline on a call; the context is the wall-clock
	// bound.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	src, _, err := p.Open(ctx, "upload", media.OpenOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()
}

// Branch on the sentinels, never on an error's text.
func Example_errors() {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()

	src, _, err := p.Open(ctx, "upload", media.OpenOptions{})
	switch {
	case err == nil:
		defer src.Close()
	case errors.Is(err, media.ErrUnsupportedType), errors.Is(err, media.ErrTooLarge):
		// This Processor will never accept the file: reject it.
	case errors.Is(err, media.ErrInvalidRequest):
		// The call was wrong, not the file: fix the caller.
	case errors.Is(err, media.ErrInputChanged):
		// The file was replaced: open it again.
	case errors.Is(err, media.ErrTimeLimit):
		// The file cost more than Resources allow: retry later or reject.
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller's own context ended the call.
	case errors.Is(err, media.ErrClosed):
		// The Processor was closed: the program is shutting down.
	default:
		// A *ToolError, most often a file its decoder cannot read, or an
		// operating-system error. The tool's stderr is for operators' logs
		// only: it can quote the input.
		log.Printf("open: %v; stderr: %s", err, media.Stderr(err))
	}
}
