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
	reads:  []string{"pam"},
	writes: append([]string{"info"}, outputCoders...),
}

// hdrToneMap linearizes HDR video, converts its primaries to BT.709,
// tone-maps it and returns it to BT.709 transfer and matrix, leaving the pixel
// format to the caller. The 203 nit reference white is the BT.2408 level for
// HDR graphic white.
const hdrToneMap = "zscale=t=linear:npl=203,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"

// videoFilter returns the -vf chain for sampling a frame, or "" when the
// frame is delivered as decoded. A reduced frame is scaled to rw x rh. HDR is
// tone-mapped, and every other frame is otherwise sized by the encode stage,
// like any other image.
//
// HDR is scaled before it is tone-mapped: the tone-map runs in 32-bit float
// and its memory grows with the frame, so it is cheaper on the small target.
// The scale writes 4:4:4 because the target size can be odd, and zscale
// rejects a chroma-subsampled frame whose dimensions are not even; the 16-bit
// format keeps precision for the linearization.
func videoFilter(hdr, reduced bool, rw, rh int) string {
	scale := fmt.Sprintf("scale=%d:%d:flags=lanczos", rw, rh)
	switch {
	case hdr && reduced:
		return scale + ",format=yuv444p16le," + hdrToneMap + ",format=yuv444p"
	case hdr:
		return hdrToneMap + ",format=yuv420p"
	case reduced:
		return scale
	}
	return ""
}

// videoReductionMargin is how many times the largest target, in each
// dimension, ffmpeg's reduced decode of an SDR frame covers. HDR is reduced to
// the target itself, because its tone map holds floats. It is the 2x margin
// the pipeline plan chose for every decode-stage reduction, from measuring
// the loss in similarity against a full decode.
const videoReductionMargin = 2

// reducedSize returns the smallest size covering margin times tw x th in both
// dimensions, in the aspect of w x h, and false when that is not below w x h.
func reducedSize(w, h, tw, th, margin int) (rw, rh int, ok bool) {
	f := math.Max(float64(margin*tw)/float64(w), float64(margin*th)/float64(h))
	if f >= 1 {
		return w, h, false
	}
	return max(1, int(math.Ceil(float64(w)*f))), max(1, int(math.Ceil(float64(h)*f))), true
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
	// streamStartUS is the stream's start time in microseconds, on the same
	// clock as containerStartUS; it equals it when either is unknown.
	streamStartUS int64
	// avgRate is the stream's average frame rate; 0 when unknown.
	avgRate float64
	// containerStartUS is the container's start time in microseconds, the
	// origin ffmpeg's -ss counts from; 0 when unknown.
	containerStartUS int64
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
		AvgFrameRate  string `json:"avg_frame_rate"`
		ColorTransfer string `json:"color_transfer"`
		StartTime     string `json:"start_time"`
		Disposition   struct {
			Default     int `json:"default"`
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		SideData []struct {
			Rotation *float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration  string `json:"duration"`
		StartTime string `json:"start_time"`
	} `json:"format"`
}

// demuxerOptions are the input options set on a demuxer by name. An option
// is listed only for demuxers that define it: ffmpeg refuses an input option
// the demuxer does not know.
var demuxerOptions = map[string][]string{
	"mov": {"-enable_drefs", "0"},
}

// detectedFormats are the demuxers an input may be detected as when the
// caller names none: the containers of video, and no playlist or list format
// such as concat or hls, which would open other files the input names.
const detectedFormats = "mov,matroska,avi,mpegts,mpeg,mpegvideo,flv,asf"

// inputArgs returns the arguments that open path in ffprobe or ffmpeg: the
// file protocol only, so no nested input is opened over the network, and the
// decoder's demuxer with its options. A decoder with no name leaves the
// demuxer to ffmpeg's detection, limited to detectedFormats.
func inputArgs(dec decoder, path string) []string {
	if dec.name == "" {
		return []string{"-protocol_whitelist", "file", "-format_whitelist", detectedFormats, "-i", path}
	}
	args := []string{"-protocol_whitelist", "file"}
	args = append(args, demuxerOptions[dec.name]...)
	return append(args, "-f", dec.name, "-i", path)
}

// videoDecoderOf returns the decoder of the video type path's leading bytes
// name, and false for any other content.
func videoDecoderOf(path string) (decoder, bool) {
	mediaType, err := sniffFile(path)
	if err != nil {
		return decoder{}, false
	}
	dec, err := videoSpec.decoderFor(mediaType)
	return dec, err == nil
}

// sniffedVideoDecoder returns videoDecoderOf path, or the unnamed decoder,
// which leaves the demuxer to ffmpeg, for any other content. It serves the
// Env, which has no gate on types.
func sniffedVideoDecoder(path string) decoder {
	if dec, ok := videoDecoderOf(path); ok {
		return dec
	}
	return unnamedVideoDecoder
}

// unnamedVideoDecoder leaves the demuxer to ffmpeg's detection, limited to
// detectedFormats.
var unnamedVideoDecoder = decoder{"ffmpeg", ""}

// probeEntries is what ffprobe is asked to show for the sampling operations;
// probeEntriesWithCodec also asks for each stream's codec name.
const (
	probeEntries = "stream=index,codec_type,width,height,duration,nb_frames,r_frame_rate,avg_frame_rate,color_transfer,start_time" +
		":stream_disposition=default,attached_pic:stream_side_data=rotation:format=duration,start_time"
	probeEntriesWithCodec = "stream=index,codec_name,codec_type,width,height,duration,nb_frames,r_frame_rate,avg_frame_rate,color_transfer,start_time" +
		":stream_disposition=default,attached_pic:stream_side_data=rotation:format=duration,start_time"
)

// probeVideo runs the probe that the sampling operations share.
func probeVideo(ctx context.Context, env Env, dec decoder, path string) (videoInfo, error) {
	return runVideoProbe(ctx, env, dec, path, probeEntries)
}

func runVideoProbe(ctx context.Context, env Env, dec decoder, path, entries string) (videoInfo, error) {
	args := []string{
		"-v", "error", "-of", "json", "-show_entries", entries,
		"-max_alloc", strconv.Itoa(ffmpegMaxAlloc), "-threads", strconv.Itoa(ffmpegThreads),
	}
	args = append(args, inputArgs(dec, path)...)
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
			streams: len(p.Streams), codec: s.CodecName, rate: parseRate(s.RFrameRate), avgRate: parseRate(s.AvgFrameRate),
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
		streamStart, err1 := strconv.ParseFloat(s.StartTime, 64)
		containerStart, err2 := strconv.ParseFloat(p.Format.StartTime, 64)
		if err2 == nil && !math.IsInf(containerStart, 0) && !math.IsNaN(containerStart) {
			v.containerStartUS = int64(math.Round(max(-1e9, min(containerStart, 1e9)) * 1e6))
		}
		v.streamStartUS = v.containerStartUS
		if offset := streamStart - containerStart; err1 == nil && err2 == nil && !math.IsInf(offset, 0) && !math.IsNaN(offset) {
			v.streamStartUS = v.containerStartUS + int64(math.Round(max(-1e9, min(offset, 1e9))*1e6))
		}
		seconds, err := strconv.ParseFloat(s.Duration, 64)
		if err != nil || seconds <= 0 {
			// The container's duration runs from the container's start, and
			// samples are counted from the stream's.
			seconds, err = strconv.ParseFloat(p.Format.Duration, 64)
			if seconds -= float64(v.streamStartUS-v.containerStartUS) / 1e6; seconds <= 0 {
				seconds = 0.001
			}
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

// selectMaxPixels is the largest stream, in decoded pixels (frames times width
// times height as stored), that is read once with a select filter rather than
// once per seek. It is below the measured crossover, so that select is the
// cheaper path wherever it is taken.
//
// Measured on x86 with 8-bit H.264 (veryfast, 250-frame GOPs, noisy test
// content), 15 and 16 samples, edge 1024: select is cheaper in wall and CPU
// time up to about 1.4e9 pixels at 720p, 3.1e9 at 1080p and well beyond 4e9
// (not reached) at 4K, and 5 to 20 times cheaper on short clips, where
// seeking pays an ffmpeg start for every sample. The value is the lowest of
// the crossovers, rounded down, because shorter GOPs make seeks cheaper and
// 10-bit HEVC was not measured. Both paths are decode-bound, so their costs
// scale together with the decoder's speed and the crossover in pixels should
// carry to a slower machine; a slower disk or a startup cost that is larger
// relative to decoding would move it towards seeking.
const selectMaxPixels int64 = 1_000_000_000

// selectLimit is selectMaxPixels; tests set it to force a path.
var selectLimit = selectMaxPixels

// samplePrefix names the lossless intermediates inside the output directory.
const samplePrefix = ".sample-"

// sampleCodec is the format of the intermediates: PAM, which ffmpeg writes
// without compression and ImageMagick reads without decoding, and which holds
// the decoder's own depth, 8 bits for most video and 16 for a 10-bit stream.
const sampleCodec = "pam"

// videoPlan is what the probe decided about sampling a video: the stream,
// the filter that sizes and tone-maps a frame and the decoder's threads.
type videoPlan struct {
	info    videoInfo
	filter  string
	fw, fh  int  // size of the frames ffmpeg delivers
	reduced bool // ffmpeg scales the frames below the source's size
	threads int
	// selectFrames is set when the stream is short enough, in decoded pixels,
	// to be read once with a select filter instead of once per seek.
	selectFrames bool
}

// probeChecked probes the video and refuses it if it exceeds a cap. It is the
// first step of every video sampling.
func (e Env) probeChecked(ctx context.Context, dec decoder, inputPath string) (videoInfo, error) {
	info, err := probeVideo(ctx, e, dec, inputPath)
	if err != nil {
		return videoInfo{}, err
	}
	return info, checkVideoInfo(info)
}

// newVideoPlan decides how the frames of a probed video are sized. They are
// delivered at the size decode needs, which is the largest of the encodings
// that will be made from them, with videoReductionMargin to spare for SDR and
// none for HDR.
func newVideoPlan(info videoInfo, decode encoding) videoPlan {
	w, h := info.upright()
	tw, th, resize := fitSize(w, h, decode.maxEdge, decode.maxArea)
	hdr := isHDRTransfer(info.transfer)
	rw, rh, reduced := tw, th, resize
	if !hdr {
		rw, rh, reduced = reducedSize(w, h, tw, th, videoReductionMargin)
	}
	return videoPlan{
		info:         info,
		filter:       videoFilter(hdr, reduced, rw, rh),
		fw:           rw,
		fh:           rh,
		reduced:      reduced,
		threads:      ffmpegThreadsFor(info.width, info.height),
		selectFrames: info.frames > 0 && int64(info.frames)*int64(info.width)*int64(info.height) <= selectLimit,
	}
}

// covers reports whether frames delivered under p serve an encode that
// planned for q: the same frames, or for SDR frames at least as large, whose
// margin is then at least q's.
func (p videoPlan) covers(q videoPlan) bool {
	if p.fw == q.fw && p.fh == q.fh && p.reduced == q.reduced {
		return true
	}
	return !isHDRTransfer(p.info.transfer) && p.fw >= q.fw && p.fh >= q.fh
}

// source returns what the encode of enc needs to know about the frames. A
// reduced frame is sized from the source's own size, not its own, since a
// reduction rounds each side separately and the output must not depend on
// which of several encodings the frame was reduced for.
func (v videoPlan) source(enc encoding) encodeSource {
	if !v.reduced {
		return encodeSource{}
	}
	w, h := v.info.upright()
	tw, th, _ := fitSize(w, h, enc.maxEdge, enc.maxArea)
	return encodeSource{width: tw, height: th}
}

// selectArgs returns the arguments of one ffmpeg run that keeps the frames
// expr selects, applies the plan's filter to them and stops after frames
// frames, followed by output. It decodes from the start of the stream, or from
// the last keyframe at or before seekUS microseconds on the container's
// timeline when that is not negative. Timestamps are kept as the container
// holds them, so expr compares them with absolute times (absUS) whichever way
// the run starts. It carries the bounds of every ffmpeg run.
func (v videoPlan) selectArgs(dec decoder, inputPath, expr string, frames int, seekUS int64, output ...string) []string {
	args := append(ffmpegArgs(v.threads), "-copyts")
	if seekUS >= 0 {
		args = append(args, "-ss", fmt.Sprintf("%d.%06d", seekUS/1_000_000, seekUS%1_000_000))
	}
	args = append(args, "-threads", strconv.Itoa(v.threads))
	args = append(args, inputArgs(dec, inputPath)...)
	filter := "select='" + expr + "'"
	if v.filter != "" {
		filter += "," + v.filter
	}
	args = append(args,
		"-map", fmt.Sprintf("0:%d", v.info.stream),
		"-an", "-sn", "-dn", "-frames:v", strconv.Itoa(frames), "-vf", filter)
	return append(args, output...)
}

// seekMarginUS is how far before a time a seek starts decoding: sixteen
// frames, and at least half a second. A seek lands on a keyframe at or before
// the time it is given. In a stream with open GOPs, the pictures that lead a
// keyframe in display order follow it in decode order and refer to frames
// before it, so they come out only if decoding starts earlier than the
// keyframe. Sixteen frames is the deepest reordering H.264 and HEVC allow
// (the size of the decoded picture buffer). The frame time is that of the
// slower of the stream's declared and average rates, since the declared rate
// of a variable-rate stream overstates it and so shortens the margin. The
// margin costs, on average, that many extra decoded frames for each sample,
// and, at worst, one more GOP when a keyframe falls just inside it.
func (v videoPlan) seekMarginUS() int64 {
	margin := int64(500_000)
	rate := v.info.rate
	if v.info.avgRate > 0 && (rate <= 0 || v.info.avgRate < rate) {
		rate = v.info.avgRate
	}
	if rate > 0 {
		margin = max(margin, int64(16e6/rate))
	}
	return margin
}

// selectSeconds formats a time in microseconds as the seconds of a select
// expression.
func selectSeconds(us int64) string {
	return strconv.FormatFloat(float64(us)/1e6, 'f', 6, 64)
}

// selectFrom returns the select-filter term true for a frame at or after us
// microseconds whose predecessor is before it: the first frame at or after
// the time. The target is taken one microsecond early, so that a frame exactly
// at the time is not lost to rounding.
func selectFrom(us int64) string {
	t := selectSeconds(us - 1)
	return "gte(t\\," + t + ")*lt(prev_t\\," + t + ")"
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

// videoFrames samples up to maxFrames frames evenly.
func (e Env) videoFrames(ctx context.Context, dec decoder, inputPath string, enc encoding, maxFrames int, dir string) ([]Image, error) {
	return e.videoFramesPicked(ctx, dec, inputPath, enc, func(info videoInfo) ([]int64, error) {
		return sampleTimesMS(info, maxFrames), nil
	}, dir)
}

// videoFramesPicked samples the frames at the times pick chooses, given the
// probe, which must be in ascending order without repeats; sampleVideo reads
// them.
func (e Env) videoFramesPicked(ctx context.Context, dec decoder, inputPath string, enc encoding, pick func(videoInfo) ([]int64, error), dir string) ([]Image, error) {
	return e.encodeVideoFiles(ctx, openRequest{
		path: inputPath, dec: dec, pinned: true, scratch: dir, video: &videoRequest{times: pick},
	}, enc, dir)
}

// videoFrameRun samples count consecutive frames from startMS in one ffmpeg
// run.
func (e Env) videoFrameRun(ctx context.Context, dec decoder, inputPath string, enc encoding, startMS int64, count int, dir string) ([]Image, error) {
	return e.encodeVideoFiles(ctx, openRequest{
		path: inputPath, dec: dec, pinned: true, scratch: dir,
		video: &videoRequest{run: &frameRun{startMS: startMS, count: count}},
	}, enc, dir)
}

// encodeVideoFiles opens the video req describes and writes its samples to
// dir.
func (e Env) encodeVideoFiles(ctx context.Context, req openRequest, enc encoding, dir string) ([]Image, error) {
	src, err := e.open(ctx, req)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	return src.encodeFiles(ctx, enc, dir)
}

// sampleVideo writes the frame at or after each of times, which must be
// ascending without repeats, to a file in dir, and returns them with the time
// each was asked for. A time past the last frame has no frame and is skipped,
// and several times that fall on the same frame give it once, with the first
// of them. A stream of at most selectLimit decoded pixels is read once, and
// a longer one by one seek per time; the frames are the same. The files are
// the caller's to remove.
func (e Env) sampleVideo(ctx context.Context, dec decoder, inputPath, dir string, plan videoPlan, times []int64) ([]sampledFrame, error) {
	if plan.selectFrames {
		return e.selectSamples(ctx, dec, inputPath, dir, plan, times)
	}
	return e.seekSamples(ctx, dec, inputPath, dir, plan, times)
}

// seekSamples samples each of times by its own ffmpeg run, which starts
// decoding a margin before the time and keeps the first frame at or after it,
// as a select run does.
func (e Env) seekSamples(ctx context.Context, dec decoder, inputPath, dir string, plan videoPlan, times []int64) ([]sampledFrame, error) {
	var samples []sampledFrame
	for _, t := range times {
		written, err := e.runSelect(ctx, dec, inputPath, dir, plan, "gte(t\\,"+selectSeconds(plan.absUS(t)-1)+")", 1, plan.seekUS(t))
		if err != nil {
			return samples, fmt.Errorf("sampling frame at %d ms: %w", t, err)
		}
		// A time on the frame of an earlier one finds its file already
		// there, which runSelect does not report.
		if len(written) == 0 {
			continue
		}
		samples = append(samples, sampledFrame{written[0].path, t})
	}
	return samples, nil
}

// selectedFrame is a frame a select run wrote, named by its presentation time
// in microseconds on the container's timeline.
type selectedFrame struct {
	path string
	us   int64
}

// runSelect runs ffmpeg once, keeping the frames expr selects, at most frames
// of them, and returns the files it newly wrote, in order. On error it removes what
// was written.
func (e Env) runSelect(ctx context.Context, dec decoder, inputPath, dir string, plan videoPlan, expr string, frames int, seekUS int64) ([]selectedFrame, error) {
	// The image muxer names each file by its frame's presentation time, which
	// -enc_time_base makes microseconds.
	pattern := strings.ReplaceAll(filepath.Join(dir, samplePrefix), "%", "%%") + "%d.pam"
	args := plan.selectArgs(dec, inputPath, expr, frames, seekUS,
		"-c:v", sampleCodec, "-fps_mode", "passthrough", "-enc_time_base", "1:1000000", "-frame_pts", "1", pattern)
	before, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	had := make(map[string]bool, len(before))
	for _, ent := range before {
		had[ent.Name()] = true
	}
	_, runErr := e.Run(ctx, "ffmpeg", args...)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var written []selectedFrame
	for _, ent := range entries {
		m := runSampleRE.FindStringSubmatch(ent.Name())
		if m == nil || had[ent.Name()] {
			continue
		}
		us, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			runErr = cmp.Or(runErr, fmt.Errorf("unexpected sample name %q", ent.Name()))
			continue
		}
		written = append(written, selectedFrame{filepath.Join(dir, ent.Name()), us})
	}
	slices.SortFunc(written, func(a, b selectedFrame) int { return cmp.Compare(a.us, b.us) })
	if runErr != nil {
		for _, f := range written {
			_ = os.Remove(f.path)
		}
		return nil, runErr
	}
	return written, nil
}

// absUS is a time on the stream's timeline, in milliseconds from its start,
// as the microseconds of ffmpeg's timestamps, which it keeps as the container
// holds them.
func (v videoPlan) absUS(ms int64) int64 {
	return v.info.streamStartUS + ms*1000
}

// seekUS is where a seek that finds the frame at or after ms starts, in
// microseconds from the container's start, which is where -ss counts from.
func (v videoPlan) seekUS(ms int64) int64 {
	return max(0, v.absUS(ms)-v.info.containerStartUS-v.seekMarginUS())
}

// selectSamples is sampleVideo for a stream read once: one decode, with a
// select filter that keeps, for each time, the first frame at or after it.
func (e Env) selectSamples(ctx context.Context, dec decoder, inputPath, dir string, plan videoPlan, times []int64) ([]sampledFrame, error) {
	if len(times) == 0 {
		return nil, nil
	}
	targets := make([]int64, len(times))
	terms := make([]string, 0, len(times)+1)
	for i, t := range times {
		targets[i] = plan.absUS(t)
		terms = append(terms, selectFrom(targets[i]))
	}
	// The first frame has no predecessor, so it is kept if it is at or after
	// the first time.
	terms = append(terms, "isnan(prev_t)*gte(t\\,"+selectSeconds(targets[0]-1)+")")
	written, err := e.runSelect(ctx, dec, inputPath, dir, plan, strings.Join(terms, "+"), len(times), -1)
	if err != nil {
		return nil, fmt.Errorf("sampling frames: %w", err)
	}
	var samples []sampledFrame
	kept := map[string]bool{}
	next, last := 0, -1
	for i, target := range targets {
		for next < len(written) && written[next].us+1 < target {
			next++
		}
		if next == len(written) {
			break
		}
		if next != last {
			samples = append(samples, sampledFrame{written[next].path, times[i]})
			kept[written[next].path] = true
			last = next
		}
	}
	for _, f := range written {
		if !kept[f.path] {
			_ = os.Remove(f.path)
		}
	}
	return samples, nil
}

// runSampleRE matches the scratch files of a sampling, which ffmpeg names by
// the frame's presentation time in microseconds, as it keeps it.
var runSampleRE = regexp.MustCompile(`^` + regexp.QuoteMeta(samplePrefix) + `(\d+)\.pam$`)

// runFrames reads count frames from the first at or after startMS, by one
// run that starts at the beginning of the stream or, if seek, a margin before
// startMS. Each is timed by its own presentation time.
func (e Env) runFrames(ctx context.Context, dec decoder, inputPath, dir string, plan videoPlan, startMS int64, count int, seek bool) ([]sampledFrame, error) {
	from := plan.absUS(startMS)
	seekUS := int64(-1)
	if seek {
		seekUS = plan.seekUS(startMS)
	}
	written, err := e.runSelect(ctx, dec, inputPath, dir, plan, "gte(t\\,"+selectSeconds(from-1)+")", count, seekUS)
	if err != nil {
		return nil, err
	}
	samples := make([]sampledFrame, 0, len(written))
	for _, f := range written {
		samples = append(samples, sampledFrame{f.path, startMS + (f.us-from+500)/1000})
	}
	return samples[:min(len(samples), count)], nil
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
	encoded, err := s.env.runEncode(ctx, encodeArgs(batch, s.src, s.enc, encodeOutput{kind: outputFiles, path: pattern}),
		encodeCheck{frames: len(batch), bounds: []encodeBound{boundOf(s.enc, s.src)}, outputs: s.batchFiles(len(batch))})
	if err != nil {
		return fmt.Errorf("encoding frames %d to %d: %w", s.seen, s.seen+len(batch)-1, err)
	}
	files := s.batchFiles(len(encoded))
	for j, f := range encoded {
		tmp := files[j]
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

// batchFiles are the files a run of n frames writes.
func (s *sampleEncoder) batchFiles(n int) []string {
	files := make([]string, n)
	for j := range files {
		files[j] = filepath.Join(s.dir, ".enc-"+strconv.Itoa(j)+s.enc.ext)
	}
	return files
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
