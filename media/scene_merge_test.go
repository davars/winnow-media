package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// grayGIF writes a GIF of 64x64 gray frames, frame k showing pattern[k]
// with a delay of delays[k] centiseconds.
func grayGIF(t *testing.T, pattern []func(x, y int) uint8, delays []int) string {
	t.Helper()
	palette := make(color.Palette, 256)
	for i := range palette {
		palette[i] = color.Gray{Y: uint8(i)}
	}
	anim := &gif.GIF{}
	for k, f := range pattern {
		img := image.NewPaletted(image.Rect(0, 0, 64, 64), palette)
		for y := range 64 {
			for x := range 64 {
				img.SetColorIndex(x, y, f(x, y))
			}
		}
		anim.Image = append(anim.Image, img)
		anim.Delay = append(anim.Delay, delays[k])
		// Without a disposal the encoder omits the graphic control extension of
		// a frame after the first, and readers give it the previous delay.
		anim.Disposal = append(anim.Disposal, gif.DisposalNone)
	}
	path := filepath.Join(t.TempDir(), "in.gif")
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Frames that differ from the one before by less than the WebP writer's lossy
// tolerance are merged into its scene; the mapping must describe the scenes
// the file holds, each at the time of the frame it shows.
func TestEncodeMapsTheScenesTheWebPWriterMerged(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	env := requireMagick(t)
	ramp := func(noise int) func(x, y int) uint8 {
		return func(x, y int) uint8 { return uint8(x*3 + y + noise*((x+y)%2)) }
	}
	checker := func(x, y int) uint8 { return uint8(((x/8+y/8)%2)*200 + 20) }
	for _, c := range []struct {
		name    string
		pattern []func(x, y int) uint8
		delays  []int
		// scenes, when set, is the delay of each scene the file must hold.
		scenes []int
	}{
		{"merged runs", []func(x, y int) uint8{ramp(0), ramp(1), ramp(2), checker, ramp(0), ramp(1)}, []int{3, 4, 5, 6, 7, 8}, nil},
		{"every frame merged", []func(x, y int) uint8{ramp(0), ramp(1), ramp(2), ramp(1), ramp(0), ramp(1)}, []int{3, 4, 5, 6, 7, 8}, nil},
		{"delays of 0 and 1 cs show as 10", []func(x, y int) uint8{ramp(0), ramp(1), checker, checker, ramp(0), ramp(1)}, []int{0, 1, 5, 0, 1, 8}, nil},
		{"exact duplicates keep their time", []func(x, y int) uint8{ramp(0), ramp(0), checker, checker}, []int{5, 5, 5, 5}, []int{10, 10}},
	} {
		t.Run(c.name, func(t *testing.T) {
			delays := c.delays
			src := openSourceOn(t, requireProcessor(t), grayGIF(t, c.pattern, delays), OpenOptions{})
			dir := t.TempDir()
			enc := Encoding{Format: WebP, Quality: 75, Animate: true}
			arts, _, err := src.Encode(context.Background(), dir, Select{Frames: All()}, enc)
			if err != nil {
				t.Fatal(err)
			}
			written, _, _, err := env.identifyScenes(context.Background(), filepath.Join(dir, arts[0].Name))
			if err != nil {
				t.Fatal(err)
			}
			if len(written) >= len(c.pattern) {
				t.Fatalf("the writer kept all %d scenes %v; the frames do not exercise the merge", len(written), written)
			}
			if c.scenes != nil && !slices.Equal(written, c.scenes) {
				t.Fatalf("file holds scenes %v, want %v", written, c.scenes)
			}
			mapping := arts[0].Frames
			if len(mapping) != len(written) {
				t.Fatalf("mapping describes %d frames, the file holds %d scenes %v", len(mapping), len(written), written)
			}
			if mapping[0].Index != 0 {
				t.Errorf("first scene shows frame %d, want 0", mapping[0].Index)
			}
			if len(written) == 1 {
				return
			}
			startMS := func(frame int) int64 {
				var ms int64
				for _, d := range delays[:frame] {
					ms += int64(displayDelayCS(d)) * 10
				}
				return ms
			}
			for i, f := range mapping {
				if f.TimeMS != startMS(f.Index) {
					t.Errorf("scene %d shows frame %d at %d ms, want %d", i, f.Index, f.TimeMS, startMS(f.Index))
				}
				end := startMS(len(delays))
				if i+1 < len(mapping) {
					end = mapping[i+1].TimeMS
				}
				if got := (end - f.TimeMS) / 10; got != int64(written[i]) {
					t.Errorf("scene %d lasts %d cs in the file, its frames span %d", i, written[i], got)
				}
			}
		})
	}
}

func TestSceneStarts(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name           string
		given, written []int
		want           []int
		fail           bool
	}{
		{name: "nothing merged", given: []int{3, 4, 5}, written: []int{3, 4, 5}, want: []int{0, 1, 2}},
		{name: "merged runs", given: []int{3, 4, 5, 6, 7}, written: []int{12, 6, 7}, want: []int{0, 3, 4}},
		{name: "merged last run", given: []int{3, 4, 5}, written: []int{3, 9}, want: []int{0, 1}},
		{name: "all merged is a still", given: []int{3, 4, 5}, written: []int{0}, want: []int{0}},
		{name: "all merged in an animation", given: []int{3, 4, 5}, written: []int{12}, want: []int{0}},
		{name: "one scene of another length", given: []int{3, 4, 5}, written: []int{7}, fail: true},
		{name: "empty scene", given: []int{3, 4, 5}, written: []int{3, 0, 9}, fail: true},
		{name: "scene starts between frames", given: []int{3, 4, 5}, written: []int{5, 7}, fail: true},
		{name: "scene ends between frames", given: []int{3, 4, 5}, written: []int{5, 5}, fail: true},
		{name: "total too long", given: []int{3, 4}, written: []int{3, 5}, fail: true},
		{name: "total too short", given: []int{3, 4, 5}, written: []int{3, 4}, fail: true},
		{name: "more scenes than frames", given: []int{3, 4}, written: []int{3, 4, 5}, fail: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := sceneStarts(c.given, c.written)
			if c.fail != (err != nil) {
				t.Fatalf("sceneStarts(%v, %v) = %v, %v; want failure %v", c.given, c.written, got, err, c.fail)
			}
			if !c.fail && !slices.Equal(got, c.want) {
				t.Errorf("sceneStarts(%v, %v) = %v, want %v", c.given, c.written, got, c.want)
			}
		})
	}
}
