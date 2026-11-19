//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// specRecorder wraps a runner and keeps the spec of each run.
type specRecorder struct {
	mu    sync.Mutex
	specs []runSpec
}

func (r *specRecorder) wrap(next runner) runner {
	return func(ctx context.Context, spec runSpec, stdout, stderr io.Writer) (toolResult, error) {
		r.mu.Lock()
		r.specs = append(r.specs, spec)
		r.mu.Unlock()
		return next(ctx, spec, stdout, stderr)
	}
}

func (r *specRecorder) last() runSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.specs[len(r.specs)-1]
}

// A transcode runs under its own limits, and is confined where render runs
// are, with its directory the one it may write; without the call's limits it
// has the Processor's.
func TestTranscodeRunsUnderItsLimitsAndConfinement(t *testing.T) {
	env := requireFFmpeg(t)
	input := avClip(t, env)
	for _, render := range []bool{true, false} {
		cfg := Config{Dir: t.TempDir()}
		if render {
			cfg.RenderTypes = RenderTypes()
		}
		var rec specRecorder
		p, err := newProcessor(context.Background(), cfg, rec.wrap)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		src := openVideo(t, p, input)

		out := t.TempDir()
		tr := Transcode{Video: SVTAV1{Preset: 12, CRF: 40}, Container: WebM{}, Limits: TranscodeLimits{
			CPUTime: 600 * time.Second, AddressSpace: 24 << 30, OutputBytes: 3 << 30,
		}}
		if _, _, err := src.Transcode(context.Background(), out, tr); err != nil {
			t.Fatal(err)
		}
		spec := rec.last()
		want := rlimits{CPU: 600, Address: 24 << 30, FileSize: 3 << 30}
		if spec.Limits == nil || *spec.Limits != want {
			t.Errorf("render %v: limits = %+v, want %+v", render, spec.Limits, want)
		}
		if !slices.Contains(spec.Env, "SVT_LOG=1") {
			t.Errorf("render %v: environment %q, want SVT-AV1's log at errors only", render, spec.Env)
		}
		if spec.Dir != out {
			t.Errorf("render %v: the run works in %s, want the output directory %s", render, spec.Dir, out)
		}
		switch {
		case render && p.Confinement().Confined:
			if spec.Confine == nil || !slices.Equal(spec.Confine.Write, []string{out, "/dev/null"}) || !spec.Confine.Input ||
				!slices.Contains(spec.Confine.Read, "/sys/devices/system/node") {
				t.Errorf("confinement = %+v, want writes to the output directory alone, the input and the NUMA topology", spec.Confine)
			}
		case spec.Confine != nil:
			t.Errorf("render %v: confined with %+v where render runs are not", render, spec.Confine)
		}

		if _, _, err := src.Transcode(context.Background(), t.TempDir(), Transcode{Video: SVTAV1{Preset: 12, CRF: 40}, Container: WebM{}}); err != nil {
			t.Fatal(err)
		}
		if got, want := rec.last().Limits, rlimitsFor(p.res); got == nil || *got != want {
			t.Errorf("render %v: default limits = %+v, want the Processor's %+v", render, got, want)
		}
	}
}

// The sandbox ends a run at the call's CPU time and output size, with
// ErrTimeLimit and ErrTooLarge, and the directory is left empty.
func TestTranscodeLimitsAreEnforced(t *testing.T) {
	env := requireFFmpeg(t)
	p := requireProcessor(t)
	long := writeClip(t, env, ".mp4", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=30",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	for _, c := range []struct {
		name   string
		t      Transcode
		want   error
		within time.Duration
	}{
		{"cpu time", Transcode{Video: SVTAV1{Preset: 2, CRF: 18}, Container: WebM{}, Limits: TranscodeLimits{CPUTime: time.Second}}, ErrTimeLimit, time.Minute},
		{"output bytes", Transcode{Video: X265{Preset: "ultrafast", CRF: 0}, Container: MP4{}, Limits: TranscodeLimits{OutputBytes: 64 << 10}}, ErrTooLarge, time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := openVideo(t, p, long)
			out := t.TempDir()
			start := time.Now()
			_, usage, err := src.Transcode(context.Background(), out, c.t)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if elapsed := time.Since(start); elapsed > c.within {
				t.Errorf("the call took %v", elapsed)
			}
			if usage.CPU <= 0 {
				t.Errorf("usage = %+v, want the failed run's", usage)
			}
			if entries, _ := os.ReadDir(out); len(entries) != 0 {
				t.Errorf("the call left %d entries in its directory", len(entries))
			}
		})
	}
}
