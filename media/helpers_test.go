package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/davars/winnow-media/tool"
)

// testEnv runs the real tools, resolved through the tool package, and stages
// outputs under a per-test directory. A test that needs a tool skips when it
// cannot be found.
func testEnv(t *testing.T) Env {
	t.Helper()
	staging := t.TempDir()
	return Env{
		Run: func(ctx context.Context, name string, args ...string) (Result, error) {
			path, err := tool.Resolve(name)
			if err != nil {
				return Result{}, err
			}
			cmd := exec.CommandContext(ctx, path, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			res := Result{Stdout: out, Stderr: stderr.Bytes(), Usage: UsageOf(cmd.ProcessState)}
			if err != nil {
				return res, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
			}
			return res, nil
		},
		Scratch: testScratch,
		Stage: func(ext string) (string, func(), error) {
			f, err := os.CreateTemp(staging, "*"+ext)
			if err != nil {
				return "", nil, err
			}
			path := f.Name()
			if err := f.Close(); err != nil {
				return "", nil, err
			}
			return path, func() { _ = os.Remove(path) }, nil
		},
	}
}

func requireMagick(t *testing.T) Env {
	t.Helper()
	if _, err := tool.Resolve("magick"); err != nil {
		t.Skip("magick not available")
	}
	return testEnv(t)
}

// requireProcessor returns a Processor over the real tools, closed when the
// test ends, and skips the test when a tool cannot be found.
func requireProcessor(t *testing.T) *Processor {
	t.Helper()
	return requireProcessorWith(t, HighMemory)
}

// requireProcessorWith is requireProcessor under the memory profile.
func requireProcessorWith(t *testing.T, profile MemoryProfile) *Processor {
	t.Helper()
	for _, name := range []string{"magick", "ffmpeg", "ffprobe"} {
		if _, err := tool.Resolve(name); err != nil {
			t.Skip(name + " not available")
		}
	}
	p, err := NewProcessor(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func magick(t *testing.T, env Env, args ...string) []byte {
	t.Helper()
	res, err := env.Run(context.Background(), "magick", args...)
	if err != nil {
		t.Fatalf("magick %q: %v", args, err)
	}
	return res.Stdout
}

// renameToContent drops path's extension, so the decoder cannot rely on the
// name.
func renameToContent(t *testing.T, path string) string {
	t.Helper()
	content := filepath.Join(filepath.Dir(path), "content")
	if err := os.Rename(path, content); err != nil {
		t.Fatal(err)
	}
	return content
}

func meanAbsoluteDifference(t *testing.T, env Env, first, second string) float64 {
	t.Helper()
	out := magick(t, env, first, second, "-compose", "difference", "-composite", "-format", "%[fx:mean]", "info:")
	value, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parsing image difference %q: %v", out, err)
	}
	return value
}

func animationFrameDifference(t *testing.T, env Env, first, second string, frame int) float64 {
	t.Helper()
	deleteExpr := strconv.Itoa(1 - frame)
	frameArgs := func(path string) []string {
		return []string{"(", path, "-coalesce", "-delete", deleteExpr, ")"}
	}
	args := append(frameArgs(first), frameArgs(second)...)
	args = append(args, "-compose", "difference", "-composite", "-format", "%[fx:mean]", "info:")
	value, err := strconv.ParseFloat(strings.TrimSpace(string(magick(t, env, args...))), 64)
	if err != nil {
		t.Fatalf("parsing animation frame %d difference: %v", frame, err)
	}
	return value
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

// still encodes the first image of path, read with dec, to dir as the
// Processor's Still does.
func (e Env) still(ctx context.Context, dec decoder, path string, enc encoding, dir string) (Image, error) {
	src, err := e.open(ctx, openRequest{path: path, dec: dec, pinned: true, caps: true, hints: Hints{Still: true}, scratch: dir})
	if err != nil {
		return Image{}, err
	}
	defer src.Close()
	return src.stillImage(ctx, enc, dir)
}

// testScratch is an Env.Scratch that makes directories under the system
// temporary directory.
func testScratch() (string, func(), error) {
	dir, err := os.MkdirTemp("", "media-test-scratch-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
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
