package media

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestVideoFilterWithARegion(t *testing.T) {
	crop := region{10, 6, 61, 45}
	const cut = `crop=w='if(gte(in_w\,71)*gte(in_h\,51)\,61\,-1)':h=45:x=10:y=6:exact=1`
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
	r := region{100, 50, 400, 300}
	enc := encoding{format: WebP, ext: ".webp", quality: 80, maxEdge: 20, maxArea: 400}
	plan := newVideoPlan(info, r, enc, defaultThreads)
	if !plan.reduced || plan.fw != 40 || plan.fh != 30 {
		t.Errorf("plan reduces to %dx%d (reduced %v), want 40x30, twice the 20x15 target", plan.fw, plan.fh, plan.reduced)
	}
	want := cropFilter(r) + ",scale=40:30:flags=lanczos"
	if plan.filter != want {
		t.Errorf("filter = %q, want %q", plan.filter, want)
	}
	if got := plan.source(enc); got != (encodeSource{width: 20, height: 15}) {
		t.Errorf("source = %+v, want the exact size 20x15 of the region", got)
	}

	// A region that needs no reduction is delivered as cut, and sized exactly.
	enc.maxEdge, enc.maxArea = 4000, 4000*4000
	plan = newVideoPlan(info, r, enc, defaultThreads)
	if plan.reduced || plan.fw != 400 || plan.fh != 300 || plan.filter != cropFilter(r) {
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
		{"SDR", "", cropFilter(region{10, 6, 40, 30}) + ",scale=32:24:flags=lanczos"},
		{"HDR", `,"color_transfer":"arib-std-b67"`, cropFilter(region{10, 6, 40, 30}) + ",scale=16:12:flags=lanczos,format=yuv444p16le," + hdrToneMap + ",format=yuv444p"},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub := newVideoStub()
			stub.probe = strings.Replace(stub.probe, `"start_time":"0"}]`, `"start_time":"0"`+c.trc+`}]`, 1)
			enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 16, MaxArea: 256}
			_ = stub.encode(t, Select{Frames: Even(15), Region: Region{X: 10, Y: 6, Width: 40, Height: 30}}, enc)
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

// With several targets the samples are decoded for the one whose region
// output is largest, which is not always the one whose whole-frame output is.
func TestVideoRegionDecodeCoversTheLargestTarget(t *testing.T) {
	forceSeek(t)
	stub := newVideoStub()
	stub.probe = strings.Replace(stub.probe, `"width":64,"height":48`, `"width":160,"height":120`, 1)
	r := Region{X: 10, Y: 10, Width: 100, Height: 20}
	// On the whole 160x120 frame the first fits to 28x21, the second to
	// 18x13; on the region, to 30x6 and 35x7.
	a := Encoding{Format: WebP, Quality: 80, MaxEdge: 30, MaxArea: 600}
	b := Encoding{Format: WebP, Quality: 80, MaxEdge: 40, MaxArea: 256}
	_ = stub.encode(t, Select{Frames: Even(15), Region: r}, a, b)
	if len(stub.runs) == 0 {
		t.Fatal("no ffmpeg run")
	}
	if got := vfScale(stub.runs[0]); got != "70x14" {
		t.Errorf("ffmpeg scale = %q, want 70x14, twice the larger 35x7 output", got)
	}
}

// ImageMagick is not asked to crop a video's samples: they are already cut.
func TestVideoRegionIsNotCroppedAgainByTheEncode(t *testing.T) {
	enc := encoding{maxEdge: 64, maxArea: 4096}
	plan := newVideoPlan(videoInfo{width: 160, height: 120}, region{1, 1, 50, 50}, enc, defaultThreads)
	if got := plan.source(enc); !got.crop.zero() {
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
			p := requireProcessor(t)
			rec := record(p)
			dir := t.TempDir()
			enc := Encoding{Format: PNG, Quality: 80, MaxEdge: 200, MaxArea: 200 * 200}
			images, _, err := openSourceOn(t, p, src, OpenOptions{}).Encode(context.Background(), dir,
				Select{Frames: Even(1), Region: c.r.exported()}, enc)
			if c.ok {
				if err != nil || len(images) == 0 {
					t.Fatalf("images = %v, %v", images, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "outside") {
				t.Fatalf("err = %v, want a refusal that the region is outside the frame", err)
			}
			if n := len(rec.ran("ffmpeg")); n != 0 {
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
	enc := Encoding{Format: PNG, Quality: 80, MaxEdge: 400, MaxArea: 400 * 400}
	src := openSourceOn(t, requireProcessor(t), rotated, OpenOptions{})

	whole := t.TempDir()
	images, _, err := src.Encode(context.Background(), whole, Select{Frames: Even(1)}, enc)
	if err != nil || len(images) != 1 {
		t.Fatalf("images = %v, %v", images, err)
	}
	if images[0].Width != 120 || images[0].Height != 160 {
		t.Fatalf("the upright frame is %dx%d, want 120x160", images[0].Width, images[0].Height)
	}
	colours := map[[3]int]bool{}
	for _, r := range []region{{0, 0, 40, 40}, {80, 0, 40, 40}, {0, 120, 40, 40}, {80, 120, 40, 40}} {
		dir := t.TempDir()
		got, _, err := src.Encode(context.Background(), dir, Select{Frames: Even(1), Region: r.exported()}, enc)
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
	src := openSourceOn(t, requireProcessor(t), clip, OpenOptions{})
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
			enc := Encoding{Format: PNG, Quality: 80, MaxEdge: c.maxEdge, MaxArea: c.maxEdge * c.maxEdge}
			images, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Even(1), Region: c.r.exported()}, enc)
			if err != nil || len(images) != 1 {
				t.Fatalf("images = %v, %v", images, err)
			}
			if images[0].Width != c.wantW || images[0].Height != c.wantH {
				t.Errorf("output is %dx%d, want %dx%d", images[0].Width, images[0].Height, c.wantW, c.wantH)
			}
		})
	}
}

// A Source keeps no decoded sample: every Encode decodes afresh, whatever its
// region.
func TestVideoSourceDecodesAgainForEveryEncode(t *testing.T) {
	forceSeek(t)
	env := requireFFmpeg(t)
	clip := sdrClip(t, env, "160x120", 10, 2)
	p := requireProcessor(t)
	rec := record(p)
	src := openSourceOn(t, p, clip, OpenOptions{})
	for i, r := range []Region{{Width: 80, Height: 60}, {Width: 80, Height: 60}, {X: 40, Y: 30, Width: 80, Height: 60}} {
		before := len(rec.ran("ffmpeg"))
		if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Even(1), Region: r},
			Encoding{Format: WebP, Quality: 80, MaxEdge: 64, MaxArea: 64 * 64}); err != nil {
			t.Fatal(err)
		}
		if len(rec.ran("ffmpeg")) == before {
			t.Errorf("encode %d ran no ffmpeg", i)
		}
		for _, e := range dirNames(t, src.dir) {
			if strings.HasPrefix(e, ".sample-") {
				t.Errorf("encode %d left the sample %s", i, e)
			}
		}
	}
}

func TestVideoPlanCoversOnlyItsOwnRegion(t *testing.T) {
	info := videoInfo{width: 160, height: 120}
	enc := encoding{maxEdge: 64, maxArea: 4096}
	a := newVideoPlan(info, region{0, 0, 80, 60}, enc, defaultThreads)
	b := newVideoPlan(info, region{40, 30, 80, 60}, enc, defaultThreads)
	if !a.covers(a) || a.covers(b) || b.covers(a) {
		t.Error("plans for different regions of one size cover each other")
	}
	whole := newVideoPlan(info, region{}, enc, defaultThreads)
	if whole.covers(a) || a.covers(whole) {
		t.Error("a plan for a region covers one for the whole frame")
	}
}

func TestVideoFramesWithoutARegionAreSampledAsBefore(t *testing.T) {
	info := videoInfo{width: 160, height: 120, frames: 10}
	plan := newVideoPlan(info, region{}, encoding{maxEdge: 20, maxArea: 400}, defaultThreads)
	if plan.filter != "scale=40:30:flags=lanczos" || !plan.region.zero() {
		t.Errorf("plan = %+v", plan)
	}
}

// ffmpeg's crop moves a region that overruns the frame back inside it rather
// than failing, so a stream whose frames shrink after the probe must not yield
// a region that was checked only against the probe's size.
func TestVideoRegionOutsideAFrameThatShrankIsRefused(t *testing.T) {
	env := requireFFmpeg(t)
	segment := func(name, src string, offset string) string {
		path := filepath.Join(t.TempDir(), name)
		if _, err := env.Run(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", src, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
			"-output_ts_offset", offset, "-f", "mpegts", path); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// 200x100 for two seconds, then 160x100 with a lime band on its left.
	wide := segment("a.ts", "color=red:size=200x100:rate=5:duration=2", "0")
	narrow := segment("b.ts", "color=blue:size=160x100:rate=5:duration=2,drawbox=x=0:y=0:w=60:h=100:color=lime:t=fill", "2")
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte(wide+narrow), 0o600); err != nil {
		t.Fatal(err)
	}
	src := openSourceOn(t, requireProcessor(t), remuxToMatroska(t, env, path), OpenOptions{})
	enc := Encoding{Format: PNG, Quality: 80, MaxEdge: 400, MaxArea: 400 * 400}
	dir := t.TempDir()
	images, _, err := src.Encode(context.Background(), dir, Select{Frames: Times(3000), Region: Region{X: 100, Width: 100, Height: 50}}, enc)
	if err == nil {
		var got [3]int
		if len(images) > 0 {
			got = pixelRGB(t, env, filepath.Join(dir, images[0].Name))
		}
		t.Errorf("a region past the 160-pixel frame gave %d images (first about %v)", len(images), got)
	}
}

// A shrink between two samples fails the call whichever way the samples are
// read, and leaves none of them, the one taken before the shrink included.
func TestVideoRegionShrinkBetweenSamplesLeavesNothing(t *testing.T) {
	env := requireFFmpeg(t)
	segment := func(name, src string) string {
		path := filepath.Join(t.TempDir(), name)
		if _, err := env.Run(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", src, "-c:v", "libx264", "-preset", "ultrafast", "-g", "5", "-pix_fmt", "yuv420p", path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	a := segment("a.ts", "color=red:size=200x100:rate=5:duration=2")
	b := segment("b.ts", "color=blue:size=160x100:rate=5:duration=2")
	list := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(list, []byte("file '"+a+"'\nfile '"+b+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	joined := filepath.Join(t.TempDir(), "joined.ts")
	if _, err := env.Run(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", list, "-c", "copy", joined); err != nil {
		t.Fatal(err)
	}
	src := openSourceOn(t, requireProcessor(t), remuxToMatroska(t, env, joined), OpenOptions{})
	for name, limit := range map[string]int64{"seek": 0, "select": 1 << 40} {
		t.Run(name, func(t *testing.T) {
			forceSelectLimit(t, limit)
			enc := Encoding{Format: PNG, Quality: 80, MaxEdge: 400, MaxArea: 400 * 400}
			dir := t.TempDir()
			images, _, err := src.Encode(context.Background(), dir, Select{Frames: Times(1000, 3000), Region: Region{X: 100, Width: 100, Height: 50}}, enc)
			if err == nil {
				t.Errorf("a region past the 160-pixel frame gave %d images", len(images))
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("the refused call left %q", names)
			}
		})
	}
}

// remuxToMatroska copies the MPEG-TS file at path into Matroska, which the
// Processor reads, keeping the in-band parameter sets that let a stream change
// size.
func remuxToMatroska(t *testing.T, env Env, path string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "input.mkv")
	if _, err := env.Run(context.Background(), "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "mpegts", "-i", path, "-c", "copy", "-f", "matroska", out); err != nil {
		t.Fatal(err)
	}
	return out
}
