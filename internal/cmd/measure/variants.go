package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/davars/winnow-media/internal/gifcomp"
	"github.com/davars/winnow-media/media"
)

// variantLimits are the extra ImageMagick limits of each variant; "go" runs
// the default limits.
var variantLimits = map[string][]string{
	"today":     nil,
	"go":        nil,
	"spill-512": {"-limit", "memory", "512MiB", "-limit", "map", "0"},
	"spill-256": {"-limit", "memory", "256MiB", "-limit", "map", "0"},
}

// variantRow is one input run under one variant, as the parent measured the
// child process: its rusage covers the child and every tool under it.
type variantRow struct {
	Name      string  `json:"name"`
	Variant   string  `json:"variant"`
	WallS     float64 `json:"wall_s"`
	CPUS      float64 `json:"cpu_s"`
	MaxRSSMiB float64 `json:"max_rss_mib"`
	ScratchMB float64 `json:"peak_scratch_mib"`
	Outputs   int     `json:"outputs"`
	Error     string  `json:"error,omitempty"`
	Load      string  `json:"load"`
}

// childResult is what the child prints on its last line of stdout.
type childResult struct {
	Outputs int    `json:"outputs"`
	Error   string `json:"error,omitempty"`
}

// runChild is the child mode: one operation over one input under one
// variant, with the output in a directory under $TMPDIR.
func runChild(ctx context.Context, op, variant, path string) {
	extra, ok := variantLimits[variant]
	if !ok {
		fatal(fmt.Errorf("unknown variant %q", variant))
	}
	media.ExtraLimitArgs = extra
	p, err := media.NewProcessor(ctx)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = p.Close() }()
	out, err := os.MkdirTemp("", "measure-out-")
	if err != nil {
		fatal(err)
	}
	var imgs []media.Image
	switch {
	case op == "video":
		imgs, _, err = p.VideoFrames(ctx, path, media.Encoding{}, 64, out)
	case variant == "go":
		imgs, _, err = p.GIFFramesComposited(ctx, path, media.Encoding{}, 64, out, composeGo(path))
	default:
		imgs, _, err = p.AnimationFrames(ctx, path, media.Encoding{}, 64, out)
	}
	res := childResult{Outputs: len(imgs)}
	if err != nil {
		res.Error = err.Error()
	}
	line, _ := json.Marshal(res)
	fmt.Println(string(line))
}

// composeGo composites the GIF in this process and writes the selected
// frames as full-canvas PNGs.
func composeGo(path string) func(selected []int, dir string) ([]string, error) {
	return func(selected []int, dir string) ([]string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var paths []string
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		err = gifcomp.Frames(data, selected, func(i int, canvas *image.NRGBA) error {
			p := filepath.Join(dir, fmt.Sprintf(".sample-%03d.png", len(paths)))
			f, err := os.Create(p)
			if err != nil {
				return err
			}
			paths = append(paths, p)
			if err := enc.Encode(f, canvas); err != nil {
				_ = f.Close()
				return err
			}
			return f.Close()
		})
		return paths, err
	}
}

// measureVariants runs each spec under each variant as a child process.
func measureVariants(ctx context.Context, dir string, specs []inputSpec, variants []string) ([]variantRow, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var rows []variantRow
	for _, s := range specs {
		path, err := filepath.Abs(filepath.Join(dir, s.file))
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(os.Stderr, "skipping %s: %v\n", s.name, err)
			continue
		}
		for _, v := range variants {
			if v == "go" && !strings.HasSuffix(s.file, ".gif") {
				continue
			}
			r := runVariant(ctx, self, dir, s, v, path)
			rows = append(rows, r)
			line, _ := json.Marshal(r)
			fmt.Println(string(line))
		}
	}
	return rows, nil
}

func runVariant(ctx context.Context, self, dir string, s inputSpec, variant, path string) variantRow {
	r := variantRow{Name: s.name, Variant: variant, Load: loadAverage()}
	scratch, err := os.MkdirTemp(dir, ".scratch-")
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	cmd := exec.CommandContext(ctx, self, "-child", "-op", s.op, "-variant", variant, path)
	cmd.Env = append(os.Environ(), "TMPDIR="+scratch)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	var peak int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			peak = max(peak, treeSize(scratch))
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	start := time.Now()
	runErr := cmd.Run()
	r.WallS = time.Since(start).Seconds()
	close(stop)
	wg.Wait()

	if u := media.UsageOf(cmd.ProcessState); u != nil {
		r.CPUS = u.CPU.Seconds()
		r.MaxRSSMiB = float64(u.MaxRSS) / (1 << 20)
	}
	r.ScratchMB = float64(peak) / (1 << 20)
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var res childResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err == nil {
		r.Outputs, r.Error = res.Outputs, res.Error
	} else if runErr != nil {
		r.Error = runErr.Error() + ": " + strings.TrimSpace(stderr.String())
	} else {
		r.Error = "child printed no result"
	}
	if runErr != nil && r.Error == "" {
		r.Error = runErr.Error()
	}
	return r
}

// treeSize totals the sizes of the files under dir.
func treeSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func loadAverage() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(b))[:3], " ")
}

func printVariantTable(rows []variantRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "input\tvariant\twall s\tcpu s\trss MiB\tscratch MiB\tout\tload\terror")
	for _, r := range rows {
		msg := strings.Join(strings.Fields(r.Error), " ")
		if len(msg) > 100 {
			msg = msg[:100] + "..."
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%.1f\t%.1f\t%.0f\t%.0f\t%d\t%s\t%s\n",
			r.Name, r.Variant, r.WallS, r.CPUS, r.MaxRSSMiB, r.ScratchMB, r.Outputs, r.Load, msg)
	}
	_ = w.Flush()
}
