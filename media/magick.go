package media

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// coderByMIME maps a mime type to the ImageMagick coder that decodes it, for
// formats magick's own signature sniffing fails to recognize on an
// extensionless input path. Only mime types confirmed failing bare are
// listed: an unlisted mime type gets no override.
var coderByMIME = map[string]string{
	"image/x-tga":              "tga",
	"image/vnd.microsoft.icon": "ico",
	"image/x-win-bitmap":       "cur",
	"image/g3fax":              "g3",
}

// CoderForMIME returns the ImageMagick coder to force, as an input prefix
// ("<coder>:<path>"), for a mime type whose bytes magick cannot identify
// without an extension. ok is false for every other mime type, including "".
func CoderForMIME(mimeType string) (coder string, ok bool) {
	coder, ok = coderByMIME[mimeType]
	return coder, ok
}

// RunMagickWithCoderFallback runs one magick invocation built by buildArgs
// with an empty coder, retrying once with the coder CoderForMIME names for
// mimeType when the bare attempt fails and one exists. Without a mapped
// coder the original output and error are returned unchanged. Both attempts
// failing yields an error naming both.
func (e Env) RunMagickWithCoderFallback(ctx context.Context, mimeType string, buildArgs func(coder string) []string) ([]byte, error) {
	first, err := e.Run(ctx, "magick", buildArgs("")...)
	if err == nil {
		return first.Stdout, nil
	}
	coder, ok := CoderForMIME(mimeType)
	if !ok {
		return first.Stdout, err
	}
	retried, retryErr := e.Run(ctx, "magick", buildArgs(coder)...)
	if retryErr != nil {
		return retried.Stdout, fmt.Errorf("magick: sniffed decode failed (%w); retried with coder %q, which also failed: %v", err, coder, retryErr)
	}
	return retried.Stdout, nil
}

// LimitArgs returns the resource-limit flags that bound one magick process:
// memory, map, area and time. Every magick invocation the package builds
// starts with them, and callers building their own invocations over the
// package's artifacts should too. The slice is fresh on every call.
func LimitArgs() []string {
	return append([]string{
		"-limit", "memory", "1GiB",
		"-limit", "map", "2GiB",
		"-limit", "area", "512MP",
		"-limit", "time", "300",
	}, ExtraLimitArgs...)
}

// ExtraLimitArgs is appended to LimitArgs. It exists on the measurement
// branch only, to run the operations under tighter limits.
var ExtraLimitArgs []string

// DeleteExpr renders the magick -delete argument dropping every one of
// sourceFrames frames that keep does not name. It returns "" when nothing is
// dropped, in which case the caller must omit -delete rather than pass an
// empty argument.
func DeleteExpr(keep []int, sourceFrames int) string {
	kept := make(map[int]bool, len(keep))
	for _, idx := range keep {
		kept[idx] = true
	}
	var drop []string
	for i := range sourceFrames {
		if !kept[i] {
			drop = append(drop, strconv.Itoa(i))
		}
	}
	return strings.Join(drop, ",")
}

// IdentifyArtifact runs "magick identify" over an encoded artifact and
// returns its scene count and the first scene's canvas width and height. It
// is the post-encode check: a still must hold exactly one scene.
func (e Env) IdentifyArtifact(ctx context.Context, path string) (scenes, width, height int, err error) {
	args := append([]string{"identify"}, LimitArgs()...)
	args = append(args, "-format", "%s %w %h\n", path)
	res, err := e.Run(ctx, "magick", args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("identifying artifact: %w", err)
	}
	trimmed := strings.TrimRight(string(res.Stdout), "\n")
	if trimmed == "" {
		return 0, 0, 0, fmt.Errorf("identify returned no output")
	}
	lines := strings.Split(trimmed, "\n")
	fields := strings.Fields(lines[0])
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("identify line %q missing fields", lines[0])
	}
	width, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parsing width from %q: %w", lines[0], err)
	}
	height, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parsing height from %q: %w", lines[0], err)
	}
	return len(lines), width, height, nil
}
