package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var videoSpec = spec{
	inputs: []acceptedInput{
		{typeMP4, decoder{"ffmpeg", "mov"}},
		{typeMOV, decoder{"ffmpeg", "mov"}},
		{typeWebM, decoder{"ffmpeg", "matroska"}},
		{typeMKV, decoder{"ffmpeg", "matroska"}},
	},
	tools:  []string{"ffprobe", "ffmpeg", "magick"},
	reads:  append([]string{"png"}, outputCoders...),
	writes: outputCoders,
}

// hdrToSDRFilter linearizes HDR video, converts its primaries to BT.709,
// tone-maps it and returns it to BT.709 transfer and matrix. The 203 nit
// reference white is the BT.2408 level for HDR graphic white.
const hdrToSDRFilter = "zscale=t=linear:npl=203,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"

// isHDRTransfer reports whether a colour transfer characteristic names HLG or PQ.
func isHDRTransfer(trc string) bool {
	return trc == "arib-std-b67" || trc == "smpte2084"
}

// videoInfo is what the probe reads about the stream samples are taken from.
type videoInfo struct {
	stream        int
	width, height int // as stored, before rotation
	rotation      int // display rotation in degrees
	durationMS    int64
	frames        int // 0 when unknown
	transfer      string
	streams       int // streams in the container, of every kind
}

// upright returns the dimensions the decoded frames have, which ffmpeg
// rotates to match the display matrix.
func (v videoInfo) upright() (int, int) {
	if v.rotation%180 != 0 {
		return v.height, v.width
	}
	return v.width, v.height
}

type probeJSON struct {
	Streams []struct {
		Index         int    `json:"index"`
		CodecType     string `json:"codec_type"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		Duration      string `json:"duration"`
		NbFrames      string `json:"nb_frames"`
		RFrameRate    string `json:"r_frame_rate"`
		ColorTransfer string `json:"color_transfer"`
		Disposition   struct {
			Default     int `json:"default"`
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		SideData []struct {
			Rotation *float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// demuxerOptions are the input options set on a demuxer by name. An option
// is listed only for demuxers that define it: ffmpeg refuses an input option
// the demuxer does not know.
var demuxerOptions = map[string][]string{
	"mov": {"-enable_drefs", "0"},
}

// inputArgs returns the arguments that open path in ffprobe or ffmpeg: only
// the file protocol, the demuxer's options and the demuxer itself, so nothing
// is chosen from content and no nested input is opened over the network.
func inputArgs(dec decoder, path string) []string {
	args := []string{"-protocol_whitelist", "file"}
	args = append(args, demuxerOptions[dec.name]...)
	return append(args, "-f", dec.name, "-i", path)
}

func probeVideo(ctx context.Context, env Env, dec decoder, path string) (videoInfo, error) {
	args := append([]string{
		"-v", "error", "-of", "json",
		"-show_entries", "stream=index,codec_type,width,height,duration,nb_frames,r_frame_rate,color_transfer" +
			":stream_disposition=default,attached_pic:stream_side_data=rotation:format=duration",
	},
		inputArgs(dec, path)...)
	res, err := env.Run(ctx, "ffprobe", args...)
	if err != nil {
		return videoInfo{}, fmt.Errorf("probing video: %w", err)
	}
	return parseVideoProbe(res.Stdout)
}

func parseVideoProbe(out []byte) (videoInfo, error) {
	var p probeJSON
	if err := json.Unmarshal(out, &p); err != nil {
		return videoInfo{}, fmt.Errorf("parsing probe output: %w", err)
	}
	pick := -1
	for i, s := range p.Streams {
		if s.CodecType != "video" || s.Disposition.AttachedPic != 0 {
			continue
		}
		if s.Disposition.Default == 1 {
			pick = i
			break
		}
		if pick < 0 {
			pick = i
		}
	}
	if pick >= 0 {
		s := p.Streams[pick]
		if s.Width < 1 || s.Height < 1 {
			return videoInfo{}, fmt.Errorf("video stream reports size %dx%d", s.Width, s.Height)
		}
		v := videoInfo{stream: s.Index, width: s.Width, height: s.Height, transfer: s.ColorTransfer, streams: len(p.Streams)}
		for _, sd := range s.SideData {
			if sd.Rotation != nil {
				v.rotation = ((int(math.Round(*sd.Rotation))%360 + 360) % 360)
			}
		}
		seconds, err := strconv.ParseFloat(s.Duration, 64)
		if err != nil || seconds <= 0 {
			seconds, err = strconv.ParseFloat(p.Format.Duration, 64)
		}
		if err == nil && seconds > 0 && !math.IsInf(seconds, 0) {
			v.durationMS = int64(math.Round(min(seconds, 1e12) * 1000))
		}
		if n, err := strconv.Atoi(s.NbFrames); err == nil && n > 0 {
			v.frames = n
		} else if rate := parseRate(s.RFrameRate); rate > 0 && v.durationMS > 0 {
			v.frames = max(1, int(math.Round(float64(v.durationMS)/1000*rate)))
		}
		return v, nil
	}
	return videoInfo{}, errors.New("input has no video stream")
}

// parseRate reads an ffprobe rational such as "30000/1001"; 0 when unusable.
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return 0
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d <= 0 || n <= 0 {
		return 0
	}
	return n / d
}

// sampleTimesMS returns the seek targets, in ms: the centres of n equal
// intervals of the duration, n being at most maxFrames and at most the
// stream's frame count when known. A target is kept inside the last frame so
// that a seek always finds a frame at or after it. Equal targets collapse.
// An unknown duration gives the single target 0.
func sampleTimesMS(v videoInfo, maxFrames int) []int64 {
	if v.durationMS <= 0 {
		return []int64{0}
	}
	n := maxFrames
	latest := v.durationMS
	if v.frames > 0 {
		n = min(n, v.frames)
		latest = max(0, v.durationMS-v.durationMS*3/(2*int64(v.frames)))
	}
	times := make([]int64, 0, n)
	for i := range n {
		t := (int64(2*i+1)*v.durationMS + int64(n)) / int64(2*n)
		t = min(t, latest)
		if len(times) == 0 || times[len(times)-1] != t {
			times = append(times, t)
		}
	}
	return times
}

// samplePrefix names the lossless intermediates inside the output directory.
const samplePrefix = ".sample-"

func (e Env) videoFrames(ctx context.Context, dec decoder, inputPath string, enc encoding, maxFrames int, dir string) ([]Image, error) {
	info, err := probeVideo(ctx, e, dec, inputPath)
	if err != nil {
		return nil, err
	}
	if err := checkVideoInfo(info); err != nil {
		return nil, err
	}
	w, h := info.upright()
	tw, th, resize := fitSize(w, h, enc.maxEdge, enc.maxArea)
	filter := ""
	if isHDRTransfer(info.transfer) {
		filter = hdrToSDRFilter
	}
	if resize {
		if filter != "" {
			filter += ","
		}
		filter += fmt.Sprintf("scale=%d:%d:flags=lanczos", tw, th)
	}

	times := sampleTimesMS(info, maxFrames)
	var scratch []string
	defer func() {
		for _, p := range scratch {
			_ = os.Remove(p)
		}
	}()
	type sample struct {
		path   string
		timeMS int64
	}
	var samples []sample
	for i, t := range times {
		path := filepath.Join(dir, fmt.Sprintf("%s%03d.png", samplePrefix, i))
		scratch = append(scratch, path)
		args := []string{
			"-hide_banner", "-nostdin", "-v", "error", "-y",
			"-max_alloc", strconv.Itoa(ffmpegMaxAlloc),
			"-filter_threads", strconv.Itoa(ffmpegThreads),
			"-ss", fmt.Sprintf("%d.%03d", t/1000, t%1000),
			"-threads", strconv.Itoa(ffmpegThreads),
		}
		args = append(args, inputArgs(dec, inputPath)...)
		args = append(args,
			"-map", fmt.Sprintf("0:%d", info.stream),
			"-an", "-sn", "-dn", "-frames:v", "1")
		if filter != "" {
			args = append(args, "-vf", filter)
		}
		args = append(args, "-c:v", "png", path)
		if _, err := e.Run(ctx, "ffmpeg", args...); err != nil {
			return nil, fmt.Errorf("sampling frame at %d ms: %w", t, err)
		}
		// A seek past the last frame writes nothing; the sample is skipped.
		if _, err := os.Stat(path); err != nil {
			continue
		}
		samples = append(samples, sample{path, t})
	}
	if len(samples) == 0 {
		return nil, errors.New("no frame could be sampled")
	}

	sigArgs := append([]string{"identify"}, LimitArgs()...)
	sigArgs = append(sigArgs, "-format", "%#\n")
	for _, s := range samples {
		sigArgs = append(sigArgs, "png:"+s.path)
	}
	res, err := e.Run(ctx, "magick", sigArgs...)
	if err != nil {
		return nil, fmt.Errorf("comparing samples: %w", err)
	}
	sigs := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if len(sigs) != len(samples) {
		return nil, fmt.Errorf("compared %d signatures for %d samples", len(sigs), len(samples))
	}
	var kept []sample
	for i, s := range samples {
		if i > 0 && sigs[i] == sigs[i-1] {
			continue
		}
		kept = append(kept, s)
	}

	encode := append([]string{}, LimitArgs()...)
	for _, s := range kept {
		encode = append(encode, "png:"+s.path)
	}
	pattern := strings.ReplaceAll(filepath.Join(dir, "frame-"), "%", "%%") + "%03d" + enc.ext
	encode = append(encode, "-strip", "-scene", "0", "+adjoin", "-quality", strconv.Itoa(enc.quality), pattern)
	if _, err := e.Run(ctx, "magick", encode...); err != nil {
		return nil, fmt.Errorf("encoding frames: %w", err)
	}

	names := make([]string, len(kept))
	paths := make([]string, len(kept))
	for i := range kept {
		names[i] = fmt.Sprintf("frame-%03d%s", i, enc.ext)
		paths[i] = filepath.Join(dir, names[i])
	}
	sizes, err := identifySizes(ctx, e, paths)
	if err != nil {
		return nil, err
	}
	images := make([]Image, len(kept))
	for i, s := range kept {
		images[i] = Image{Name: names[i], Width: sizes[i][0], Height: sizes[i][1], TimeMS: s.timeMS}
	}
	return images, nil
}
