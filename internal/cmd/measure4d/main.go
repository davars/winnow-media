// Command measure4d runs one media operation over one file in a child
// process and records wall time, CPU, peak RSS, the container's cgroup
// memory, and per-process peak RSS sampled from /proc.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davars/winnow-media/media"
)

type result struct {
	Name        string             `json:"name"`
	Op          string             `json:"op"`
	WallS       float64            `json:"wall_s"`
	CPUS        float64            `json:"cpu_s"`
	MaxRSSMiB   float64            `json:"max_rss_mib"`
	CgroupPeak  float64            `json:"cgroup_peak_mib"`
	CgroupMax   string             `json:"cgroup_max"`
	AnonAtPeak  float64            `json:"anon_at_cur_peak_mib"`
	FileAtPeak  float64            `json:"file_at_cur_peak_mib"`
	AnonMax     float64            `json:"anon_max_mib"`
	FileMax     float64            `json:"file_max_mib"`
	CurMax      float64            `json:"cgroup_current_max_sampled_mib"`
	TreeRSSMax  float64            `json:"tree_rss_sum_max_mib"`
	ProcHWM     map[string]float64 `json:"proc_hwm_mib"`
	ScratchMiB  float64            `json:"peak_scratch_mib"`
	OOMKill     string             `json:"memory_events"`
	Outputs     int                `json:"outputs"`
	Samples     int                `json:"samples"`
	PerSampleS  float64            `json:"per_sample_s"`
	Threads     string             `json:"ffmpeg_threads_env"`
	Error       string             `json:"error,omitempty"`
	ChildSignal string             `json:"child_signal,omitempty"`
}

type childResult struct {
	Outputs int    `json:"outputs"`
	Error   string `json:"error,omitempty"`
}

var samples *int

func main() {
	media.Init()
	child := flag.Bool("child", false, "internal")
	op := flag.String("op", "", "still, anim or video")
	samples = flag.Int("samples", 64, "video samples")
	flag.Parse()
	if flag.NArg() != 1 || *op == "" {
		fmt.Fprintln(os.Stderr, "usage: measure4d -op still|anim|video <file>")
		os.Exit(2)
	}
	if *child {
		runChild(*op, flag.Arg(0))
		return
	}
	parent(*op, flag.Arg(0))
}

func runChild(op, path string) {
	ctx := context.Background()
	p, err := media.NewProcessor(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = p.Close() }()
	out, err := os.MkdirTemp("", "measure-out-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	n := 0
	switch op {
	case "video":
		var imgs []media.Image
		imgs, _, err = p.VideoFrames(ctx, path, media.Encoding{}, *samples, out)
		n = len(imgs)
	case "anim":
		var imgs []media.Image
		imgs, _, err = p.AnimationFrames(ctx, path, media.Encoding{}, 64, out)
		n = len(imgs)
	default:
		_, _, err = p.Still(ctx, path, media.Encoding{}, out)
		if err == nil {
			n = 1
		}
	}
	res := childResult{Outputs: n}
	if err != nil {
		res.Error = err.Error()
	}
	line, _ := json.Marshal(res)
	fmt.Println(string(line))
}

const mib = 1 << 20

func readInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil
}

func readStat(path string) map[string]int64 {
	m := map[string]int64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				m[f[0]] = n
			}
		}
	}
	return m
}

// procInfo reads comm, VmHWM and VmRSS (kB) of a pid.
func procInfo(pid int) (comm string, hwm, rss int64, ok bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "", 0, 0, false
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "Name:":
			comm = f[1]
		case "VmHWM:":
			hwm, _ = strconv.ParseInt(f[1], 10, 64)
		case "VmRSS:":
			rss, _ = strconv.ParseInt(f[1], 10, 64)
		}
	}
	return comm, hwm, rss, comm != ""
}

func pids() map[int]bool {
	m := map[int]bool{}
	es, _ := os.ReadDir("/proc")
	for _, e := range es {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			m[n] = true
		}
	}
	return m
}

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

func parent(op, path string) {
	self, _ := os.Executable()
	base := pids()
	scratch, err := os.MkdirTemp("", "scratch-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	cmd := exec.Command(self, "-child", "-op", op, "-samples", strconv.Itoa(*samples), path)
	cmd.Env = append(os.Environ(), "TMPDIR="+scratch)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r := result{Name: filepath.Base(path), Op: op, ProcHWM: map[string]float64{}}
	if err := cmd.Start(); err != nil {
		r.Error = err.Error()
		emit(r)
		return
	}
	childPID := cmd.Process.Pid
	done := make(chan struct{})
	fin := make(chan struct{})
	hwm := map[string]int64{}
	var treeMax, scratchMax, curMax, anonMax, fileMax, anonAt, fileAt int64
	go func() {
		defer close(fin)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		n := 0
		for {
			var sum int64
			for pid := range pids() {
				if base[pid] || pid == os.Getpid() {
					continue
				}
				comm, h, rss, ok := procInfo(pid)
				if !ok {
					continue
				}
				label := comm
				if pid == childPID {
					label = "go-processor(" + comm + ")"
				} else if comm == "measure4d" {
					label = "go-helper(" + comm + ")"
				}
				hwm[label] = max(hwm[label], h)
				sum += rss
			}
			treeMax = max(treeMax, sum)
			if cur, ok := readInt("/sys/fs/cgroup/memory.current"); ok {
				st := readStat("/sys/fs/cgroup/memory.stat")
				if cur > curMax {
					curMax, anonAt, fileAt = cur, st["anon"], st["file"]
				}
				anonMax = max(anonMax, st["anon"])
				fileMax = max(fileMax, st["file"])
			}
			if n%5 == 0 {
				scratchMax = max(scratchMax, treeSize(scratch))
			}
			n++
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	start := time.Now()
	runErr := cmd.Wait()
	r.WallS = time.Since(start).Seconds()
	r.Samples = *samples
	r.Threads = os.Getenv("MEASURE_FFMPEG_THREADS")
	r.PerSampleS = r.WallS / float64(*samples)
	close(done)
	<-fin
	if u := media.UsageOf(cmd.ProcessState); u != nil {
		r.CPUS = u.CPU.Seconds()
		r.MaxRSSMiB = float64(u.MaxRSS) / mib
	}
	if v, ok := readInt("/sys/fs/cgroup/memory.peak"); ok {
		r.CgroupPeak = float64(v) / mib
	}
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		r.CgroupMax = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.events"); err == nil {
		r.OOMKill = strings.Join(strings.Fields(string(b)), " ")
	}
	r.AnonAtPeak, r.FileAtPeak = float64(anonAt)/mib, float64(fileAt)/mib
	r.AnonMax, r.FileMax, r.CurMax = float64(anonMax)/mib, float64(fileMax)/mib, float64(curMax)/mib
	r.TreeRSSMax = float64(treeMax) / 1024
	r.ScratchMiB = float64(scratchMax) / mib
	for k, v := range hwm {
		r.ProcHWM[k] = float64(v) / 1024
	}
	if runErr != nil {
		r.ChildSignal = runErr.Error()
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var res childResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err == nil {
		r.Outputs, r.Error = res.Outputs, res.Error
	} else {
		r.Error = "child printed no result: " + strings.TrimSpace(stderr.String())
	}
	emit(r)
}

func emit(r result) {
	line, _ := json.Marshal(r)
	fmt.Println(string(line))
}
