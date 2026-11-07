package media

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const policyRefusal = "security policy"

// runMagick runs magick through the processor's own runner.
func runMagick(p *Processor, args ...string) (Result, error) {
	return p.run(context.Background(), "magick", args...)
}

// mvgFile writes a vector drawing that composites the text file textPath.
func mvgFile(t *testing.T, textPath string) string {
	t.Helper()
	return writeFile(t, "draw.mvg", []byte("push graphic-context\nviewbox 0 0 64 64\n"+
		"image over 0,0 0,0 'text:"+textPath+"'\npop graphic-context\n"))
}

// gifBytes returns a GIF whose logical screen and only frame are w x h.
func gifBytes(w, h int) []byte {
	b := []byte("GIF89a")
	b = binary.LittleEndian.AppendUint16(b, uint16(w))
	b = binary.LittleEndian.AppendUint16(b, uint16(h))
	b = append(b, 0x80, 0, 0, 0, 0, 0, 255, 255, 255)
	b = append(b, 0x2C, 0, 0, 0, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(w))
	b = binary.LittleEndian.AppendUint16(b, uint16(h))
	return append(b, 0, 0x02, 0x02, 0x44, 0x01, 0x00, 0x3B)
}

func TestRunnerEnvironmentIgnoresCallerConfiguration(t *testing.T) {
	p := requireProcessor(t)
	widening := `<policymap>
<policy domain="resource" name="memory" value="8GiB"/>
<policy domain="coder" rights="read|write" pattern="{MVG,TEXT,PNG}"/>
</policymap>`
	home, xdg := t.TempDir(), t.TempDir()
	for _, path := range []string{
		filepath.Join(home, ".config", "ImageMagick", "policy.xml"),
		filepath.Join(xdg, "ImageMagick", "policy.xml"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(widening), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	secret := writeFile(t, "secret.txt", []byte("secret"))
	out := filepath.Join(t.TempDir(), "out.png")
	_, err := runMagick(p, "mvg:"+mvgFile(t, secret), "png:"+out)
	if err == nil || !strings.Contains(err.Error(), policyRefusal) {
		t.Fatalf("MVG read with a widening user policy: err = %v, want a policy refusal", err)
	}
}

func TestRunnerRefusesDangerousCoders(t *testing.T) {
	p := requireProcessor(t)
	text := writeFile(t, "secret.txt", []byte("secret"))
	out := filepath.Join(t.TempDir(), "out.png")
	for _, input := range []string{
		"mvg:" + mvgFile(t, text),
		"text:" + text,
		"msl:" + text,
		"ps:" + text,
		"eps:" + text,
		"pdf:" + text,
		"https://example.invalid/x.png",
		"http://example.invalid/x.png",
		"@" + text,
	} {
		t.Run(input[:strings.IndexAny(input, ":@")+1], func(t *testing.T) {
			_, err := runMagick(p, input, "png:"+out)
			if err == nil || !strings.Contains(err.Error(), policyRefusal) {
				t.Errorf("err = %v, want a policy refusal", err)
			}
			if _, err := os.Stat(out); err == nil {
				t.Error("output was written")
			}
		})
	}
}

func TestMVGGivenToOperationsIsUnsupportedWithoutRunningATool(t *testing.T) {
	p := requireProcessor(t)
	rec := record(p)
	mvg := mvgFile(t, writeFile(t, "secret.txt", []byte("secret")))
	if _, _, err := p.Still(context.Background(), mvg, Encoding{}, t.TempDir()); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("Still: err = %v, want ErrUnsupportedType", err)
	}
	for name, op := range animationOps(p) {
		if _, err := op(mvg, t.TempDir()); !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("%s: err = %v, want ErrUnsupportedType", name, err)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("%d tool runs", n)
	}
}

func TestPNGNamedGIFIsUnsupportedWithoutRunningATool(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	rec := record(p)
	forbidCompositor(t, p)
	png := filepath.Join(t.TempDir(), "x.gif")
	magick(t, env, "-size", "8x8", "xc:red", "png:"+png)
	for name, op := range animationOps(p) {
		if _, err := op(png, t.TempDir()); !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("%s: err = %v, want ErrUnsupportedType", name, err)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("%d tool runs", n)
	}
}

func zipBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, err := w.Create("payload.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("payload"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestPolyglotIsReadAsItsSniffedType(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	clean := writeGIF(t, env, solid, 6)
	cleanData, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	wantImages, cleanOut := framesInto(t, p, clean, Encoding{}, 0)

	for name, tail := range map[string][]byte{
		"zip":          zipBytes(t),
		"hls playlist": []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:1,\nfile:///etc/passwd\n#EXT-X-ENDLIST\n"),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, "x.gif", append(slices.Clone(cleanData), tail...))
			rec := record(p)
			defer func() { p.run = rec.inner }()
			images, out := framesInto(t, p, path, Encoding{}, 0)
			if !slices.Equal(images, wantImages) {
				t.Errorf("images = %+v, want %+v", images, wantImages)
			}
			for _, img := range images {
				got, err := os.ReadFile(filepath.Join(out, img.Name))
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(filepath.Join(cleanOut, img.Name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s differs from the clean GIF's frame", img.Name)
				}
			}
			for _, call := range rec.calls {
				if !slices.ContainsFunc(call, func(a string) bool {
					return strings.HasPrefix(a, "gif:"+path) || strings.HasPrefix(a, out)
				}) {
					t.Errorf("run %q neither pins the gif decoder nor reads an output", call)
				}
			}
		})
	}
}

func TestInputSizeCap(t *testing.T) {
	p := requireProcessor(t)
	rec := record(p)
	header := gifBytes(1, 1)
	over := writeFile(t, "big.gif", header)
	if err := os.Truncate(over, maxInputBytes+1); err != nil {
		t.Fatal(err)
	}
	for name, op := range animationOps(p) {
		if _, err := op(over, t.TempDir()); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: one byte over the cap: err = %v, want ErrTooLarge", name, err)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("%d tool runs for an oversize file", n)
	}

	exact := writeFile(t, "exact.gif", header)
	if err := os.Truncate(exact, maxInputBytes); err != nil {
		t.Fatal(err)
	}
	for name, op := range animationOps(p) {
		if _, err := op(exact, t.TempDir()); errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: exactly the cap was refused as too large: %v", name, err)
		}
	}
}

func TestHeaderCapRefusesBeforeDecoding(t *testing.T) {
	p := requireProcessor(t)
	for name, g := range map[string][]byte{
		"too wide":        gifBytes(maxDimension+1, 10),
		"too tall":        gifBytes(10, maxDimension+1),
		"too many pixels": gifBytes(maxDimension, maxDimension),
	} {
		for opName, op := range animationOps(p) {
			t.Run(name+"/"+opName, func(t *testing.T) {
				rec := record(p)
				defer func() { p.run = rec.inner }()
				out := t.TempDir()
				_, err := op(writeFile(t, "big.gif", g), out)
				if !errors.Is(err, ErrTooLarge) {
					t.Fatalf("err = %v, want ErrTooLarge", err)
				}
				if rec.count() != 1 || !slices.Contains(rec.calls[0], "-ping") {
					t.Errorf("runs before refusal: %q, want the one header read", rec.calls)
				}
				if got := dirNames(t, out); len(got) != 0 {
					t.Errorf("output dir holds %v", got)
				}
			})
		}
	}
}

func TestCheckSceneHeaders(t *testing.T) {
	frames := func(n, w, h int) []sceneHeader {
		s := make([]sceneHeader, n)
		for i := range s {
			s[i] = sceneHeader{width: w, height: h, canvasW: w, canvasH: h}
		}
		return s
	}
	cases := []struct {
		name     string
		scenes   []sceneHeader
		animated bool
		tooLarge bool
	}{
		{"small", frames(3, 64, 48), true, false},
		{"frame count at cap", frames(maxAnimationFrames, 8, 8), true, false},
		{"frame count over cap", frames(maxAnimationFrames+1, 8, 8), true, true},
		{"2000 frames", frames(2000, 8, 8), true, false},
		{"2001 frames", frames(2001, 8, 8), true, true},
		{"frame count unchecked for a still", frames(maxAnimationFrames+1, 8, 8), false, false},
		{"dimension at cap", frames(1, maxDimension, 100), false, false},
		{"width over cap", frames(1, maxDimension+1, 1), false, true},
		{"height over cap", frames(1, 1, maxDimension+1), false, true},
		{"canvas width over cap", []sceneHeader{{width: 10, height: 10, canvasW: maxDimension + 1, canvasH: 10}}, false, true},
		{"frame pixels over cap", frames(1, 5311, 9791), false, true},
		{"frame pixels at cap", frames(1, 8000, 6500), false, false},
		{"decoded pixels at cap", frames(900, 1000, 1000), true, false},
		{"decoded pixels one row over", frames(900, 1000, 1001), true, true},
		{"decoded pixels over cap", frames(901, 1000, 1000), true, true},
		{"decoded pixels at cap, large canvas", frames(225, 2000, 2000), true, false},
		{"decoded pixels one row over, large canvas", frames(225, 2000, 2001), true, true},
		{"frame cap at decoded cap", frames(maxAnimationFrames, 670, 670), true, false},
		{"frame cap over decoded cap", frames(maxAnimationFrames, 671, 671), true, true},
		{"two frames at the frame pixel cap", frames(2, 8000, 6500), true, false},
		{"two frames a row over the frame pixel cap", frames(2, 8000, 6501), true, true},
		{"decoded pixels use largest canvas", append(frames(1000, 10, 10), sceneHeader{width: 1000, height: 1000, canvasW: 1000, canvasH: 1000}), true, true},
	}
	for _, c := range cases {
		err := checkSceneHeaders(c.scenes, c.animated)
		if got := errors.Is(err, ErrTooLarge); got != c.tooLarge || (err != nil && !got) {
			t.Errorf("%s: err = %v, want ErrTooLarge = %v", c.name, err, c.tooLarge)
		}
	}
	if err := checkVideoSize(maxDimension, maxDimension); err != nil {
		t.Errorf("video at cap: %v", err)
	}
	if err := checkVideoSize(maxDimension+1, 1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("wide video: err = %v", err)
	}
	if err := checkVideoSize(1, maxDimension+1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("tall video: err = %v", err)
	}
}

func TestCheckReturnedPixels(t *testing.T) {
	cases := []struct {
		name         string
		count, w, h  int
		refused      bool
		wantFitCount string
	}{
		{"at cap", 16, 2000, 2000, false, ""},
		{"one row over", 16, 2000, 2001, true, "at most 15 frames fit"},
		{"fewer frames", 15, 2000, 2001, false, ""},
		{"one frame of the largest frame", 1, 8000, 6500, false, ""},
		{"64 frames of a small canvas", 64, 1000, 1000, false, ""},
		{"64 frames one row over", 64, 1000, 1001, true, "at most 63 frames fit"},
	}
	for _, c := range cases {
		err := checkReturnedPixels(c.count, c.w, c.h)
		if got := errors.Is(err, ErrTooLarge); got != c.refused || (err != nil && !got) {
			t.Errorf("%s: err = %v, want refused = %v", c.name, err, c.refused)
		}
		if c.refused && !strings.Contains(err.Error(), c.wantFitCount) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.wantFitCount)
		}
	}
}

func TestReadHeadersReportsTheProfileOfAStillWebP(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	path := filepath.Join(t.TempDir(), "icc.webp")
	magick(t, env, "-size", "100x80", "gradient:red-blue", "+profile", "*", "-profile", writeSRGBProfile(t), path)
	var m meter
	scenes, err := readHeaders(context.Background(), p.env(&m), decoder{"magick", "webp"}, path, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenes) != 1 || !scenes[0].hasICC {
		t.Errorf("scenes = %+v, want one with an ICC profile", scenes)
	}
}

func TestUsageSumsRunsAndKeepsLargestPeak(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	var runs []Usage
	inner := p.run
	p.run = func(ctx context.Context, name string, args ...string) (Result, error) {
		res, err := inner(ctx, name, args...)
		if res.Usage != nil {
			runs = append(runs, *res.Usage)
		}
		return res, err
	}
	webp := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "x.webp")
		magick(t, env, "-size", "64x48", "-delay", "5", "xc:red", "-delay", "5", "xc:blue", "-delay", "5", "xc:green", "-loop", "0", path)
		return stage(t, path)
	}
	// The compositor's run is not among the Runner's, and counts on top.
	check := func(t *testing.T, got Usage, compositor bool) {
		t.Helper()
		var cpu time.Duration
		var peak int64
		for _, u := range runs {
			cpu += u.CPU
			peak = max(peak, u.MaxRSS)
		}
		if len(runs) < 2 {
			t.Fatalf("%d measured runs", len(runs))
		}
		if compositor {
			if got.CPU <= cpu || got.MaxRSS < peak {
				t.Errorf("usage = %+v, want more CPU than %v and a peak of at least %d", got, cpu, peak)
			}
		} else if got.CPU != cpu || got.MaxRSS != peak {
			t.Errorf("usage = %+v, want CPU %v and peak %d", got, cpu, peak)
		}
		if got.CPU <= 0 || got.MaxRSS <= 0 {
			t.Errorf("usage = %+v, want non-zero", got)
		}
	}
	for _, c := range []struct {
		name       string
		input      func(t *testing.T) string
		compositor bool
	}{
		{"webp", webp, true},
		{"gif", func(t *testing.T) string { return writeGIF(t, env, solid, 4) }, true},
	} {
		t.Run(c.name+" success", func(t *testing.T) {
			runs = nil
			_, usage, err := p.AnimationFrames(context.Background(), c.input(t), Encoding{}, 0, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			check(t, usage, c.compositor)
		})
		t.Run(c.name+" failure", func(t *testing.T) {
			runs = nil
			rec := record(p)
			defer func() { p.run = rec.inner }()
			rec.failAfter = 2
			_, usage, err := p.AnimationFrames(context.Background(), c.input(t), Encoding{}, 0, t.TempDir())
			if err == nil {
				t.Fatal("operation succeeded")
			}
			check(t, usage, c.compositor)
		})
	}
}

// manyFrameGIF returns a GIF with a w x h logical screen and n frames of 1x1.
func manyFrameGIF(w, h, n int) []byte {
	b := []byte("GIF89a")
	b = binary.LittleEndian.AppendUint16(b, uint16(w))
	b = binary.LittleEndian.AppendUint16(b, uint16(h))
	b = append(b, 0x80, 0, 0, 0, 0, 0, 255, 255, 255)
	frame := []byte{0x2C, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0x02, 0x02, 0x44, 0x01, 0x00}
	for range n {
		b = append(b, frame...)
	}
	return append(b, 0x3B)
}

func TestAnimationCapsRefuseAfterOneHeaderRead(t *testing.T) {
	p := requireProcessor(t)
	cases := map[string][]byte{
		"too many frames":       manyFrameGIF(1, 1, maxAnimationFrames+1),
		"too many total pixels": manyFrameGIF(1000, 1000, 901),
		"one row over the cap":  manyFrameGIF(2000, 2001, 225),
	}
	for name, g := range cases {
		for opName, op := range animationOps(p) {
			t.Run(name+"/"+opName, func(t *testing.T) {
				rec := record(p)
				defer func() { p.run = rec.inner }()
				forbidCompositor(t, p)
				_, err := op(writeFile(t, "many.gif", g), t.TempDir())
				if !errors.Is(err, ErrTooLarge) {
					t.Fatalf("err = %v, want ErrTooLarge", err)
				}
				if rec.count() != 1 || !slices.Contains(rec.calls[0], "-ping") {
					t.Errorf("runs before refusal: %q, want the one header read", rec.calls)
				}
			})
		}
	}
}

func TestVideoSizeCapRefusesBeforeSampling(t *testing.T) {
	var names []string
	env := Env{Run: func(_ context.Context, name string, _ ...string) (Result, error) {
		names = append(names, name)
		return Result{Stdout: []byte(`{"streams":[{"index":0,"codec_type":"video","width":16385,"height":100,"duration":"1.0","nb_frames":"10"}]}`)}, nil
	}}
	enc, err := Encoding{}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.videoFrames(context.Background(), decoder{"ffmpeg", "mov"}, "in", enc, 4, t.TempDir())
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if !slices.Equal(names, []string{"ffprobe"}) {
		t.Errorf("tool runs %v, want only ffprobe", names)
	}
}
