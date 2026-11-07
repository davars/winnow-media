package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const mib = 1 << 20

// peaks are the maxima one sampler saw, in bytes.
type peaks struct {
	Anon     int64 // RssAnon summed over our descendants
	RSS      int64 // RssAnon+RssFile+RssShmem summed over our descendants
	CgAnon   int64 // anon in memory.stat of our cgroup
	CgFileMp int64 // file_mapped in memory.stat
	CgCur    int64 // memory.current
}

// sampler polls /proc and the cgroup while one operation runs.
type sampler struct {
	cg     string
	stop   chan struct{}
	done   sync.WaitGroup
	mu     sync.Mutex
	p      peaks
	oomPre int64
	pkPre  int64
}

func cgroupDir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			d := "/sys/fs/cgroup" + strings.TrimSpace(rest)
			if _, err := os.Stat(d + "/memory.current"); err == nil {
				return d
			}
		}
	}
	if _, err := os.Stat("/sys/fs/cgroup/memory.current"); err == nil {
		return "/sys/fs/cgroup"
	}
	return ""
}

func readInt(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return -1
	}
	return v
}

func readKV(path, key string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			return v
		}
	}
	return -1
}

func newSampler() *sampler {
	s := &sampler{cg: cgroupDir(), stop: make(chan struct{})}
	if s.cg != "" {
		s.oomPre = readKV(s.cg+"/memory.events", "oom_kill")
		s.pkPre = readInt(s.cg + "/memory.peak")
	}
	return s
}

func (s *sampler) start() {
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		for {
			s.sample()
			select {
			case <-s.stop:
				s.sample()
				return
			case <-t.C:
			}
		}
	}()
}

func (s *sampler) finish() peaks {
	close(s.stop)
	s.done.Wait()
	return s.p
}

func (s *sampler) sample() {
	var anon, rss int64
	for _, pid := range descendants(os.Getpid()) {
		a, f, sh := statusRSS(pid)
		anon += a
		rss += a + f + sh
	}
	s.p.Anon = max(s.p.Anon, anon)
	s.p.RSS = max(s.p.RSS, rss)
	if s.cg != "" {
		s.p.CgAnon = max(s.p.CgAnon, readKV(s.cg+"/memory.stat", "anon"))
		s.p.CgFileMp = max(s.p.CgFileMp, readKV(s.cg+"/memory.stat", "file_mapped"))
		s.p.CgCur = max(s.p.CgCur, readInt(s.cg+"/memory.current"))
	}
}

// descendants returns the pids below root, from the ppid of every process.
func descendants(root int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		str := string(b)
		i := strings.LastIndexByte(str, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(str[i+1:])
		if len(f) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		children[ppid] = append(children[ppid], pid)
	}
	var out []int
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// statusRSS returns RssAnon, RssFile and RssShmem of a pid, in bytes.
func statusRSS(pid int) (anon, file, shmem int64) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "RssAnon:":
			anon = v << 10
		case "RssFile:":
			file = v << 10
		case "RssShmem:":
			shmem = v << 10
		}
	}
	return
}
