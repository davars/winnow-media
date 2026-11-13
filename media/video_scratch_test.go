package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

const stubProbe4K = `{"streams":[{"index":0,"codec_type":"video","width":3840,"height":2160,` +
	`"duration":"10","nb_frames":"300","r_frame_rate":"30/1","pix_fmt":%q%s}],` +
	`"format":{"duration":"10","start_time":"0"}}`

const hlgSideData = `,"color_transfer":"arib-std-b67"`

// A video call whose samples would not fit in Resources.Disk is refused before
// any frame is decoded; one that fits is sampled.
func TestVideoRefusesSamplesBeyondDiskBeforeDecoding(t *testing.T) {
	t.Parallel()
	const frame = 3840 * 2160
	full := Encoding{Format: WebP, MaxEdge: 4000, MaxArea: 4000 * 4000}
	for name, c := range map[string]struct {
		probe string
		sel   Select
		disk  int64
		want  bool // refused
	}{
		"8-bit over":    {fmt.Sprintf(stubProbe4K, "yuv420p", ""), Select{Frames: Even(16)}, 16*frame*3 - 1, true},
		"8-bit within":  {fmt.Sprintf(stubProbe4K, "yuv420p", ""), Select{Frames: Even(16)}, 16 * frame * 3, false},
		"10-bit over":   {fmt.Sprintf(stubProbe4K, "yuv420p10le", ""), Select{Frames: Even(16)}, 16*frame*6 - 1, true},
		"10-bit within": {fmt.Sprintf(stubProbe4K, "yuv420p10le", ""), Select{Frames: Even(16)}, 16 * frame * 6, false},
		"HDR is 8-bit":  {fmt.Sprintf(stubProbe4K, "yuv420p10le", hlgSideData), Select{Frames: Even(16)}, 16 * frame * 3, false},
		"consecutive":   {fmt.Sprintf(stubProbe4K, "yuv420p", ""), Select{Frames: Consecutive(0, 16)}, 16*frame*3 - 1, true},
		"times":         {fmt.Sprintf(stubProbe4K, "yuv420p", ""), Select{Frames: Times(0, 1e9, 2e9)}, 3*frame*3 - 1, true},
		"even 64":       {fmt.Sprintf(stubProbe4K, "yuv420p", ""), Select{Frames: Even(64)}, 64*frame*3 - 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := &videoStub{probe: c.probe, res: Resources{Disk: c.disk}}
			src, err := stub.open(t)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = src.Encode(context.Background(), t.TempDir(), c.sel, full)
			if got := errors.Is(err, ErrTooLarge); got != c.want {
				t.Errorf("err = %v, want refused = %v", err, c.want)
			}
			if c.want && len(stub.runs) != 0 {
				t.Errorf("%d ffmpeg runs before the refusal", len(stub.runs))
			}
			if !c.want && len(stub.runs) == 0 {
				t.Error("a call within Disk did not sample")
			}
			if c.want && !strings.Contains(err.Error(), fmt.Sprintf("Resources.Disk is %d", c.disk)) {
				t.Errorf("err = %v does not state the numbers", err)
			}
		})
	}
}

// The size the guard counts for a sample is the size of the file ffmpeg writes.
func TestVideoSampleBytesAreTheSampleFileSize(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	ten := writeClip(t, env, ".mp4", append(testsrc("160x120", 10, 2), "-c:v", "libx264", "-preset", "ultrafast",
		"-profile:v", "high10", "-pix_fmt", "yuv420p10le")...)
	for name, path := range map[string]string{
		"8-bit":  sdrClip(t, env, "160x120", 10, 2),
		"10-bit": ten,
		"HDR":    hlgClip(t, env, testsrc("160x120", 10, 2)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dec := decoder{"ffmpeg", "mov"}
			for _, enc := range []encoding{{maxEdge: 100, maxArea: 10000}, {maxEdge: 4000, maxArea: 16_000_000}} {
				plan, err := env.planVideo(context.Background(), dec, path, enc)
				if err != nil {
					t.Fatal(err)
				}
				samples, err := env.sampleVideo(context.Background(), dec, path, t.TempDir(), plan, []int64{500})
				if err != nil || len(samples) != 1 {
					t.Fatalf("samples = %v, %v", samples, err)
				}
				fi, err := os.Stat(samples[0].path)
				if err != nil {
					t.Fatal(err)
				}
				// The header is a few dozen bytes.
				if got, want := plan.sampleBytes(), fi.Size(); got > want || want-got > 100 {
					t.Errorf("%dx%d: sampleBytes = %d, file is %d", plan.fw, plan.fh, got, want)
				}
			}
		})
	}
}
