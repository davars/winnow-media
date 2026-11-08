package media

import (
	"context"
	"errors"
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
// failing yields an error naming both. The Env adds its memory profile's
// limit flags, after a leading "identify" if there is one; buildArgs must not
// include them, and must produce identify or convert-style arguments. Other
// magick subcommands (compare, composite, montage, mogrify, stream) are not
// supported. A run that reaches ImageMagick's time limit fails with an error
// wrapping ErrTimeLimit, even though ImageMagick exits 0, and is not retried;
// this needs the Runner to fill Result.Stderr.
func (e Env) RunMagickWithCoderFallback(ctx context.Context, mimeType string, buildArgs func(coder string) []string) ([]byte, error) {
	first, err := e.runMagick(ctx, buildArgs("")...)
	if err == nil {
		return first.Stdout, nil
	}
	coder, ok := CoderForMIME(mimeType)
	// A time limit is not a decode failure; a retry would reach it again.
	if !ok || errors.Is(err, ErrTimeLimit) {
		return first.Stdout, err
	}
	retried, retryErr := e.runMagick(ctx, buildArgs(coder)...)
	if retryErr != nil {
		return retried.Stdout, fmt.Errorf("magick: sniffed decode failed (%w); retried with coder %q, which also failed: %w", err, coder, retryErr)
	}
	return retried.Stdout, nil
}

// LimitArgs returns the resource-limit flags that bound one magick process
// under HighMemory; it is HighMemory.LimitArgs().
func LimitArgs() []string { return HighMemory.LimitArgs() }

// LimitArgs returns the resource-limit flags for one magick process under the
// profile: memory, map, area and list length bound the process, and time
// bounds each image (its clock starts when the image is created, and
// ImageMagick stops work on an image that has run past it, exiting 0 in most
// cases; see ErrTimeLimit). It does not bound the process's total time.
// Callers building their own magick invocations over the package's artifacts
// should start them with these (after a leading "identify"). The slice is
// fresh on every call. It panics on an undefined profile; check Valid first
// when the value comes from outside the program.
func (p MemoryProfile) LimitArgs() []string {
	s, err := p.settings()
	if err != nil {
		panic(err)
	}
	return []string{
		"-limit", "memory", s.memory,
		"-limit", "map", s.mapLimit,
		"-limit", "area", policyArea,
		"-limit", "list-length", policyListLength,
		"-limit", "time", strconv.Itoa(policyTimeSeconds),
	}
}

// Valid reports whether p is a defined profile.
func (p MemoryProfile) Valid() bool {
	_, ok := memoryProfiles[p]
	return ok
}

// runMagick runs magick with args under the Env's memory profile: the limit
// flags follow a leading "identify" subcommand, which must stay first, and
// otherwise start the arguments. Every run that reads or writes an image goes
// through it, so none reports success for a run that reached the time limit: ImageMagick
// exits 0 then, and the error wraps ErrTimeLimit whatever the exit status.
func (e Env) runMagick(ctx context.Context, args ...string) (Result, error) {
	if !e.Memory.Valid() {
		return Result{}, fmt.Errorf("unknown memory profile %d", int(e.Memory))
	}
	limit := e.Memory.LimitArgs()
	full := make([]string, 0, len(limit)+len(args))
	if len(args) > 0 && args[0] == "identify" {
		full, args = append(full, "identify"), args[1:]
	}
	full = append(append(full, limit...), args...)
	res, err := e.Run(ctx, "magick", full...)
	return res, timeLimitError(res.Stderr, err)
}

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
	args := []string{"identify", "-ping", "-format", "%s %w %h\n", path}
	res, err := e.runMagick(ctx, args...)
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
