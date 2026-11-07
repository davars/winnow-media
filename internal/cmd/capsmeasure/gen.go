package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davars/winnow-media/tool"
)

// budget is where generated lossy files stop: just under the 25 MiB input cap.
const budget = 24 << 20

type genRecord struct {
	Case    string  `json:"case"`
	File    string  `json:"file"`
	Bytes   int64   `json:"bytes"`
	Quality int     `json:"quality,omitempty"`
	Over    bool    `json:"over_input_cap"`
	GenSecs float64 `json:"gen_s"`
}

func magick(ctx context.Context, args ...string) error {
	path, err := tool.Resolve("magick")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("magick %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// genAll writes the inputs of the named cases into dir, building one master
// per size and encoding every format from it.
func genAll(ctx context.Context, dir string, cases []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	icc := filepath.Join(dir, ".profile.icc")
	raw, err := base64.StdEncoding.DecodeString(iccProfile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(icc, raw, 0o600); err != nil {
		return err
	}
	bySize := map[string][]string{}
	var order []string
	for _, c := range cases {
		_, s, err := parseCase(c)
		if err != nil {
			return err
		}
		if _, ok := bySize[s.key]; !ok {
			order = append(order, s.key)
		}
		bySize[s.key] = append(bySize[s.key], c)
	}
	for _, key := range order {
		s, _ := sizeByKey(key)
		master := filepath.Join(dir, ".master-"+key+".miff")
		start := time.Now()
		// Fractal detail plus noise keeps the compressed size in the class
		// of a camera's output.
		if err := magick(ctx, "-size", fmt.Sprintf("%dx%d", s.w, s.h), "plasma:fractal",
			"-attenuate", "0.4", "+noise", "Gaussian", "-profile", icc, "-depth", "16", master); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "master %s %dx%d in %v\n", key, s.w, s.h, time.Since(start).Round(time.Second))
		for _, c := range bySize[key] {
			f, _, _ := parseCase(c)
			rec, err := genOne(ctx, dir, master, c, f)
			if err != nil {
				_ = os.Remove(master)
				return err
			}
			line, _ := json.Marshal(rec)
			fmt.Println(string(line))
		}
		_ = os.Remove(master)
	}
	return nil
}

func genOne(ctx context.Context, dir, master, name string, f format) (genRecord, error) {
	path := filepath.Join(dir, name+f.ext)
	rec := genRecord{Case: name, File: filepath.Base(path)}
	start := time.Now()
	qualities := f.ladder
	if len(qualities) == 0 {
		qualities = []int{0}
	}
	for _, q := range qualities {
		args := append([]string{master}, f.args...)
		if q > 0 {
			args = append(args, "-quality", strconv.Itoa(q))
		}
		args = append(args, f.coder+":"+path+".partial")
		if err := magick(ctx, args...); err != nil {
			return rec, err
		}
		info, err := os.Stat(path + ".partial")
		if err != nil {
			return rec, err
		}
		rec.Bytes, rec.Quality = info.Size(), q
		if rec.Bytes <= budget {
			break
		}
	}
	rec.Over = rec.Bytes > 25<<20
	rec.GenSecs = time.Since(start).Seconds()
	return rec, os.Rename(path+".partial", path)
}
