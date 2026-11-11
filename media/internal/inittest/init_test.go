// Package inittest holds a test with no TestMain: it shows that importing the
// media package is all a binary needs for its Processor's sandbox helper and
// frame compositor to start.
package inittest_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/davars/winnow-media/media"
	"github.com/davars/winnow-media/tool"
)

func requireSandbox(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("the sandbox needs Linux on amd64 or arm64")
	}
}

// NewProcessor's probe starts the helper, which only the package init can
// run, and requires socket denied and every limit set; the operations run a
// tool, and the GIF compositor, the same way.
func TestProcessorRunsWithoutTestMain(t *testing.T) {
	requireSandbox(t)
	runOperations(t, media.Config{})
}

// Without the sandbox the GIF compositor is still this executable started
// again, which only the package init can run as the compositor.
func TestUnsandboxedProcessorRunsWithoutTestMain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tools run directly only on Unix hosts")
	}
	runOperations(t, media.Config{Unsandboxed: true})
}

func runOperations(t *testing.T, cfg media.Config) {
	t.Helper()
	for _, name := range []string{"magick", "ffmpeg", "ffprobe", "webp-frames"} {
		if _, err := tool.Resolve(name); err != nil {
			t.Skip(name + " not available")
		}
	}
	p, err := media.NewProcessor(context.Background(), withDir(cfg, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })

	t.Run("still", func(t *testing.T) {
		dir := t.TempDir()
		out, err := encode(p, filepath.Join("..", "..", "testdata", "heic", "adobe-64x64.heic"), media.Select{}, dir,
			media.Encoding{Format: media.WebP, Quality: 80, MaxEdge: 32, MaxArea: 1024})
		if err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(filepath.Join(dir, out[0].Name)); err != nil || info.Size() == 0 {
			t.Errorf("output %q: %v", out[0].Name, err)
		}
	})

	t.Run("gif compositor", func(t *testing.T) {
		var buf bytes.Buffer
		frames := &gif.GIF{}
		for i := range 3 {
			pm := image.NewPaletted(image.Rect(0, 0, 16, 16), color.Palette{color.Black, color.White, color.RGBA{R: uint8(80 * i), A: 255}})
			for x := range 16 {
				pm.SetColorIndex(x, i, 2)
			}
			frames.Image = append(frames.Image, pm)
			frames.Delay = append(frames.Delay, 5)
		}
		if err := gif.EncodeAll(&buf, frames); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "a.gif")
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := encode(p, path, media.Select{Frames: media.Even(3)}, t.TempDir(),
			media.Encoding{Format: media.WebP, Quality: 80, MaxEdge: 16, MaxArea: 256})
		if err != nil {
			t.Fatal(err)
		}
		if len(out) == 0 {
			t.Error("no frames")
		}
	})
}

// encode opens path and makes one Encode of sel into dir.
func encode(p *media.Processor, path string, sel media.Select, dir string, enc media.Encoding) ([]media.Image, error) {
	src, _, err := p.Open(context.Background(), path, media.OpenOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	out, _, err := src.Encode(context.Background(), dir, sel, enc)
	return out, err
}

func withDir(cfg media.Config, dir string) media.Config {
	cfg.Dir = dir
	return cfg
}
