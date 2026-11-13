package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stillCapPaths are the ways a Processor under the measured caps opens a still,
// as a function of the file, its ImageMagick coder and the target's MaxEdge.
func stillCapPaths(t *testing.T) map[string]func(path, coder string, maxEdge int) error {
	t.Helper()
	p := requireProcessorWith(t, Config{Caps: measuredCapsConfig})
	return map[string]func(string, string, int) error{"processor": func(path, coder string, maxEdge int) error {
		src, _, err := p.Open(context.Background(), path, OpenOptions{Type: map[string]string{"jpeg": "image/jpeg", "png": "image/png"}[coder]})
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		_, _, err = src.Encode(context.Background(), t.TempDir(), Select{}, stillTarget(maxEdge))
		return err
	}}
}

func stillTarget(maxEdge int) Encoding {
	return Encoding{Format: WebP, Quality: 50, MaxEdge: maxEdge, MaxArea: min(maxEdge*maxEdge, 100_000_000)}
}

func flatImage(t *testing.T, name string, w, h int, extra ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	args := append([]string{"-size", fmt.Sprintf("%dx%d", w, h), "xc:gray"}, extra...)
	magick(t, requireMagick(t), append(args, path)...)
	return path
}

// A JPEG over the pixel cap by its stored size is admitted when the targets
// let the decode reduce it under the cap, and refused when they do not.
func TestStillCapsJudgeTheReducedJPEG(t *testing.T) {
	t.Parallel()
	for _, sz := range [][2]int{{7308, 10000}, {16382, 3628}} {
		path := flatImage(t, "big.jpg", sz[0], sz[1], "-quality", "50")
		for pathName, run := range stillCapPaths(t) {
			for _, edge := range []int{2048, 1024, 256} {
				t.Run(fmt.Sprintf("%dx%d/%s/%d", sz[0], sz[1], pathName, edge), func(t *testing.T) {
					if err := run(path, "jpeg", edge); err != nil {
						t.Fatalf("admitted size refused: %v", err)
					}
				})
			}
			t.Run(fmt.Sprintf("%dx%d/%s/unreduced", sz[0], sz[1], pathName), func(t *testing.T) {
				err := run(path, "jpeg", 16384)
				if !errors.Is(err, ErrTooLarge) {
					t.Fatalf("err = %v, want ErrTooLarge", err)
				}
			})
		}
	}
}

// The error names the reduced and the stored size.
func TestStillCapRefusalNamesBothSizes(t *testing.T) {
	t.Parallel()
	s := &frameSource{jpeg: true, stored: [2]int{10000, 7308}, upright: [2]int{10000, 7308}}
	// A target of 9000 reduces by nothing: 8*2*9000/10000 rounds above 8.
	err := checkStillSize(s.stored, s.decodedSize([]encoding{{maxEdge: 9000, maxArea: 81_000_000}}), measuredCaps)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "10000x7308") {
		t.Fatalf("err = %v", err)
	}
	big := &frameSource{jpeg: true, stored: [2]int{20000, 16000}, upright: [2]int{20000, 16000}}
	err = checkStillSize(big.stored, big.decodedSize([]encoding{{maxEdge: 5000, maxArea: 25_000_000}}), measuredCaps)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "stored 20000x16000") {
		t.Fatalf("err = %v, want both sizes", err)
	}
}

// The cap on a side applies after reduction: a 20000-wide JPEG reduced below
// 16384 is admitted, and the same size is refused when nothing reduces it.
func TestStillSideCapAppliesAfterReduction(t *testing.T) {
	t.Parallel()
	path := flatImage(t, "wide.jpg", 20000, 1000, "-quality", "50")
	for name, run := range stillCapPaths(t) {
		t.Run(name, func(t *testing.T) {
			if err := run(path, "jpeg", 1024); err != nil {
				t.Fatalf("reduced to under the side cap: %v", err)
			}
			if err := run(path, "jpeg", 16384); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("unreduced: err = %v, want ErrTooLarge", err)
			}
		})
	}
	// Reduced, but still over the side cap.
	s := &frameSource{jpeg: true, stored: [2]int{60000, 100}, upright: [2]int{60000, 100}}
	err := checkStillSize(s.stored, s.decodedSize([]encoding{{maxEdge: 20000, maxArea: 1 << 40}}), measuredCaps)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// A still that nothing reduces is judged at its stored size: 60 MP is
// refused and 52 MP admitted.
func TestStillCapsOnNonReducibleFormats(t *testing.T) {
	t.Parallel()
	over := flatImage(t, "over.png", 10000, 6000)
	exact := flatImage(t, "exact.png", 8000, 6500)
	for name, run := range stillCapPaths(t) {
		t.Run(name+"/over", func(t *testing.T) {
			if err := run(over, "png", 256); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("err = %v, want ErrTooLarge", err)
			}
		})
		t.Run(name+"/at cap", func(t *testing.T) {
			if err := run(exact, "png", 256); err != nil {
				t.Fatalf("52 MP refused: %v", err)
			}
		})
	}
}

// Processor.Still, the one public operation on a still, refuses a HEIC by
// the size its header declares and admits one at the cap.
func TestProcessorStillCapsOnHEIC(t *testing.T) {
	t.Parallel()
	p := requireProcessorWith(t, Config{Caps: measuredCapsConfig})
	original, err := os.ReadFile(filepath.Join("testdata", "heic", "gradient-righttop.heic"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		w, h    uint32
		refused bool
	}{
		"60 MP":                {10000, 6000, true},
		"52 MP":                {8000, 6500, false},
		"over the side cap":    {16385, 100, true},
		"the side cap exactly": {16384, 100, false},
	} {
		t.Run(name, func(t *testing.T) {
			data := slices.Clone(original)
			if n := setHEICExtents(t, data, c.w, c.h); n == 0 {
				t.Fatal("no ispe box in the fixture")
			}
			path := writeFile(t, "in.heic", data)
			ctx := context.Background()
			if c.refused {
				if _, _, err := p.encodeFirst(ctx, path, Encoding{}, t.TempDir()); !errors.Is(err, ErrTooLarge) {
					t.Fatalf("err = %v, want ErrTooLarge", err)
				}
				return
			}
			// The cap judges the signalled size at Open. The forged extents
			// differ from the pixels, which the decoder refuses at Encode.
			src, _, err := p.Open(ctx, path, OpenOptions{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = src.Close() })
			_, _, err = src.Encode(ctx, t.TempDir(), Select{}, Encoding{})
			if te := new(ToolError); err != nil &&
				(!errors.As(err, &te) || !strings.Contains(string(te.Stderr), "does not have the size signaled")) {
				t.Fatalf("Encode: %v, want none or a decoder's size mismatch", err)
			}
		})
	}
}

// The size the cap judges is the size ImageMagick produces from the hint
// the reduction gives.
func TestDecodedSizeIsWhatTheHintDecodes(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	for _, sz := range [][2]int{{6000, 4000}, {4001, 3001}, {8064, 6048}, {3000, 9001}} {
		path := flatImage(t, "x.jpg", sz[0], sz[1], "-quality", "50")
		for _, edge := range []int{256, 600, 1024, 2048} {
			s := &frameSource{jpeg: true, stored: sz, upright: sz}
			encs := []encoding{{maxEdge: edge, maxArea: edge * edge}}
			ops, _ := s.reduction(encs)
			if ops == nil {
				continue
			}
			args := append(append([]string{}, ops...), "jpeg:"+path, "-format", "%w %h", "info:")
			got := strings.Fields(string(magick(t, env, args...)))
			want := s.decodedSize(encs)
			if got[0] != fmt.Sprint(want[0]) || got[1] != fmt.Sprint(want[1]) {
				t.Errorf("%dx%d at %d: decoded %v, decodedSize %v", sz[0], sz[1], edge, got, want)
			}
			// The decode is the smallest eighth covering twice the output.
			tw, th, _ := fitSize(sz[0], sz[1], edge, edge*edge)
			covers := func(n int) bool {
				return sz[0]*n >= 8*decodeMargin*tw && sz[1]*n >= 8*decodeMargin*th
			}
			n := want[0] * 8 / sz[0]
			if !covers(n) || (n > 1 && covers(n-1)) {
				t.Errorf("%dx%d at %d: decodes at %v, %d eighths, not the smallest that covers %dx%d twice",
					sz[0], sz[1], edge, want, n, tw, th)
			}
		}
	}
}

// A still no reduction can shrink is capped on its header, before the
// profile read that may decode it.
func TestUnreducibleStillIsCappedBeforeTheProfileRead(t *testing.T) {
	t.Parallel()
	var runs int
	env := toolEnv{resources: defaultResources(), caps: measuredCaps, run: func(context.Context, string, ...string) (runResult, error) {
		runs++
		return runResult{Stdout: []byte("0||10000|6000|0|0|JXL|Undefined|DirectClass sRGB|False|END\n")}, nil
	}}
	_, err := env.readSource(context.Background(), openRequest{path: "x", dec: decoder{"magick", "jxl"}})
	if !errors.Is(err, ErrTooLarge) || runs != 1 {
		t.Fatalf("err = %v after %d runs, want ErrTooLarge after the header read", err, runs)
	}
}

// Several targets: the largest decides. A target that does not reduce,
// beside one that does, leaves the decode unreduced.
func TestStillCapLargestTargetDecides(t *testing.T) {
	t.Parallel()
	path := flatImage(t, "big.jpg", 7308, 10000, "-quality", "50")
	src := openSourceOn(t, requireProcessorWith(t, Config{Caps: measuredCapsConfig}), path, OpenOptions{})
	_, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, stillTarget(256), stillTarget(16384))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// A 73 MP JPEG reduced only to seven eighths is still over the pixel cap.
func TestStillCapAtSevenEighths(t *testing.T) {
	t.Parallel()
	path := flatImage(t, "big.jpg", 7308, 10000, "-quality", "50")
	p := requireProcessorWith(t, Config{Caps: measuredCapsConfig})
	for edge, refused := range map[int]bool{3750: false, 3751: true, 4375: true} {
		src := openSourceOn(t, p, path, OpenOptions{})
		_, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, stillTarget(edge))
		if got := errors.Is(err, ErrTooLarge); got != refused || (!refused && err != nil) {
			t.Errorf("target %d: err = %v, want ErrTooLarge = %v", edge, err, refused)
		}
	}
}

// losslessJPEG writes a lossless (SOF3) JPEG of a w x h gray image, which
// ImageMagick reports as JPEG and libjpeg-turbo never scales.
func losslessJPEG(t *testing.T, w, h int) string {
	t.Helper()
	cjpeg, err := exec.LookPath("cjpeg")
	if err != nil {
		t.Fatal("cjpeg is not available; run the tests under nix develop")
	}
	pgm := flatImage(t, "in.pgm", w, h)
	path := filepath.Join(t.TempDir(), "lossless.jpg")
	out, err := exec.Command(cjpeg, "-lossless", "1", "-outfile", path, pgm).CombinedOutput()
	if err != nil {
		t.Fatalf("cjpeg: %v: %s", err, out)
	}
	return path
}

// A lossless JPEG ignores the scaling hint, so it is judged at its stored
// size: refused over the cap whatever the target, and, under it, decoded
// without the hint.
func TestStillCapLosslessJPEG(t *testing.T) {
	t.Parallel()
	over := losslessJPEG(t, 10000, 6000)
	p := requireProcessorWith(t, Config{Caps: measuredCapsConfig})
	src := openSourceOn(t, p, over, OpenOptions{})
	_, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, stillTarget(256))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("60 MP lossless JPEG at 256: err = %v, want ErrTooLarge", err)
	}

	under := losslessJPEG(t, 4001, 3001)
	rec := record(p)
	src = openSourceOn(t, p, under, OpenOptions{})
	if _, _, err = src.Encode(context.Background(), t.TempDir(), Select{}, stillTarget(256)); err != nil {
		t.Fatalf("lossless JPEG under the cap: %v", err)
	}
	for _, c := range rec.calls {
		if slices.Contains(c, "-define") && slices.Contains(c, "jpeg:size=1001x751") && !slices.Contains(c, "identify") {
			t.Errorf("the encode used the hint: %q", c)
		}
	}
}

// The header read with the hint gives the size decodedSize expects for every
// JPEG flavour the decoder scales.
func TestPingedDecodeSizeAgreesWithDecodedSize(t *testing.T) {
	t.Parallel()
	real := requireMagick(t)
	cjpeg, err := exec.LookPath("cjpeg")
	if err != nil {
		t.Fatal("cjpeg is not available; run the tests under nix develop")
	}
	dir := t.TempDir()
	flavours := map[string]func(path string){
		"baseline": func(p string) { magick(t, real, "-size", "4001x3001", "xc:gray", "-quality", "50", p) },
		"progressive": func(p string) {
			magick(t, real, "-size", "4001x3001", "xc:gray", "-interlace", "Plane", "-quality", "50", p)
		},
		"gray": func(p string) {
			magick(t, real, "-size", "4001x3001", "xc:gray", "-colorspace", "Gray", "-quality", "50", p)
		},
		"cmyk": func(p string) {
			magick(t, real, "-size", "4001x3001", "xc:gray", "-colorspace", "CMYK", "-quality", "50", p)
		},
	}
	for name, extra := range map[string][]string{"arithmetic": {"-arithmetic"}, "12-bit": {"-precision", "12"}} {
		flavours[name] = func(p string) {
			ppm := filepath.Join(dir, name+".ppm")
			magick(t, real, "-size", "4001x3001", "xc:gray", ppm)
			args := append(append([]string{}, extra...), "-outfile", p, ppm)
			if out, err := exec.Command(cjpeg, args...).CombinedOutput(); err != nil {
				t.Fatalf("cjpeg %v: %v: %s", extra, err, out)
			}
		}
	}
	for name, build := range flavours {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".jpg")
			build(path)
			p := requireProcessor(t)
			opened := openSourceOn(t, p, path, OpenOptions{})
			src, err := opened.stillAt(context.Background(), p.env(&meter{}, opened.file), 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, edge := range []int{256, 1024} {
				encs := []encoding{{maxEdge: edge, maxArea: edge * edge}}
				ops, _ := src.reduction(encs)
				if ops == nil {
					t.Fatalf("edge %d: no reduction chosen", edge)
				}
				got, err := src.pingDecoded(context.Background(), ops)
				if err != nil {
					t.Fatal(err)
				}
				if want := src.decodedSize(encs); got != want {
					t.Errorf("edge %d: ping %v, decodedSize %v", edge, got, want)
				}
			}
		})
	}
}
