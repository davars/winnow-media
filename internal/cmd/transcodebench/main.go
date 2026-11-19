// Command transcodebench runs one Source.Transcode of a file through a real
// media.Processor and prints what it cost, as one JSON line: wall time, CPU,
// peak resident memory and the output's size. It records the transcode rows
// of docs/measurements.md.
//
//	transcodebench -video x265 -preset medium -crf 18 -audio aac -bitrate 256000 \
//	    -container mp4 -faststart -threads 32 -cpu 86400s -wall 24h -as 64GiB <input> <empty-dir>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/davars/winnow-media/media"
	"github.com/davars/winnow-media/tool"
)

func main() {
	var (
		video     = flag.String("video", "x265", "svtav1 or x265")
		preset    = flag.String("preset", "medium", "the video encoder's preset")
		crf       = flag.Int("crf", 18, "the video encoder's CRF")
		audio     = flag.String("audio", "", "opus, aac or empty for none")
		bitrate   = flag.Int("bitrate", 256_000, "the audio bitrate in bits per second")
		container = flag.String("container", "mp4", "webm or mp4")
		faststart = flag.Bool("faststart", false, "MP4 FastStart")
		bitexact  = flag.Bool("bitexact", false, "the container's BitExact")
		stream    = flag.Int("stream", -1, "the video stream's index; -1 is the default choice")
		threads   = flag.Int("threads", 0, "Limits.Threads")
		cpu       = flag.Duration("cpu", 0, "Limits.CPUTime")
		wall      = flag.Duration("wall", 0, "Limits.WallTime")
		as        = flag.String("as", "0", "Limits.AddressSpace in bytes, or with a GiB suffix")
		outBytes  = flag.String("output-bytes", "0", "Limits.OutputBytes in bytes, or with a GiB suffix")
		scratch   = flag.String("scratch", "", "Config.Dir")
	)
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: transcodebench [flags] <input> <empty-dir>")
		os.Exit(2)
	}
	t := media.Transcode{Limits: media.TranscodeLimits{Threads: *threads, CPUTime: *cpu, WallTime: *wall}}
	var err error
	if t.Limits.AddressSpace, err = bytesFlag(*as); err != nil {
		fatal(err)
	}
	if t.Limits.OutputBytes, err = bytesFlag(*outBytes); err != nil {
		fatal(err)
	}
	switch *video {
	case "svtav1":
		p, err := strconv.Atoi(*preset)
		if err != nil {
			fatal(err)
		}
		t.Video = media.SVTAV1{Preset: p, CRF: *crf}
	case "x265":
		t.Video = media.X265{Preset: *preset, CRF: *crf}
	default:
		fatal(fmt.Errorf("unknown video encoder %q", *video))
	}
	switch *audio {
	case "opus":
		t.Audio = media.Opus{Bitrate: *bitrate}
	case "aac":
		t.Audio = media.AAC{Bitrate: *bitrate}
	case "":
	default:
		fatal(fmt.Errorf("unknown audio encoder %q", *audio))
	}
	switch *container {
	case "webm":
		t.Container = media.WebM{BitExact: *bitexact}
	case "mp4":
		t.Container = media.MP4{FastStart: *faststart, BitExact: *bitexact}
	default:
		fatal(fmt.Errorf("unknown container %q", *container))
	}
	if *stream >= 0 {
		t.VideoStream = stream
	}

	ctx := context.Background()
	p, err := media.NewProcessor(ctx, media.Config{Dir: *scratch, RenderTypes: media.RenderTypes()})
	if err != nil {
		fatal(err)
	}
	defer func() { _ = p.Close() }()
	src, _, err := p.Open(ctx, flag.Arg(0), media.OpenOptions{})
	if err != nil {
		fatal(err)
	}
	defer func() { _ = src.Close() }()
	start := time.Now()
	out, usage, err := src.Transcode(ctx, flag.Arg(1), t)
	row := map[string]any{
		"started":     start.UTC().Format(time.RFC3339),
		"wall_s":      time.Since(start).Seconds(),
		"tool_wall_s": out.Wall.Seconds(),
		"cpu_s":       usage.CPU.Seconds(),
		"max_rss_mib": float64(usage.MaxRSS) / (1 << 20),
		"output":      out.Name,
		"bytes":       out.Bytes,
		"confinement": p.Confinement().String(),
		"goarch":      runtime.GOARCH,
		"cpus":        runtime.NumCPU(),
	}
	if path, err := tool.Resolve("ffmpeg"); err == nil {
		row["ffmpeg"] = path
	}
	if err != nil {
		row["error"] = err.Error()
		if stderr := media.Stderr(err); len(stderr) > 0 {
			row["stderr"] = string(stderr)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(row); err != nil {
		fatal(err)
	}
	if row["error"] != nil {
		os.Exit(1)
	}
}

func bytesFlag(s string) (int64, error) {
	if n, ok := strings.CutSuffix(s, "GiB"); ok {
		v, err := strconv.ParseInt(n, 10, 64)
		return v << 30, err
	}
	return strconv.ParseInt(s, 10, 64)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
