package media

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// argvProcessor returns a Processor that starts no process. It has the fields
// NewProcessor sets from cfg (the resolved Resources, Caps, Types and
// RenderTypes, and what follows from them), a scratch directory like a real
// one's, and start as its runner; the sandbox and ImageMagick policy checks,
// and the lookup of the tools, are what it leaves out. The tools are named as
// a real Processor names them, so a run reaches start with the argv a real
// one would give it. TestArgvProcessorMatchesReal holds it to that.
func argvProcessor(t testing.TB, cfg Config, start runner) *Processor {
	t.Helper()
	res, err := cfg.Resources.resolve()
	if err != nil {
		t.Fatal(err)
	}
	limits, err := cfg.Caps.resolve()
	if err != nil {
		t.Fatal(err)
	}
	types, err := resolveTypes(cfg.Types)
	if err != nil {
		t.Fatal(err)
	}
	renderTypes, err := resolveRenderTypes(cfg.RenderTypes)
	if err != nil {
		t.Fatal(err)
	}
	renders := make(map[string]bool, len(renderTypes))
	for _, typ := range renderTypes {
		renders[typ] = true
	}
	accepts := make(map[string]bool, len(types))
	for _, typ := range types {
		accepts[typ] = true
	}
	tools := map[string]string{}
	for _, name := range toolNames(renders) {
		tools[name] = filepath.Join("/nonexistent-tools", name)
	}
	parent := cfg.Dir
	if parent == "" {
		parent = t.TempDir()
	}
	dir, err := os.MkdirTemp(parent, "media-processor-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := &Processor{openInput: openInputFile, dir: dir, res: res, caps: limits, types: types, accepts: accepts, renders: renders, detect: detectedDemuxers(types), tools: tools, start: start, unsandboxed: cfg.Unsandboxed}
	p.run = p.runTool
	p.compose = p.composeFrames
	switch {
	case len(renders) == 0:
		p.confinement.Reason = reasonNoRender
	case cfg.Unsandboxed:
		p.confinement.Reason = "the sandbox is off"
	default:
		p.confinement = Confinement{Confined: true, ABI: 1}
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// toolNames returns the tools a Processor with these render types resolves.
func toolNames(renders map[string]bool) []string {
	names := []string{"magick", "ffmpeg", "ffprobe", webpFramesTool}
	if renders[typePDF] {
		names = append(names, "pdfinfo", "pdftoppm")
	}
	return names
}

// processorShape is every field of a Processor that decides the arguments of
// its tool runs, in a form two Processors can be compared by.
type processorShape struct {
	Res         Resources
	Caps        caps
	Types       []string
	Accepts     map[string]bool
	Renders     map[string]bool
	Detect      string
	Tools       []string
	Unsandboxed bool
	Reason      string
}

func shapeOf(p *Processor) processorShape {
	s := processorShape{
		Res: p.res, Caps: p.caps, Types: p.types, Accepts: p.accepts, Renders: p.renders, Detect: p.detect,
		Tools: slices.Sorted(maps.Keys(p.tools)), Unsandboxed: p.unsandboxed,
	}
	// How a render run is confined is a fact of the host, so only the reasons
	// that do not depend on it are compared.
	if r := p.confinement.Reason; r == reasonNoRender || r == "the sandbox is off" {
		s.Reason = r
	}
	return s
}

// shapeFields are the fields of a Processor shapeOf compares or that are state
// of a running Processor rather than part of how it builds a run.
var shapeFields = map[string]string{
	"dir": "the scratch directory", "res": "compared", "caps": "compared", "types": "compared",
	"accepts": "compared", "renders": "compared", "confinement": "compared where the host does not decide it",
	"detect": "compared", "tools": "compared by name; the argv Processor's paths do not exist", "unsandboxed": "compared",
	"start": "the runner the test chooses", "run": "set from runTool by both", "openInput": "set by both",
	"compose": "set from composeFrames by both", "mu": "state", "closed": "state", "opening": "state",
	"sources": "state", "docs": "state",
}

// The fake Processor is compared with the real one, so that an argv recorded
// from a fake is the argv a real Processor gives.
func TestArgvProcessorMatchesReal(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	for _, f := range reflect.VisibleFields(reflect.TypeFor[Processor]()) {
		if _, ok := shapeFields[f.Name]; !ok {
			t.Errorf("Processor.%s is not accounted for in shapeFields: say whether the argv Processor must match it", f.Name)
		}
	}
	for name, cfg := range map[string]Config{
		"zero":          {},
		"pdf":           pdfConfig,
		"svg":           svgConfig,
		"unsandboxed":   {Unsandboxed: true},
		"unsandboxedPD": {Unsandboxed: true, RenderTypes: []string{"application/pdf"}},
		"types":         {Types: []string{"image/png", "video/mp4"}},
		"limits":        {Resources: Resources{Disk: 1 << 30, Threads: 3}, Caps: measuredCapsConfig},
	} {
		t.Run(name, func(t *testing.T) {
			cfg.Dir = t.TempDir()
			real, err := NewProcessor(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = real.Close() }()
			fake := argvProcessor(t, cfg, nil)
			if got, want := shapeOf(fake), shapeOf(real); !reflect.DeepEqual(got, want) {
				t.Errorf("fake = %+v\nreal = %+v", got, want)
			}
		})
	}
}
