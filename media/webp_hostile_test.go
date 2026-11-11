package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/davars/winnow-media/tool"
)

// anmfOffsets returns the offsets of the ANMF chunks of an animated WebP.
func anmfOffsets(data []byte) []int {
	var offs []int
	for pos := 12; pos+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		if string(data[pos:pos+4]) == "ANMF" {
			offs = append(offs, pos)
		}
		pos += 8 + size + size&1
	}
	return offs
}

func put24(b []byte, n int) {
	b[0], b[1], b[2] = byte(n), byte(n>>8), byte(n>>16)
}

func cloneBytes(b []byte) []byte { return append([]byte{}, b...) }

// hostileWebPs returns damaged animated WebPs by name, each built from a
// six-frame animation, with the canvas size and frame count the tool must be
// told for it.
func hostileWebPs(t *testing.T) map[string]hostileWebP {
	t.Helper()
	good, err := os.ReadFile(buildWebP(t, webpCase{codec: "lossless", blend: true, frames: 6}))
	if err != nil {
		t.Fatal(err)
	}
	frames := anmfOffsets(good)
	if len(frames) != 6 {
		t.Fatalf("fixture has %d frames", len(frames))
	}
	// An ANMF payload starts with x/2, y/2, width-1, height-1 and duration,
	// three bytes each.
	patchFrame := func(frame, field, value int) []byte {
		d := cloneBytes(good)
		put24(d[frames[frame]+8+3*field:], value)
		return d
	}
	canvasSmall := cloneBytes(good)
	put24(canvasSmall[24:], 9)
	put24(canvasSmall[27:], 9)
	garbageFrame := cloneBytes(good)
	copy(garbageFrame[frames[2]+8+16:], "ZZZZ")
	garbageData := cloneBytes(good)
	for i := frames[3] + 8 + 24; i < frames[4]; i++ {
		garbageData[i] = 0xFF
	}
	corruptPixels := cloneBytes(good)
	for i := frames[3] + 8 + 24 + 12; i < frames[4]; i++ {
		corruptPixels[i] = 0xFF
	}
	overlong := cloneBytes(good)
	binary.LittleEndian.PutUint32(overlong[frames[1]+4:], 1<<30)
	headers := webpOutcome{err: "reading headers"}
	notDecodable := map[string]webpOutcome{
		"AnimationFrames":   {err: "is not a decodable animated WebP"},
		"AnimationFramesAt": {err: "is outside the animation's 1 frames"},
	}
	return map[string]hostileWebP{
		"header only":               {data: good[:30], w: 40, h: 30, frames: 6, refusal: "decodable", through: both(headers)},
		"truncated in a frame":      {data: good[:frames[3]+40], w: 40, h: 30, frames: 6, refusal: "decodable", through: both(headers)},
		"half the file":             {data: good[:len(good)/2], w: 40, h: 30, frames: 6, refusal: "decodable", through: both(headers)},
		"cut between frames":        {data: good[:frames[4]], w: 40, h: 30, frames: 6, refusal: "decodable", through: both(headers)},
		"frame outside the canvas":  {data: patchFrame(2, 0, 500), w: 40, h: 30, frames: 6, refusal: "decodable", through: notDecodable},
		"canvas smaller than frame": {data: canvasSmall, w: 10, h: 10, frames: 6, refusal: "decodable", through: notDecodable},
		// How many scenes ImageMagick reads from these two damaged files is its
		// own leniency, so the contract is a refusal or at most the frames asked
		// for.
		"garbage frame header": {data: garbageFrame, w: 40, h: 30, frames: 6, refusal: "header read found", through: map[string]webpOutcome{
			"AnimationFrames": {atMost: 6}, "AnimationFramesAt": {atMost: 3},
		}},
		"garbage frame data": {data: garbageData, w: 40, h: 30, frames: 6, refusal: "decodable", through: notDecodable},
		"corrupt frame pixels": {data: corruptPixels, w: 40, h: 30, frames: 6, refusal: "decoding frame", through: map[string]webpOutcome{
			"AnimationFrames": {atMost: 6}, "AnimationFramesAt": {atMost: 3},
		}},
		"chunk size past the end": {data: overlong, w: 40, h: 30, frames: 6, refusal: "decodable", through: notDecodable},
	}
}

type hostileWebP struct {
	data         []byte
	w, h, frames int
	// refusal is what the tool's message must contain when it is told the
	// undamaged file's canvas and frame count.
	refusal string
	// through is what each entry of animationImageEntries does with the file.
	through map[string]webpOutcome
}

// webpOutcome is what an operation does with a damaged file: it writes images
// frames, or, when err is set, fails with a message containing it and writes
// none, or, when atMost is set, either fails or writes no more than atMost
// frames.
type webpOutcome struct {
	images int
	err    string
	atMost int
}

func both(o webpOutcome) map[string]webpOutcome {
	return map[string]webpOutcome{"AnimationFrames": o, "AnimationFramesAt": o}
}

// TestHostileAnimatedWebPsAreRefusedOrBounded runs damaged animated WebPs
// through every operation that writes their frames. Each gives the outcome
// its fixture states, or, where that depends on how far ImageMagick reads the
// file, fails or writes no more frames than were asked for. None exceeds the
// time and memory bounds, and a failure leaves nothing behind.
func TestHostileAnimatedWebPsAreRefusedOrBounded(t *testing.T) {
	requireWebPBuilders(t)
	p := requireProcessor(t)
	for name, c := range hostileWebPs(t) {
		for _, entry := range animationImageEntries(p) {
			want, ok := c.through[entry.name]
			if !ok {
				t.Fatalf("%s: no expected outcome for %s", name, entry.name)
			}
			t.Run(name+"/"+entry.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				path := writeFile(t, "x.webp", c.data)
				out := t.TempDir()
				images, usage, err := entry.run(ctx, path, out)
				if ctx.Err() != nil {
					t.Fatalf("timed out: %v", err)
				}
				if usage.MaxRSS > 512<<20 {
					t.Errorf("peak RSS %d bytes, the bound is %d", usage.MaxRSS, int64(512<<20))
				}
				if want.atMost > 0 {
					if err != nil {
						if len(images) != 0 {
							t.Errorf("%d images returned with the error", len(images))
						}
						if got := dirNames(t, out); len(got) != 0 {
							t.Errorf("failed, and the output dir holds %v", got)
						}
						return
					}
					if len(images) > want.atMost {
						t.Errorf("%d images, want at most %d", len(images), want.atMost)
					}
					if got := dirNames(t, out); len(got) != len(images) {
						t.Errorf("the output dir holds %v, want %d files", got, len(images))
					}
					return
				}
				if want.err != "" {
					if err == nil || !strings.Contains(err.Error(), want.err) {
						t.Errorf("err = %v, want it to say %q", err, want.err)
					}
					if len(images) != 0 {
						t.Errorf("%d images returned with the error", len(images))
					}
					if got := dirNames(t, out); len(got) != 0 {
						t.Errorf("failed, and the output dir holds %v", got)
					}
					return
				}
				if err != nil || len(images) != want.images {
					t.Errorf("%d images, err %v; want %d images", len(images), err, want.images)
				}
				if got := dirNames(t, out); len(got) != want.images {
					t.Errorf("the output dir holds %v, want %d files", got, want.images)
				}
			})
		}
	}
}

// TestWebPFramesRefusesDamagedAnimations runs the tool directly, told the
// canvas and frame count of the undamaged file (the header read ImageMagick
// would have had to make), over the damaged files, and checks that it fails
// with nothing written.
func TestWebPFramesRefusesDamagedAnimations(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	exe, err := tool.Resolve(webpFramesTool)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range hostileWebPs(t) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(exe, writeFile(t, "x.webp", c.data), dir, "100",
				strconv.Itoa(c.w), strconv.Itoa(c.h), strconv.Itoa(c.frames), "0,1,2,3,4,5", "60000")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			type result struct {
				out []byte
				err error
			}
			done := make(chan result, 1)
			go func() { out, err := cmd.Output(); done <- result{out, err} }()
			select {
			case r := <-done:
				if r.err == nil {
					t.Error("no error")
				}
				if !strings.HasPrefix(stderr.String(), "webp-frames: ") || !strings.Contains(stderr.String(), c.refusal) {
					t.Errorf("stderr %q, want it to mention %q", stderr.String(), c.refusal)
				}
				if name != "corrupt frame pixels" && len(r.out) != 0 {
					t.Errorf("stdout %q before failing", r.out)
				}
				// Frames written before the failure stay for the caller to
				// remove with the rest of the directory; stdout lists them.
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("hung")
			}
		})
	}
}

// TestPatchedCanvasIsRefusedBeforeTheToolStarts checks that a canvas the caps
// refuse, in a few hundred bytes, never reaches webp-frames.
func TestPatchedCanvasIsRefusedBeforeTheToolStarts(t *testing.T) {
	requireWebPBuilders(t)
	p := requireProcessorWith(t, Config{Caps: envTestCaps})
	good, err := os.ReadFile(buildWebP(t, webpCase{codec: "lossless", blend: true, frames: 6}))
	if err != nil {
		t.Fatal(err)
	}
	for name, side := range map[string][2]int{
		"too many pixels in one frame":    {16000, 16000},
		"too many pixels in the animated": {8000, 8000},
	} {
		t.Run(name, func(t *testing.T) {
			d := cloneBytes(good)
			put24(d[24:], side[0]-1)
			put24(d[27:], side[1]-1)
			started := false
			old := p.compose
			t.Cleanup(func() { p.compose = old })
			p.compose = func(context.Context, composeRequest, func(composedFrame) error) (*Usage, error) {
				started = true
				return nil, nil
			}
			requireAnimationRefusals(t, p, refusalBounds{wall: 2 * time.Second, maxRSS: 256 << 20}, d)
			if started {
				t.Error("the tool was started")
			}
		})
	}
}

// TestWebPFramesRefusesBadArguments checks the tool's own validation, which
// holds whoever calls it.
func TestWebPFramesRefusesBadArguments(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	exe, err := tool.Resolve(webpFramesTool)
	if err != nil {
		t.Fatal(err)
	}
	path := buildWebP(t, webpCase{codec: "lossless", blend: true, frames: 4})
	dir := t.TempDir()
	notWebP := writeFile(t, "x.webp", []byte("RIFF\x04\x00\x00\x00WEBP"))
	w, h := strconv.Itoa(webpCanvasW), strconv.Itoa(webpCanvasH)
	for name, args := range map[string][]string{
		"no arguments":          {},
		"too few":               {path, dir, "2", w, h, "4", "0"},
		"too many":              {path, dir, "2", w, h, "4", "0", "60000", "1"},
		"no timeout":            {path, dir, "2", w, h, "4", "0"},
		"timeout zero":          {path, dir, "2", w, h, "4", "0", "0"},
		"timeout not a count":   {path, dir, "2", w, h, "4", "0", "1s"},
		"in-flight zero":        {path, dir, "0", w, h, "4", "0", "60000"},
		"in-flight not a count": {path, dir, "x", w, h, "4", "0", "60000"},
		"negative width":        {path, dir, "2", "-40", h, "4", "0", "60000"},
		"width with a sign":     {path, dir, "2", "+40", h, "4", "0", "60000"},
		"width with a suffix":   {path, dir, "2", "40x", h, "4", "0", "60000"},
		"width zero":            {path, dir, "2", "0", h, "4", "0", "60000"},
		"canvas too large":      {path, dir, "2", "16777216", "16777216", "4", "0", "60000"},
		"frame count zero":      {path, dir, "2", w, h, "0", "0", "60000"},
		"empty selection":       {path, dir, "2", w, h, "4", "", "60000"},
		"frame out of range":    {path, dir, "2", w, h, "4", "4", "60000"},
		"descending":            {path, dir, "2", w, h, "4", "2,1", "60000"},
		"duplicate":             {path, dir, "2", w, h, "4", "1,1", "60000"},
		"empty entry":           {path, dir, "2", w, h, "4", "0,,2", "60000"},
		"trailing comma":        {path, dir, "2", w, h, "4", "0,1,", "60000"},
		"width differs":         {path, dir, "2", "41", h, "4", "0", "60000"},
		"height differs":        {path, dir, "2", w, "29", "4", "0", "60000"},
		"more frames":           {path, dir, "2", w, h, "5", "0", "60000"},
		"fewer frames":          {path, dir, "2", w, h, "3", "0", "60000"},
		"missing file":          {filepath.Join(dir, "missing"), dir, "2", w, h, "4", "0", "60000"},
		"directory as input":    {dir, dir, "2", w, h, "4", "0", "60000"},
		"not a WebP":            {notWebP, dir, "2", w, h, "4", "0", "60000"},
		"output directory gone": {path, filepath.Join(dir, "missing"), "2", w, h, "4", "0", "60000"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(exe, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			var ee *exec.ExitError
			if err == nil || !errors.As(err, &ee) || ee.ExitCode() != 1 {
				t.Fatalf("err = %v, want exit status 1", err)
			}
			if !strings.HasPrefix(stderr.String(), "webp-frames: ") {
				t.Errorf("stderr = %q", stderr.String())
			}
			if len(out) != 0 {
				t.Errorf("stdout = %q, want nothing", out)
			}
			if got := dirNames(t, dir); len(got) != 0 {
				t.Errorf("output dir holds %v", got)
			}
		})
	}
}

// TestWebPFramesWaitsForConsumedFrames checks the tool's backpressure: with
// two frames in flight it writes two and waits until the first is deleted.
func TestWebPFramesWaitsForConsumedFrames(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	exe, err := tool.Resolve(webpFramesTool)
	if err != nil {
		t.Fatal(err)
	}
	path := buildWebP(t, webpCase{codec: "lossless", blend: true, frames: 8})
	dir := t.TempDir()
	cmd := exec.Command(exe, path, dir, "2", strconv.Itoa(webpCanvasW), strconv.Itoa(webpCanvasH), "8", "0,1,2,3,4,5,6,7", "60000")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	lines := make(chan string)
	go func() {
		buf := make([]byte, 256)
		var acc string
		for {
			n, err := stdout.Read(buf)
			acc += string(buf[:n])
			for {
				i := strings.IndexByte(acc, '\n')
				if i < 0 {
					break
				}
				lines <- acc[:i]
				acc = acc[i+1:]
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	next := func(wait time.Duration) (string, bool) {
		select {
		case l, ok := <-lines:
			return l, ok
		case <-time.After(wait):
			return "", false
		}
	}
	for i := range 2 {
		if l, ok := next(30 * time.Second); !ok || !strings.HasPrefix(l, strconv.Itoa(i)+" ") {
			t.Fatalf("frame %d: line %q, ok %v", i, l, ok)
		}
	}
	if l, ok := next(200 * time.Millisecond); ok {
		t.Fatalf("a third frame, %q, was written with two in flight and none consumed", l)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{gifSampleName(0), gifSampleName(1)}) {
		t.Fatalf("directory holds %v, want two samples", got)
	}
	if err := os.Remove(filepath.Join(dir, gifSampleName(0))); err != nil {
		t.Fatal(err)
	}
	if l, ok := <-lines; !ok || !strings.HasPrefix(l, "2 ") {
		t.Fatalf("after consuming frame 0: line %q, ok %v", l, ok)
	}
}

// TestWebPFramesGoToMagickInBatches checks that no ImageMagick run reads more
// frames than the batch bound, and that the tool leaves no more than a batch
// on disk.
func TestWebPFramesGoToMagickInBatches(t *testing.T) {
	requireWebPBuilders(t)
	p := requireStreamingProcessor(t)
	old, oldTimeout := encodeBatchPixels, composeTimeout
	encodeBatchPixels, composeTimeout = 2*webpCanvasW*webpCanvasH, 10*time.Second
	t.Cleanup(func() { encodeBatchPixels, composeTimeout = old, oldTimeout })
	rec := record(p)
	path := buildWebP(t, webpCase{codec: "lossy", blend: true, frames: 24})
	images, out := framesInto(t, p, stage(t, path), Encoding{Format: PNG}, 12)
	if len(images) != 12 {
		t.Fatalf("%d images, want 12", len(images))
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
	if inputs != 12 || runs != 6 {
		t.Errorf("%d inputs in %d runs, want 12 in 6", inputs, runs)
	}
	if got := dirNames(t, out); len(got) != 12 {
		t.Errorf("output dir holds %v", got)
	}
}

// TestWebPFramesCleansUpWhenAMagickRunFails checks that the tool is stopped and
// its files removed when the encoder fails between batches.
func TestWebPFramesCleansUpWhenAMagickRunFails(t *testing.T) {
	requireWebPBuilders(t)
	p := requireStreamingProcessor(t)
	old := encodeBatchPixels
	encodeBatchPixels = 2 * webpCanvasW * webpCanvasH
	t.Cleanup(func() { encodeBatchPixels = old })
	rec := record(p)
	rec.failAfter = 3 // the header read, then the first batch, then the second
	out := t.TempDir()
	path := buildWebP(t, webpCase{codec: "lossy", blend: true, frames: 24})
	if _, _, err := p.AnimationFrames(context.Background(), stage(t, path), Encoding{Format: PNG}, 12, out); err == nil {
		t.Fatal("no error")
	}
	if got := dirNames(t, out); len(got) != 0 {
		t.Errorf("output dir holds %v", got)
	}
}

// TestWebPFramesStopsAfterTheLastSelectedFrame checks that frames after the
// last selected one are not decoded: a file whose later frames are corrupt
// still yields the earlier ones.
func TestWebPFramesStopsAfterTheLastSelectedFrame(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	exe, err := tool.Resolve(webpFramesTool)
	if err != nil {
		t.Fatal(err)
	}
	c := hostileWebPs(t)["corrupt frame pixels"]
	dir := t.TempDir()
	out, err := exec.Command(exe, writeFile(t, "x.webp", c.data), dir, "100",
		strconv.Itoa(c.w), strconv.Itoa(c.h), strconv.Itoa(c.frames), "0,2", "60000").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := dirNames(t, dir); !slices.Equal(got, []string{gifSampleName(0), gifSampleName(1)}) {
		t.Errorf("directory holds %v", got)
	}
}

// TestWebPFramesGivesUpOnAnUnconsumedFrame checks that the tool fails, rather
// than waiting for ever, when the frame it waits on is not consumed within its
// timeout.
func TestWebPFramesGivesUpOnAnUnconsumedFrame(t *testing.T) {
	requireWebPBuilders(t)
	exe, err := tool.Resolve(webpFramesTool)
	if err != nil {
		t.Fatal(err)
	}
	path := buildWebP(t, webpCase{codec: "lossless", blend: true, frames: 4})
	dir := t.TempDir()
	start := time.Now()
	cmd := exec.Command(exe, path, dir, "1", strconv.Itoa(webpCanvasW), strconv.Itoa(webpCanvasH), "4", "0,1,2,3", "150")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("err = %v, want exit status 1", err)
	}
	if !strings.Contains(stderr.String(), "webp-frames: frame 0 not consumed in 150 ms") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if string(out) != "0 0 0\n" {
		t.Errorf("stdout = %q, want only frame 0", out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
}
