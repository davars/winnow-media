package media

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrientationOutOfRangeIsAnInvalidRequest(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path := heicFixtureFile(t, "adobe-64x32")
	for _, n := range []int{-1, 9} {
		if _, _, err := p.Open(context.Background(), path, OpenOptions{Orientation: n}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Orientation %d: err = %v, want ErrInvalidRequest", n, err)
		}
	}
}

func TestUseAfterCloseIsErrClosed(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, Encoding{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Encode after Close: err = %v, want ErrClosed", err)
	}
}

// An error names neither the input's path nor the output directory, which
// the caller knows and can add.
func TestErrorsNameNoPaths(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	missing := filepath.Join(t.TempDir(), "no-such-input-file")
	_, _, err := p.Open(context.Background(), missing, OpenOptions{})
	if !errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), "no-such-input-file") {
		t.Errorf("Open of a missing file: err = %v, want ErrNotExist naming no path", err)
	}
	src := openSourceOn(t, p, heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	dir := filepath.Join(t.TempDir(), "no-such-output-dir")
	_, _, err = src.Encode(context.Background(), dir, Select{}, Encoding{})
	if !errors.Is(err, ErrInvalidRequest) || !errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), "no-such-output-dir") {
		t.Errorf("Encode into a missing dir: err = %v, want ErrInvalidRequest and ErrNotExist naming no path", err)
	}
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "leftover"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = src.Encode(context.Background(), full, Select{}, Encoding{})
	if !errors.Is(err, ErrInvalidRequest) || strings.Contains(err.Error(), full) {
		t.Errorf("Encode into a full dir: err = %v, want ErrInvalidRequest naming no path", err)
	}
}

func TestToolErrorTextHasNoStderr(t *testing.T) {
	t.Parallel()
	e := &ToolError{Tool: "magick", ExitCode: 1, Stderr: []byte("secret path /in/file")}
	if got := e.Error(); got != "magick: exit status 1" {
		t.Errorf("Error() = %q", got)
	}
}

// errText is err's text with the standard error of the tool run it wraps,
// for tests that look for what a tool said.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error() + ": " + string(stderrOf(err))
}
