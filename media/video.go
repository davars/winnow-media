package media

import (
	"context"
	"os"
	"path/filepath"
)

// GenerateVideoThumbnails samples fifteen frames of a video at the centres of
// equal intervals of its duration and writes them as two animated WebPs whose
// longest sides are at most sizeSmall and sizeLarge, at the given quality,
// each frame shown for half a second. Frames are sampled, tone-mapped, sized
// and stripped as Processor.VideoFrames does, and a frame identical to the one
// before it once resized is dropped.
//
// It requires smallPath and largePath to end in .webp and their directory to
// be writable, where it stages samples in a subdirectory it removes. The video
// stream is ffprobe's choice; streamIndex and frames are ignored. A stream
// over the package's caps is refused with ErrTooLarge before any frame is
// decoded.
func (e Env) GenerateVideoThumbnails(ctx context.Context, inputPath, smallPath, largePath string, streamIndex, frames, sizeSmall, sizeLarge, quality int) error {
	dir, err := os.MkdirTemp(filepath.Dir(smallPath), ".video-thumbs-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	outputs := []string{smallPath, largePath}
	e.Stage = func(string) (string, func(), error) {
		path := outputs[0]
		outputs = outputs[1:]
		return path, func() { _ = os.Remove(path) }, nil
	}
	src, err := e.open(ctx, openRequest{
		path: inputPath, dec: sniffedVideoDecoder(inputPath), pinned: true, scratch: dir,
		video: &videoRequest{},
	})
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = src.encode(ctx, 0, []encoding{thumbnailEncoding(sizeSmall, quality), thumbnailEncoding(sizeLarge, quality)})
	return err
}

// thumbnailEncoding is the encoding of an animated WebP thumbnail whose longest
// side is at most edge.
func thumbnailEncoding(edge, quality int) encoding {
	return encoding{format: WebP, ext: ".webp", quality: quality, maxEdge: edge, maxArea: edge * edge}
}
