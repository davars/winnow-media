package media

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestVideoFilterWithARegion(t *testing.T) {
	crop := region{10, 6, 61, 45}
	const cut = "crop=61:45:10:6:exact=1"
	for _, c := range []struct {
		name         string
		hdr, reduced bool
		crop         region
		want         string
	}{
		{"SDR, whole frame", false, false, region{}, ""},
		{"SDR reduced, whole frame", false, true, region{}, "scale=80:60:flags=lanczos"},
		{"HDR, whole frame", true, false, region{}, hdrToneMap + ",format=yuv420p"},
		{"HDR reduced, whole frame", true, true, region{}, "scale=80:60:flags=lanczos,format=yuv444p16le," + hdrToneMap + ",format=yuv444p"},
		{"SDR", false, false, crop, cut},
		{"SDR reduced", false, true, crop, cut + ",scale=80:60:flags=lanczos"},
		{"HDR", true, false, crop, cut + ",format=yuv444p16le," + hdrToneMap + ",format=yuv444p"},
		{"HDR reduced", true, true, crop, cut + ",scale=80:60:flags=lanczos,format=yuv444p16le," + hdrToneMap + ",format=yuv444p"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := videoFilter(c.hdr, c.reduced, 80, 60, c.crop); got != c.want {
				t.Errorf("videoFilter = %q\nwant %q", got, c.want)
			}
		})
	}
}

// The reduction that covers twice the target is taken of the region, not of
// the frame.
func TestVideoPlanReducesTheRegion(t *testing.T) {
	info := videoInfo{width: 1600, height: 1200, frames: 10}
	enc := encoding{format: WebP, ext: ".webp", quality: 80, maxEdge: 20, maxArea: 400, region: region{100, 50, 400, 300}}
	plan := newVideoPlan(info, enc)
	if !plan.reduced || plan.fw != 40 || plan.fh != 30 {
		t.Errorf("plan reduces to %dx%d (reduced %v), want 40x30, twice the 20x15 target", plan.fw, plan.fh, plan.reduced)
	}
	want := "crop=400:300:100:50:exact=1,scale=40:30:flags=lanczos"
	if plan.filter != want {
		t.Errorf("filter = %q, want %q", plan.filter, want)
	}
	if got := plan.source(enc); got != (encodeSource{width: 20, height: 15}) {
		t.Errorf("source = %+v, want the exact size 20x15 of the region", got)
	}

	// A region that needs no reduction is delivered as cut, and sized exactly.
	enc.maxEdge, enc.maxArea = 4000, 4000*4000
	plan = newVideoPlan(info, enc)
	if plan.reduced || plan.fw != 400 || plan.fh != 300 || plan.filter != "crop=400:300:100:50:exact=1" {
		t.Errorf("plan = %+v, want the region delivered unscaled", plan)
	}
	if got := plan.source(enc); got != (encodeSource{width: 400, height: 300}) {
		t.Errorf("source = %+v, want the region's size", got)
	}
}

func vfOf(args []string) string {
	if i := slices.Index(args, "-vf"); i >= 0 {
		return args[i+1]
	}
	return ""
}

// The crop comes after the frames are selected and before anything resizes or
// tone-maps them.
func TestVideoRunCropsBeforeScalingAndToneMapping(t *testing.T) {
	forceSeek(t)
	for _, c := range []struct {
		name string
		trc  string
		want string
	}{
		{"SDR", "", "crop=40:30:10:6:exact=1,scale=16:12:flags=lanczos"},
		{"HDR", `,"color_transfer":"arib-std-b67"`, "crop=40:30:10:6:exact=1,scale=8:6:flags=lanczos,format=yuv444p16le," + hdrToneMap + ",format=yuv444p"},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub := newVideoStub()
			stub.probe = strings.Replace(stub.probe, `"start_time":"0"}]`, `"start_time":"0"`+c.trc+`}]`, 1)
			env := stub.env()
			env.Scratch, env.Stage = testScratch, stagerIn(t.TempDir())
			src, err := env.open(context.Background(), openRequest{
				path: "/in.mov", dec: sniffedVideoDecoder("/in.mov"), pinned: true, scratch: t.TempDir(), video: &videoRequest{},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			enc := encoding{format: WebP, ext: ".webp", quality: 80, maxEdge: 8, maxArea: 64, region: region{10, 6, 40, 30}}
			_, _ = src.encode(context.Background(), 0, []encoding{enc})
			if len(stub.runs) == 0 {
				t.Fatal("no ffmpeg run")
			}
			vf := vfOf(stub.runs[0])
			if i := strings.Index(vf, ",crop="); i < 0 || !strings.HasPrefix(vf[i+1:], c.want) || !strings.HasPrefix(vf, "select='") {
				t.Errorf("-vf = %q, want select, then %q", vf, c.want)
			}
		})
	}
}

// ImageMagick is not asked to crop a video's samples: they are already cut.
func TestVideoRegionIsNotCroppedAgainByTheEncode(t *testing.T) {
	plan := newVideoPlan(videoInfo{width: 160, height: 120}, encoding{maxEdge: 64, maxArea: 4096, region: region{1, 1, 50, 50}})
	if got := plan.source(encoding{maxEdge: 64, maxArea: 4096, region: region{1, 1, 50, 50}}); !got.crop.zero() {
		t.Errorf("source crops %+v", got.crop)
	}
}

func TestVideoRegionBounds(t *testing.T) {
	env := requireFFmpeg(t)
	src := rotatedClip(t, env) // 160x120 stored, 120x160 upright
	for _, c := range []struct {
		name string
		r    region
		ok   bool
	}{
		{"x+width is the upright width", region{100, 0, 20, 20}, true},
		{"x+width is one more", region{101, 0, 20, 20}, false},
		{"y+height is the upright height", region{0, 140, 20, 20}, true},
		{"y+height is one more", region{0, 141, 20, 20}, false},
		{"a width the stored frame has", region{0, 0, 160, 20}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			c2 := withRunCounts(env)
			dir := t.TempDir()
			enc := encoding{format: PNG, ext: ".png", quality: 80, maxEdge: 200, maxArea: 200 * 200, region: c.r}
			images, err := c2.videoFrames(context.Background(), sniffedVideoDecoder(src), src, enc, 1, dir)
			if c.ok {
				if err != nil || len(images) == 0 {
					t.Fatalf("images = %v, %v", images, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "outside") {
				t.Fatalf("err = %v, want a refusal that the region is outside the frame", err)
			}
			if n := len(c2.runs["ffmpeg"]); n != 0 {
				t.Errorf("the refused call ran ffmpeg %d times", n)
			}
		})
	}
}

// quadrantClip writes a clip of four flat quarters: yellow top-left, lime
// top-right, red bottom-left, black bottom-right.
func quadrantClip(t *testing.T, env Env, extra ...string) string {
	t.Helper()
	args := []string{
		"-f", "lavfi", "-i", "nullsrc=size=160x120:rate=5:duration=2,geq=r='if(lt(X,80),255,0)':g='if(lt(Y,60),255,0)':b=0",
		"-c:v", "libx264", "-preset", "ultrafast", "-qp", "0", "-pix_fmt", "yuv444p",
	}
	return writeClip(t, env, ".mp4", append(args, extra...)...)
}

// A rotated clip is cut in the picture as shown, which is the upright frame.
func TestVideoRegionOfARotatedClipIsInUprightCoordinates(t *testing.T) {
	env := requireFFmpeg(t)
	plain := quadrantClip(t, env)
	rotated := filepath.Join(t.TempDir(), "rot.mp4")
	if _, err := env.Run(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-display_rotation", "90", "-f", "mov", "-i", plain, "-c", "copy", rotated); err != nil {
		t.Fatal(err)
	}
	rotated = stage(t, rotated)
	enc := encoding{format: PNG, ext: ".png", quality: 80, maxEdge: 400, maxArea: 400 * 400}
	dec := sniffedVideoDecoder(rotated)

	whole := t.TempDir()
	images, err := env.videoFrames(context.Background(), dec, rotated, enc, 1, whole)
	if err != nil || len(images) != 1 {
		t.Fatalf("images = %v, %v", images, err)
	}
	if images[0].Width != 120 || images[0].Height != 160 {
		t.Fatalf("the upright frame is %dx%d, want 120x160", images[0].Width, images[0].Height)
	}
	colours := map[[3]int]bool{}
	for _, r := range []region{{0, 0, 40, 40}, {80, 0, 40, 40}, {0, 120, 40, 40}, {80, 120, 40, 40}} {
		dir := t.TempDir()
		enc.region = r
		got, err := env.videoFrames(context.Background(), dec, rotated, enc, 1, dir)
		if err != nil || len(got) != 1 {
			t.Fatalf("region %+v: images = %v, %v", r, got, err)
		}
		if got[0].Width != 40 || got[0].Height != 40 {
			t.Errorf("region %+v: output is %dx%d", r, got[0].Width, got[0].Height)
		}
		ref := filepath.Join(t.TempDir(), "ref.png")
		magick(t, env, filepath.Join(whole, images[0].Name), "-crop", sizeAt(r), "+repage", ref)
		if d := meanAbsoluteDifference(t, env, filepath.Join(dir, got[0].Name), ref); d > 0.002 {
			t.Errorf("region %+v differs from the same part of the whole frame by %g", r, d)
		}
		colours[pixelRGB(t, env, filepath.Join(dir, got[0].Name))] = true
	}
	if len(colours) != 4 {
		t.Errorf("the four corners of the upright frame gave %d colours, want 4: %v", len(colours), colours)
	}
}

// A 10-bit HLG frame cut to odd sizes still tone-maps: zscale refuses a
// chroma-subsampled frame whose dimensions are odd.
func TestVideoRegionOfAnHDRClipMayBeOdd(t *testing.T) {
	env := requireFFmpeg(t)
	clip := hlgClip(t, env, testsrc("160x120", 10, 2))
	dec := sniffedVideoDecoder(clip)
	for _, c := range []struct {
		name         string
		r            region
		maxEdge      int
		wantW, wantH int
	}{
		{"unscaled", region{5, 7, 31, 25}, 64, 31, 25},
		{"reduced", region{5, 7, 61, 45}, 30, 30, 22},
	} {
		t.Run(c.name, func(t *testing.T) {
			enc := encoding{format: PNG, ext: ".png", quality: 80, maxEdge: c.maxEdge, maxArea: c.maxEdge * c.maxEdge, region: c.r}
			images, err := env.videoFrames(context.Background(), dec, clip, enc, 1, t.TempDir())
			if err != nil || len(images) != 1 {
				t.Fatalf("images = %v, %v", images, err)
			}
			if images[0].Width != c.wantW || images[0].Height != c.wantH {
				t.Errorf("output is %dx%d, want %dx%d", images[0].Width, images[0].Height, c.wantW, c.wantH)
			}
		})
	}
}

// Samples decoded for one region do not serve an encode of another, though
// they have the same size.
func TestVideoSecondEncodeWithAnotherRegionDecodesAgain(t *testing.T) {
	forceSeek(t)
	env := requireFFmpeg(t)
	clip := sdrClip(t, env, "160x120", 10, 2)
	c := withRunCounts(env)
	c.Stage = stagerIn(t.TempDir())
	src, err := c.open(context.Background(), openRequest{
		path: clip, dec: sniffedVideoDecoder(clip), pinned: true, scratch: t.TempDir(), video: &videoRequest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	encode := func(r region) {
		t.Helper()
		enc := encoding{format: WebP, ext: ".webp", quality: 80, maxEdge: 64, maxArea: 64 * 64, region: r}
		out, err := src.encode(context.Background(), 1, []encoding{enc})
		if err != nil {
			t.Fatal(err)
		}
		out[0].Cleanup()
	}
	runs := func() int { return len(c.runs["ffmpeg"]) }

	encode(region{0, 0, 80, 60})
	first := runs()
	if first == 0 {
		t.Fatal("the first encode ran no ffmpeg")
	}
	encode(region{0, 0, 80, 60})
	if runs() != first {
		t.Errorf("the same region decoded again: %d runs after %d", runs(), first)
	}
	encode(region{40, 30, 80, 60})
	if runs() == first {
		t.Error("another region of the same size was served from the samples of the first")
	}
}

func TestVideoPlanCoversOnlyItsOwnRegion(t *testing.T) {
	info := videoInfo{width: 160, height: 120}
	a := newVideoPlan(info, encoding{maxEdge: 64, maxArea: 4096, region: region{0, 0, 80, 60}})
	b := newVideoPlan(info, encoding{maxEdge: 64, maxArea: 4096, region: region{40, 30, 80, 60}})
	if !a.covers(a) || a.covers(b) || b.covers(a) {
		t.Error("plans for different regions of one size cover each other")
	}
	whole := newVideoPlan(info, encoding{maxEdge: 64, maxArea: 4096})
	if whole.covers(a) || a.covers(whole) {
		t.Error("a plan for a region covers one for the whole frame")
	}
}

func TestVideoFramesWithoutARegionAreSampledAsBefore(t *testing.T) {
	info := videoInfo{width: 160, height: 120, frames: 10}
	plan := newVideoPlan(info, encoding{maxEdge: 20, maxArea: 400})
	if plan.filter != "scale=40:30:flags=lanczos" || !plan.region.zero() {
		t.Errorf("plan = %+v", plan)
	}
}
