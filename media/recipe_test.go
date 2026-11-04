package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/davars/winnow-media/tool"
)

var update = flag.Bool("update", false, "rewrite the golden argv files")

// recorder wraps an Env's Runner, recording every invocation and optionally
// failing one after it has run for real.
type recorder struct {
	inner Runner
	mu    sync.Mutex
	calls [][]string
	// failAfter fails the n-th call (1-based) after running it; 0 disables.
	failAfter int
	// dropUsage clears the Usage of every result.
	dropUsage bool
	// dropUsageOn clears the Usage of the n-th call (1-based); 0 disables.
	dropUsageOn int
}

func (r *recorder) run(ctx context.Context, name string, args ...string) (Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	n := len(r.calls)
	r.mu.Unlock()
	res, err := r.inner(ctx, name, args...)
	if r.dropUsage || n == r.dropUsageOn {
		res.Usage = nil
	}
	if err == nil && n == r.failAfter {
		err = errors.New("injected failure")
	}
	return res, err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func newRecorder(env Env) (*recorder, Env) {
	r := &recorder{inner: env.Run}
	return r, Env{Run: r.run, Stage: env.Stage}
}

func newExecutor(t *testing.T, env Env, names ...string) *Executor {
	t.Helper()
	x, err := NewExecutor(env, names...)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func requireExiftool(t *testing.T) {
	t.Helper()
	if _, err := tool.Resolve("exiftool"); err != nil {
		t.Skip("exiftool not available")
	}
}

// stage moves path to a file named "input" in a fresh directory, so the
// recipe cannot rely on an extension.
func stage(t *testing.T, path string) string {
	t.Helper()
	dir := t.TempDir()
	staged := filepath.Join(dir, "input")
	if err := os.Rename(path, staged); err != nil {
		t.Fatal(err)
	}
	return staged
}

// writeHEIC returns a staged copy of a 640x480 gradient tagged RightTop,
// which reads back as 480x640.
func writeHEIC(t *testing.T) string {
	t.Helper()
	return heicFixture(t, "gradient-righttop")
}

func exiftool(t *testing.T, env Env, args ...string) string {
	t.Helper()
	res, err := env.Run(context.Background(), "exiftool", args...)
	if err != nil {
		t.Fatalf("exiftool %q: %v", args, err)
	}
	return string(res.Stdout)
}

// addMetadata writes EXIF, GPS and XMP fields into path.
func addMetadata(t *testing.T, env Env, path string) {
	t.Helper()
	exiftool(t, env, "-overwrite_original", "-Artist=someone", "-GPSLatitude=1", "-GPSLatitudeRef=N",
		"-XMP-dc:Creator=bar", "-f", "-m", "-ext", "*", path)
}

func metadataGroups(t *testing.T, env Env, path string) string {
	t.Helper()
	return exiftool(t, env, "-G1", "-a", "-s", "-EXIF:all", "-GPS:all", "-XMP:all", "-IPTC:all", "-ICC_Profile:all", path)
}

func solid(i int) string {
	return fmt.Sprintf("xc:rgb(%d,%d,%d)", i*13%256, i*71%256, i*37%256)
}

// writeGIF writes frames solid 64x48 frames of 50 ms each, staged without an
// extension.
func writeGIF(t *testing.T, env Env, colour func(i int) string, frames int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.gif")
	args := []string{"-size", "64x48"}
	for i := range frames {
		args = append(args, "-delay", "5", colour(i))
	}
	args = append(args, "-loop", "0", path)
	magick(t, env, args...)
	return stage(t, path)
}

func execute(t *testing.T, x *Executor, recipe, inputType, input string, params map[string]string) (Response, string) {
	t.Helper()
	out := t.TempDir()
	resp, err := x.Execute(context.Background(), Request{
		Recipe: recipe, Params: params, InputPath: input, InputType: inputType, OutputDir: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp, out
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestNewExecutorRefusals(t *testing.T) {
	env := Env{Run: func(context.Context, string, ...string) (Result, error) { return Result{}, nil }}
	if _, err := NewExecutor(env, "nope"); err == nil || !strings.Contains(err.Error(), "unknown recipe") {
		t.Errorf("unknown name: err = %v", err)
	}
	if _, err := NewExecutor(env, "still", "still"); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate name: err = %v", err)
	}
	if _, err := NewExecutor(Env{}, "still"); err == nil {
		t.Error("nil Run accepted")
	}
}

func TestDeclarations(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Recipes() {
		if seen[r.Name] || r.Revision < 1 || len(r.Inputs) == 0 || len(r.Engines) == 0 {
			t.Errorf("recipe %+v is malformed", r)
		}
		seen[r.Name] = true
		if _, err := validateParams(r.Params, nil); err != nil {
			t.Errorf("%s: defaults do not validate: %v", r.Name, err)
		}
	}
	if !seen["still"] || !seen["frames"] {
		t.Errorf("registry holds %v", seen)
	}
	first := Recipes()
	first[0].Inputs[0].MediaType = "mutated"
	first[0].Params[0].Default = "mutated"
	if got := Recipes(); got[0].Inputs[0].MediaType == "mutated" || got[0].Params[0].Default == "mutated" {
		t.Error("Recipes shares state between calls")
	}
}

func TestEmbeddedProfile(t *testing.T) {
	sum := sha256.Sum256(srgbProfile)
	const want = "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("embedded profile sha256 = %s, want %s", got, want)
	}
}

func TestFitSize(t *testing.T) {
	cases := []struct {
		w, h, edge, area int
		tw, th           int
		resize           bool
	}{
		{480, 640, 4000, 100_000_000, 480, 640, false},
		{480, 640, 320, 100_000_000, 240, 320, true},
		{480, 640, 16384, 100000, 273, 365, true},
		{480, 640, 400, 100000, 273, 365, true},
		{480, 640, 640, 307200, 480, 640, false},
		{6000, 4000, 2576, 3750000, 2371, 1581, true},
		{1, 4000, 100, 100000, 1, 100, true},
	}
	for _, c := range cases {
		tw, th, resize := fitSize(c.w, c.h, c.edge, c.area)
		if tw != c.tw || th != c.th || resize != c.resize {
			t.Errorf("fitSize(%d,%d,%d,%d) = %d,%d,%v want %d,%d,%v", c.w, c.h, c.edge, c.area, tw, th, resize, c.tw, c.th, c.resize)
		}
		if tw*th > c.area || max(tw, th) > c.edge {
			t.Errorf("fitSize(%d,%d,%d,%d) = %dx%d exceeds a bound", c.w, c.h, c.edge, c.area, tw, th)
		}
	}
}

func TestExecuteRefusesBeforeRunningTools(t *testing.T) {
	real := requireMagick(t)
	heic := writeHEIC(t)
	gif := writeGIF(t, real, solid, 2)
	rec, env := newRecorder(real)
	x := newExecutor(t, env, "still")

	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  Request
	}{
		{"unknown recipe", Request{Recipe: "nope", InputPath: heic, InputType: "image/heic", OutputDir: t.TempDir()}},
		{"recipe not enabled", Request{Recipe: "frames", InputPath: gif, InputType: "image/gif", OutputDir: t.TempDir()}},
		{"unknown param", Request{Recipe: "still", Params: map[string]string{"bogus": "1"}, InputPath: heic, InputType: "image/heic", OutputDir: t.TempDir()}},
		{"unparsable int", Request{Recipe: "still", Params: map[string]string{"quality": "high"}, InputPath: heic, InputType: "image/heic", OutputDir: t.TempDir()}},
		{"int too large", Request{Recipe: "still", Params: map[string]string{"quality": "101"}, InputPath: heic, InputType: "image/heic", OutputDir: t.TempDir()}},
		{"int too small", Request{Recipe: "still", Params: map[string]string{"max_edge": "0"}, InputPath: heic, InputType: "image/heic", OutputDir: t.TempDir()}},
		{"disallowed enum", Request{Recipe: "still", Params: map[string]string{"format": "gif"}, InputPath: heic, InputType: "image/heic", OutputDir: t.TempDir()}},
		{"input type not accepted", Request{Recipe: "still", InputPath: gif, InputType: "image/gif", OutputDir: t.TempDir()}},
		{"no input type", Request{Recipe: "still", InputPath: heic, OutputDir: t.TempDir()}},
		{"missing input", Request{Recipe: "still", InputPath: filepath.Join(t.TempDir(), "absent"), InputType: "image/heic", OutputDir: t.TempDir()}},
		{"output dir not empty", Request{Recipe: "still", InputPath: heic, InputType: "image/heic", OutputDir: nonEmpty}},
		{"output dir missing", Request{Recipe: "still", InputPath: heic, InputType: "image/heic", OutputDir: filepath.Join(t.TempDir(), "absent")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := x.Execute(context.Background(), c.req); err == nil {
				t.Fatal("Execute succeeded")
			}
			if n := rec.count(); n != 0 {
				t.Errorf("%d tool runs before refusal", n)
			}
		})
	}
	if got := dirNames(t, nonEmpty); !slices.Equal(got, []string{"leftover"}) {
		t.Errorf("refused directory now holds %v", got)
	}
}

func TestStillOutput(t *testing.T) {
	requireExiftool(t)
	env := requireMagick(t)
	src := writeHEIC(t)
	addMetadata(t, env, src)
	if got := metadataGroups(t, env, src); !strings.Contains(got, "Artist") || !strings.Contains(got, "GPSLatitude") || !strings.Contains(got, "Creator") {
		t.Fatalf("fixture lacks metadata:\n%s", got)
	}
	x := newExecutor(t, env, "still")

	for _, format := range []string{"", "jpeg", "webp", "png"} {
		t.Run("format="+format, func(t *testing.T) {
			params := map[string]string{"max_edge": "400"}
			wantType, wantName := "image/jpeg", "still.jpg"
			switch format {
			case "webp":
				wantType, wantName = "image/webp", "still.webp"
			case "png":
				wantType, wantName = "image/png", "still.png"
			}
			if format != "" {
				params["format"] = format
			}
			resp, out := execute(t, x, "still", "image/heic", src, params)
			if len(resp.Files) != 1 {
				t.Fatalf("files = %+v", resp.Files)
			}
			f := resp.Files[0]
			if f.Name != wantName || f.MediaType != wantType {
				t.Errorf("file = %+v, want %s %s", f, wantName, wantType)
			}
			if f.Width != 300 || f.Height != 400 || f.SourceFrame != 0 || f.SourceTimeMS != 0 {
				t.Errorf("file = %+v, want 300x400 frame 0 at 0ms", f)
			}
			if got := dirNames(t, out); !slices.Equal(got, []string{wantName}) {
				t.Errorf("output dir holds %v", got)
			}
			path := filepath.Join(out, wantName)
			if got := metadataGroups(t, env, path); strings.TrimSpace(got) != "" {
				t.Errorf("output carries metadata:\n%s", got)
			}
			if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%w %h", path))); got != "300 400" {
				t.Errorf("identify = %q", got)
			}
			if format == "" {
				if got := exiftool(t, env, "-s3", "-MIMEType", path); strings.TrimSpace(got) != "image/jpeg" {
					t.Errorf("default format is %q", got)
				}
			}
		})
	}
}

func TestStillSizing(t *testing.T) {
	env := requireMagick(t)
	src := writeHEIC(t) // 480x640 upright
	x := newExecutor(t, env, "still")
	cases := []struct {
		name         string
		edge, area   string
		wantW, wantH int
	}{
		{"both bounds exceeded", "400", "100000", 273, 365},
		{"only edge exceeded", "320", "100000000", 240, 320},
		{"only area exceeded", "16384", "100000", 273, 365},
		{"neither exceeded", "4000", "100000000", 480, 640},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, _ := execute(t, x, "still", "image/heic", src, map[string]string{"max_edge": c.edge, "max_area": c.area})
			f := resp.Files[0]
			if f.Width != c.wantW || f.Height != c.wantH {
				t.Errorf("got %dx%d, want %dx%d", f.Width, f.Height, c.wantW, c.wantH)
			}
		})
	}
}

func TestStillConvertsProfiledInputToSRGB(t *testing.T) {
	env := requireMagick(t)
	dir := t.TempDir()
	srgb := writeSRGBProfile(t)
	heic := heicFixtureFile(t, "adobe-256x96")
	if got := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%[profiles]", heic))); !strings.Contains(got, "icc") {
		t.Skipf("magick wrote no ICC profile into the HEIC (profiles %q)", got)
	}
	reference := filepath.Join(dir, "reference.png")
	magick(t, env, heic, "-auto-orient", "-profile", srgb, reference)
	naive := filepath.Join(dir, "naive.png")
	magick(t, env, heic, "-auto-orient", naive)

	x := newExecutor(t, env, "still")
	resp, out := execute(t, x, "still", "image/heic", stage(t, heic), map[string]string{"format": "png"})
	if got := dirNames(t, out); !slices.Equal(got, []string{"still.png"}) {
		t.Errorf("output dir holds %v", got)
	}
	got := filepath.Join(out, resp.Files[0].Name)
	if profiles := strings.TrimSpace(string(magick(t, env, "identify", "-format", "%[profiles]", got))); profiles != "" {
		t.Errorf("output embeds profiles %q", profiles)
	}
	converted := meanAbsoluteDifference(t, env, got, reference)
	unconverted := meanAbsoluteDifference(t, env, got, naive)
	if converted > 0.004 || unconverted < 5*converted {
		t.Errorf("difference from sRGB reference %.6f, from unconverted %.6f", converted, unconverted)
	}
}

func TestFramesSelection(t *testing.T) {
	env := requireMagick(t)
	src := writeGIF(t, env, solid, 20)
	x := newExecutor(t, env, "frames")
	resp, out := execute(t, x, "frames", "image/gif", src, nil)
	if len(resp.Files) != 16 {
		t.Fatalf("%d files, want 16", len(resp.Files))
	}
	var names []string
	for i, f := range resp.Files {
		wantIdx := i * 20 / 16
		if f.SourceFrame != wantIdx || f.SourceTimeMS != int64(wantIdx)*50 {
			t.Errorf("file %d: source frame %d at %d ms, want %d at %d ms", i, f.SourceFrame, f.SourceTimeMS, wantIdx, wantIdx*50)
		}
		if f.Name != fmt.Sprintf("frame-%03d.jpg", i) || f.MediaType != "image/jpeg" || f.Width != 64 || f.Height != 48 {
			t.Errorf("file %d = %+v", i, f)
		}
		names = append(names, f.Name)
	}
	if got := dirNames(t, out); !slices.Equal(got, names) {
		t.Errorf("output dir holds %v, want %v", got, names)
	}
	// The written frame is the selected source frame, not its neighbour.
	for _, i := range []int{3, 4} {
		want := strings.NewReplacer("xc:", "").Replace(solid(i * 20 / 16))
		got := strings.TrimSpace(string(magick(t, env, filepath.Join(out, resp.Files[i].Name), "-format", "rgb(%[fx:round(255*p{1,1}.r)],%[fx:round(255*p{1,1}.g)],%[fx:round(255*p{1,1}.b)])", "info:")))
		if rgbDistance(t, got, want) > 8 {
			t.Errorf("file %d pixel %s, want about %s", i, got, want)
		}
	}
}

func rgbDistance(t *testing.T, a, b string) int {
	t.Helper()
	parse := func(s string) [3]int {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "rgb("), ")")
		var out [3]int
		for i, p := range strings.Split(s, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				t.Fatalf("parsing %q: %v", s, err)
			}
			out[i] = n
		}
		return out
	}
	pa, pb := parse(a), parse(b)
	d := 0
	for i := range pa {
		d = max(d, max(pa[i]-pb[i], pb[i]-pa[i]))
	}
	return d
}

func TestFramesDropsConsecutiveDuplicates(t *testing.T) {
	env := requireMagick(t)
	src := writeGIF(t, env, func(int) string { return "xc:rgb(10,120,200)" }, 4)
	x := newExecutor(t, env, "frames")
	resp, out := execute(t, x, "frames", "image/gif", src, nil)
	if len(resp.Files) != 1 || resp.Files[0].Name != "frame-000.jpg" || resp.Files[0].SourceFrame != 0 {
		t.Fatalf("files = %+v", resp.Files)
	}
	if got := dirNames(t, out); !slices.Equal(got, []string{"frame-000.jpg"}) {
		t.Errorf("output dir holds %v", got)
	}
}

func TestFramesRenumbersAfterDedup(t *testing.T) {
	env := requireMagick(t)
	// Frames 0-1 identical, 2-3 identical: two kept frames, numbered 0 and 1.
	src := writeGIF(t, env, func(i int) string { return solid(i / 2) }, 4)
	x := newExecutor(t, env, "frames")
	resp, out := execute(t, x, "frames", "image/gif", src, nil)
	var got [][2]int64
	for _, f := range resp.Files {
		got = append(got, [2]int64{int64(f.SourceFrame), f.SourceTimeMS})
	}
	if len(got) != 2 || got[0] != [2]int64{0, 0} || got[1] != [2]int64{2, 100} {
		t.Errorf("source frames %v", got)
	}
	if names := dirNames(t, out); !slices.Equal(names, []string{"frame-000.jpg", "frame-001.jpg"}) {
		t.Errorf("output dir holds %v", names)
	}
}

func TestFramesFlattensTransparencyOnWhite(t *testing.T) {
	env := requireMagick(t)
	path := filepath.Join(t.TempDir(), "t.gif")
	magick(t, env, "-size", "40x40", "-dispose", "background", "-delay", "5",
		"xc:none", "-fill", "red", "-draw", "circle 20,20 20,6",
		"(", "-size", "40x40", "xc:none", "-fill", "blue", "-draw", "circle 20,20 20,12", ")",
		"-loop", "0", path)
	x := newExecutor(t, env, "frames")
	resp, out := execute(t, x, "frames", "image/gif", stage(t, path), nil)
	if len(resp.Files) != 2 {
		t.Fatalf("%d files, want 2", len(resp.Files))
	}
	for _, f := range resp.Files {
		if f.MediaType != "image/jpeg" {
			t.Errorf("%s is %s", f.Name, f.MediaType)
		}
		got := strings.TrimSpace(string(magick(t, env, filepath.Join(out, f.Name), "-format", "rgb(%[fx:round(255*p{1,1}.r)],%[fx:round(255*p{1,1}.g)],%[fx:round(255*p{1,1}.b)])", "info:")))
		if d := rgbDistance(t, got, "rgb(255,255,255)"); d > 3 {
			t.Errorf("%s corner pixel %s, want white", f.Name, got)
		}
	}
}

func TestFramesStripsMetadataAndAcceptsWebP(t *testing.T) {
	requireExiftool(t)
	env := requireMagick(t)
	path := filepath.Join(t.TempDir(), "m.webp")
	magick(t, env, "-size", "300x200", "gradient:red-blue", path)
	addMetadata(t, env, path)
	if got := metadataGroups(t, env, path); !strings.Contains(got, "Artist") {
		t.Fatalf("fixture lacks metadata:\n%s", got)
	}
	x := newExecutor(t, env, "frames")
	resp, out := execute(t, x, "frames", "image/webp", stage(t, path), map[string]string{"format": "png", "max_edge": "100"})
	if len(resp.Files) != 1 || resp.Files[0].Width != 100 || resp.Files[0].Height != 66 {
		t.Fatalf("files = %+v", resp.Files)
	}
	if got := metadataGroups(t, env, filepath.Join(out, resp.Files[0].Name)); strings.TrimSpace(got) != "" {
		t.Errorf("output carries metadata:\n%s", got)
	}
}

func TestFramesSizing(t *testing.T) {
	env := requireMagick(t)
	src := writeGIF(t, env, solid, 3) // 64x48
	x := newExecutor(t, env, "frames")
	cases := []struct {
		name         string
		params       map[string]string
		wantW, wantH int
	}{
		{"edge", map[string]string{"max_edge": "32"}, 32, 24},
		{"area", map[string]string{"max_area": "768"}, 32, 24},
		{"both", map[string]string{"max_edge": "48", "max_area": "768"}, 32, 24},
		{"no upscale", map[string]string{"max_edge": "1000"}, 64, 48},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, _ := execute(t, x, "frames", "image/gif", src, c.params)
			for _, f := range resp.Files {
				if f.Width != c.wantW || f.Height != c.wantH {
					t.Errorf("%s is %dx%d, want %dx%d", f.Name, f.Width, f.Height, c.wantW, c.wantH)
				}
			}
		})
	}
}

func TestFramesMaxFramesParam(t *testing.T) {
	env := requireMagick(t)
	src := writeGIF(t, env, solid, 20)
	x := newExecutor(t, env, "frames")
	resp, _ := execute(t, x, "frames", "image/gif", src, map[string]string{"max_frames": "4"})
	var idx []int
	for _, f := range resp.Files {
		idx = append(idx, f.SourceFrame)
	}
	if !slices.Equal(idx, []int{0, 5, 10, 15}) {
		t.Errorf("source frames %v", idx)
	}
}

func TestExecuteFileRecords(t *testing.T) {
	env := requireMagick(t)
	src := writeGIF(t, env, solid, 5)
	x := newExecutor(t, env, "frames")
	resp, out := execute(t, x, "frames", "image/gif", src, nil)
	for _, f := range resp.Files {
		data, err := os.ReadFile(filepath.Join(out, f.Name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if f.SHA256 != hex.EncodeToString(sum[:]) || f.Size != int64(len(data)) {
			t.Errorf("%s: record %s/%d, file %x/%d", f.Name, f.SHA256, f.Size, sum, len(data))
		}
	}
}

func TestExecuteCleansUpOnFailure(t *testing.T) {
	real := requireMagick(t)
	wide := func() string { return heicFixture(t, "adobe-64x64") }
	cases := []struct {
		name, recipe, inputType string
		input                   func() string
		failAfter               int
	}{
		{"still after encode", "still", "image/heic", func() string { return writeHEIC(t) }, 2},
		{"still with profile after encode", "still", "image/heic", wide, 2},
		{"still at encode", "still", "image/heic", func() string { return writeHEIC(t) }, 3},
		{"frames after probe", "frames", "image/gif", func() string { return writeGIF(t, real, solid, 6) }, 2},
		{"frames after encode", "frames", "image/gif", func() string { return writeGIF(t, real, solid, 6) }, 3},
		{"frames after output identify", "frames", "image/gif", func() string { return writeGIF(t, real, solid, 6) }, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, env := newRecorder(real)
			rec.failAfter = c.failAfter
			x := newExecutor(t, env, c.recipe)
			out := t.TempDir()
			_, err := x.Execute(context.Background(), Request{
				Recipe: c.recipe, InputPath: c.input(), InputType: c.inputType, OutputDir: out,
			})
			if err == nil {
				t.Fatal("Execute succeeded")
			}
			if got := dirNames(t, out); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}
}

func TestAccounting(t *testing.T) {
	real := requireMagick(t)
	src := writeGIF(t, real, solid, 8)
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("measured", func(t *testing.T) {
		rec, env := newRecorder(real)
		x := newExecutor(t, env, "frames")
		resp, out := execute(t, x, "frames", "image/gif", src, nil)
		a := resp.Accounting
		if a.Runs != rec.count() || a.Runs != 4 {
			t.Errorf("Runs = %d, recorder saw %d, want 4", a.Runs, rec.count())
		}
		if a.Usage == nil || a.Usage.CPU <= 0 || a.Usage.MaxRSS < 1<<20 {
			t.Errorf("Usage = %+v", a.Usage)
		}
		var total int64
		for _, name := range dirNames(t, out) {
			fi, err := os.Stat(filepath.Join(out, name))
			if err != nil {
				t.Fatal(err)
			}
			total += fi.Size()
		}
		if a.BytesIn != info.Size() || a.BytesOut != total || total == 0 {
			t.Errorf("bytes in/out = %d/%d, want %d/%d", a.BytesIn, a.BytesOut, info.Size(), total)
		}
	})

	t.Run("sums CPU and takes the largest peak", func(t *testing.T) {
		usage := []*Usage{{CPU: 10, MaxRSS: 5 << 20}, {CPU: 20, MaxRSS: 9 << 20}, {CPU: 30, MaxRSS: 7 << 20}, {CPU: 40, MaxRSS: 1 << 20}}
		var n int
		env := Env{Run: func(ctx context.Context, name string, args ...string) (Result, error) {
			res, err := real.Run(ctx, name, args...)
			res.Usage = usage[n]
			n++
			return res, err
		}}
		resp, _ := execute(t, newExecutor(t, env, "frames"), "frames", "image/gif", src, nil)
		if u := resp.Accounting.Usage; u == nil || u.CPU != 100 || u.MaxRSS != 9<<20 {
			t.Errorf("Usage = %+v, want CPU 100 and peak 9 MiB", u)
		}
	})

	t.Run("no usage", func(t *testing.T) {
		rec, env := newRecorder(real)
		rec.dropUsage = true
		resp, _ := execute(t, newExecutor(t, env, "frames"), "frames", "image/gif", src, nil)
		if resp.Accounting.Usage != nil || resp.Accounting.Runs != 4 {
			t.Errorf("Accounting = %+v", resp.Accounting)
		}
	})

	t.Run("one run without usage", func(t *testing.T) {
		rec, env := newRecorder(real)
		rec.dropUsageOn = 2
		resp, _ := execute(t, newExecutor(t, env, "frames"), "frames", "image/gif", src, nil)
		if resp.Accounting.Usage != nil {
			t.Errorf("Usage = %+v, want nil", resp.Accounting.Usage)
		}
	})
}

func TestDescribe(t *testing.T) {
	env := requireMagick(t)
	for _, enabled := range [][]string{{"still"}, {"frames"}, {"still", "frames"}} {
		x := newExecutor(t, env, enabled...)
		d, err := x.Describe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if d.FlakeRevision != tool.FlakeRevision() {
			t.Errorf("FlakeRevision = %q", d.FlakeRevision)
		}
		if len(d.Engines) != 1 || d.Engines[0].Tool != "magick" {
			t.Fatalf("%v: engines = %+v, want only magick", enabled, d.Engines)
		}
		e := d.Engines[0]
		if !strings.HasPrefix(e.Version, "Version: ImageMagick") || strings.Contains(e.Version, "\n") {
			t.Errorf("version = %q", e.Version)
		}
		resolved, err := tool.Resolve("magick")
		if err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(resolved)
		if err != nil {
			t.Fatal(err)
		}
		if e.Path != real || !filepath.IsAbs(e.Path) {
			t.Errorf("path = %q, want %q", e.Path, real)
		}
		var names []string
		for _, r := range d.Recipes {
			names = append(names, r.Name)
		}
		if !slices.Equal(names, sortedCopy(enabled)) {
			t.Errorf("recipes = %v, want %v", names, enabled)
		}
	}

	d, err := newExecutor(t, env).Describe(context.Background())
	if err != nil || len(d.Engines) != 0 || len(d.Recipes) != 0 {
		t.Errorf("empty executor describes %+v, %v", d, err)
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func TestGoldenArgv(t *testing.T) {
	real := requireMagick(t)
	wide := func() string { return heicFixture(t, "adobe-64x32") }
	cases := []struct {
		name, recipe, inputType string
		input                   func() string
		params                  map[string]string
	}{
		{"still-jpeg-default", "still", "image/heic", func() string { return writeHEIC(t) }, nil},
		{
			"still-png-bounded", "still", "image/heif", func() string { return writeHEIC(t) },
			map[string]string{"format": "png", "max_edge": "400", "max_area": "100000", "quality": "75"},
		},
		{"still-profiled", "still", "image/heic", wide, nil},
		{"frames-gif-default", "frames", "image/gif", func() string { return writeGIF(t, real, solid, 20) }, nil},
		{
			"frames-gif-webp-bounded", "frames", "image/gif", func() string { return writeGIF(t, real, solid, 10) },
			map[string]string{"format": "webp", "max_frames": "3", "max_edge": "32"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, env := newRecorder(real)
			input := c.input()
			out := t.TempDir()
			if _, err := newExecutor(t, env, c.recipe).Execute(context.Background(), Request{
				Recipe: c.recipe, Params: c.params, InputPath: input, InputType: c.inputType, OutputDir: out,
			}); err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for _, call := range rec.calls {
				for _, arg := range call {
					arg = strings.ReplaceAll(arg, filepath.Dir(input), "$IN")
					arg = strings.ReplaceAll(arg, out, "$OUT")
					b.WriteString(strconv.Quote(arg))
					b.WriteByte('\n')
				}
				b.WriteByte('\n')
			}
			path := filepath.Join("testdata", "golden-argv", c.name+".txt")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(want) != b.String() {
				t.Errorf("argv differs from %s:\n--- got\n%s\n--- want\n%s", path, b.String(), want)
			}
		})
	}
}
