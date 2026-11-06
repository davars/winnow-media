package media

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/davars/winnow-media/tool"
)

func TestVideoSampleArgsSeekToOneFrame(t *testing.T) {
	args := videoSampleArgs("/in.mov", "/tmp/s.nut", 2, 12.5, 4)
	want := []string{
		"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-max_alloc", "1073741824",
		"-filter_threads", "4",
		"-ss", "12.500000",
		"-threads", "4",
		"-protocol_whitelist", "file",
		"-i", "/in.mov",
		"-map", "0:2",
		"-an", "-sn", "-dn", "-frames:v", "1",
		"-c:v", "ffv1", "-f", "nut", "/tmp/s.nut",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestVideoEncodeArgsWriteAnimatedWebP(t *testing.T) {
	args := videoEncodeArgs("/d/samples.txt", "/small.webp", "/large.webp", 15, 256, 1024, 80, 4)
	want := []string{
		"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-max_alloc", "1073741824",
		"-filter_threads", "4",
		"-protocol_whitelist", "file",
		"-f", "concat", "-safe", "0", "-i", "/d/samples.txt",
		"-filter_complex", "[0:v]setpts=N/(2*TB),split=2[selected_small][selected_large];[selected_small]scale='min(256,iw)':'min(256,ih)':force_original_aspect_ratio=decrease[small];[selected_large]scale='min(1024,iw)':'min(1024,ih)':force_original_aspect_ratio=decrease[large]",
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

func TestVideoSeekSecondsTargetsHalfFrameBeforeIndex(t *testing.T) {
	cases := []struct {
		timing videoTiming
		index  int
		want   float64
	}{
		{videoTiming{rate: 25}, 0, 0},
		{videoTiming{rate: 25}, 10, 0.38},
		{videoTiming{rate: 25, offset: 1.5}, 10, 1.88},
		{videoTiming{rate: 25, offset: -2}, 10, 0},
		{videoTiming{}, 10, 0},
	}
	for _, c := range cases {
		if got := videoSeekSeconds(c.timing, c.index); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("videoSeekSeconds(%+v, %d) = %v, want %v", c.timing, c.index, got, c.want)
		}
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
		if got := videoSample(frames); !slices.Equal(got, want) {
			t.Errorf("videoSample(%v) = %+v, want %+v", frames, got, want)
		}
	}
}

func TestGenerateVideoThumbnailsSeeksEverySample(t *testing.T) {
	env := testEnv(t)
	for _, name := range []string{"ffmpeg", "ffprobe", "magick"} {
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
	var mu sync.Mutex
	var ffmpegRuns [][]string
	inner := env.Run
	env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		if name == "ffmpeg" {
			mu.Lock()
			ffmpegRuns = append(ffmpegRuns, args)
			mu.Unlock()
		}
		return inner(ctx, name, args...)
	}
	small, large := filepath.Join(dir, "small.webp"), filepath.Join(dir, "large.webp")
	if err := env.GenerateVideoThumbnails(context.Background(), src, small, large, 0, 75, 64, 200, 80); err != nil {
		t.Fatal(err)
	}

	// A run that reads the stream through a filter decodes all of it, so its
	// cost grows with the video's length. Each run must either seek to a
	// sample or read only the scratch samples.
	if len(ffmpegRuns) != maxVideoSampleFrames+1 {
		t.Errorf("ffmpeg runs = %d, want %d samples and one encode", len(ffmpegRuns), maxVideoSampleFrames)
	}
	encodes := 0
	for _, args := range ffmpegRuns {
		for _, a := range args {
			if strings.Contains(a, "select=") || strings.Contains(a, "select'") {
				t.Errorf("run filters the whole stream: %v", args)
			}
		}
		if slices.Contains(args, "-ss") {
			continue
		}
		encodes++
		if !slices.Contains(args, "concat") {
			t.Errorf("run neither seeks nor reads scratch samples: %v", args)
		}
	}
	if encodes != 1 {
		t.Errorf("%d runs without a seek, want the one encode", encodes)
	}

	for path, wantLong := range map[string]int{small: 64, large: 200} {
		out, err := env.Run(context.Background(), "magick", "identify", "-format", "%w %h\n", path)
		if err != nil {
			t.Fatalf("identify %s: %v", path, err)
		}
		lines := strings.Split(strings.TrimSpace(string(out.Stdout)), "\n")
		if len(lines) != 15 {
			t.Errorf("%s has %d frames, want 15", filepath.Base(path), len(lines))
		}
		var w, h int
		if _, err := fmt.Sscanf(lines[0], "%d %d", &w, &h); err != nil {
			t.Fatalf("parsing %q: %v", out.Stdout, err)
		}
		if max(w, h) != wantLong {
			t.Errorf("%s is %dx%d, want long side %d", filepath.Base(path), w, h, wantLong)
		}
	}
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "winnow-video-thumbs-*"))
	if len(left) != 0 {
		t.Errorf("scratch directories left behind: %v", left)
	}
}
