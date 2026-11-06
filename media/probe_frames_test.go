package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// countingEnv wraps env so the argv of every magick run is recorded.
func countingEnv(env Env) (Env, func() [][]string) {
	var mu sync.Mutex
	var calls [][]string
	inner := env.Run
	env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		mu.Lock()
		calls = append(calls, append([]string{name}, args...))
		mu.Unlock()
		return inner(ctx, name, args...)
	}
	return env, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

// ProbeFrames reads headers only and applies no size caps: it reports a still
// and an animation of any size, with one -ping read.
func TestProbeFramesReportsLargeInputFromHeaders(t *testing.T) {
	real := requireMagick(t)
	const width, height = 8000, 6501
	// Made by magick, so that this process does not hold the pixels: a tool
	// it starts later inherits this process's peak size, which the resource
	// tests of the Processor bound.
	grayPNG := func(w, h int) []byte {
		path := filepath.Join(t.TempDir(), "gray.png")
		magick(t, real, "-size", fmt.Sprintf("%dx%d", w, h), "xc:gray", path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	dir := t.TempDir()
	multiPage := filepath.Join(dir, "multi.tif")
	magick(t, real, "-size", strconv.Itoa(maxDimension+1)+"x1", "xc:red", "-size", "8x8", "xc:blue", multiPage)
	multiPageData, err := os.ReadFile(multiPage)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		data   []byte
		mime   string
		frames int
	}{
		{"still with a side over the cap", grayPNG(maxDimension+1, 1), "image/png", 1},
		{"still with pixels over the cap", grayPNG(width, height), "image/png", 1},
		{"pages of a TIFF over the cap", multiPageData, "image/tiff", 2},
		{"one-frame GIF over the cap", oneColourGIF(t, width, height, 1), "image/gif", 1},
		{"GIF over the frame cap", oneColourGIF(t, 1, 1, maxAnimationFrames+1), "image/gif", maxAnimationFrames + 1},
		{"GIF with a side over the cap", oneColourGIF(t, maxDimension+1, 1, 2), "image/gif", 2},
		{"GIF with pixels over the cap", oneColourGIF(t, width, height, 2), "image/gif", 2},
		{"GIF over the decoded-pixel cap", oneColourGIF(t, 1000, 1000, maxDecodedPixels/1_000_000+1), "image/gif", maxDecodedPixels/1_000_000 + 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, calls := countingEnv(real)
			frames, err := env.ProbeFrames(context.Background(), writeFile(t, "content", c.data), c.mime)
			if err != nil {
				t.Fatal(err)
			}
			if len(frames) != c.frames {
				t.Errorf("%d frames, want %d", len(frames), c.frames)
			}
			got := calls()
			if len(got) != 1 || !slices.Contains(got[0], "-ping") {
				t.Errorf("runs = %q, want one -ping header read", got)
			}
		})
	}
}

// Only a still can lose its profile to the header-only read, so a still is
// read in full once its size is known to be within the caps.
func TestProbeFramesReportsProfilesFromHeaders(t *testing.T) {
	requireWebPBuilders(t)
	env := requireMagick(t)
	srgb, err := os.ReadFile(writeSRGBProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	still := filepath.Join(dir, "still.webp")
	magick(t, env, "-size", "8x8", "xc:red", still)
	profiledStill := withWebPChunk(t, still, "icc", srgb)

	gif := filepath.Join(dir, "anim.gif")
	profiled := filepath.Join(dir, "profiled.png")
	magick(t, env, "-size", "8x8", "xc:red", "-profile", writeSRGBProfile(t), profiled)
	magick(t, env, "-delay", "7", profiled, "-size", "8x8", "xc:blue", gif)

	cases := []struct {
		name string
		path string
		mime string
		want []FrameInfo
	}{
		{"profiled WebP still", profiledStill, "image/webp", []FrameInfo{{hasICC: true}}},
		{"plain WebP still", still, "image/webp", []FrameInfo{{}}},
		{"GIF with one profiled frame", gif, "image/gif", []FrameInfo{{delayCS: 7, hasICC: true}, {delayCS: 7}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := env.ProbeFrames(context.Background(), c.path, c.mime)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("%d frames, want %d", len(got), len(c.want))
			}
			for i := range got {
				if got[i].hasICC != c.want[i].hasICC || (c.want[i].delayCS != 0 && got[i].delayCS != c.want[i].delayCS) {
					t.Errorf("frame %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestProbeFramesDoesNotDecodeAnAnimation(t *testing.T) {
	real := requireMagick(t)
	env, calls := countingEnv(real)
	if _, err := env.ProbeFrames(context.Background(), writeFile(t, "content", oneColourGIF(t, 64, 64, 3)), "image/gif"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 || !slices.Contains(got[0], "-ping") {
		t.Errorf("runs = %q, want one -ping header read", got)
	}
	if !strings.Contains(strings.Join(got[0], " "), "identify") {
		t.Errorf("run %q is not identify", got[0])
	}
}
