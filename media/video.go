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

// maxVideoSampleFrames caps the frames a video animation samples;
// it is tied to the animation's playback rate.
const maxVideoSampleFrames = 15

// GenerateVideoThumbnails writes two animated WebP thumbnails of one video
// stream, at maximum sizes sizeSmall and sizeLarge and the given WebP quality.
// frames is the stream's frame count, from which an evenly spaced sample is
// taken; zero or less samples the first frame only.
//
// Each sample is read by seeking to it, so the cost depends on the number of
// samples and not on the length of the video. Frame i of the stream is taken
// at its presentation time, start + i/rate, where rate is the stream's average
// frame rate; for variable frame rate video that is the nearest frame to the
// time, not frame i. Scratch files live in a private directory that is removed
// before the call returns.
func (e Env) GenerateVideoThumbnails(ctx context.Context, inputPath, smallPath, largePath string, streamIndex, frames, sizeSmall, sizeLarge, quality int) error {
	info, err := probeVideoTiming(ctx, e, inputPath, streamIndex)
	if err != nil {
		return err
	}
	if err := checkVideoSize(info.width, info.height); err != nil {
		return err
	}
	indices := videoSample(frames)
	if info.rate <= 0 {
		indices = []int{0}
	}
	dir, err := os.MkdirTemp("", "winnow-video-thumbs-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	threads := ffmpegThreadsFor(info.width, info.height)
	count := 0
	for _, index := range indices {
		path := filepath.Join(dir, fmt.Sprintf(videoSampleName, count))
		args := videoSampleArgs(inputPath, path, streamIndex, videoSeekSeconds(info, index), threads)
		if _, err := e.Run(ctx, "ffmpeg", args...); err != nil {
			return fmt.Errorf("sampling frame %d: %w", index, err)
		}
		// A seek past the last frame writes nothing; the sample is skipped.
		if _, err := os.Stat(path); err == nil {
			count++
		}
	}
	if count == 0 {
		return errors.New("no frame could be sampled")
	}
	var list strings.Builder
	for i := range count {
		fmt.Fprintf(&list, "file '%s'\n", fmt.Sprintf(videoSampleName, i))
	}
	listPath := filepath.Join(dir, videoSampleList)
	if err := os.WriteFile(listPath, []byte(list.String()), 0o600); err != nil {
		return err
	}
	args := videoEncodeArgs(listPath, smallPath, largePath, count, sizeSmall, sizeLarge, quality, threads)
	_, err = e.Run(ctx, "ffmpeg", args...)
	return err
}

// videoSampleName is the printf pattern of the numbered lossless samples. A
// sample keeps the decoder's pixel format, so the encode run scales from the
// same pixels a single pass over the stream would.
const videoSampleName = "sample-%03d.nut"

// videoSampleList names the concat list of the samples, in order.
const videoSampleList = "samples.txt"

// videoTiming is what the probe reads about the stream thumbnails are
// sampled from.
type videoTiming struct {
	width, height int
	rate          float64 // frames per second; 0 when unknown
	// offset is the stream's start relative to the container's, the origin
	// that ffmpeg's -ss counts from.
	offset float64
}

type timingProbe struct {
	Streams []struct {
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		AvgFrameRate string `json:"avg_frame_rate"`
		RFrameRate   string `json:"r_frame_rate"`
		StartTime    string `json:"start_time"`
	} `json:"streams"`
	Format struct {
		StartTime string `json:"start_time"`
	} `json:"format"`
}

func probeVideoTiming(ctx context.Context, env Env, path string, streamIndex int) (videoTiming, error) {
	res, err := env.Run(ctx, "ffprobe",
		"-v", "error", "-of", "json",
		"-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "stream=width,height,avg_frame_rate,r_frame_rate,start_time:format=start_time",
		"-protocol_whitelist", "file", "-i", path)
	if err != nil {
		return videoTiming{}, fmt.Errorf("probing video: %w", err)
	}
	var p timingProbe
	if err := json.Unmarshal(res.Stdout, &p); err != nil {
		return videoTiming{}, fmt.Errorf("parsing probe output: %w", err)
	}
	if len(p.Streams) != 1 {
		return videoTiming{}, fmt.Errorf("input has no stream %d", streamIndex)
	}
	s := p.Streams[0]
	t := videoTiming{width: s.Width, height: s.Height, rate: parseRate(s.AvgFrameRate)}
	if t.rate <= 0 {
		t.rate = parseRate(s.RFrameRate)
	}
	streamStart, err1 := strconv.ParseFloat(s.StartTime, 64)
	containerStart, err2 := strconv.ParseFloat(p.Format.StartTime, 64)
	if err1 == nil && err2 == nil && !math.IsInf(streamStart-containerStart, 0) {
		t.offset = streamStart - containerStart
	}
	return t, nil
}

// videoSeekSeconds returns the -ss target for frame index: half a frame
// before its presentation time, so that rounding cannot step past it to the
// next frame. Accurate seeking then decodes forward to the first frame at or
// after the target.
func videoSeekSeconds(t videoTiming, index int) float64 {
	if t.rate <= 0 {
		return 0
	}
	return math.Max(0, t.offset+(float64(index)-0.5)/t.rate)
}

// videoSampleArgs returns the ffmpeg arguments that write the first frame at
// or after seconds to a lossless NUT file at out.
func videoSampleArgs(inputPath, out string, streamIndex int, seconds float64, threads int) []string {
	args := append(ffmpegArgs(threads),
		"-ss", strconv.FormatFloat(seconds, 'f', 6, 64),
		"-threads", strconv.Itoa(threads),
		"-protocol_whitelist", "file",
		"-i", inputPath,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-an", "-sn", "-dn", "-frames:v", "1",
		"-c:v", "ffv1", "-f", "nut", out)
	return args
}

// videoEncodeArgs returns the ffmpeg arguments that read the samples listed
// in the concat file list and write both animated WebP thumbnails from the
// first count of them. The samples' own timestamps are replaced by setpts.
func videoEncodeArgs(list, smallPath, largePath string, count, sizeSmall, sizeLarge, quality, threads int) []string {
	frameCount := strconv.Itoa(count)
	args := append(ffmpegArgs(threads),
		"-protocol_whitelist", "file",
		"-f", "concat", "-safe", "0", "-i", list,
		"-filter_complex", fmt.Sprintf(
			"[0:v]setpts=N/(2*TB),split=2[selected_small][selected_large];[selected_small]%s[small];[selected_large]%s[large]",
			videoScaleFilter(sizeSmall),
			videoScaleFilter(sizeLarge),
		))
	for _, out := range []struct{ label, path string }{{"[small]", smallPath}, {"[large]", largePath}} {
		args = append(args,
			"-map", out.label,
			"-frames:v", frameCount,
			"-loop", "0",
			"-c:v", "libwebp",
			"-quality", strconv.Itoa(quality),
			"-r", "2",
			out.path)
	}
	return args
}

func videoScaleFilter(maxSize int) string {
	return fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", maxSize, maxSize)
}

// videoSample returns the indices of the sampled frames of a stream of the
// given length: evenly spaced, i*frames/count for count samples.
func videoSample(frames int) []int {
	if frames <= 0 {
		return []int{0}
	}
	count := min(frames, maxVideoSampleFrames)
	indices := make([]int, 0, count)
	for i := range count {
		indices = append(indices, i*frames/count)
	}
	return indices
}
