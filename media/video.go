package media

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// maxVideoSampleFrames caps the frames a video animation samples;
// it is tied to the animation's playback rate.
const maxVideoSampleFrames = 15

// GenerateVideoThumbnails runs ffmpeg once to write two animated WebP
// thumbnails of one video stream, at maximum sizes sizeSmall and sizeLarge and
// the given WebP quality. frames is the stream's frame count, from which an
// evenly spaced sample is taken; zero or less samples the first frame only.
func (e Env) GenerateVideoThumbnails(ctx context.Context, inputPath, smallPath, largePath string, streamIndex, frames, sizeSmall, sizeLarge, quality int) error {
	_, err := e.Run(ctx, "ffmpeg", VideoThumbnailArgs(inputPath, smallPath, largePath, streamIndex, frames, sizeSmall, sizeLarge, quality)...)
	return err
}

// VideoThumbnailArgs returns the ffmpeg arguments GenerateVideoThumbnails
// runs.
func VideoThumbnailArgs(inputPath, smallPath, largePath string, streamIndex, frames, sizeSmall, sizeLarge, quality int) []string {
	sample := videoSample(frames)
	frameCount := strconv.Itoa(len(sample.indices))
	stream := fmt.Sprintf("0:%d", streamIndex)
	return []string{
		"-y",
		"-i", inputPath,
		"-an",
		"-filter_complex", fmt.Sprintf(
			"[%s]select='%s',setpts=N/(2*TB),split=2[selected_small][selected_large];[selected_small]%s[small];[selected_large]%s[large]",
			stream, sample.selectExpr,
			videoScaleFilter(sizeSmall),
			videoScaleFilter(sizeLarge),
		),
		"-map", "[small]",
		"-frames:v", frameCount,
		"-loop", "0",
		"-c:v", "libwebp",
		"-quality", fmt.Sprintf("%d", quality),
		"-r", "2",
		smallPath,
		"-map", "[large]",
		"-frames:v", frameCount,
		"-loop", "0",
		"-c:v", "libwebp",
		"-quality", fmt.Sprintf("%d", quality),
		"-r", "2",
		largePath,
	}
}

func videoScaleFilter(maxSize int) string {
	return fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", maxSize, maxSize)
}

type videoSamplePlan struct {
	indices    []int
	selectExpr string
}

func videoSample(frames int) videoSamplePlan {
	if frames <= 0 {
		return videoSamplePlan{indices: []int{0}, selectExpr: "eq(n\\,0)"}
	}
	count := min(frames, maxVideoSampleFrames)
	indices := make([]int, 0, count)
	terms := make([]string, 0, count)
	for i := range count {
		index := i * frames / count
		indices = append(indices, index)
		terms = append(terms, fmt.Sprintf("eq(n\\,%d)", index))
	}
	return videoSamplePlan{indices: indices, selectExpr: strings.Join(terms, "+")}
}
