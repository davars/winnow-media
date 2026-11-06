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

// ffmpegArgs returns the arguments every ffmpeg run starts with: no banner,
// no stdin, errors only, and the allocation and filter-thread bounds.
func ffmpegArgs() []string {
	return []string{
		"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-max_alloc", strconv.Itoa(ffmpegMaxAlloc),
		"-filter_threads", strconv.Itoa(ffmpegThreads),
	}
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
	filter := videoFilter(isHDRTransfer(info.transfer), resize, tw, th)

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
		args := append(ffmpegArgs(),
			"-ss", fmt.Sprintf("%d.%03d", t/1000, t%1000),
			"-threads", strconv.Itoa(ffmpegThreads))
		args = append(args, inputArgs(dec, inputPath)...)
		args = append(args,
			"-map", fmt.Sprintf("0:%d", info.stream),
			"-an", "-sn", "-dn", "-frames:v", "1")
		if filter != "" {
			args = append(args, "-vf", filter)
		}
		args = append(args, "-c:v", "png", "-update", "1", path)
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

	var paths []string
	for _, s := range samples {
		paths = append(paths, s.path)
	}
	var images []Image
	encoder := sampleEncoder{
		env: e, coder: "png", middle: []string{"-strip"}, enc: enc, dir: dir, framePixels: tw * th,
		keep: func(i, n, w, h int) {
			images = append(images, Image{Name: frameName(n, enc.ext), Width: w, Height: h, TimeMS: samples[i].timeMS})
		},
	}
	if err := encoder.encodeSamples(ctx, paths); err != nil {
		return nil, err
	}
	return images, nil
}

// frameName is the name of the n-th output frame.
func frameName(n int, ext string) string {
	return fmt.Sprintf("frame-%03d%s", n, ext)
}

// encodeBatchPixels bounds the input pixels one ImageMagick run reads: at
// 16 bytes per pixel, 256 MiB, a quarter of the policy's memory ceiling.
var encodeBatchPixels = 16_000_000

// sampleEncoder encodes lossless samples to output frames in dir, one
// ImageMagick run per batch of samples, and drops a sample whose pixels equal
// those of the one before it, also across batches. Kept frames are numbered
// consecutively from 0.
type sampleEncoder struct {
	env    Env
	coder  string // ImageMagick coder of the samples
	middle []string
	enc    encoding
	dir    string
	// framePixels is the pixel count of each sample; it sets the batch size.
	framePixels int
	// keep is called for each kept sample with its index among all the
	// samples given, its output number and its size.
	keep func(i, n, w, h int)

	lastSig string
	seen, n int
}

// batchSize is the number of samples one run reads: at least one, at most
// what encodeBatchPixels allows.
func (s *sampleEncoder) batchSize() int {
	return max(1, encodeBatchPixels/max(1, s.framePixels))
}

// encode encodes batch, the next samples in order.
func (s *sampleEncoder) encode(ctx context.Context, batch []string) error {
	if len(batch) == 0 {
		return nil
	}
	args := append([]string{}, LimitArgs()...)
	for _, p := range batch {
		args = append(args, s.coder+":"+p)
	}
	args = append(args, s.middle...)
	pattern := strings.ReplaceAll(filepath.Join(s.dir, ".enc-"), "%", "%%") + "%d" + s.enc.ext
	args = append(args, "-quality", strconv.Itoa(s.enc.quality), "+adjoin", "-scene", "0",
		"-write", pattern, "-format", "%#|%w|%h\n", "info:")
	res, err := s.env.Run(ctx, "magick", args...)
	if err != nil {
		return fmt.Errorf("encoding frames %d to %d: %w", s.seen, s.seen+len(batch)-1, err)
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if len(lines) != len(batch) {
		return fmt.Errorf("encoding frames %d to %d: %d results", s.seen, s.seen+len(batch)-1, len(lines))
	}
	for j, line := range lines {
		tmp := filepath.Join(s.dir, ".enc-"+strconv.Itoa(j)+s.enc.ext)
		f := strings.Split(line, "|")
		if len(f) != 3 {
			return fmt.Errorf("encoding frame %d: unexpected output %q", s.seen+j, line)
		}
		if s.seen+j > 0 && f[0] == s.lastSig {
			if err := os.Remove(tmp); err != nil {
				return err
			}
			continue
		}
		w, err1 := strconv.Atoi(f[1])
		h, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("encoding frame %d: unexpected size %q", s.seen+j, line)
		}
		if err := os.Rename(tmp, filepath.Join(s.dir, frameName(s.n, s.enc.ext))); err != nil {
			return err
		}
		s.lastSig = f[0]
		s.keep(s.seen+j, s.n, w, h)
		s.n++
	}
	s.seen += len(batch)
	return nil
}

// encodeSamples encodes samples, all of s.framePixels pixels, in batches.
func (s *sampleEncoder) encodeSamples(ctx context.Context, samples []string) error {
	for batch := s.batchSize(); len(samples) > 0; samples = samples[min(batch, len(samples)):] {
		if err := s.encode(ctx, samples[:min(batch, len(samples))]); err != nil {
			return err
		}
	}
	return nil
}
