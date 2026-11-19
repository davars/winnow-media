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

// argvProcessor returns a Processor that starts no process. Its settings come
// from deriveProcessor, as a real one's do; it adds a scratch directory like a
// real one's and start as its runner, and leaves out the sandbox and
// ImageMagick policy checks and the lookup of the tools. A run reaches start
// with the argv a real Processor would give it.
func argvProcessor(t testing.TB, cfg Config, start runner) *Processor {
	t.Helper()
	d, err := deriveProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{}
	for _, name := range d.toolNames {
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
	p := &Processor{openInput: openInputFile, dir: dir, res: d.res, caps: d.caps, types: d.types, accepts: d.accepts, renders: d.renders, detect: d.detect, tools: tools, start: start, unsandboxed: cfg.Unsandboxed}
	p.run = p.runTool
	p.compose = p.composeFrames
	switch {
	case len(d.renders) == 0:
		p.confinement.Reason = reasonNoRender
	case cfg.Unsandboxed:
		p.confinement.Reason = "the sandbox is off"
	default:
		p.confinement = Confinement{Confined: true, ABI: 1}
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// argvFields says where each field of a Processor comes from in an
// argvProcessor. A new field must be added here, which is the moment to decide
// whether argvProcessor has to set it.
var argvFields = map[string]string{
	"res": "deriveProcessor", "caps": "deriveProcessor", "types": "deriveProcessor",
	"accepts": "deriveProcessor", "renders": "deriveProcessor", "detect": "deriveProcessor",
	"tools": "deriveProcessor's names, with paths that do not exist", "unsandboxed": "Config",
	"confinement": "Config; a run's confinement is a fact of the host", "dir": "a scratch directory",
	"start": "the runner the test chooses", "run": "runTool, as in NewProcessor", "openInput": "as in NewProcessor",
	"compose": "composeFrames, as in NewProcessor", "mu": "state", "closed": "state", "opening": "state",
	"sources": "state", "docs": "state",
}

func TestArgvProcessorAccountsForEveryField(t *testing.T) {
	t.Parallel()
	for _, f := range reflect.VisibleFields(reflect.TypeFor[Processor]()) {
		if _, ok := argvFields[f.Name]; !ok {
			t.Errorf("Processor.%s is not in argvFields: say how an argvProcessor gets it", f.Name)
		}
	}
	p := argvProcessor(t, pdfConfig, nil)
	d, err := deriveProcessor(pdfConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range d.toolNames {
		if _, ok := p.tools[name]; !ok {
			t.Errorf("no tool %s", name)
		}
	}
	if !reflect.DeepEqual(p.res, d.res) || p.caps != d.caps || p.detect != d.detect || !p.renders[typePDF] {
		t.Errorf("the settings are not the derived ones: %+v", p)
	}
}

// What an argvProcessor sets beyond deriveProcessor's output is compared with
// a real Processor's where the host does not decide it.
func TestArgvProcessorMatchesRealBeyondTheDerivation(t *testing.T) {
	requireTools(t, "magick", "ffmpeg", "ffprobe", "pdfinfo", "pdftoppm")
	for name, cfg := range map[string]Config{
		"zero": {}, "pdf": pdfConfig, "svg": svgConfig,
		"unsandboxed": {Unsandboxed: true}, "unsandboxedPDF": {Unsandboxed: true, RenderTypes: []string{"application/pdf"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg.Dir = t.TempDir()
			real, err := NewProcessor(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = real.Close() }()
			fake := argvProcessor(t, cfg, nil)
			if fake.unsandboxed != real.unsandboxed {
				t.Errorf("unsandboxed = %v, real %v", fake.unsandboxed, real.unsandboxed)
			}
			if !reflect.DeepEqual(slices.Sorted(maps.Keys(fake.tools)), slices.Sorted(maps.Keys(real.tools))) {
				t.Errorf("tools = %v, real %v", fake.tools, real.tools)
			}
			// A render run's confinement is the host's; the reasons that are not are compared.
			if r := real.confinement.Reason; (r == reasonNoRender || r == "the sandbox is off") && fake.confinement.Reason != r {
				t.Errorf("confinement reason = %q, real %q", fake.confinement.Reason, r)
			}
		})
	}
}
