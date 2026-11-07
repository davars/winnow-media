package media

// This file exists only on the measure-caps branch. It gives the still-cap
// measurement harness (internal/cmd/capsmeasure) the pieces of the package it
// needs and does not export: a Processor whose policy also reads and writes
// the still formats under measurement, an Env that runs through its sandbox,
// and the fused JPEG still encode with and without a jpeg:size hint.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/davars/winnow-media/tool"
)

// capsCoders are the ImageMagick coders the measured still formats use.
var capsCoders = []string{"JPEG", "JPG", "PNG", "TIFF", "TIF", "WEBP", "HEIC", "AVIF", "JXL", "ICC", "INFO"}

// NewCapsProcessor is NewProcessor with the policy widened to the coders of
// capsCoders; everything else (sandbox probe, resource ceilings, rlimits) is
// the Processor's own.
func NewCapsProcessor(ctx context.Context) (*Processor, error) {
	if err := sandboxAvailable(); err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	if !initCalled.Load() {
		return nil, fmt.Errorf("processor: %w", errNoInit)
	}
	if err := probeSandbox(ctx); err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	tools := map[string]string{}
	for _, name := range []string{"magick", "ffmpeg", "ffprobe", webpFramesTool} {
		path, err := tool.Resolve(name)
		if err != nil {
			return nil, fmt.Errorf("processor: %w", err)
		}
		if tools[name], err = filepath.Abs(path); err != nil {
			return nil, fmt.Errorf("processor: %w", err)
		}
	}
	dir, err := os.MkdirTemp("", "media-processor-")
	if err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	p := &Processor{dir: dir, tools: tools}
	p.run = p.runTool
	p.compose = p.composeFrames
	rules := processorPolicy()
	last := rules[len(rules)-1]
	rules = rules[:len(rules)-1]
	for _, c := range capsCoders {
		rules = append(rules, policyRule{domain: "coder", rights: "read|write", pattern: c})
	}
	rules = append(rules, last)
	if err := p.install(rules); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("processor: %w", err)
	}
	if err := p.verifyPolicy(ctx, rules); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("processor: %w", err)
	}
	return p, nil
}

// CapsEnv returns an Env that runs every tool through the Processor's sandbox
// and stages artifacts in stageDir.
func (p *Processor) CapsEnv(stageDir string) Env {
	return Env{
		Run: p.runTool,
		Stage: func(ext string) (string, func(), error) {
			f, err := os.CreateTemp(stageDir, "stage-*"+ext)
			if err != nil {
				return "", nil, err
			}
			path := f.Name()
			_ = f.Close()
			return path, func() { _ = os.Remove(path) }, nil
		},
	}
}

// CapsHint is the jpeg:size hint for a w x h source and the 2x margin: the
// sampling-factor numerator N and the WxH of the decode. N is
// min(8, ceil(8*margin*target/side)) on both sides, at least 1.
func CapsHint(w, h, maxEdge, maxArea, margin int) (n int, hint string) {
	tw, th, _ := fitSize(w, h, maxEdge, maxArea)
	n = 1
	for _, p := range [][2]int{{tw, w}, {th, h}} {
		need := (8*margin*p[0] + p[1] - 1) / p[1]
		n = max(n, min(8, need))
	}
	hw, hh := (w*n+7)/8, (h*n+7)/8
	return n, strconv.Itoa(hw) + "x" + strconv.Itoa(hh)
}

// CapsJPEGStill runs the one fused encode of a still to a JPEG at maxEdge
// (and the default area and quality), the way Processor.Still builds it:
// <coder>:<path>[0] through encodeArgs. hint, when not empty, is added as
// -define jpeg:size=<hint> before the read. It returns the output size.
func (e Env) CapsJPEGStill(ctx context.Context, coder, path string, profiled bool, hint string, maxEdge int) (width, height int, err error) {
	out, cleanup, err := e.Stage(".jpg")
	if err != nil {
		return 0, 0, err
	}
	defer cleanup()
	destination := ""
	if profiled {
		scratch, err := os.MkdirTemp(filepath.Dir(out), ".caps-still-*")
		if err != nil {
			return 0, 0, err
		}
		defer func() { _ = os.RemoveAll(scratch) }()
		if destination, err = placeSRGB(scratch); err != nil {
			return 0, 0, err
		}
	}
	enc := encoding{format: JPEG, ext: ".jpg", quality: defaultQuality, maxEdge: maxEdge, maxArea: defaultMaxArea}
	args := encodeArgs([]encodeFrame{{input: coder + ":" + path + "[0]", convertTo: destination}},
		encodeSource{}, enc, encodeOutput{kind: outputStill, path: out})
	if hint != "" {
		n := len(LimitArgs())
		args = slices.Insert(args, n, "-define", "jpeg:size="+hint)
	}
	res, err := e.Run(ctx, "magick", args...)
	if err != nil {
		return 0, 0, err
	}
	frames, err := parseEncoded(res.Stdout)
	if err != nil {
		return 0, 0, err
	}
	if len(frames) != 1 {
		return 0, 0, fmt.Errorf("still encode produced %d scenes", len(frames))
	}
	return frames[0].width, frames[0].height, nil
}
