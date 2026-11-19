package media

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// replayStep is one tool run of a recorded call, in the order the call made
// it.
type replayStep struct {
	Tool string `json:"tool"`
	// Argv is the run's arguments, with the directories named as in a
	// replayFile and the input and output directories as in the golden argv
	// files.
	Argv   []string `json:"argv"`
	Stdout string   `json:"stdout,omitempty"`
	// Files are the files the run created or grew, which a later step may
	// read, rename or list.
	Files []replayFile `json:"files,omitempty"`
}

// replayFile is a file a run wrote. Its path starts with $IN, the directory
// of the call's input, $OUT, the directory the call writes into, or $RUN, the
// directory of a render run. Its name, size and first bytes are kept, which is
// all the library reads of the files the goldens' runs write: it takes the
// size of an image from its header.
type replayFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Head []byte `json:"head,omitempty"`
}

// replayHead is how many leading bytes of each file a recording keeps: enough
// for the headers and palettes the library decodes.
const replayHead = 1024

// replayDir is where the recorded runs of the golden checks are kept.
const replayDir = "testdata/golden-replay"

func replayPath(name string) string { return filepath.Join(replayDir, name+".json") }

// replayed is the runs of one golden check, played back by an argv Processor
// or recorded from a real one.
type replayed struct {
	t    *testing.T
	name string
	p    *Processor
	// out is the directory the call writes into; the test sets it before the
	// call.
	out string

	mu    sync.Mutex
	steps []replayStep
	pos   int
}

// goldenProcessor returns a Processor for the golden check name and a recorder
// of its tool runs, including those of an external compositor. Under -short
// the Processor is a fake that plays back the runs recorded in
// testdata/golden-replay and starts nothing. Otherwise it runs the real tools,
// and the runs are compared with that recording, or with -update replace it.
func goldenProcessor(t *testing.T, name string, cfg Config) (*Processor, *recorder, *replayed) {
	t.Helper()
	g := &replayed{t: t, name: name}
	if testing.Short() {
		if *update {
			t.Fatal("-update needs the real tools; run without -short")
		}
		data, err := os.ReadFile(replayPath(name))
		if err != nil {
			t.Fatalf("%v (run the full tests with -update to create it)", err)
		}
		if err := json.Unmarshal(data, &g.steps); err != nil {
			t.Fatalf("%s: %v", replayPath(name), err)
		}
		g.p = argvProcessor(t, cfg, compositeInProcess(g.play))
		t.Cleanup(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if !t.Failed() && g.pos < len(g.steps) {
				t.Errorf("%s: %d of the %d recorded runs were not made; the call has gone another way", name, len(g.steps)-g.pos, len(g.steps))
			}
		})
	} else {
		g.p = requireProcessorWith(t, cfg)
		g.p.start = g.tap(g.p.start)
		t.Cleanup(g.finish)
	}
	return g.p, recordSource(g.p), g
}

// finish compares the runs a real Processor made with the recording, or
// replaces it under -update.
func (g *replayed) finish() {
	if g.t.Failed() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	got, err := json.MarshalIndent(g.steps, "", "\t")
	if err != nil {
		g.t.Error(err)
		return
	}
	got = append(got, '\n')
	path := replayPath(g.name)
	if *update {
		if err := os.MkdirAll(replayDir, 0o755); err != nil {
			g.t.Error(err)
		} else if err := os.WriteFile(path, got, 0o644); err != nil {
			g.t.Error(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		g.t.Errorf("%v (run with -update to create it)", err)
		return
	}
	var stored []replayStep
	if err := json.Unmarshal(want, &stored); err != nil {
		g.t.Errorf("%s: %v", path, err)
		return
	}
	if !equalSteps(stored, g.steps) {
		g.t.Errorf("the tool runs differ from %s, which the -short run plays back; run with -update:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// equalSteps compares what a replay needs: the tool, its arguments, its
// standard output and the names of the files it wrote. Standard error, the contents of encoded
// files and the pixel signatures an encode reports vary with the encoder's
// build and the host, since the clips of a recording are encoded by the tools
// under test.
func equalSteps(a, b []replayStep) bool {
	return slices.EqualFunc(a, b, func(x, y replayStep) bool {
		return x.Tool == y.Tool && slices.Equal(x.Argv, y.Argv) && maskSignatures(x.Stdout) == maskSignatures(y.Stdout) &&
			slices.EqualFunc(x.Files, y.Files, func(f, g replayFile) bool { return f.Path == g.Path })
	})
}

var signaturePrefix = regexp.MustCompile(`(?m)^[0-9a-f]{64}\|`)

func maskSignatures(stdout string) string {
	return signaturePrefix.ReplaceAllString(stdout, "SIGNATURE|")
}

// places names the directories a run's files are recorded relative to.
func (g *replayed) places(spec runSpec) map[string]string {
	places := map[string]string{}
	if g.out != "" {
		places["$OUT"] = g.out
	}
	if spec.Dir != g.p.dir && spec.Dir != g.out {
		places["$RUN"] = spec.Dir
	}
	if ins, _ := filepath.Glob(filepath.Join(g.p.tmpDir(), "in-*")); len(ins) == 1 {
		places["$IN"] = ins[0]
	}
	return places
}

// argv returns the arguments of spec as a recording keeps them.
func (g *replayed) argv(spec runSpec) []string {
	out := make([]string, len(spec.Args))
	for i, arg := range spec.Args {
		arg = neutralLinkDir.ReplaceAllLiteralString(arg, "$IN")
		arg = strings.ReplaceAll(arg, "$IN/input", inputFDPath)
		if spec.Dir != g.p.dir && spec.Dir != g.out {
			arg = strings.ReplaceAll(arg, spec.Dir, "$RUN")
		}
		if g.out != "" {
			arg = strings.ReplaceAll(arg, g.out, "$OUT")
		}
		out[i] = arg
	}
	return out
}

// snapshot returns every file under each place, by recorded path.
func snapshot(places map[string]string) map[string]replayFile {
	out := map[string]replayFile{}
	for place, dir := range places {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(dir, path)
			name := place + "/" + filepath.ToSlash(rel)
			out[name] = replayFile{Path: name, Size: info.Size()}
			if f, err := os.Open(path); err == nil {
				head := make([]byte, replayHead)
				n, _ := io.ReadFull(f, head)
				_ = f.Close()
				out[name] = replayFile{Path: name, Size: info.Size(), Head: head[:n]}
			}
			return nil
		})
	}
	return out
}

// tap returns start recording each run.
func (g *replayed) tap(start runner) runner {
	return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
		if isCompositor(spec) {
			return start(ctx, spec, stdout, stderr)
		}
		places := g.places(spec)
		before := snapshot(places)
		var out bytes.Buffer
		res, err := start(ctx, spec, io.MultiWriter(stdout, &out), stderr)
		after := snapshot(places)
		step := replayStep{Tool: filepath.Base(spec.Tool), Argv: g.argv(spec), Stdout: out.String()}
		for _, path := range slices.Sorted(maps.Keys(after)) {
			if old, ok := before[path]; !ok || old.Size != after[path].Size {
				step.Files = append(step.Files, after[path])
			}
		}
		g.mu.Lock()
		g.steps = append(g.steps, step)
		g.mu.Unlock()
		return res, err
	}
}

// isCompositor reports whether spec starts the GIF compositor, which a
// recording leaves out: its frames are the library's own, not a tool's.
func isCompositor(spec runSpec) bool { return slices.Contains(spec.Env, compositorEnv+"=1") }

// compositeInProcess returns a runner that runs the GIF compositor as a call
// of the function the compositor process runs, and any other run through next.
func compositeInProcess(next runner) runner {
	return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
		if !isCompositor(spec) {
			return next(ctx, spec, stdout, stderr)
		}
		// The process reads its input as descriptor 3; here the file is
		// opened by its own path.
		args := slices.Clone(spec.Args)
		if spec.Input != nil && len(args) > 0 && args[0] == inputFDPath {
			args[0] = spec.Input.Name()
		}
		if err := runCompositor(args, stdout); err != nil {
			_, _ = io.WriteString(stderr, err.Error())
			return toolResult{Code: 1}, nil
		}
		return toolResult{}, nil
	}
}

// play is the runner of an argv Processor: it answers each run with the next
// recorded one and writes the files that one wrote. A run of another tool or
// with other arguments than the recording's is an error, since the call has
// gone another way and the recorded output is not the tool's for this run.
func (g *replayed) play(_ context.Context, spec runSpec, stdout, _ io.Writer) (toolResult, error) {
	step, err := g.next(spec)
	if err != nil {
		return toolResult{}, err
	}
	// The lock is not held here: an external compositor's output reaches its
	// consumer, which may start another run.
	_, _ = io.WriteString(stdout, step.Stdout)
	return toolResult{}, nil
}

// next takes the next recorded step for spec and writes its files.
func (g *replayed) next(spec runSpec) (replayStep, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	tool := filepath.Base(spec.Tool)
	if g.pos >= len(g.steps) {
		return replayStep{}, fmt.Errorf("%s: a run of %s after the %d recorded", g.name, tool, len(g.steps))
	}
	step := g.steps[g.pos]
	g.pos++
	if argv := g.argv(spec); step.Tool != tool || !slices.Equal(step.Argv, argv) {
		return replayStep{}, fmt.Errorf("%s: run %d differs from the recording\n got  %s %q\n want %s %q", g.name, g.pos, tool, argv, step.Tool, step.Argv)
	}
	places := g.places(spec)
	for _, f := range step.Files {
		place, rel, _ := strings.Cut(f.Path, "/")
		dir, ok := places[place]
		if !ok {
			return replayStep{}, fmt.Errorf("%s: run %d wrote %s, which has no directory", g.name, g.pos, f.Path)
		}
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return replayStep{}, err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return replayStep{}, err
		}
		_, err = file.Write(f.Head[:min(len(f.Head), int(f.Size))])
		if err = cmp.Or(err, file.Truncate(f.Size), file.Close()); err != nil {
			return replayStep{}, err
		}
	}
	return step, nil
}

// A replayed run answers only the argv it was recorded for: a probe whose
// arguments have changed must not be served the old probe's output, or a
// golden that pins only the later runs passes under -short on output the
// real tool would not give.
func TestReplayRefusesARunWithOtherArguments(t *testing.T) {
	g := &replayed{t: t, name: "transcode-hevc-mp4"}
	data, err := os.ReadFile(replayPath(g.name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &g.steps); err != nil {
		t.Fatal(err)
	}
	g.p = argvProcessor(t, Config{}, g.play)
	if _, err := g.p.run(context.Background(), nil, "ffprobe", "-version"); err == nil {
		t.Error("an ffprobe run with other arguments was answered from the recording")
	}
}
