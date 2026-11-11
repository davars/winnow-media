package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// animationImageEntry is an entry point that writes frames of an animation.
type animationImageEntry struct {
	name string
	run  func(ctx context.Context, path, dir string) ([]Image, Usage, error)
}

// animationImageEntries returns every entry point that writes frames of an
// animation, each asked for its first frames. Tests of the hostile-input gate
// run all of them, so that no path to the compositors skips it.
func animationImageEntries(p *Processor) []animationImageEntry {
	return []animationImageEntry{
		{"AnimationFrames", func(ctx context.Context, path, dir string) ([]Image, Usage, error) {
			return p.AnimationFrames(ctx, path, Encoding{}, 0, dir)
		}},
		{"AnimationFramesAt", func(ctx context.Context, path, dir string) ([]Image, Usage, error) {
			return p.AnimationFramesAt(ctx, path, Encoding{}, []int{0, 1, 2}, dir)
		}},
	}
}

var animationFrameOpNames = []string{"AnimationFrames", "AnimationFramesAt"}

// animationFrameOps is animationImageEntries by name, without the images.
func animationFrameOps(p *Processor) map[string]func(path, dir string) (Usage, error) {
	ops := map[string]func(path, dir string) (Usage, error){}
	for _, e := range animationImageEntries(p) {
		ops[e.name] = func(path, dir string) (Usage, error) {
			_, u, err := e.run(context.Background(), path, dir)
			return u, err
		}
	}
	return ops
}

// animationOps adds ProbeAnimation, which reads the same headers and writes
// nothing, to animationFrameOps.
func animationOps(p *Processor) map[string]func(path, dir string) (Usage, error) {
	ops := animationFrameOps(p)
	ops["ProbeAnimation"] = func(path, _ string) (Usage, error) {
		_, u, err := p.ProbeAnimation(context.Background(), path)
		return u, err
	}
	return ops
}

// videoEntry is an entry point that samples a video. call is given the
// arguments the refusal tests vary: n stands for VideoFrames' maxFrames and
// is mapped onto the entry point's own count; gateOnly reports an entry point
// that takes no encoding or directory, which applies to type and size
// refusals alone.
type videoEntry struct {
	name     string
	gateOnly bool
	call     func(p *Processor, path string, enc Encoding, n int, dir string) error
}

// videoEntries returns every entry point that reads a video. Tests of the
// hostile-input gate run all of them, so that no path to ffmpeg skips it.
func videoEntries() []videoEntry {
	ctx := context.Background()
	return []videoEntry{
		{"VideoFrames", false, func(p *Processor, path string, enc Encoding, n int, dir string) error {
			_, _, err := p.VideoFrames(ctx, path, enc, n, dir)
			return err
		}},
		{"VideoFramesAt", false, func(p *Processor, path string, enc Encoding, n int, dir string) error {
			times := []int64{0}
			switch {
			case n < 0:
				times = []int64{-1}
			case n > 1:
				times = make([]int64, n)
				for i := range times {
					times[i] = int64(i)
				}
			}
			_, _, err := p.VideoFramesAt(ctx, path, enc, times, dir)
			return err
		}},
		{"VideoFrameRun", false, func(p *Processor, path string, enc Encoding, n int, dir string) error {
			count := n
			if n == 0 {
				count = 1
			}
			_, _, err := p.VideoFrameRun(ctx, path, enc, 0, count, dir)
			return err
		}},
		{"ProbeVideo", true, func(p *Processor, path string, _ Encoding, _ int, _ string) error {
			_, _, err := p.ProbeVideo(ctx, path)
			return err
		}},
	}
}

func readFrames(t *testing.T, dir string, images []Image) [][]byte {
	t.Helper()
	out := make([][]byte, len(images))
	for i, img := range images {
		data, err := os.ReadFile(filepath.Join(dir, img.Name))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = data
	}
	return out
}

func TestProbeVideo(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	ctx := context.Background()

	withAudio := writeClip(t, env, ".mp4",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10:duration=4",
		"-f", "lavfi", "-i", "sine=duration=4",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest")
	info, usage, err := p.ProbeVideo(ctx, withAudio)
	if err != nil {
		t.Fatal(err)
	}
	if info.DurationMS < 3900 || info.DurationMS > 4100 || info.FrameRate != 10 ||
		info.Width != 160 || info.Height != 120 || info.Rotation != 0 || info.HDR ||
		info.Codec != "h264" || !info.HasAudio || info.Frames != 40 {
		t.Errorf("info = %+v", info)
	}
	if usage.CPU <= 0 {
		t.Errorf("usage = %+v, want the probe's CPU", usage)
	}

	silent := sdrClip(t, env, "160x120", 10, 4)
	if info, _, err = p.ProbeVideo(ctx, silent); err != nil || info.HasAudio {
		t.Errorf("silent clip: %+v, %v", info, err)
	}

	info, _, err = p.ProbeVideo(ctx, rotatedClip(t, env))
	if err != nil || info.Rotation != 90 || info.Width != 120 || info.Height != 160 {
		t.Errorf("rotated clip: %+v, %v", info, err)
	}

	info, _, err = p.ProbeVideo(ctx, hlgClip(t, env, testsrc("160x120", 10, 2)))
	if err != nil || !info.HDR || info.Codec != "hevc" {
		t.Errorf("HLG clip: %+v, %v", info, err)
	}

	noVideo := writeClip(t, env, ".mp4", "-f", "lavfi", "-i", "sine=duration=1", "-c:a", "aac")
	if _, _, err := p.ProbeVideo(ctx, noVideo); err == nil {
		t.Error("a file with no video stream was probed")
	}
}

func TestProbeAnimation(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	ctx := context.Background()

	full := image.Rect(0, 0, 24, 20)
	gifPath := writeFile(t, "x.gif", buildGIF(t, 24, 20, paletteA, []gifFrameSpec{
		{rect: full, delay: 5, palette: paletteA},
		{rect: full, delay: 0, palette: paletteA, shift: 1},
		{rect: full, delay: 1, palette: paletteA, shift: 2},
		{rect: full, delay: 30, palette: paletteA, shift: 3},
	}))
	info, _, err := p.ProbeAnimation(ctx, gifPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Frames != 4 || info.Width != 24 || info.Height != 20 || !slices.Equal(info.DelaysMS, []int{50, 100, 100, 300}) {
		t.Errorf("gif: %+v", info)
	}

	info, _, err = p.ProbeAnimation(ctx, writeGIF(t, solid, 7))
	if err != nil || info.Frames != 7 || info.Width != 64 || info.Height != 48 || len(info.DelaysMS) != 7 {
		t.Errorf("magick gif: %+v, %v", info, err)
	}

	t.Run("webp", func(t *testing.T) {
		requireWebPBuilders(t)
		info, _, err := p.ProbeAnimation(ctx, buildWebP(t, webpCase{codec: "lossless", frames: 6}))
		if err != nil || info.Frames != 6 || info.Width != webpCanvasW || info.Height != webpCanvasH || len(info.DelaysMS) != 6 {
			t.Errorf("webp: %+v, %v", info, err)
		}
	})
}

func TestVideoFramesAt(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	ctx := context.Background()
	src := sdrClip(t, env, "160x120", 10, 16)

	t.Run("sorted, de-duplicated and equal to the evenly spaced samples", func(t *testing.T) {
		out := t.TempDir()
		images, _, err := p.VideoFramesAt(ctx, src, Encoding{}, []int64{9500, 500, 4500, 500}, out)
		if err != nil {
			t.Fatal(err)
		}
		var times []int64
		for i, img := range images {
			times = append(times, img.Frames[0].TimeMS)
			if img.Name != frameName(i, ".jpg") || img.Width != 160 || img.Height != 120 || img.Frames[0].Index != 0 {
				t.Errorf("image %d = %+v", i, img)
			}
		}
		if !slices.Equal(times, []int64{500, 4500, 9500}) {
			t.Fatalf("times = %v", times)
		}
		if got := dirNames(t, out); len(got) != 3 {
			t.Errorf("output dir holds %v", got)
		}

		even, evenDir := videoInto(t, p, src, Encoding{}, 16)
		for i, img := range images {
			j := int(img.Frames[0].TimeMS / 1000)
			if !bytes.Equal(readFrames(t, out, images[i:i+1])[0], readFrames(t, evenDir, even[j:j+1])[0]) {
				t.Errorf("frame at %d ms differs from VideoFrames' sample", img.Frames[0].TimeMS)
			}
		}
	})

	t.Run("start of the video", func(t *testing.T) {
		images, _, err := p.VideoFramesAt(ctx, src, Encoding{}, []int64{0}, t.TempDir())
		if err != nil || len(images) != 1 || images[0].Frames[0].TimeMS != 0 {
			t.Errorf("images = %+v, err %v", images, err)
		}
	})

	// An argument is refused before any tool runs; a time outside the video
	// is refused after the one probe run.
	refusals := map[string]struct {
		times []int64
		runs  int
		want  string
	}{
		"empty":             {[]int64{}, 0, "times"},
		"negative":          {[]int64{1000, -1}, 0, "is negative"},
		"at the duration":   {[]int64{16000}, 1, "is not inside the video"},
		"past the duration": {[]int64{1000, 99_000}, 1, "is not inside the video"},
	}
	for name, c := range refusals {
		t.Run("refuses "+name, func(t *testing.T) {
			rec := record(p)
			defer func() { p.run = rec.inner }()
			out := t.TempDir()
			_, _, err := p.VideoFramesAt(ctx, src, Encoding{}, c.times, out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to say %q", err, c.want)
			}
			if n := rec.count(); n != c.runs {
				t.Errorf("%d tool runs, want %d", n, c.runs)
			}
			if got := dirNames(t, out); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}

	t.Run("refuses more than the maximum before any tool runs", func(t *testing.T) {
		rec := record(p)
		defer func() { p.run = rec.inner }()
		times := make([]int64, maxMaxFrames+1)
		for i := range times {
			times[i] = int64(i)
		}
		if _, _, err := p.VideoFramesAt(ctx, src, Encoding{}, times, t.TempDir()); err == nil {
			t.Fatal("succeeded")
		}
		if n := rec.count(); n != 0 {
			t.Errorf("%d tool runs", n)
		}
	})

	t.Run("accepts the maximum", func(t *testing.T) {
		p := requireProcessor(t)
		p.run = func(context.Context, string, ...string) (Result, error) { return Result{}, errors.New("tool run") }
		times := make([]int64, maxMaxFrames)
		for i := range times {
			times[i] = int64(i)
		}
		_, _, err := p.VideoFramesAt(ctx, src, Encoding{}, times, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "tool run") {
			t.Errorf("err = %v, want the stubbed tool failure", err)
		}
	})

	t.Run("does not change the caller's slice", func(t *testing.T) {
		times := []int64{9500, 500, 500}
		if _, _, err := p.VideoFramesAt(ctx, src, Encoding{}, times, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(times, []int64{9500, 500, 500}) {
			t.Errorf("times = %v", times)
		}
	})
}

func TestVideoFrameRun(t *testing.T) {
	forceSeek(t)
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	ctx := context.Background()
	src := sdrClip(t, env, "160x120", 10, 4)

	t.Run("consecutive frames from one ffmpeg run", func(t *testing.T) {
		rec := record(p)
		defer func() { p.run = rec.inner }()
		out := t.TempDir()
		images, _, err := p.VideoFrameRun(ctx, src, Encoding{}, 1500, 5, out)
		if err != nil {
			t.Fatal(err)
		}
		var times []int64
		var names []string
		for _, img := range images {
			times = append(times, img.Frames[0].TimeMS)
			names = append(names, img.Name)
		}
		if !slices.Equal(times, []int64{1500, 1600, 1700, 1800, 1900}) {
			t.Errorf("times = %v", times)
		}
		if want := []string{"frame-000.jpg", "frame-001.jpg", "frame-002.jpg", "frame-003.jpg", "frame-004.jpg"}; !slices.Equal(names, want) {
			t.Errorf("names = %v", names)
		}
		if got := dirNames(t, out); !slices.Equal(got, names) {
			t.Errorf("output dir holds %v, want only %v", got, names)
		}
		tools := map[string]int{}
		for _, call := range rec.calls {
			tools[call[0]]++
		}
		if tools["ffmpeg"] != 1 || tools["ffprobe"] != 1 {
			t.Errorf("tool runs = %v, want one probe and one ffmpeg run", tools)
		}
		for _, call := range rec.calls {
			if call[0] != "ffmpeg" {
				continue
			}
			joined := " " + strings.Join(call[1:], " ") + " "
			for _, want := range []string{" -copyts ", " -ss ", " -frames:v 5 ", "gte(t\\,1.499999)"} {
				if !strings.Contains(joined, want) {
					t.Errorf("ffmpeg call lacks %q: %q", want, call)
				}
			}
		}
	})

	t.Run("agrees with a single seek", func(t *testing.T) {
		out := t.TempDir()
		run, _, err := p.VideoFrameRun(ctx, src, Encoding{}, 2000, 3, out)
		if err != nil || len(run) != 3 {
			t.Fatalf("run = %+v, err %v", run, err)
		}
		at := t.TempDir()
		one, _, err := p.VideoFramesAt(ctx, src, Encoding{}, []int64{2000, 2200}, at)
		if err != nil || len(one) != 2 {
			t.Fatalf("seeks = %+v, err %v", one, err)
		}
		a, b := readFrames(t, out, []Image{run[0], run[2]}), readFrames(t, at, one)
		if !bytes.Equal(a[0], b[0]) || !bytes.Equal(a[1], b[1]) {
			t.Error("a frame of the run differs from the seek to its time")
		}
	})

	t.Run("stops at the end of the video", func(t *testing.T) {
		images, _, err := p.VideoFrameRun(ctx, src, Encoding{}, 3700, 10, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var times []int64
		for _, img := range images {
			times = append(times, img.Frames[0].TimeMS)
		}
		if !slices.Equal(times, []int64{3700, 3800, 3900}) {
			t.Errorf("times = %v", times)
		}
	})

	t.Run("tone-maps HDR", func(t *testing.T) {
		green := hlgClip(t, env, []string{"-f", "lavfi", "-i", "color=c=0x00c000:size=160x120:rate=10:duration=2"})
		out := t.TempDir()
		images, _, err := p.VideoFrameRun(ctx, green, Encoding{}, 0, 3, out)
		if err != nil || len(images) == 0 {
			t.Fatalf("images = %+v, err %v", images, err)
		}
		if rgb := pixelRGB(t, env, filepath.Join(out, images[0].Name)); rgb[1] < rgb[0]+60 || rgb[1] < rgb[2]+60 {
			t.Errorf("mean colour %v is not strongly green", rgb)
		}
	})

	t.Run("writes into a directory with a percent sign", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "100%d")
		if err := os.Mkdir(out, 0o700); err != nil {
			t.Fatal(err)
		}
		images, _, err := p.VideoFrameRun(ctx, src, Encoding{}, 0, 2, out)
		if err != nil || len(images) != 2 {
			t.Errorf("images = %+v, err %v", images, err)
		}
		if got := dirNames(t, out); len(got) != 2 {
			t.Errorf("output dir holds %v", got)
		}
	})

	// An argument is refused before any tool runs; a start outside the video
	// is refused after the one probe run.
	refusals := map[string]struct {
		start int64
		count int
		runs  int
		want  string
	}{
		"zero count":         {0, 0, 0, "count"},
		"negative count":     {0, -1, 0, "count"},
		"over the maximum":   {0, maxMaxFrames + 1, 0, "count"},
		"negative start":     {-1, 1, 0, "is negative"},
		"start at the end":   {4000, 1, 1, "is not inside the video"},
		"start past the end": {99_000, 1, 1, "is not inside the video"},
	}
	for name, c := range refusals {
		t.Run("refuses "+name, func(t *testing.T) {
			rec := record(p)
			defer func() { p.run = rec.inner }()
			out := t.TempDir()
			_, _, err := p.VideoFrameRun(ctx, src, Encoding{}, c.start, c.count, out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to say %q", err, c.want)
			}
			if n := rec.count(); n != c.runs {
				t.Errorf("%d tool runs, want %d", n, c.runs)
			}
			if got := dirNames(t, out); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}

	t.Run("cleans up when the run fails", func(t *testing.T) {
		// Calls: probe, ffmpeg, encode.
		for _, failAfter := range []int{1, 2, 3} {
			p := requireProcessor(t)
			rec := record(p)
			rec.failAfter = failAfter
			out := t.TempDir()
			if _, _, err := p.VideoFrameRun(ctx, src, Encoding{}, 0, 4, out); err == nil {
				t.Fatalf("failAfter=%d: succeeded", failAfter)
			}
			if got := dirNames(t, out); len(got) != 0 {
				t.Errorf("failAfter=%d: output dir holds %v", failAfter, got)
			}
		}
	})
}

func TestAnimationFramesAt(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	ctx := context.Background()

	check := func(t *testing.T, src string, frames int) {
		all := t.TempDir()
		every, _, err := p.AnimationFrames(ctx, src, Encoding{Format: PNG}, frames, all)
		if err != nil || len(every) != frames {
			t.Fatalf("every frame: %d images, err %v", len(every), err)
		}
		out := t.TempDir()
		images, _, err := p.AnimationFramesAt(ctx, src, Encoding{Format: PNG}, []int{frames - 1, 2, 2, 0}, out)
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for i, img := range images {
			got = append(got, img.Frames[0].Index)
			if img.Name != frameName(i, ".png") {
				t.Errorf("image %d is named %s", i, img.Name)
			}
			if !bytes.Equal(readFrames(t, out, images[i:i+1])[0], readFrames(t, all, every[img.Frames[0].Index:img.Frames[0].Index+1])[0]) {
				t.Errorf("frame %d differs from AnimationFrames' output", img.Frames[0].Index)
			}
		}
		if !slices.Equal(got, []int{0, 2, frames - 1}) {
			t.Errorf("frames = %v, want 0, 2, %d", got, frames-1)
		}

		// An argument is refused before any tool runs; an index outside the
		// animation is refused after the one header read, by the gate and
		// not by a compositor that fails later.
		forbidCompositor(t, p)
		for name, c := range map[string]struct {
			indices []int
			runs    int
			want    string
		}{
			"empty":                {[]int{}, 0, "indices"},
			"negative":             {[]int{1, -1}, 0, "is negative"},
			"at the frame count":   {[]int{frames}, 1, "is outside the animation's"},
			"past the frame count": {[]int{0, frames + 40}, 1, "is outside the animation's"},
		} {
			rec := record(p)
			dir := t.TempDir()
			_, _, err := p.AnimationFramesAt(ctx, src, Encoding{}, c.indices, dir)
			p.run = rec.inner
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want it to say %q", name, err, c.want)
			}
			if n := rec.count(); n != c.runs {
				t.Errorf("%s: %d tool runs, want %d", name, n, c.runs)
			}
			if got := dirNames(t, dir); len(got) != 0 {
				t.Errorf("%s: output dir holds %v", name, got)
			}
		}
	}

	t.Run("gif", func(t *testing.T) { check(t, writeGIF(t, solid, 12), 12) })
	t.Run("webp", func(t *testing.T) {
		requireWebPBuilders(t)
		check(t, buildWebP(t, webpCase{codec: "lossless", frames: 6}), 6)
	})

	t.Run("refuses more than the maximum before any tool runs", func(t *testing.T) {
		rec := record(p)
		defer func() { p.run = rec.inner }()
		indices := make([]int, maxMaxFrames+1)
		for i := range indices {
			indices[i] = i
		}
		if _, _, err := p.AnimationFramesAt(ctx, writeGIF(t, solid, 3), Encoding{}, indices, t.TempDir()); err == nil {
			t.Fatal("succeeded")
		}
		if n := rec.count(); n != 0 {
			t.Errorf("%d tool runs", n)
		}
	})

	t.Run("accepts the maximum", func(t *testing.T) {
		indices := make([]int, maxMaxFrames)
		for i := range indices {
			indices[i] = i
		}
		images, _, err := p.AnimationFramesAt(ctx, writeGIF(t, solid, maxMaxFrames), Encoding{}, indices, t.TempDir())
		if err != nil || len(images) != maxMaxFrames {
			t.Errorf("%d images, err %v", len(images), err)
		}
	})

	t.Run("picks the frames asked for, not evenly spaced ones", func(t *testing.T) {
		images, _, err := p.AnimationFramesAt(ctx, writeGIF(t, solid, 12), Encoding{}, []int{1, 2, 3}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, img := range images {
			got = append(got, img.Frames[0].Index)
		}
		if !slices.Equal(got, []int{1, 2, 3}) {
			t.Errorf("frames = %v, want 1, 2, 3", got)
		}
	})
}

// The TimeMS of an animation frame is when it starts in the source: the sum
// of the display delays before it, a stored delay below 20 ms counting as
// 100 ms. An Image reports the same time as EnvSource.Frames.
func TestAnimationImagesAreTimedFromTheirSourceStart(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	p := requireProcessor(t)
	ctx := context.Background()

	check := func(t *testing.T, src string, indices []int, want []int64) {
		t.Helper()
		images, _, err := p.AnimationFramesAt(ctx, src, Encoding{Format: PNG}, indices, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, img := range images {
			got = append(got, img.Frames[0].TimeMS)
		}
		if !slices.Equal(got, want) {
			t.Errorf("AnimationFramesAt%v times = %v, want %v", indices, got, want)
		}

		staged := stagedEnv(env, t.TempDir())
		source := openSource(t, staged, src, Hints{})
		encoded, err := source.EncodeFrames(ctx, Encoding{Format: PNG}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		frames := source.Frames()
		for _, img := range encoded {
			if f := frames[img.Frames[0].Index]; img.Frames[0].TimeMS != f.TimeMS {
				t.Errorf("EncodeFrames: frame %d at %d ms, Frames says %d ms", img.Frames[0].Index, img.Frames[0].TimeMS, f.TimeMS)
			}
		}
	}

	t.Run("gif", func(t *testing.T) {
		// Shown for 50, 100, 100, 300 and 70 ms.
		src := writeGrayAnimGIF(t, env, []int{10, 60, 110, 160, 210}, []int{5, 0, 1, 30, 7})
		check(t, src, []int{4, 1, 3}, []int64{50, 250, 550})
	})
	t.Run("webp", func(t *testing.T) {
		requireWebPBuilders(t)
		dir := t.TempDir()
		var args []string
		for k, ms := range []int{40, 250, 70, 30} {
			png := filepath.Join(dir, fmt.Sprintf("f%d.png", k))
			magick(t, env, "-size", "40x30", fmt.Sprintf("xc:gray(%d)", 20+50*k), png)
			webp := filepath.Join(dir, fmt.Sprintf("f%d.webp", k))
			run(t, "cwebp", "-quiet", "-lossless", png, "-o", webp)
			args = append(args, "-frame", webp, fmt.Sprintf("+%d+0+0+0-b", ms))
		}
		out := filepath.Join(dir, "anim.webp")
		run(t, "webpmux", append(args, "-loop", "0", "-o", out)...)
		check(t, stage(t, out), []int{0, 2, 3}, []int64{0, 290, 360})
	})
}

// An index at or past the frame count is refused by the gate after the header
// read, not by the compositor: the error is the gate's and no compositor runs.
func TestAnimationFramesAtRefusesAnIndexOutsideTheAnimationBeforeCompositing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		src    func(t *testing.T) string
		frames int
	}{
		"gif": {func(t *testing.T) string { return writeGIF(t, solid, 12) }, 12},
		"webp": {func(t *testing.T) string {
			requireWebPBuilders(t)
			return buildWebP(t, webpCase{codec: "lossless", frames: 6})
		}, 6},
	} {
		t.Run(name, func(t *testing.T) {
			src := c.src(t)
			p := requireProcessor(t)
			forbidCompositor(t, p)
			for _, indices := range [][]int{{c.frames}, {0, c.frames + 40}} {
				dir := t.TempDir()
				_, _, err := p.AnimationFramesAt(context.Background(), src, Encoding{}, indices, dir)
				if err == nil || !strings.Contains(err.Error(), "is outside the animation's") {
					t.Errorf("%v: err = %v, want the gate's refusal", indices, err)
				}
				if got := dirNames(t, dir); len(got) != 0 {
					t.Errorf("%v: output dir holds %v", indices, got)
				}
			}
		})
	}
}

// ProbeAnimation reads the headers without decoding pixels, for a one-frame
// WebP too, and the header read of the animation operations reports the
// still's profile.
func TestProbeAnimationDoesNotDecodeAStillWebP(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	env := requireMagick(t)
	p := requireProcessor(t)
	srgb, err := os.ReadFile(writeSRGBProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	still := filepath.Join(t.TempDir(), "still.webp")
	magick(t, env, "-size", "8x8", "xc:red", still)
	for name, path := range map[string]string{"plain": still, "profiled": withWebPChunk(t, still, "icc", srgb)} {
		t.Run(name, func(t *testing.T) {
			rec := record(p)
			defer func() { p.run = rec.inner }()
			info, _, err := p.ProbeAnimation(context.Background(), path)
			if err != nil || info.Frames != 1 {
				t.Fatalf("info %+v, err %v", info, err)
			}
			var m meter
			hdr, err := p.env(&m, nil).readSource(context.Background(), openRequest{path: path, dec: decoder{"magick", "webp"}, pinned: true, caps: true})
			layout := hdr.layout
			if err != nil || len(layout.infos) != 1 || layout.infos[0].hasICC != (name == "profiled") {
				t.Fatalf("layout %+v, err %v; want hasICC %t", layout, err, name == "profiled")
			}
			for _, call := range rec.calls {
				if !slices.Contains(call, "-ping") {
					t.Errorf("a still WebP was decoded to read its headers: %q", call)
				}
			}
		})
	}
}
