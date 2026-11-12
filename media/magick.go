package media

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// runMagick runs magick with args under the toolEnv's resource limits: the limit
// flags follow a leading "identify" subcommand, which must stay first, and
// otherwise start the arguments. Every run that reads or writes an image goes
// through it, so none reports success for a run that reached the time limit: ImageMagick
// exits 0 then, and the error wraps ErrTimeLimit whatever the exit status.
func (e toolEnv) runMagick(ctx context.Context, args ...string) (runResult, error) {
	limit := e.resources.limitArgs()
	full := make([]string, 0, len(limit)+len(args))
	if len(args) > 0 && args[0] == "identify" {
		full, args = append(full, "identify"), args[1:]
	}
	full = append(append(full, limit...), args...)
	res, err := e.run(ctx, "magick", full...)
	return res, timeLimitError(res.Stderr, err)
}

// identifyScenes runs "magick identify" over an encoded artifact and returns
// the delay of each scene in centiseconds as the file states it, and the
// first scene's canvas width and height. A still reports one scene with a
// delay of 0. It reads headers only.
func (e toolEnv) identifyScenes(ctx context.Context, path string) (delaysCS []int, width, height int, err error) {
	args := []string{"identify", "-ping", "-format", "%s %w %h %T\n", path}
	res, err := e.runMagick(ctx, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("identifying artifact: %w", err)
	}
	trimmed := strings.TrimRight(string(res.Stdout), "\n")
	if trimmed == "" {
		return nil, 0, 0, fmt.Errorf("identify returned no output")
	}
	for _, line := range strings.Split(trimmed, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return nil, 0, 0, fmt.Errorf("identify line %q missing fields", line)
		}
		delay, err := strconv.Atoi(fields[3])
		if err != nil {
			return nil, 0, 0, fmt.Errorf("parsing delay from %q: %w", line, err)
		}
		if delaysCS == nil {
			if width, err = strconv.Atoi(fields[1]); err != nil {
				return nil, 0, 0, fmt.Errorf("parsing width from %q: %w", line, err)
			}
			if height, err = strconv.Atoi(fields[2]); err != nil {
				return nil, 0, 0, fmt.Errorf("parsing height from %q: %w", line, err)
			}
		}
		delaysCS = append(delaysCS, delay)
	}
	return delaysCS, width, height, nil
}
