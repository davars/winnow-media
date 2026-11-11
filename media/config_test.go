package media

import (
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDefaultResourcesAreTheFormerConstants(t *testing.T) {
	t.Parallel()
	got, err := Resources{}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if want := (Resources{Memory: 1 << 30, Map: 2 << 30, Disk: 4 << 30, Threads: 4, TimeLimit: 300 * time.Second}); got != want {
		t.Errorf("defaults = %+v, want %+v", got, want)
	}
	var rules []string
	for _, r := range processorPolicy(got) {
		if r.domain == "resource" {
			rules = append(rules, r.name+"="+r.value)
		}
	}
	wantRules := []string{
		"memory=1GiB", "map=2GiB", "disk=4GiB", "area=512MP", "width=65536", "height=65536",
		"list-length=2048", "time=300", "thread=4",
	}
	if !slices.Equal(rules, wantRules) {
		t.Errorf("policy resources = %q, want %q", rules, wantRules)
	}
	wantFlags := []string{
		"-limit", "memory", "1GiB", "-limit", "map", "2GiB", "-limit", "area", "512MP",
		"-limit", "list-length", "2048", "-limit", "time", "300",
	}
	if flags := got.limitArgs(); !slices.Equal(flags, wantFlags) {
		t.Errorf("limit flags = %q, want %q", flags, wantFlags)
	}
	if l := rlimitsFor(got); l != (rlimits{CPU: 300, Address: 8 << 30, FileSize: 5 << 30}) {
		t.Errorf("rlimits = %+v, want 300 s, 8 GiB and 5 GiB", l)
	}
	if low, err := LowMemory.resources(); err != nil || low != (Resources{Memory: 512 << 20, Map: 1 << 30, Disk: 4 << 30, Threads: 4, TimeLimit: 300 * time.Second}) {
		t.Errorf("LowMemory = %+v, %v", low, err)
	}
}

func TestResourcesValidation(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]Resources{
		"negative memory": {Memory: -1},
		"negative map":    {Map: -1},
		"negative disk":   {Disk: -1},
		"negative thread": {Threads: -1},
		"negative time":   {TimeLimit: -time.Second},
		"huge threads":    {Threads: math.MaxInt32 + 1},
		"sub-second time": {TimeLimit: 500 * time.Millisecond},
		"fractional time": {TimeLimit: 1500 * time.Millisecond},
	} {
		if _, err := r.resolve(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := NewProcessor(context.Background(), Config{Resources: r, Unsandboxed: true}); err == nil {
			t.Errorf("%s: NewProcessor accepted", name)
		}
	}
	if _, err := (Resources{TimeLimit: 2 * time.Second}).resolve(); err != nil {
		t.Errorf("whole seconds refused: %v", err)
	}
}

func TestConfigDir(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	parent := t.TempDir()
	p, err := NewProcessor(context.Background(), Config{Dir: parent, Unsandboxed: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if filepath.Dir(p.dir) != parent || !strings.HasPrefix(filepath.Base(p.dir), "media-processor-") {
		t.Errorf("private directory %q is not a media-processor- directory of %q", p.dir, parent)
	}
	missing := filepath.Join(parent, "missing")
	if _, err := NewProcessor(context.Background(), Config{Dir: missing, Unsandboxed: true}); err == nil {
		t.Error("a missing Dir was accepted")
	}
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProcessor(context.Background(), Config{Dir: file, Unsandboxed: true}); err == nil {
		t.Error("a Dir that is a file was accepted")
	}
}

// Each field of Resources reaches the policy; Memory, Map and TimeLimit the
// -limit flags; Threads ffmpeg; TimeLimit the compositor's wait.
func TestResourcesReachTheirTargets(t *testing.T) {
	t.Parallel()
	cfg := Resources{Memory: 3 << 30, Map: 5 << 30, Disk: 7 << 30, Threads: 3, TimeLimit: 77 * time.Second}
	p := requireProcessorWith(t, Config{Resources: cfg, Unsandboxed: true})

	data, err := os.ReadFile(filepath.Join(p.dir, "policy.xml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"memory": "3GiB", "map": "5GiB", "disk": "7GiB", "thread": "3", "time": "77"} {
		if !strings.Contains(string(data), `name="`+name+`" value="`+want+`"`) {
			t.Errorf("policy.xml lacks %s=%s:\n%s", name, want, data)
		}
	}

	var m meter
	env := p.env(&m)
	var ran [][]string
	inner := env.Run
	env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
		ran = append(ran, append([]string{name}, args...))
		return inner(ctx, name, args...)
	}
	missing := filepath.Join(t.TempDir(), "none.webp")
	_, _, _, _ = env.IdentifyArtifact(context.Background(), missing)
	_, _ = runVideoProbe(context.Background(), env, decoder{"ffmpeg", ""}, missing, probeEntries)
	if len(ran) != 2 || ran[0][0] != "magick" || ran[1][0] != "ffprobe" {
		t.Fatalf("runs = %q", ran)
	}
	for name, want := range map[string]string{"memory": "3GiB", "map": "5GiB", "time": "77"} {
		if got := flagValue(t, ran[0], name); got != want {
			t.Errorf("magick -limit %s = %s, want %s", name, got, want)
		}
	}
	if i := slices.Index(ran[1], "-threads"); i < 0 || ran[1][i+1] != "3" {
		t.Errorf("ffprobe args %q lack -threads 3", ran[1])
	}

	if got := env.threads(); got != 3 {
		t.Errorf("Env threads = %d, want 3", got)
	}
	if got := env.consumeTimeout(); got != 77*time.Second {
		t.Errorf("compositor wait = %v, want 77s", got)
	}
}

// NewProcessor reads every limit back from "magick -list resource". It lists
// sizes to six significant digits, so a value that is not a whole number of
// the unit it picks reads back rounded up: 3000 MiB lists as 2.92969GiB and
// 3e9 bytes as 2.79397GiB, both above the value written. It also lists a week
// as "1 weeks" and a pebibyte as "1PiB".
func TestResourcesReadBackAsWritten(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	for name, r := range map[string]Resources{
		"MiB memory":   {Memory: 3000 << 20},
		"byte map":     {Map: 3_000_000_000},
		"byte disk":    {Disk: 3_000_000_000},
		"under 2 GiB":  {Map: 2<<30 - 1},
		"aligned more": {Memory: 3 << 30},
		"a week":       {TimeLimit: 7 * 24 * time.Hour},
		"a PiB disk":   {Disk: 1 << 50},
	} {
		p, err := NewProcessor(context.Background(), Config{Resources: r, Unsandboxed: true, Dir: t.TempDir()})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		_ = p.Close()
	}
}

// Raising Memory and Map must never lower the address-space limit derived
// from them: the sum must not wrap.
func TestRlimitsDoNotWrap(t *testing.T) {
	t.Parallel()
	def := rlimitsFor(defaultResources())
	for name, r := range map[string]Resources{
		"max memory and map": {Memory: math.MaxInt64, Map: math.MaxInt64},
		"max disk":           {Disk: math.MaxInt64},
	} {
		got := rlimitsFor(r.withDefaults())
		if got.Address < def.Address || got.FileSize < def.FileSize {
			t.Errorf("%s: rlimits %+v, below the defaults %+v", name, got, def)
		}
	}
}

func TestFFmpegThreadsFollowResourcesAndTheLargeSourceRule(t *testing.T) {
	t.Parallel()
	large := [2]int{4096, 2161}
	for _, c := range []struct {
		threads int
		w, h    int
		want    int
	}{
		{3, 1920, 1080, 3}, {8, 4096, 2160, 8}, {8, large[0], large[1], 2}, {3, large[0], large[1], 2}, {1, large[0], large[1], 1},
	} {
		if got := ffmpegThreadsFor(c.threads, c.w, c.h); got != c.want {
			t.Errorf("ffmpegThreadsFor(%d, %d, %d) = %d, want %d", c.threads, c.w, c.h, got, c.want)
		}
		plan := newVideoPlan(videoInfo{width: c.w, height: c.h}, region{}, encoding{maxEdge: 64, maxArea: 4096}, c.threads)
		if plan.threads != c.want {
			t.Errorf("plan threads for %d/%dx%d = %d, want %d", c.threads, c.w, c.h, plan.threads, c.want)
		}
	}
}

// The wait of the compositor of a source opened under a Processor's Env is
// the Processor's time limit.
func TestCompositorWaitIsTheTimeLimit(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	env, _ := sourceEnv(t)
	r := Resources{TimeLimit: 77 * time.Second}.withDefaults()
	env.resources = &r
	gif := filepath.Join(t.TempDir(), "x.gif")
	if err := os.WriteFile(gif, oneColourGIF(t, 8, 8, 3), 0o600); err != nil {
		t.Fatal(err)
	}
	src := openSource(t, env, gif, Hints{})
	if src.req.consumeTimeout != 77*time.Second {
		t.Errorf("consume timeout = %v, want 77s", src.req.consumeTimeout)
	}
	if got := src.req.timeoutMS(); got != "77000" {
		t.Errorf("timeoutMS = %q, want 77000", got)
	}
}

func TestUnsandboxedProcessorMatchesTheSandboxedOne(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	direct, err := NewProcessor(context.Background(), Config{Unsandboxed: true, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Close() })
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 32, MaxArea: 1024}
	src := heicFixture(t, "gradient-righttop")
	outDirect := t.TempDir()
	img, _, err := direct.Still(context.Background(), src, enc, outDirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandboxAvailable(); err != nil {
		t.Skip(err)
	}
	boxed, err := NewProcessor(context.Background(), Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = boxed.Close() })
	outBoxed := t.TempDir()
	if _, _, err := boxed.Still(context.Background(), src, enc, outBoxed); err != nil {
		t.Fatal(err)
	}
	a, b := mustRead(t, filepath.Join(outDirect, img.Name)), mustRead(t, filepath.Join(outBoxed, img.Name))
	if string(a) != string(b) {
		t.Errorf("unsandboxed output (%d bytes) differs from the sandboxed one (%d bytes)", len(a), len(b))
	}
}

// A runner that drops the environment lets ImageMagick read policies the
// Processor did not write; NewProcessor must refuse to return.
func TestProcessorRefusesARunnerThatStripsTheEnvironment(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	strip := func(next runner) runner {
		return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
			spec.Env = []string{}
			return next(ctx, spec, stdout, stderr)
		}
	}
	dir := t.TempDir()
	p, err := newProcessor(context.Background(), Config{Unsandboxed: true, Dir: dir}, strip)
	if err == nil {
		_ = p.Close()
		t.Fatal("a runner without the policy's environment was accepted")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("a refused Processor left %d entries in Dir", len(left))
	}
	if p, err := newProcessor(context.Background(), Config{Unsandboxed: true, Dir: dir}, nil); err != nil {
		t.Fatal(err)
	} else {
		_ = p.Close()
	}
}

// Usage covers the runs of its own call only: it equals what the runner
// reported for those runs, whatever ran before.
func TestUsageIsPerCall(t *testing.T) {
	t.Parallel()
	requireTools(t, "magick", "ffmpeg", "ffprobe")
	var mu sync.Mutex
	var usages []*Usage
	record := func(next runner) runner {
		return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
			res, err := next(ctx, spec, stdout, stderr)
			mu.Lock()
			usages = append(usages, res.Usage)
			mu.Unlock()
			return res, err
		}
	}
	p, err := newProcessor(context.Background(), Config{Unsandboxed: true, Dir: t.TempDir()}, record)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 64, MaxArea: 4096}
	for _, fixture := range []string{"gradient-righttop", "adobe-64x32", "gradient-righttop"} {
		mu.Lock()
		first := len(usages)
		mu.Unlock()
		_, usage, err := p.Still(context.Background(), heicFixture(t, fixture), enc, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		own := usages[first:]
		mu.Unlock()
		if len(own) == 0 {
			t.Fatal("the call ran no tool")
		}
		var want Usage
		for _, u := range own {
			want.CPU += u.CPU
			want.MaxRSS = max(want.MaxRSS, u.MaxRSS)
		}
		if usage != want {
			t.Errorf("%s: Usage = %+v, want %+v from its own %d runs", fixture, usage, want, len(own))
		}
	}
}
