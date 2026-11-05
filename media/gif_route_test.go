package media

import (
	"context"
	"encoding/binary"
	"image"
	"slices"
	"strings"
	"testing"
)

// forbidCompositor fails the test if the processor starts the GIF
// compositor, and restores it when the test ends.
func forbidCompositor(t *testing.T, p *Processor) {
	t.Helper()
	old := p.compose
	t.Cleanup(func() { p.compose = old })
	p.compose = func(context.Context, composeRequest, func(string) error) (*Usage, error) {
		t.Error("the compositor was started")
		return nil, nil
	}
}

func TestGIFFramesGoToMagickInBatches(t *testing.T) {
	env := requireMagick(t)
	p := requireProcessor(t)
	// Two 64x48 frames per run.
	old := encodeBatchPixels
	encodeBatchPixels = 2 * 64 * 48
	t.Cleanup(func() { encodeBatchPixels = old })
	rec := record(p)
	images, out := framesInto(t, p, writeGIF(t, env, solid, 12), Encoding{}, 6)
	if len(images) != 6 {
		t.Fatalf("%d images, want 6", len(images))
	}
	var frames []int
	for _, im := range images {
		frames = append(frames, im.Frame)
	}
	if !slices.Equal(frames, []int{0, 2, 4, 6, 8, 10}) {
		t.Errorf("source frames %v", frames)
	}
	inputs, runs := 0, 0
	for _, call := range rec.calls {
		if call[0] != "magick" || slices.Contains(call, "identify") {
			continue
		}
		n := 0
		for _, a := range call[1:] {
			if strings.HasPrefix(a, "pam:") {
				n++
			}
		}
		if n > 2 {
			t.Errorf("magick run names %d frames, the batch is 2: %q", n, call)
		}
		inputs += n
		runs++
	}
	if inputs != 6 || runs != 3 {
		t.Errorf("%d inputs in %d runs, want 6 in 3", inputs, runs)
	}
	if got := dirNames(t, out); len(got) != 6 {
		t.Errorf("output dir holds %v", got)
	}
}

func TestScanGIF(t *testing.T) {
	good := buildGIF(t, 24, 20, paletteA, []gifFrameSpec{
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA},
		{rect: image.Rect(4, 3, 16, 12), disposal: 1, delay: 5, palette: paletteB},
	})
	if l, err := scanGIF(good, 2); err != nil || l.frames != 2 || l.width != 24 || l.height != 20 {
		t.Fatalf("scan of a good GIF = %+v, %v", l, err)
	}
	wide := append([]byte{}, good...)
	binary.LittleEndian.PutUint16(wide[6:], 8)
	cases := map[string][]byte{
		"truncated in a frame":       good[:len(good)/2],
		"no trailer":                 good[:len(good)-1],
		"truncated header":           good[:9],
		"frame outside the canvas":   wide,
		"unknown block":              append(append([]byte{}, good[:len(good)-1]...), 0x55, 0x3B),
		"not a GIF":                  []byte("GIF90a" + strings.Repeat("\x00", 20)),
		"no frames":                  append(append([]byte{}, good[:10]...), 0, 0, 0, 0x3B),
		"more frames than the limit": good,
	}
	for name, data := range cases {
		limit := 2
		if name == "more frames than the limit" {
			limit = 1
		}
		if _, err := scanGIF(data, limit); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestBrokenGIFsFailCleanly runs GIFs the header read accepts but the
// compositor cannot decode through AnimationFrames.
func TestBrokenGIFsFailCleanly(t *testing.T) {
	requireMagick(t)
	p := requireProcessor(t)
	good := buildGIF(t, 24, 20, paletteA, []gifFrameSpec{
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA},
		{rect: image.Rect(4, 3, 16, 12), disposal: 1, delay: 5, palette: paletteB, shift: 1},
		{rect: image.Rect(0, 0, 24, 20), disposal: 1, delay: 5, palette: paletteA, shift: 2},
	})
	wide := append([]byte{}, good...)
	binary.LittleEndian.PutUint16(wide[6:], 8)
	for name, data := range map[string][]byte{
		"truncated":                good[:len(good)-40],
		"no trailer":               good[:len(good)-1],
		"garbage after last frame": append(append([]byte{}, good[:len(good)-1]...), 0x55, 0x3B),
		"frame outside the canvas": wide,
	} {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			_, _, err := p.AnimationFrames(context.Background(), writeFile(t, "x.gif", data), Encoding{}, 0, out)
			if err == nil {
				t.Fatal("no error")
			}
			if got := dirNames(t, out); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}
}

func TestRunCompositorRefusesBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"too few":            {"a"},
		"not a count":        {"a", "b", "x", "1", "1", "1", "0"},
		"frame out of range": {"a", "b", "1", "2", "1", "1", "5"},
		"missing file":       {"/nonexistent.gif", t.TempDir(), "1", "1", "1", "1", "0"},
	} {
		if err := runCompositor(args, &strings.Builder{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
