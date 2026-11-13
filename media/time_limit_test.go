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

// timeLimitArgs returns args preceded by the limit flags of the default Resources with
// the time limit replaced by seconds. The flags follow the ones runMagick
// adds, so they win.
func timeLimitArgs(t *testing.T, args []string, seconds string) []string {
	t.Helper()
	limit := defaultResources().limitArgs()
	for i := 0; i+2 < len(limit); i++ {
		if limit[i] == "-limit" && limit[i+1] == "time" {
			limit[i+2] = seconds
			return append(limit, args...)
		}
	}
	t.Fatal("no time limit in the limit flags")
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

func writeBigPNG(t *testing.T, env toolEnv) string {
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
	t.Parallel()
	out := filepath.Join(t.TempDir(), "out.jpg")
	if err := os.WriteFile(out, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := toolEnv{resources: defaultResources(), caps: measuredCaps, run: func(context.Context, string, ...string) (runResult, error) {
		return runResult{
			Stdout: []byte("100|75\n"),
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
	t.Parallel()
	bound := encodeBound{maxEdge: 100, maxArea: 5000}
	exact := encodeBound{maxEdge: 100, maxArea: 5000, width: 50, height: 40}
	for _, c := range []struct {
		name   string
		bounds []encodeBound
		report string
		ok     bool
	}{
		{"inside", []encodeBound{bound}, "100|50\n", true},
		{"edge over", []encodeBound{bound}, "101|10\n", false},
		{"area over", []encodeBound{bound}, "90|60\n", false},
		{"the source size", []encodeBound{bound}, "10328|7745\n", false},
		{"exact", []encodeBound{exact}, "50|40\n", true},
		{"not exact", []encodeBound{exact}, "49|40\n", false},
		{"second frame over", []encodeBound{bound}, "10|10\n10328|7745\n", false},
		{"per frame bound", []encodeBound{bound, exact}, "10|10\n51|40\n", false},
		{"bounds and frames differ", []encodeBound{bound, bound}, "10|10\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out.webp")
			if err := os.WriteFile(out, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			env := toolEnv{resources: defaultResources(), caps: measuredCaps, run: func(context.Context, string, ...string) (runResult, error) {
				return runResult{Stdout: []byte(c.report)}, nil
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
	t.Parallel()
	env := testEnv(t)
	src := writeBigPNG(t, env)
	out := filepath.Join(t.TempDir(), "out.jpg")
	args := timeLimitArgs(t, slowEncode(src, out), "1")

	raw, err := env.run(context.Background(), "magick", args...)
	if err != nil || !strings.Contains(string(raw.Stderr), "time limit exceeded") || !strings.Contains(string(raw.Stdout), "4000|4000") {
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
	t.Parallel()
	p := requireProcessor(t)
	src := writeBigPNG(t, requireMagick(t))
	out := filepath.Join(t.TempDir(), "out.jpg")
	rules := processorPolicy(defaultResources(), Types())
	for i, r := range rules {
		if r.domain == "resource" && r.name == "time" {
			rules[i].value = "1"
		}
	}
	if err := os.WriteFile(filepath.Join(p.dir, "policy.xml"), policyXML(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	env := p.env(&meter{}, nil)

	_, err := env.runEncode(context.Background(), slowEncode(src, out), slowEncodeCheck(out))
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
	assertGone(t, out)
}

// skipResize wraps run so that every frame of the encodes that write output is
// reported at size w x h, as an encode whose resize was skipped would.
func skipResize(inner toolRun, w, h int) toolRun {
	return func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		res, err := inner(ctx, in, name, args...)
		if err != nil || !slices.Contains(args, "-resize") || !slices.Contains(args, "-write") {
			return res, err
		}
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
			prefix := ""
			if strings.Count(line, "|") == 2 {
				sig, _, _ := strings.Cut(line, "|")
				prefix = sig + "|"
			}
			lines = append(lines, fmt.Sprintf("%s%d|%d", prefix, w, h))
		}
		res.Stdout = []byte(strings.Join(lines, "\n") + "\n")
		return res, nil
	}
}

// Every route that encodes checks the size the run reports and leaves no
// output behind when it refuses.
func TestEncodeRoutesRefuseAnUnresizedFrame(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	dir := t.TempDir()
	jpg := filepath.Join(dir, "x.jpg")
	magick(t, real, "-size", "600x400", "gradient:red-blue", jpg)
	gif := writeGrayAnimGIF(t, real, []int{0, 40, 80, 120}, repeatInt(5, 4))
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100}
	wider := enc
	wider.MaxEdge, wider.MaxArea = 200, 200*200
	animated := enc
	animated.Animate = true

	for _, c := range []struct {
		name    string
		path    string
		sel     Select
		targets []Encoding
	}{
		{"one still target", jpg, Select{}, []Encoding{enc}},
		{"two still targets", jpg, Select{}, []Encoding{enc, wider}},
		{"animation", gif, Select{Frames: Even(4)}, []Encoding{animated}},
		{"animation frame files", gif, Select{Frames: Even(4)}, []Encoding{enc}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			p.run = skipResize(p.run, 600, 400)
			src := openSourceOn(t, p, c.path, OpenOptions{})
			out := t.TempDir()
			_, _, err := src.Encode(context.Background(), out, c.sel, c.targets...)
			requireBoundsError(t, err)
			assertEmptyDir(t, out)
		})
	}
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
	t.Parallel()
	out := filepath.Join(t.TempDir(), "out.jpg")
	if err := os.WriteFile(out, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := toolEnv{resources: defaultResources(), caps: measuredCaps, run: func(context.Context, string, ...string) (runResult, error) {
		return runResult{Stderr: []byte(timeLimitStderr)},
			&ToolError{Tool: "magick", ExitCode: 1, Stderr: []byte(strings.TrimSpace(timeLimitStderr))}
	}}
	_, err := env.runEncode(context.Background(), nil, encodeCheck{outputs: []string{out}})
	if !errors.Is(err, ErrTimeLimit) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
	if strings.Contains(err.Error(), "x.tif") {
		t.Errorf("the message quotes stderr: %q", err)
	}
	assertGone(t, out)
}

// Header reads, including the full reads of fullSoft, report a time limit
// whatever the exit status.
func TestHeaderReadsReportATimeLimit(t *testing.T) {
	t.Parallel()
	for name, res := range map[string]struct {
		res runResult
		err error
	}{
		"exit 1": {runResult{Stderr: []byte(timeLimitStderr)}, errors.New("magick: exit status 1")},
		"exit 0": {runResult{Stdout: []byte("0|||1|1|1|1|TIFF||DirectClass|False|END\n"), Stderr: []byte(timeLimitStderr)}, nil},
	} {
		env := toolEnv{resources: defaultResources(), caps: measuredCaps, run: func(context.Context, string, ...string) (runResult, error) { return res.res, res.err }}
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
	t.Parallel()
	for name, report := range map[string]string{"fewer": "10|10\n", "more": "10|10\n10|10\n10|10\n"} {
		out := filepath.Join(t.TempDir(), "out.webp")
		if err := os.WriteFile(out, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := toolEnv{resources: defaultResources(), caps: measuredCaps, run: func(context.Context, string, ...string) (runResult, error) {
			return runResult{Stdout: []byte(report)}, nil
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
	t.Parallel()
	real := requireMagick(t)
	jpg := filepath.Join(t.TempDir(), "x.jpg")
	magick(t, real, "-size", "600x400", "gradient:red-blue", jpg)
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100}
	wider := enc
	wider.MaxEdge, wider.MaxArea = 200, 200*200
	for name, targets := range map[string][]Encoding{"one target": {enc}, "two targets": {enc, wider}} {
		t.Run(name, func(t *testing.T) {
			p := requireProcessor(t)
			p.run = skipResize(p.run, 90, 60)
			src := openSourceOn(t, p, jpg, OpenOptions{})
			readOps, sizes := src.stills[0].reduction(mustResolveAll(t, targets))
			if readOps == nil || sizes == nil {
				t.Fatal("the fixture does not take the reduced route")
			}
			out := t.TempDir()
			_, _, err := src.Encode(context.Background(), out, Select{}, targets...)
			if err == nil || !strings.Contains(err.Error(), "want exactly") {
				t.Fatalf("err = %v, want an exact size refusal", err)
			}
			assertEmptyDir(t, out)
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

// dropLastFrame wraps run so that the last line of the report of every encode
// that writes output is removed.
func dropLastFrame(inner toolRun) toolRun { return editReport(inner, -1) }

// repeatLastFrame wraps run so that the last line of the report of every
// encode that writes output is repeated.
func repeatLastFrame(inner toolRun) toolRun { return editReport(inner, 1) }

// editReport wraps run so that a line is added (delta 1) or removed (delta -1)
// in the report of the encodes that write output.
func editReport(inner toolRun, delta int) toolRun {
	return func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		res, err := inner(ctx, in, name, args...)
		if err != nil || name != "magick" || !slices.Contains(args, "-write") {
			return res, err
		}
		var uprights, reports []string
		for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
			if strings.HasPrefix(line, uprightPrefix) {
				uprights = append(uprights, line)
			} else {
				reports = append(reports, line)
			}
		}
		// The size of each frame is reported with its upright size, if any.
		for _, lines := range []*[]string{&uprights, &reports} {
			switch {
			case len(*lines) == 0:
			case delta < 0:
				*lines = (*lines)[:len(*lines)-1]
			default:
				*lines = append(*lines, (*lines)[len(*lines)-1])
			}
		}
		res.Stdout = []byte(strings.Join(append(uprights, reports...), "\n") + "\n")
		return res, nil
	}
}

// Every route holds the report to the number of frames it asked for and
// leaves no output when the number differs.
func TestEncodeRoutesRefuseAShortReport(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	jpg := filepath.Join(t.TempDir(), "x.jpg")
	magick(t, real, "-size", "600x400", "gradient:red-blue", jpg)
	gif := writeGrayAnimGIF(t, real, []int{0, 40, 80, 120}, repeatInt(5, 4))
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 100, MaxArea: 100 * 100}
	wider := enc
	wider.MaxEdge, wider.MaxArea = 200, 200*200
	animated := enc
	animated.Animate = true

	for _, c := range []struct {
		name    string
		wrap    func(toolRun) toolRun
		path    string
		sel     Select
		targets []Encoding
		want    string
	}{
		{"two still targets", dropLastFrame, jpg, Select{}, []Encoding{enc, wider}, "reported 1 frames, want 2"},
		{"one still target", repeatLastFrame, jpg, Select{}, []Encoding{enc}, "reported 2 frames, want 1"},
		{"animation", dropLastFrame, gif, Select{Frames: Even(4)}, []Encoding{animated}, "frames, want"},
		{"animation frame files", dropLastFrame, gif, Select{Frames: Even(4)}, []Encoding{enc}, "frames, want"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			p.run = c.wrap(p.run)
			src := openSourceOn(t, p, c.path, OpenOptions{})
			out := t.TempDir()
			if _, _, err := src.Encode(context.Background(), out, c.sel, c.targets...); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want a frame count refusal saying %q", err, c.want)
			}
			assertEmptyDir(t, out)
		})
	}
}

// The animated WebPs of a video go through the same checks: the animated
// encode, with the bound of their encoding and, for a frame reduced in
// decoding, its exact size.
func TestVideoAnimatedWebPsRefuseABadReport(t *testing.T) {
	t.Parallel()
	real := requireFFmpeg(t)
	small := writeClip(t, real, ".mp4", testsrc("160x120", 10, 3)...)
	big := writeClip(t, real, ".mp4", testsrc("3840x2160", 5, 1)...)
	for _, c := range []struct {
		name string
		clip string
		wrap func(toolRun) toolRun
		want string
	}{
		{"encode over the bounds", small, func(r toolRun) toolRun { return skipResize(r, 160, 120) }, "above the bounds"},
		{"encode too short", small, func(r toolRun) toolRun { return editReport(r, -1) }, "frames, want"},
		{"encode of the wrong exact size", big, func(r toolRun) toolRun { return skipResize(r, 30, 20) }, "want exactly"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessor(t)
			p.run = c.wrap(p.run)
			src := openSourceOn(t, p, c.clip, OpenOptions{})
			out := t.TempDir()
			animated := func(edge int) Encoding {
				enc := webpAtEdge(edge)
				enc.Animate = true
				return enc
			}
			_, _, err := src.Encode(context.Background(), out, Select{Frames: Even(3)}, animated(64), animated(100))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			assertEmptyDir(t, out)
		})
	}
}

// A header read whose stderr carries both messages is a time limit, not a
// scene-count refusal.
func TestIdentifyHeadersTimeLimitOutranksListLength(t *testing.T) {
	t.Parallel()
	env := toolEnv{resources: defaultResources(), caps: measuredCaps, run: func(context.Context, string, ...string) (runResult, error) {
		return runResult{Stderr: []byte(timeLimitStderr)}, &ToolError{Tool: "magick", ExitCode: 1, Stderr: []byte(listLengthRefusal)}
	}}
	_, err := identifyHeaders(context.Background(), env, "x.tif", true)
	if !errors.Is(err, ErrTimeLimit) || errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTimeLimit", err)
	}
}
