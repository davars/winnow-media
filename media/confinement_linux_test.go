//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestProcessorReportsItsConfinement(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	p, err := NewProcessor(context.Background(), pdfConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if c := p.Confinement(); !c.Confined || c.ABI < 1 || c.Reason != "" {
		t.Errorf("Confinement() = %+v, want confined at ABI 1 or later", c)
	}
	for name, cfg := range map[string]Config{
		"no render types": {},
		"unsandboxed":     {RenderTypes: pdfConfig.RenderTypes, Unsandboxed: true},
	} {
		p, err := NewProcessor(context.Background(), cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c := p.Confinement(); c.Confined || c.ABI != 0 || c.Reason == "" {
			t.Errorf("%s: Confinement() = %+v, want unconfined with a reason", name, c)
		}
		_ = p.Close()
	}
}

// Where the kernel has no Landlock, render runs go on under the rest of the
// sandbox, unconfined, and the Processor says so.
func TestRenderWithoutLandlockRunsUnconfined(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	probeFault = "landlock-unavailable"
	defer func() { probeFault = "" }()
	for name, cfg := range map[string]Config{
		"pdf": pdfConfig,
		"svg": svgConfig,
	} {
		t.Run(name, func(t *testing.T) {
			p, err := NewProcessor(context.Background(), cfg)
			if err != nil {
				t.Fatalf("NewProcessor without Landlock: %v", err)
			}
			defer func() { _ = p.Close() }()
			c := p.Confinement()
			if c.Confined || c.ABI != 0 || !strings.Contains(c.Reason, "file confinement is unavailable") {
				t.Errorf("Confinement() = %+v, want unconfined because file confinement is unavailable", c)
			}
			if !strings.Contains(c.String(), "not confined") {
				t.Errorf("String() = %q", c.String())
			}
			var mu sync.Mutex
			var ran, confined int
			inner := p.start
			p.start = func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
				mu.Lock()
				ran++
				if spec.Confine != nil {
					confined++
				}
				mu.Unlock()
				return inner(ctx, spec, stdout, stderr)
			}
			dir := t.TempDir()
			if name == "pdf" {
				doc := openDoc(t, p, "testdata/pdf/base14-two-pages.pdf")
				rasters, _, err := doc.Render(context.Background(), dir, Indices(0), LongEdge(200))
				if err != nil || len(rasters) != 1 {
					t.Fatalf("Render: %v, %v", rasters, err)
				}
			} else {
				r, _, err := p.RenderSVG(context.Background(), filepath.Join("testdata", "svg", "alpha-shapes.svg"), dir, SVGRender{})
				if err != nil || r.Width == 0 {
					t.Fatalf("RenderSVG: %+v, %v", r, err)
				}
			}
			if ran == 0 || confined != 0 {
				t.Errorf("%d tool runs, %d confined; want runs and none confined", ran, confined)
			}
		})
	}
}

func TestRequireConfinement(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	t.Run("unavailable", func(t *testing.T) {
		probeFault = "landlock-unavailable"
		defer func() { probeFault = "" }()
		cfg := pdfConfig
		cfg.RequireConfinement = true
		if p, err := NewProcessor(context.Background(), cfg); err == nil {
			_ = p.Close()
			t.Error("NewProcessor with RequireConfinement succeeded without Landlock")
		} else if !strings.Contains(err.Error(), "file confinement is unavailable") {
			t.Errorf("err = %v, want one saying file confinement is unavailable", err)
		}
		// Nothing renders, so nothing needs confining.
		p, err := NewProcessor(context.Background(), Config{RequireConfinement: true})
		if err != nil {
			t.Errorf("RequireConfinement without RenderTypes: %v", err)
		} else {
			_ = p.Close()
		}
	})
	t.Run("unsandboxed", func(t *testing.T) {
		p, err := NewProcessor(context.Background(), Config{RenderTypes: pdfConfig.RenderTypes, Unsandboxed: true, RequireConfinement: true})
		if err == nil {
			_ = p.Close()
			t.Error("NewProcessor with RequireConfinement and Unsandboxed succeeded")
		}
	})
	t.Run("available", func(t *testing.T) {
		cfg := pdfConfig
		cfg.RequireConfinement = true
		p, err := NewProcessor(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Close() }()
		if !p.Confinement().Confined {
			t.Error("not confined")
		}
	})
}

func TestConfinementStringIsNeutralWithoutRenderTypes(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	p, err := NewProcessor(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if s := p.Confinement().String(); strings.Contains(s, "not confined") || !strings.Contains(s, "no render runs") {
		t.Errorf("String() = %q, want a neutral note that there are no render runs", s)
	}
}
