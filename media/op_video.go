package media

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	reads:  []string{"png"},
	writes: append([]string{"info"}, outputCoders...),
}

// hdrToneMap linearizes HDR video, converts its primaries to BT.709,
// tone-maps it and returns it to BT.709 transfer and matrix, leaving the pixel
// format to the caller. The 203 nit reference white is the BT.2408 level for
// HDR graphic white.
const hdrToneMap = "zscale=t=linear:npl=203,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"

// videoFilter returns the -vf chain for sampling a frame, or "" when the frame
// needs neither tone mapping nor resizing.
//
// HDR is scaled before it is tone-mapped: the tone-map runs in 32-bit float
// and its memory grows with the frame, so it is cheaper on the small target.
// The scale writes 4:4:4 because the target size can be odd, and zscale
// rejects a chroma-subsampled frame whose dimensions are not even; the 16-bit
// format keeps precision for the linearization.
func videoFilter(hdr, resize bool, tw, th int) string {
	scale := fmt.Sprintf("scale=%d:%d:flags=lanczos", tw, th)
	switch {
	case hdr && resize:
		return scale + ",format=yuv444p16le," + hdrToneMap + ",format=yuv444p"
	case hdr:
		return hdrToneMap + ",format=yuv420p"
	case resize:
		return scale
	}
	return ""
}

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
	declared      int // the container's frame count; 0 when it gives none
	rate          float64
	codec         string // empty unless the probe asked for it
	hasAudio      bool
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
		CodecName     string `json:"codec_name"`
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

// probeEntries is what ffprobe is asked to show for the sampling operations;
// probeEntriesWithCodec also asks for each stream's codec name.
const (
	probeEntries = "stream=index,codec_type,width,height,duration,nb_frames,r_frame_rate,color_transfer" +
		":stream_disposition=default,attached_pic:stream_side_data=rotation:format=duration"
	probeEntriesWithCodec = "stream=index,codec_name,codec_type,width,height,duration,nb_frames,r_frame_rate,color_transfer" +
		":stream_disposition=default,attached_pic:stream_side_data=rotation:format=duration"
)

// probeVideo runs the probe that the sampling operations share.
func probeVideo(ctx context.Context, env Env, dec decoder, path string) (videoInfo, error) {
	return runVideoProbe(ctx, env, dec, path, probeEntries)
}

func runVideoProbe(ctx context.Context, env Env, dec decoder, path, entries string) (videoInfo, error) {
	args := append([]string{"-v", "error", "-of", "json", "-show_entries", entries}, inputArgs(dec, path)...)
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
		v := videoInfo{
			stream: s.Index, width: s.Width, height: s.Height, transfer: s.ColorTransfer,
			streams: len(p.Streams), codec: s.CodecName, rate: parseRate(s.RFrameRate),
		}
		for _, o := range p.Streams {
			if o.CodecType == "audio" {
				v.hasAudio = true
			}
		}
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
			v.frames, v.declared = n, n
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

// ffmpegArgs returns the arguments every ffmpeg run starts with: no banner,
// no stdin, errors only, and the allocation and filter-thread bounds.
func ffmpegArgs(threads int) []string {
	return []string{
		"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-max_alloc", strconv.Itoa(ffmpegMaxAlloc),
		"-filter_threads", strconv.Itoa(threads),
	}
}

// samplePrefix names the lossless intermediates inside the output directory.
const samplePrefix = ".sample-"

// videoPlan is what the probe decided about sampling a video: the stream,
// the filter that sizes and tone-maps a frame and the decoder's threads.
type videoPlan struct {
	info    videoInfo
	filter  string
	tw, th  int // output size
	threads int
}

// planVideo probes the video, refuses it if it exceeds a cap and decides how
// its frames are sized. It is the first step of every video sampling.
func (e Env) planVideo(ctx context.Context, dec decoder, inputPath string, enc encoding) (videoPlan, error) {
	info, err := probeVideo(ctx, e, dec, inputPath)
	if err != nil {
		return videoPlan{}, err
	}
	if err := checkVideoInfo(info); err != nil {
		return videoPlan{}, err
	}
	w, h := info.upright()
	tw, th, resize := fitSize(w, h, enc.maxEdge, enc.maxArea)
	return videoPlan{
		info:    info,
		filter:  videoFilter(isHDRTransfer(info.transfer), resize, tw, th),
		tw:      tw,
		th:      th,
		threads: ffmpegThreadsFor(info.width, info.height),
	}, nil
}

// ffmpegArgs returns the arguments of one ffmpeg run that seeks to seekMS,
// decodes frames frames of the plan's stream and applies the plan's filter,
// followed by output.
func (v videoPlan) ffmpegArgs(dec decoder, inputPath string, seekMS int64, frames int, output ...string) []string {
	args := append(ffmpegArgs(v.threads),
		"-ss", fmt.Sprintf("%d.%03d", seekMS/1000, seekMS%1000),
		"-threads", strconv.Itoa(v.threads))
	args = append(args, inputArgs(dec, inputPath)...)
	args = append(args,
		"-map", fmt.Sprintf("0:%d", v.info.stream),
		"-an", "-sn", "-dn", "-frames:v", strconv.Itoa(frames))
	if v.filter != "" {
		args = append(args, "-vf", v.filter)
	}
	return append(args, output...)
}

// checkSeekTime refuses a time that is negative or, unless it is 0, not
// before the end of the video.
func checkSeekTime(v videoInfo, ms int64) error {
	if ms < 0 {
		return fmt.Errorf("time %d ms is negative", ms)
	}
	if ms > 0 && ms >= v.durationMS {
		return fmt.Errorf("time %d ms is not inside the video, whose duration is %d ms (0 when unknown)", ms, v.durationMS)
	}
	return nil
}

// sampledFrame is a lossless frame written by ffmpeg.
type sampledFrame struct {
	path   string
	timeMS int64
}

// encodeVideoSamples encodes the samples to output frames in dir.
func (e Env) encodeVideoSamples(ctx context.Context, enc encoding, dir string, plan videoPlan, samples []sampledFrame) ([]Image, error) {
	if len(samples) == 0 {
		return nil, errors.New("no frame could be sampled")
	}
	frames := make([]encodeFrame, len(samples))
	for i, s := range samples {
		frames[i] = encodeFrame{input: "png:" + s.path}
	}
	var images []Image
	encoder := sampleEncoder{
		env: e, src: encodeSource{width: plan.tw, height: plan.th}, enc: enc, dir: dir, framePixels: plan.tw * plan.th,
		keep: func(i, n, w, h int) {
			images = append(images, Image{Name: frameName(n, enc.ext), Width: w, Height: h, TimeMS: samples[i].timeMS})
		},
	}
	if err := encoder.encodeSamples(ctx, frames); err != nil {
		return nil, err
	}
	return images, nil
}

// videoFrames samples up to maxFrames frames evenly.
func (e Env) videoFrames(ctx context.Context, dec decoder, inputPath string, enc encoding, maxFrames int, dir string) ([]Image, error) {
	return e.videoFramesPicked(ctx, dec, inputPath, enc, func(info videoInfo) ([]int64, error) {
		return sampleTimesMS(info, maxFrames), nil
	}, dir)
}

// videoFramesPicked samples the frames at the times pick chooses, given the
// probe, which must be in ascending order without repeats, each by its own
// seek.
func (e Env) videoFramesPicked(ctx context.Context, dec decoder, inputPath string, enc encoding, pick func(videoInfo) ([]int64, error), dir string) ([]Image, error) {
	plan, err := e.planVideo(ctx, dec, inputPath, enc)
	if err != nil {
		return nil, err
	}
	times, err := pick(plan.info)
	if err != nil {
		return nil, err
	}
	var scratch []string
	defer func() {
		for _, p := range scratch {
			_ = os.Remove(p)
		}
	}()
	var samples []sampledFrame
	for i, t := range times {
		path := filepath.Join(dir, fmt.Sprintf("%s%03d.png", samplePrefix, i))
		scratch = append(scratch, path)
		args := plan.ffmpegArgs(dec, inputPath, t, 1, "-c:v", "png", "-update", "1", path)
		if _, err := e.Run(ctx, "ffmpeg", args...); err != nil {
			return nil, fmt.Errorf("sampling frame at %d ms: %w", t, err)
		}
		// A seek past the last frame writes nothing; the sample is skipped.
		if _, err := os.Stat(path); err != nil {
			continue
		}
		samples = append(samples, sampledFrame{path, t})
	}
	return e.encodeVideoSamples(ctx, enc, dir, plan, samples)
}

// runSampleRE matches the scratch files of videoFrameRun, which ffmpeg names
// by the frame's presentation time in ms relative to the seek.
var runSampleRE = regexp.MustCompile(`^` + regexp.QuoteMeta(samplePrefix) + `(\d+)\.png$`)

// videoFrameRun samples count consecutive frames from startMS in one ffmpeg
// run.
func (e Env) videoFrameRun(ctx context.Context, dec decoder, inputPath string, enc encoding, startMS int64, count int, dir string) ([]Image, error) {
	plan, err := e.planVideo(ctx, dec, inputPath, enc)
	if err != nil {
		return nil, err
	}
	if err := checkSeekTime(plan.info, startMS); err != nil {
		return nil, err
	}
	var scratch []string
	defer func() {
		for _, p := range scratch {
			_ = os.Remove(p)
		}
	}()
	pattern := strings.ReplaceAll(filepath.Join(dir, samplePrefix), "%", "%%") + "%d.png"
	// The image muxer names each file by its frame's presentation time, which
	// -enc_time_base makes milliseconds, so the names carry the real
	// timestamps of frames the seek decided.
	args := plan.ffmpegArgs(dec, inputPath, startMS, count,
		"-c:v", "png", "-fps_mode", "passthrough", "-enc_time_base", "1:1000", "-frame_pts", "1", pattern)
	_, runErr := e.Run(ctx, "ffmpeg", args...)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var samples []sampledFrame
	for _, ent := range entries {
		m := runSampleRE.FindStringSubmatch(ent.Name())
		if m == nil {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		scratch = append(scratch, path)
		rel, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unexpected sample name %q", ent.Name())
		}
		samples = append(samples, sampledFrame{path, startMS + rel})
	}
	if runErr != nil {
		return nil, fmt.Errorf("sampling frames from %d ms: %w", startMS, runErr)
	}
	slices.SortFunc(samples, func(a, b sampledFrame) int { return cmp.Compare(a.timeMS, b.timeMS) })
	if len(samples) > count {
		samples = samples[:count]
	}
	return e.encodeVideoSamples(ctx, enc, dir, plan, samples)
}

// frameName is the name of the n-th output frame.
func frameName(n int, ext string) string {
	return fmt.Sprintf("frame-%03d%s", n, ext)
}

// encodeBatchPixels bounds the input pixels one ImageMagick run reads: at
// 16 bytes per pixel, 256 MiB, a quarter of the policy's memory ceiling.
var encodeBatchPixels = 16_000_000

// sampleEncoder encodes frames to output files in dir, one ImageMagick run per
// batch of frames, and drops a frame whose pixels equal those of the one
// before it, also across batches. Kept frames are numbered consecutively from
// 0.
type sampleEncoder struct {
	env Env
	src encodeSource
	enc encoding
	dir string
	// framePixels is the pixel count of each frame as read; it sets the batch
	// size.
	framePixels int
	// keep is called for each kept frame with its index among all the frames
	// given, its output number and its size.
	keep func(i, n, w, h int)

	lastSig string
	seen, n int
}

// batchSize is the number of frames one run reads: at least one, at most
// what encodeBatchPixels allows.
func (s *sampleEncoder) batchSize() int {
	return max(1, encodeBatchPixels/max(1, s.framePixels))
}

// encode encodes batch, the next frames in order.
func (s *sampleEncoder) encode(ctx context.Context, batch []encodeFrame) error {
	if len(batch) == 0 {
		return nil
	}
	pattern := strings.ReplaceAll(filepath.Join(s.dir, ".enc-"), "%", "%%") + "%d" + s.enc.ext
	encoded, err := s.env.runEncode(ctx, encodeArgs(batch, s.src, s.enc, encodeOutput{kind: outputFiles, path: pattern}))
	if err != nil {
		return fmt.Errorf("encoding frames %d to %d: %w", s.seen, s.seen+len(batch)-1, err)
	}
	if len(encoded) != len(batch) {
		return fmt.Errorf("encoding frames %d to %d: %d results", s.seen, s.seen+len(batch)-1, len(encoded))
	}
	for j, f := range encoded {
		tmp := filepath.Join(s.dir, ".enc-"+strconv.Itoa(j)+s.enc.ext)
		if s.seen+j > 0 && f.signature == s.lastSig {
			if err := os.Remove(tmp); err != nil {
				return err
			}
			continue
		}
		if err := os.Rename(tmp, filepath.Join(s.dir, frameName(s.n, s.enc.ext))); err != nil {
			return err
		}
		s.lastSig = f.signature
		s.keep(s.seen+j, s.n, f.width, f.height)
		s.n++
	}
	s.seen += len(batch)
	return nil
}

// encodeSamples encodes frames, all of s.framePixels pixels, in batches.
func (s *sampleEncoder) encodeSamples(ctx context.Context, frames []encodeFrame) error {
	for batch := s.batchSize(); len(frames) > 0; frames = frames[min(batch, len(frames)):] {
		if err := s.encode(ctx, frames[:min(batch, len(frames))]); err != nil {
			return err
		}
	}
	return nil
}
