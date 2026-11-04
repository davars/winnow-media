package media

import (
	"bytes"
	"context"
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
			res := Result{Stdout: out, Usage: UsageOf(cmd.ProcessState)}
			if err != nil {
				return res, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
			}
			return res, nil
		},
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
