// Command measure generates the largest legitimate inputs the media
// operations are meant to accept and records what each costs: wall time, CPU
// and peak resident memory through a real media.Processor. The caps in the
// media package are set from its output.
//
// Generation and measurement are separate steps so that inputs can be
// generated once and measured on other machines:
//
//	measure <dir>                 generate what is missing, then measure
//	measure -gen-only <dir>       only generate
//	measure -measure-only <dir>   only measure what <dir> holds
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/davars/winnow-media/media"
)

// inputSpec is one measured input: how to generate it and which operation
// to run on it.
type inputSpec struct {
	name string
	file string
	op   string // "video", "still" or "animation"
	gen  func(ctx context.Context, path, work string) error
}

type row struct {
	Name     string  `json:"name"`
	Op       string  `json:"op"`
	Bytes    int64   `json:"input_bytes"`
	Runs     int     `json:"runs"`
	WallMS   float64 `json:"wall_ms"`
	CPUMS    float64 `json:"cpu_ms"`
	MaxRSSMB float64 `json:"max_rss_mib"`
	Outputs  int     `json:"outputs"`
	Error    string  `json:"error,omitempty"`
	GOARCH   string  `json:"goarch"`
}

func main() {
	var (
		only        = flag.String("only", "", "run the input with this name")
		genOnly     = flag.Bool("gen-only", false, "generate inputs and stop")
		measureOnly = flag.Bool("measure-only", false, "measure the inputs already in the directory")
		repeat      = flag.Int("repeat", 1, "runs per input; wall time and CPU are medians")
		extra       = flag.String("extra", "", "comma-separated extra animations, each kind:frames:WxH, e.g. gif:1000:600x600")
		list        = flag.Bool("list", false, "list the input names and stop")
	)
	flag.Parse()
	if flag.NArg() != 1 || *repeat < 1 || (*genOnly && *measureOnly) {
		fmt.Fprintln(os.Stderr, "usage: measure [-only name] [-gen-only | -measure-only] [-repeat N] [-extra kind:frames:WxH,...] <dir>")
		os.Exit(2)
	}
	dir := flag.Arg(0)
	specs, err := inputs(*extra)
	if err != nil {
		fatal(err)
	}
	if *list {
		for _, s := range specs {
			fmt.Println(s.name)
		}
		return
	}
	if *only != "" {
		specs = slices.DeleteFunc(specs, func(s inputSpec) bool { return s.name != *only })
		if len(specs) == 0 {
			fatal(fmt.Errorf("no input named %q", *only))
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal(err)
	}
	ctx := context.Background()
	if !*measureOnly {
		if err := generate(ctx, dir, specs); err != nil {
			fatal(err)
		}
	}
	if *genOnly {
		return
	}
	rows, err := measure(ctx, dir, specs, *repeat)
	if err != nil {
		fatal(err)
	}
	printTable(rows)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "measure:", err)
	os.Exit(1)
}

func generate(ctx context.Context, dir string, specs []inputSpec) error {
	work, err := os.MkdirTemp(dir, ".gen-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	for _, s := range specs {
		path := filepath.Join(dir, s.file)
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(os.Stderr, "generate %s: present\n", s.name)
			continue
		}
		start := time.Now()
		partial := path + ".partial"
		if err := s.gen(ctx, partial, work); err != nil {
			_ = os.Remove(partial)
			return fmt.Errorf("generating %s: %w", s.name, err)
		}
		if err := os.Rename(partial, path); err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "generate %s: %.2f MiB in %v\n", s.name, float64(info.Size())/(1<<20), time.Since(start).Round(time.Second))
	}
	return nil
}

func measure(ctx context.Context, dir string, specs []inputSpec, repeat int) ([]row, error) {
	p, err := media.NewProcessor(ctx, media.Config{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = p.Close() }()
	var rows []row
	for _, s := range specs {
		path := filepath.Join(dir, s.file)
		info, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipping %s: %v\n", s.name, err)
			continue
		}
		r := run(ctx, p, s, path, repeat)
		r.Bytes = info.Size()
		rows = append(rows, r)
		line, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		fmt.Println(string(line))
	}
	return rows, nil
}

func run(ctx context.Context, p *media.Processor, s inputSpec, path string, repeat int) row {
	r := row{Name: s.name, Op: s.op, GOARCH: runtime.GOARCH}
	var walls, cpus []time.Duration
	var rss int64
	for range repeat {
		out, err := os.MkdirTemp("", "measure-out-")
		if err != nil {
			r.Error = err.Error()
			return r
		}
		start := time.Now()
		sel := media.Select{}
		if s.op == "video" || s.op == "animation" {
			sel.Frames = media.Even(64)
		}
		imgs, usage, err := encodeFile(ctx, p, path, sel, out)
		n := len(imgs)
		wall := time.Since(start)
		_ = os.RemoveAll(out)
		r.Runs++
		rss = max(rss, usage.MaxRSS)
		r.Outputs = n
		if err != nil {
			r.Error = err.Error()
			r.WallMS = ms(wall)
			r.CPUMS = ms(usage.CPU)
			r.MaxRSSMB = float64(rss) / (1 << 20)
			return r
		}
		walls = append(walls, wall)
		cpus = append(cpus, usage.CPU)
	}
	slices.Sort(walls)
	slices.Sort(cpus)
	r.WallMS = ms(walls[len(walls)/2])
	r.CPUMS = ms(cpus[len(cpus)/2])
	r.MaxRSSMB = float64(rss) / (1 << 20)
	return r
}

// encodeFile opens path, makes one Encode into out and closes the Source; the
// Usage covers both calls.
func encodeFile(ctx context.Context, p *media.Processor, path string, sel media.Select, out string) ([]media.Image, media.Usage, error) {
	src, usage, err := p.Open(ctx, path, media.OpenOptions{})
	if err != nil {
		return nil, usage, err
	}
	defer func() { _ = src.Close() }()
	imgs, used, err := src.Encode(ctx, out, sel, media.Encoding{})
	usage.CPU += used.CPU
	usage.MaxRSS = max(usage.MaxRSS, used.MaxRSS)
	return imgs, usage, err
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func printTable(rows []row) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "input\top\tMiB\truns\twall s\tcpu s\tcpu/wall\trss MiB\tout\terror")
	for _, r := range rows {
		ratio := 0.0
		if r.WallMS > 0 {
			ratio = r.CPUMS / r.WallMS
		}
		msg := strings.Join(strings.Fields(r.Error), " ")
		if len(msg) > 110 {
			msg = msg[:110] + "..."
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%.1f\t%d\t%.2f\t%.2f\t%.2f\t%.0f\t%d\t%s\n",
			r.Name, r.Op, float64(r.Bytes)/(1<<20), r.Runs, r.WallMS/1000, r.CPUMS/1000, ratio, r.MaxRSSMB, r.Outputs, msg)
	}
	_ = w.Flush()
}
