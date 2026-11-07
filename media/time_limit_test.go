package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// timeLimitArgs returns args with the time limit of LimitArgs replaced by
// seconds.
func timeLimitArgs(t *testing.T, args []string, seconds string) []string {
	t.Helper()
	args = slices.Clone(args)
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "-limit" && args[i+1] == "time" {
			args[i+2] = seconds
			return args
		}
	}
	t.Fatal("no time limit in the arguments")
	return nil
}

// slowEncode returns the arguments of an encode of the 4000x4000 PAM at src to
// a 100 pixel JPEG at out whose blur takes several seconds, so that a time
// limit of one second stops it.
func slowEncode(src, out string) []string {
	frame := encodeFrame{input: "pam:" + src, prelude: []string{"-blur", "0x10"}}
	enc := encoding{format: JPEG, ext: ".jpg", quality: 80, maxEdge: 100, maxArea: 100 * 100}
	return encodeArgs([]encodeFrame{frame}, encodeSource{}, enc, encodeOutput{kind: outputStill, path: out})
}

func slowEncodeCheck(out string) encodeCheck {
	return encodeCheck{
		bounds:  []encodeBound{{maxEdge: 100, maxArea: 100 * 100}},
		outputs: []string{out},
	}
}

func writeBigPNG(t *testing.T, env Env) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.pam")
	magick(t, env, "-size", "4000x4000", "gradient:red-blue", path)
	return path
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s was left behind (stat error %v)", path, err)
	}
}

// ImageMagick exits 0 on a time limit and leaves the frame it was about to
// resize in the output; the encode must refuse that run through the message
// alone, with a frame inside the bounds.
func TestEncodeRefusesATimeLimitReportedOnStderr(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.jpg")
	if err := os.WriteFile(out, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := Env{Run: func(context.Context, string, ...string) (Result, error) {
		return Result{
			Stdout: []byte("sig|100|75\n"),
			Stderr: []byte("magick: time limit exceeded `x.png' @ error/cache.c/GetImagePixelCache/1743.\n"),
		}, nil
	}}
	_, err := env.runEncode(context.Background(), []string{"-version"}, slowEncodeCheck(out))
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
	assertGone(t, out)
}

// A frame above the bounds, or not of the exact size asked for, is refused
// when the run exits 0 and says nothing on stderr.
func TestEncodeRefusesAFrameOutsideItsBound(t *testing.T) {
	bound := encodeBound{maxEdge: 100, maxArea: 5000}
	exact := encodeBound{maxEdge: 100, maxArea: 5000, width: 50, height: 40}
	for _, c := range []struct {
		name   string
		bounds []encodeBound
		report string
		ok     bool
	}{
		{"inside", []encodeBound{bound}, "s|100|50\n", true},
		{"edge over", []encodeBound{bound}, "s|101|10\n", false},
		{"area over", []encodeBound{bound}, "s|90|60\n", false},
		{"the source size", []encodeBound{bound}, "s|10328|7745\n", false},
		{"exact", []encodeBound{exact}, "s|50|40\n", true},
		{"not exact", []encodeBound{exact}, "s|49|40\n", false},
		{"second frame over", []encodeBound{bound}, "s|10|10\ns|10328|7745\n", false},
		{"per frame bound", []encodeBound{bound, exact}, "s|10|10\ns|51|40\n", false},
		{"bounds and frames differ", []encodeBound{bound, bound}, "s|10|10\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out.webp")
			if err := os.WriteFile(out, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			env := Env{Run: func(context.Context, string, ...string) (Result, error) {
				return Result{Stdout: []byte(c.report)}, nil
			}}
			_, err := env.runEncode(context.Background(), nil, encodeCheck{bounds: c.bounds, outputs: []string{out}})
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want success %v", err, c.ok)
			}
			if !c.ok {
				assertGone(t, out)
			}
		})
	}
}

// With ImageMagick's time limit reached by the -limit flags of LimitArgs, the
// run exits 0 with the frame unresized; runEncode turns it into ErrTimeLimit
// and removes the output.
func TestEncodeTimeLimitThroughTheLimitFlags(t *testing.T) {
	env := testEnv(t)
	src := writeBigPNG(t, env)
	out := filepath.Join(t.TempDir(), "out.jpg")
	args := timeLimitArgs(t, slowEncode(src, out), "1")

	raw, err := env.Run(context.Background(), "magick", args...)
	if err != nil || !strings.Contains(string(raw.Stderr), "time limit exceeded") || !strings.Contains(string(raw.Stdout), "|4000|4000") {
		t.Fatalf("premise: err = %v, stderr %q, report %q; want exit 0, a time limit and the unresized frame",
			err, raw.Stderr, raw.Stdout)
	}

	_, err = env.runEncode(context.Background(), args, slowEncodeCheck(out))
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
	assertGone(t, out)
}

// The same through the Processor, whose policy sets the ceiling.
func TestEncodeTimeLimitThroughTheProcessorPolicy(t *testing.T) {
	p := requireProcessor(t)
	src := writeBigPNG(t, requireMagick(t))
	out := filepath.Join(t.TempDir(), "out.jpg")
	rules := processorPolicy()
	for i, r := range rules {
		if r.domain == "resource" && r.name == "time" {
			rules[i].value = "1"
		}
	}
	if err := os.WriteFile(filepath.Join(p.dir, "policy.xml"), policyXML(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	env := p.env(&meter{})

	_, err := env.runEncode(context.Background(), slowEncode(src, out), slowEncodeCheck(out))
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
	assertGone(t, out)
}

// skipResize reports every frame of the encodes that write output when
// writes is set, and of the probes that do not otherwise, at size w x h, as
// an encode whose resize was skipped would.
func skipResize(real Env, w, h int, writes bool) Env {
	inner := real.Run
	real.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		res, err := inner(ctx, name, args...)
		if err != nil || !slices.Contains(args, "-resize") || slices.Contains(args, "-write") != writes {
			return res, err
		}
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
			sig, _, _ := strings.Cut(line, "|")
			lines = append(lines, fmt.Sprintf("%s|%d|%d", sig, w, h))
		}
		res.Stdout = []byte(strings.Join(lines, "\n") + "\n")
		return res, nil
	}
	return real
}

// Every route that encodes checks the size the run reports and leaves no
// output behind when it refuses.
func TestEncodeRoutesRefuseAnUnresizedFrame(t *testing.T) {
	real := requireMagick(t)
	dir := t.TempDir()
	jpg := filepath.Join(dir, "x.jpg")
	magick(t, real, "-size", "600x400", "gradient:red-blue", jpg)
	gif := writeGrayAnimGIF(t, real, []int{0, 40, 80, 120}, repeatInt(5, 4))
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100}

	t.Run("one still target", func(t *testing.T) {
		staging := t.TempDir()
		env := skipResize(stagedEnv(real, staging), 600, 400, true)
		src := openSource(t, env, jpg, Hints{})
		_, err := src.Encode(context.Background(), 0, enc)
		requireBoundsError(t, err)
		assertEmptyDir(t, staging)
	})
	t.Run("two still targets", func(t *testing.T) {
		staging := t.TempDir()
		env := skipResize(stagedEnv(real, staging), 600, 400, true)
		src := openSource(t, env, jpg, Hints{})
		wider := enc
		wider.MaxEdge, wider.MaxArea = 200, 200*200
		_, err := src.Encode(context.Background(), 0, enc, wider)
		requireBoundsError(t, err)
		assertEmptyDir(t, staging)
	})
	t.Run("still frame files", func(t *testing.T) {
		out := t.TempDir()
		env := skipResize(stagedEnv(real, t.TempDir()), 600, 400, true)
		src := openSource(t, env, jpg, Hints{})
		_, err := src.EncodeFrames(context.Background(), enc, out)
		requireBoundsError(t, err)
		assertEmptyDir(t, out)
	})
	t.Run("animation", func(t *testing.T) {
		staging := t.TempDir()
		env := skipResize(stagedEnv(real, staging), 600, 400, true)
		src := openSource(t, env, gif, Hints{Frames: 4})
		_, err := src.Encode(context.Background(), 4, enc)
		requireBoundsError(t, err)
		assertEmptyDir(t, staging)
	})
	t.Run("animation probe", func(t *testing.T) {
		staging := t.TempDir()
		env := skipResize(stagedEnv(real, staging), 600, 400, false)
		src := openSource(t, env, gif, Hints{Frames: 4})
		_, err := src.Encode(context.Background(), 4, enc)
		requireBoundsError(t, err)
		assertEmptyDir(t, staging)
	})
	t.Run("animation frame files", func(t *testing.T) {
		out := t.TempDir()
		env := skipResize(stagedEnv(real, t.TempDir()), 600, 400, true)
		src := openSource(t, env, gif, Hints{Frames: 4})
		_, err := src.EncodeFrames(context.Background(), enc, out)
		requireBoundsError(t, err)
		assertEmptyDir(t, out)
	})
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("%s holds %s", dir, e.Name())
	}
}

// requireBoundsError fails unless err is the refusal of a frame above its
// bounds or not of its exact size, so that a route failing for another reason
// does not pass.
func requireBoundsError(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "above the bounds") && !strings.Contains(err.Error(), "want exactly") {
		t.Fatalf("err = %v, want a frame outside its bounds", err)
	}
}

const timeLimitStderr = "magick: time limit exceeded `x.tif' @ error/cache.c/GetImagePixelCache/1743.\n"

// A run that exits non-zero with the message is a time limit, and the message
// appears once in the error.
func TestEncodeTimeLimitWithAFailingExit(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.jpg")
	if err := os.WriteFile(out, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := Env{Run: func(context.Context, string, ...string) (Result, error) {
		return Result{Stderr: []byte(timeLimitStderr)},
			fmt.Errorf("magick: exit status 1: %s", strings.TrimSpace(timeLimitStderr))
	}}
	_, err := env.runEncode(context.Background(), nil, encodeCheck{outputs: []string{out}})
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
	if n := strings.Count(err.Error(), "time limit exceeded `x.tif'"); n != 1 {
		t.Errorf("the message appears %d times in %q", n, err)
	}
	assertGone(t, out)
}

// Header reads, including the full reads of fullSoft, report a time limit
// whatever the exit status.
func TestHeaderReadsReportATimeLimit(t *testing.T) {
	for name, res := range map[string]struct {
		res Result
		err error
	}{
		"exit 1": {Result{Stderr: []byte(timeLimitStderr)}, errors.New("magick: exit status 1")},
		"exit 0": {Result{Stdout: []byte("0|||1|1|1|1|TIFF||DirectClass|False|END\n"), Stderr: []byte(timeLimitStderr)}, nil},
	} {
		env := Env{Run: func(context.Context, string, ...string) (Result, error) { return res.res, res.err }}
		if _, err := identifyHeaders(context.Background(), env, "x.tif", false); !errors.Is(err, ErrTimeLimit) {
			t.Errorf("%s: identifyHeaders err = %v, want ErrTimeLimit", name, err)
		}
		if _, err := fullSoft(context.Background(), env, "x.tif"); !errors.Is(err, ErrTimeLimit) {
			t.Errorf("%s: fullSoft err = %v, want ErrTimeLimit", name, err)
		}
	}
}

// A frame count other than the expected one removes the outputs, on the
// bound-less checks too.
func TestEncodeRefusesAFrameCountMismatch(t *testing.T) {
	for name, report := range map[string]string{"fewer": "s|10|10\n", "more": "s|10|10\ns|10|10\ns|10|10\n"} {
		out := filepath.Join(t.TempDir(), "out.webp")
		if err := os.WriteFile(out, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := Env{Run: func(context.Context, string, ...string) (Result, error) {
			return Result{Stdout: []byte(report)}, nil
		}}
		_, err := env.runEncode(context.Background(), nil, encodeCheck{frames: 2, outputs: []string{out}})
		if err == nil {
			t.Errorf("%s: a report of the wrong frame count was accepted", name)
		}
		assertGone(t, out)
	}
}

// On the reduced-JPEG route the size is exact: a smaller frame inside the
// bounds is refused.
func TestEncodeRefusesAWrongExactSize(t *testing.T) {
	real := requireMagick(t)
	jpg := filepath.Join(t.TempDir(), "x.jpg")
	magick(t, real, "-size", "600x400", "gradient:red-blue", jpg)
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100}
	wider := enc
	wider.MaxEdge, wider.MaxArea = 200, 200*200
	for name, targets := range map[string][]Encoding{"one target": {enc}, "two targets": {enc, wider}} {
		t.Run(name, func(t *testing.T) {
			staging := t.TempDir()
			env := skipResize(stagedEnv(real, staging), 90, 60, true)
			src := openSource(t, env, jpg, Hints{})
			readOps, sizes := src.reduction(mustResolveAll(t, targets))
			if readOps == nil || sizes == nil {
				t.Fatal("the fixture does not take the reduced route")
			}
			_, err := src.Encode(context.Background(), 0, targets...)
			if err == nil || !strings.Contains(err.Error(), "want exactly") {
				t.Fatalf("err = %v, want an exact size refusal", err)
			}
			assertEmptyDir(t, staging)
		})
	}
}

func mustResolveAll(t *testing.T, targets []Encoding) []encoding {
	t.Helper()
	encs := make([]encoding, len(targets))
	for i, e := range targets {
		encs[i] = mustResolve(t, e)
	}
	return encs
}

// dropLastFrame removes the last line of the report of every encode that
// writes output.
func dropLastFrame(real Env) Env { return editReport(real, -1, true) }

// repeatLastFrame repeats the last line of the report of every encode that
// writes output.
func repeatLastFrame(real Env) Env { return editReport(real, 1, true) }

// editReport adds a line (delta 1) or removes one (delta -1) in the report of
// the encodes that write output when writes is set, and of the probes
// otherwise.
func editReport(real Env, delta int, writes bool) Env {
	inner := real.Run
	real.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		res, err := inner(ctx, name, args...)
		if err != nil || name != "magick" || slices.Contains(args, "-write") != writes {
			return res, err
		}
		lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
		if delta < 0 {
			lines = lines[:len(lines)-1]
		} else {
			lines = append(lines, lines[len(lines)-1])
		}
		res.Stdout = []byte(strings.Join(lines, "\n") + "\n")
		return res, nil
	}
	return real
}

// Every route holds the report to the number of frames it asked for and
// leaves no output when the number differs.
func TestEncodeRoutesRefuseAShortReport(t *testing.T) {
	real := requireMagick(t)
	jpg := filepath.Join(t.TempDir(), "x.jpg")
	magick(t, real, "-size", "600x400", "gradient:red-blue", jpg)
	gif := writeGrayAnimGIF(t, real, []int{0, 40, 80, 120}, repeatInt(5, 4))
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100}
	wider := enc
	wider.MaxEdge, wider.MaxArea = 200, 200*200

	t.Run("two still targets", func(t *testing.T) {
		staging := t.TempDir()
		src := openSource(t, dropLastFrame(stagedEnv(real, staging)), jpg, Hints{})
		if _, err := src.Encode(context.Background(), 0, enc, wider); err == nil || !strings.Contains(err.Error(), "reported 1 frames, want 2") {
			t.Fatalf("err = %v, want a frame count refusal", err)
		}
		assertEmptyDir(t, staging)
	})
	t.Run("one still target", func(t *testing.T) {
		staging := t.TempDir()
		src := openSource(t, repeatLastFrame(stagedEnv(real, staging)), jpg, Hints{})
		if _, err := src.Encode(context.Background(), 0, enc); err == nil || !strings.Contains(err.Error(), "reported 2 frames, want 1") {
			t.Fatalf("err = %v, want a frame count refusal", err)
		}
		assertEmptyDir(t, staging)
	})
	t.Run("animation", func(t *testing.T) {
		staging := t.TempDir()
		src := openSource(t, dropLastFrame(stagedEnv(real, staging)), gif, Hints{Frames: 4})
		if _, err := src.Encode(context.Background(), 4, enc); err == nil || !strings.Contains(err.Error(), "frames, want") {
			t.Fatalf("err = %v, want a frame count refusal", err)
		}
		assertEmptyDir(t, staging)
	})
	t.Run("animation frame files", func(t *testing.T) {
		out := t.TempDir()
		src := openSource(t, dropLastFrame(stagedEnv(real, t.TempDir())), gif, Hints{Frames: 4})
		if _, err := src.EncodeFrames(context.Background(), enc, out); err == nil || !strings.Contains(err.Error(), "frames, want") {
			t.Fatalf("err = %v, want a frame count refusal", err)
		}
		assertEmptyDir(t, out)
	})
}

// The video thumbnails go through the same checks: the probe of the sampled
// frames and the animated encode, with the bound of their encoding and, for a
// frame reduced in decoding, its exact size.
func TestVideoThumbnailsRefuseABadReport(t *testing.T) {
	real := requireFFmpeg(t)
	small := writeClip(t, real, ".mp4", testsrc("160x120", 10, 3)...)
	big := writeClip(t, real, ".mp4", testsrc("3840x2160", 5, 1)...)
	for _, c := range []struct {
		name string
		clip string
		env  func(Env) Env
		want string
	}{
		{"probe over the bounds", small, func(e Env) Env { return skipResize(e, 160, 120, false) }, "above the bounds"},
		{"encode over the bounds", small, func(e Env) Env { return skipResize(e, 160, 120, true) }, "above the bounds"},
		{"probe too short", small, func(e Env) Env { return editReport(e, -1, false) }, "frames, want"},
		{"encode too short", small, func(e Env) Env { return editReport(e, -1, true) }, "frames, want"},
		{"probe of the wrong exact size", big, func(e Env) Env { return skipResize(e, 30, 20, false) }, "want exactly"},
		{"encode of the wrong exact size", big, func(e Env) Env { return skipResize(e, 30, 20, true) }, "want exactly"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := t.TempDir()
			err := c.env(real).GenerateVideoThumbnails(context.Background(), c.clip,
				filepath.Join(out, "small.webp"), filepath.Join(out, "large.webp"), 0, 10, 64, 100, 80)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			assertEmptyDir(t, out)
		})
	}
}
