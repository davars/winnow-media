package media

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/davars/winnow-media/tool"
)

func TestVideoThumbnailArgsSampleAnimatedWebP(t *testing.T) {
	args := VideoThumbnailArgs("/in.mov", "/small.webp", "/large.webp", 2, 150, 256, 1024, 80)
	want := []string{
		"-y",
		"-i", "/in.mov",
		"-an",
		"-filter_complex", "[0:2]select='eq(n\\,0)+eq(n\\,10)+eq(n\\,20)+eq(n\\,30)+eq(n\\,40)+eq(n\\,50)+eq(n\\,60)+eq(n\\,70)+eq(n\\,80)+eq(n\\,90)+eq(n\\,100)+eq(n\\,110)+eq(n\\,120)+eq(n\\,130)+eq(n\\,140)',setpts=N/(2*TB),split=2[selected_small][selected_large];[selected_small]scale='min(256,iw)':'min(256,ih)':force_original_aspect_ratio=decrease[small];[selected_large]scale='min(1024,iw)':'min(1024,ih)':force_original_aspect_ratio=decrease[large]",
		"-map", "[small]",
		"-frames:v", "15",
		"-loop", "0",
		"-c:v", "libwebp",
		"-quality", "80",
		"-r", "2",
		"/small.webp",
		"-map", "[large]",
		"-frames:v", "15",
		"-loop", "0",
		"-c:v", "libwebp",
		"-quality", "80",
		"-r", "2",
		"/large.webp",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestVideoSampleCapsAtFifteenEvenlySpacedFrames(t *testing.T) {
	cases := map[int][]int{
		0:   {0},
		8:   {0, 1, 2, 3, 4, 5, 6, 7},
		15:  {0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14},
		16:  {0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14},
		150: {0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140},
	}
	for frames, want := range cases {
		if got := videoSample(frames); !slices.Equal(got.indices, want) {
			t.Errorf("videoSample(%v) indices = %+v, want %+v", frames, got.indices, want)
		}
	}
}

func TestGenerateVideoThumbnailsWritesBothSizes(t *testing.T) {
	env := testEnv(t)
	for _, name := range []string{"ffmpeg", "magick"} {
		if _, err := tool.Resolve(name); err != nil {
			t.Skipf("%s not available", name)
		}
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	if _, err := env.Run(context.Background(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=25",
		"-pix_fmt", "yuv420p", src); err != nil {
		t.Fatalf("generating video fixture: %v", err)
	}
	small, large := filepath.Join(dir, "small.webp"), filepath.Join(dir, "large.webp")
	if err := env.GenerateVideoThumbnails(context.Background(), src, small, large, 0, 75, 64, 200, 80); err != nil {
		t.Fatal(err)
	}
	for path, wantLong := range map[string]int{small: 64, large: 200} {
		out, err := env.Run(context.Background(), "magick", "identify", "-format", "%w %h\n", path)
		if err != nil {
			t.Fatalf("identify %s: %v", path, err)
		}
		var w, h int
		if _, err := fmt.Sscanf(strings.SplitN(string(out.Stdout), "\n", 2)[0], "%d %d", &w, &h); err != nil {
			t.Fatalf("parsing %q: %v", out.Stdout, err)
		}
		if max(w, h) != wantLong {
			t.Errorf("%s is %dx%d, want long side %d", filepath.Base(path), w, h, wantLong)
		}
	}
}
