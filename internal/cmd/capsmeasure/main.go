// Command capsmeasure measures what the fused still encode costs per source
// format and size, to set the still caps from evidence.
//
//	capsmeasure list                          the case names
//	capsmeasure gen <dir> <case>...           write the inputs
//	capsmeasure run <dir> <case> <output>     one measured encode, JSON on stdout
//
// An output is webp2048 (EncodeRung at the face_source ceiling), jpeg2576
// (the Processor's default MaxEdge) or jpeg2576+hint (the same with
// -define jpeg:size at the 2x margin).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/davars/winnow-media/media"
)

type result struct {
	Case      string  `json:"case"`
	Output    string  `json:"output"`
	SrcW      int     `json:"src_w"`
	SrcH      int     `json:"src_h"`
	MP        float64 `json:"mp"`
	FileBytes int64   `json:"file_bytes"`
	Hint      string  `json:"hint,omitempty"`
	OutW      int     `json:"out_w,omitempty"`
	OutH      int     `json:"out_h,omitempty"`
	Error     string  `json:"error,omitempty"`
	OOMKills  int64   `json:"oom_kills"`

	WallS      float64 `json:"wall_s"`
	CPUS       float64 `json:"cpu_s"`
	RUMaxRSSMB float64 `json:"ru_maxrss_mib"`
	AnonMB     float64 `json:"anon_peak_mib"`
	RSSMB      float64 `json:"rss_peak_mib"`
	CgAnonMB   float64 `json:"cg_anon_peak_mib"`
	CgCurMB    float64 `json:"cg_current_peak_mib"`
	CgPeakMB   float64 `json:"cg_memory_peak_mib"`
	CgPeakNew  bool    `json:"cg_memory_peak_rose"`

	ProbeWallS float64 `json:"probe_wall_s"`
	ProbeAnon  float64 `json:"probe_anon_peak_mib"`

	GOARCH string `json:"goarch"`
}

func main() {
	media.Init()
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "list":
		for _, c := range matrix() {
			fmt.Println(c)
		}
	case "gen":
		if len(os.Args) < 4 {
			usage()
		}
		cases := os.Args[3:]
		if len(cases) == 1 && cases[0] == "all" {
			cases = matrix()
		}
		if err := genAll(ctx, os.Args[2], cases); err != nil {
			fatal(err)
		}
	case "run":
		if len(os.Args) != 5 {
			usage()
		}
		r := run(ctx, os.Args[2], os.Args[3], os.Args[4])
		line, _ := json.Marshal(r)
		fmt.Println(string(line))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: capsmeasure list | gen <dir> <case>...|all | run <dir> <case> <output>")
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "capsmeasure:", err)
	os.Exit(1)
}

func mb(v int64) float64 { return float64(v) / mib }

func run(ctx context.Context, dir, name, output string) (r result) {
	r = result{Case: name, Output: output, GOARCH: runtime.GOARCH}
	f, s, err := parseCase(name)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.SrcW, r.SrcH, r.MP = s.w, s.h, s.mp()
	dir, err = filepath.Abs(dir)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	path := filepath.Join(dir, name+f.ext)
	info, err := os.Stat(path)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.FileBytes = info.Size()

	p, err := media.NewCapsProcessor(ctx)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer func() { _ = p.Close() }()
	stage, err := os.MkdirTemp("", "caps-stage-")
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer func() { _ = os.RemoveAll(stage) }()

	// usage accumulates what every tool run of the phase consumed.
	var cpu time.Duration
	var maxRSS int64
	base := p.CapsEnv(stage)
	env := media.Env{Stage: base.Stage, Run: func(ctx context.Context, tool string, args ...string) (media.Result, error) {
		res, err := base.Run(ctx, tool, args...)
		if res.Usage != nil {
			cpu += res.Usage.CPU
			maxRSS = max(maxRSS, res.Usage.MaxRSS)
		}
		return res, err
	}}

	// The header read the pipeline does before the encode, measured apart.
	probe := newSampler()
	probe.start()
	t0 := time.Now()
	frames, err := env.ProbeFrames(ctx, path, "")
	r.ProbeWallS = time.Since(t0).Seconds()
	pk := probe.finish()
	r.ProbeAnon = mb(pk.Anon)
	if err != nil {
		r.Error = "probe: " + err.Error()
		return r
	}
	cpu, maxRSS = 0, 0
	profiled := media.AnyProfiled(frames)

	var hint string
	if output == "jpeg2576+hint" {
		_, hint = media.CapsHint(s.w, s.h, 2576, 3_750_000, 2)
		r.Hint = hint
	}

	smp := newSampler()
	smp.start()
	start := time.Now()
	switch output {
	case "webp2048":
		var out string
		var cleanup func()
		out, cleanup, r.OutW, r.OutH, _, err = env.EncodeRung(ctx, path, "", false,
			media.Rung{CeilingPx: 2048, MaxFrames: 1}, frames, "", "", 80)
		if err == nil {
			cleanup()
			_ = out
		}
	case "jpeg2576", "jpeg2576+hint":
		r.OutW, r.OutH, err = env.CapsJPEGStill(ctx, f.coder, path, profiled, hint, 2576)
	default:
		err = fmt.Errorf("unknown output %q", output)
	}
	r.WallS = time.Since(start).Seconds()
	pk = smp.finish()
	if err != nil {
		r.Error = err.Error()
		if len(r.Error) > 400 {
			r.Error = r.Error[:400]
		}
	}
	r.CPUS = cpu.Seconds()
	r.RUMaxRSSMB = mb(maxRSS)
	r.AnonMB, r.RSSMB = mb(pk.Anon), mb(pk.RSS)
	r.CgAnonMB, r.CgCurMB = mb(pk.CgAnon), mb(pk.CgCur)
	if smp.cg != "" {
		r.OOMKills = readKV(smp.cg+"/memory.events", "oom_kill") - smp.oomPre
		peak := readInt(smp.cg + "/memory.peak")
		r.CgPeakMB = mb(peak)
		r.CgPeakNew = peak > smp.pkPre
	}
	return r
}
