package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/davars/winnow-media/tool"
)

// testEnv runs the real tools, resolved through the tool package, directly
// and without the sandbox, under the default Resources and the measured caps.
// A test that needs a tool fails when it cannot be found.
func testEnv(t *testing.T) toolEnv {
	t.Helper()
	return toolEnv{
		run: func(ctx context.Context, name string, args ...string) (runResult, error) {
			path, err := tool.Resolve(name)
			if err != nil {
				return runResult{}, err
			}
			cmd := exec.CommandContext(ctx, path, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			res := runResult{Stdout: out, Stderr: stderr.Bytes(), Usage: usageOf(cmd.ProcessState)}
			if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
				return res, &ToolError{Tool: name, ExitCode: ee.ExitCode(), Stderr: bytes.TrimSpace(stderr.Bytes())}
			} else if err != nil {
				return res, fmt.Errorf("%s: %w", name, err)
			}
			return res, nil
		},
		resources: defaultResources(),
		caps:      measuredCaps,
	}
}

// requireTools fails the test unless every named tool resolves, and returns
// testEnv. A missing pinned tool means the tests are not running under nix
// develop.
func requireTools(t *testing.T, names ...string) toolEnv {
	t.Helper()
	for _, name := range names {
		if _, err := tool.Resolve(name); err != nil {
			t.Fatal(name + " is not available; run the tests under nix develop")
		}
	}
	return testEnv(t)
}

func requireMagick(t *testing.T) toolEnv { return requireTools(t, "magick") }

// requireProcessor returns a Processor over the real tools, closed when the
// test ends, and fails the test when a tool cannot be found.
func requireProcessor(t *testing.T) *Processor {
	t.Helper()
	return requireProcessorWith(t, Config{})
}

// requireProcessorWith is requireProcessor under cfg.
func requireProcessorWith(t *testing.T, cfg Config) *Processor {
	t.Helper()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	p, err := NewProcessor(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func magick(t *testing.T, env toolEnv, args ...string) []byte {
	t.Helper()
	res, err := env.run(context.Background(), "magick", args...)
	if err != nil {
		t.Fatalf("magick %q: %v", args, err)
	}
	return res.Stdout
}

func meanAbsoluteDifference(t *testing.T, env toolEnv, first, second string) float64 {
	t.Helper()
	out := magick(t, env, first, second, "-compose", "difference", "-composite", "-format", "%[fx:mean]", "info:")
	value, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parsing image difference %q: %v", out, err)
	}
	return value
}

// animationFrameDifference is the mean absolute difference between frame
// number frame of two animations of n frames each.
func animationFrameDifference(t *testing.T, env toolEnv, first, second string, frame, n int) float64 {
	t.Helper()
	pick := func(path string) []string {
		return []string{"(", path, "-coalesce", "-delete", deleteExpr([]int{frame}, n), ")"}
	}
	out := magick(t, env, append(append(pick(first), pick(second)...),
		"-compose", "difference", "-composite", "-format", "%[fx:mean]", "info:")...)
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parsing animation frame %d difference: %v", frame, err)
	}
	return v
}

// writeFile writes data to a new file called name in a fresh directory.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ftypBox returns an ISO base media ftyp box.
func ftypBox(major string, compatible ...string) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(16+4*len(compatible)))
	b = append(b, "ftyp"...)
	b = append(b, major...)
	b = append(b, 0, 0, 0, 0)
	for _, c := range compatible {
		b = append(b, c...)
	}
	return b
}

// ebmlHeader returns an EBML header whose document type is docType.
func ebmlHeader(docType string) []byte {
	version := []byte{0x42, 0x86, 0x81, 0x01}
	doc := append([]byte{0x42, 0x82, 0x80 | byte(len(docType))}, docType...)
	body := append(version, doc...)
	return append([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80 | byte(len(body))}, body...)
}

// withProfile places the package's sRGB profile in outDir for the duration of
// fn when profiled is set, and passes fn its path.
func withProfile(outDir string, profiled bool, fn func(profilePath string) error) error {
	if !profiled {
		return fn("")
	}
	path, err := placeSRGB(outDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	return fn(path)
}

// headerWithOrientation returns the output of a header read with the
// orientation of every scene replaced by name.
func headerWithOrientation(out []byte, name string) []byte {
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if f := strings.Split(line, "|"); len(f) == 11 {
			f[7] = name
			lines[i] = strings.Join(f, "|")
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// The caps below are the values docs/measurements.md records, which owns them
// and their rationale; the tests of the cap checks hold the gate to them. A zero
// Config has only the library ceilings.
const (
	maxInputBytes     = 25 << 20
	maxFramePixels    = 52_000_000
	maxReturnedPixels = 64_000_000
	maxDecodedPixels  = 900_000_000
)

// measuredCaps are the measured caps, resolved.
var measuredCaps = caps{
	inputBytes:     maxInputBytes,
	framePixels:    maxFramePixels,
	returnedPixels: maxReturnedPixels,
	decodedPixels:  maxDecodedPixels,
	frames:         maxAnimationFrames,
}

// measuredCapsConfig are the measured caps, as a Config takes them.
var measuredCapsConfig = Caps{
	MaxInputBytes:      maxInputBytes,
	MaxFramePixels:     maxFramePixels,
	MaxReturnedPixels:  maxReturnedPixels,
	MaxDecodedPixels:   maxDecodedPixels,
	MaxAnimationFrames: maxAnimationFrames,
}

func defaultResources() Resources { return Resources{}.withDefaults() }

// deleteExpr renders the magick -delete argument dropping every one of
// sourceFrames frames that keep does not name.
func deleteExpr(keep []int, sourceFrames int) string {
	kept := make(map[int]bool, len(keep))
	for _, idx := range keep {
		kept[idx] = true
	}
	var drop []string
	for i := range sourceFrames {
		if !kept[i] {
			drop = append(drop, strconv.Itoa(i))
		}
	}
	return strings.Join(drop, ",")
}

// inputAt returns the input that reads the file at path, which is closed when
// the test ends.
func inputAt(t testing.TB, path string) input {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return input{f: f}
}

// sniffPath returns the media type the leading bytes of the file at path name.
func sniffPath(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	buf, err := readHead(input{f: f})
	if err != nil {
		return "", err
	}
	return sniff(buf)
}

// countingEnv wraps env so the argv of every magick run is recorded.
func countingEnv(env toolEnv) (toolEnv, func() [][]string) {
	var mu sync.Mutex
	var calls [][]string
	inner := env.run
	env.run = func(ctx context.Context, name string, args ...string) (runResult, error) {
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

// stillRegion encodes the region r of the first image of the file at path
// through p into dir, opened under opts.
func stillRegion(ctx context.Context, p *Processor, r region, path string, opts OpenOptions, enc Encoding, dir string) (Image, error) {
	src, _, err := p.Open(ctx, path, opts)
	if err != nil {
		return Image{}, err
	}
	defer func() { _ = src.Close() }()
	images, _, err := src.Encode(ctx, dir, Select{Region: r.exported()}, enc)
	if err != nil {
		return Image{}, err
	}
	return images[0], nil
}

// sourceProcessor returns a Processor over the real tools, closed when the test
// ends, and a recorder of its tool runs, including those of an external frame
// compositor such as webp-frames.
func sourceProcessor(t *testing.T) (*Processor, *recorder) {
	t.Helper()
	return sourceProcessorWith(t, Config{})
}

// sourceProcessorWith is sourceProcessor under cfg.
func sourceProcessorWith(t *testing.T, cfg Config) (*Processor, *recorder) {
	t.Helper()
	p := requireProcessorWith(t, cfg)
	rec := record(p)
	inner := p.compose
	p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
		if req.external != "" {
			rec.mu.Lock()
			rec.calls = append(rec.calls, append([]string{req.external}, req.args()...))
			rec.mu.Unlock()
		}
		return inner(ctx, in, req, onFrame)
	}
	return p, rec
}

// sameImages reports whether a and b describe the same files.
func sameImages(a, b []Image) bool { return reflect.DeepEqual(a, b) }

// stillAs encodes the first image of the file at path through Open and Encode,
// reading it as typ when that is not empty.
func stillAs(ctx context.Context, p *Processor, path, typ string, enc Encoding, dir string) (Image, Usage, error) {
	src, usage, err := p.Open(ctx, path, OpenOptions{Type: typ})
	if err != nil {
		return Image{}, usage, err
	}
	defer func() { _ = src.Close() }()
	images, used, err := src.Encode(ctx, dir, Select{}, enc)
	usage.CPU += used.CPU
	usage.MaxRSS = max(usage.MaxRSS, used.MaxRSS)
	if err != nil {
		return Image{}, usage, err
	}
	return images[0], usage, nil
}

// streamingDiskBytes is a Resources.Disk below the composited frames of any
// animation of more than one frame in the tests, which makes a Source stream:
// about one batch of frames is on disk at a time and none is kept.
const streamingDiskBytes = 16 << 10

// requireStreamingProcessor is requireProcessor with Resources.Disk at
// streamingDiskBytes.
func requireStreamingProcessor(t *testing.T) *Processor {
	t.Helper()
	return requireProcessorWith(t, Config{Resources: Resources{Disk: streamingDiskBytes}})
}

// exported is r as the Region of a Select.
func (r region) exported() Region { return Region{X: r.x, Y: r.y, Width: r.width, Height: r.height} }

// defaultFrames is the number of evenly spaced frames the tests that do not
// care take of an animation or video.
const defaultFrames = 16
