package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/davars/winnow-media/tool"
)

// Executor runs the recipes it was constructed with. It is safe for
// concurrent use when its Env is.
type Executor struct {
	env     Env
	enabled map[string]registered
	names   []string // enabled recipe names, sorted
}

// NewExecutor returns an Executor that runs the named recipes through env.
// It fails on a name no recipe is registered under, on a repeated name, and
// when env.Run is nil. env.Stage is not used.
func NewExecutor(env Env, enabled ...string) (*Executor, error) {
	if env.Run == nil {
		return nil, errors.New("executor needs Env.Run")
	}
	x := &Executor{env: env, enabled: make(map[string]registered, len(enabled))}
	for _, name := range enabled {
		reg, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("unknown recipe %q", name)
		}
		if _, dup := x.enabled[name]; dup {
			return nil, fmt.Errorf("recipe %q enabled twice", name)
		}
		x.enabled[name] = reg
		x.names = append(x.names, name)
	}
	slices.Sort(x.names)
	return x, nil
}

// Request is one unit of work for Execute.
type Request struct {
	// Recipe is the name of an enabled recipe.
	Recipe string
	// Params are the recipe's parameters by name. Omitted ones take their
	// declared defaults.
	Params map[string]string
	// InputPath is one file the caller has staged. Execute only reads it.
	InputPath string
	// InputType is the media type the caller asserts for the input; it must
	// be one the recipe accepts and selects the decoder.
	InputType string
	// OutputDir is an existing, empty directory the caller owns.
	OutputDir string
}

// OutputFile describes one file Execute wrote.
type OutputFile struct {
	Name      string // relative to Request.OutputDir
	SHA256    string // lower-case hex
	Size      int64
	MediaType string
	Width     int
	Height    int
	// SourceFrame is the frame of the input the file shows; 0 for a still.
	SourceFrame  int
	SourceTimeMS int64
}

// Accounting is what one Execute call consumed.
type Accounting struct {
	// Runs is the number of tool runs.
	Runs int
	// Usage is the CPU time summed over the runs and the largest peak RSS of
	// any one run. It is nil when any run reported no usage, so a partial
	// total is never presented as a whole.
	Usage    *Usage
	BytesIn  int64 // size of the input file
	BytesOut int64 // total size of the output files
}

// Response is the result of a successful Execute.
type Response struct {
	Files      []OutputFile
	Accounting Accounting
}

// Execute runs one recipe over one staged input and writes the outputs into
// Request.OutputDir, naming the files itself. The request is validated before
// any tool runs: the recipe must be enabled, every parameter known and in
// range, the input type accepted, and the output directory empty. Execute
// moves no bytes beyond reading the input and writing the outputs.
//
// On error it removes everything it wrote, leaving the output directory as
// it found it. Files lists the outputs in recipe order.
func (x *Executor) Execute(ctx context.Context, req Request) (Response, error) {
	reg, ok := x.enabled[req.Recipe]
	if !ok {
		if _, known := registry[req.Recipe]; known {
			return Response{}, fmt.Errorf("recipe %q is not enabled", req.Recipe)
		}
		return Response{}, fmt.Errorf("unknown recipe %q", req.Recipe)
	}
	decl := reg.recipe()
	params, err := validateParams(decl.Params, req.Params)
	if err != nil {
		return Response{}, fmt.Errorf("recipe %q: %w", decl.Name, err)
	}
	inputType, err := resolveInputType(req)
	if err != nil {
		return Response{}, err
	}
	decoder, ok := decoderFor(decl, inputType)
	if !ok {
		return Response{}, fmt.Errorf("recipe %q does not accept input type %q", decl.Name, inputType)
	}
	info, err := os.Stat(req.InputPath)
	if err != nil {
		return Response{}, fmt.Errorf("input: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Response{}, fmt.Errorf("input %s is not a regular file", req.InputPath)
	}
	if err := requireEmptyDir(req.OutputDir); err != nil {
		return Response{}, err
	}

	m := &meter{}
	env := Env{Run: m.wrap(x.env.Run), Stage: x.env.Stage}
	files, err := reg.run(ctx, env, runInput{
		inputPath: req.InputPath,
		decoder:   decoder,
		outputDir: req.OutputDir,
		params:    params,
	})
	if err == nil {
		err = completeFiles(req.OutputDir, files)
	}
	if err != nil {
		clearDir(req.OutputDir)
		return Response{}, fmt.Errorf("recipe %q: %w", decl.Name, err)
	}

	acct := m.accounting()
	acct.BytesIn = info.Size()
	for _, f := range files {
		acct.BytesOut += f.Size
	}
	return Response{Files: files, Accounting: acct}, nil
}

// resolveInputType is the one place the input's media type is decided. It
// returns the caller's claim; content sniffing replaces it here.
func resolveInputType(req Request) (string, error) {
	if req.InputType == "" {
		return "", errors.New("request has no input type")
	}
	return req.InputType, nil
}

func decoderFor(r Recipe, mediaType string) (Decoder, bool) {
	for _, in := range r.Inputs {
		if in.MediaType == mediaType {
			return in.Decoder, true
		}
	}
	return Decoder{}, false
}

func requireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("output directory: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("output directory %s is not empty", dir)
	}
	return nil
}

// clearDir removes the entries of dir, which Execute found empty.
func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// completeFiles fills in the size and digest of every file a recipe named.
func completeFiles(dir string, files []OutputFile) error {
	for i := range files {
		f, err := os.Open(filepath.Join(dir, files[i].Name))
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		files[i].Size = n
		files[i].SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	return nil
}

// meter totals the usage of the runs made through a wrapped Runner.
type meter struct {
	mu         sync.Mutex
	runs       int
	cpu        time.Duration
	maxRSS     int64
	unmeasured bool
}

func (m *meter) wrap(run Runner) Runner {
	return func(ctx context.Context, name string, args ...string) (Result, error) {
		res, err := run(ctx, name, args...)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.runs++
		if res.Usage == nil {
			m.unmeasured = true
		} else {
			m.cpu += res.Usage.CPU
			m.maxRSS = max(m.maxRSS, res.Usage.MaxRSS)
		}
		return res, err
	}
}

func (m *meter) accounting() Accounting {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := Accounting{Runs: m.runs}
	if m.runs > 0 && !m.unmeasured {
		a.Usage = &Usage{CPU: m.cpu, MaxRSS: m.maxRSS}
	}
	return a
}

// Fingerprint identifies one tool the enabled recipes run.
type Fingerprint struct {
	Tool    string
	Version string // first line of the tool's version output
	// Path is the executable's path with symbolic links resolved, so a tool
	// directory of links reports the real location.
	Path string
}

// Description is what an Executor runs and with which tools.
type Description struct {
	// FlakeRevision is tool.FlakeRevision(); empty in a development build.
	FlakeRevision string
	// Engines has one entry per tool the enabled recipes use, by name.
	Engines []Fingerprint
	// Recipes are the enabled recipes' declarations, by name.
	Recipes []Recipe
}

// versionArgs are the arguments that make a tool print its version.
var versionArgs = map[string][]string{
	"magick": {"-version"},
	"ffmpeg": {"-version"},
}

// Describe reports the tools and recipes of x. It runs each tool once
// through Env.Run to read its version.
func (x *Executor) Describe(ctx context.Context) (Description, error) {
	d := Description{FlakeRevision: tool.FlakeRevision()}
	engines := map[string]bool{}
	for _, name := range x.names {
		r := x.enabled[name].recipe()
		d.Recipes = append(d.Recipes, r)
		for _, e := range r.Engines {
			engines[e] = true
		}
	}
	names := make([]string, 0, len(engines))
	for e := range engines {
		names = append(names, e)
	}
	slices.Sort(names)
	for _, name := range names {
		args, ok := versionArgs[name]
		if !ok {
			return Description{}, fmt.Errorf("no version command for tool %q", name)
		}
		res, err := x.env.Run(ctx, name, args...)
		if err != nil {
			return Description{}, fmt.Errorf("%s version: %w", name, err)
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\n")
		path, err := tool.Resolve(name)
		if err != nil {
			return Description{}, err
		}
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		} else {
			return Description{}, fmt.Errorf("resolving %s: %w", path, err)
		}
		d.Engines = append(d.Engines, Fingerprint{Tool: name, Version: strings.TrimSpace(line), Path: path})
	}
	return d, nil
}
