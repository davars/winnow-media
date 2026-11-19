package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// noiseJPEG returns a w by h JPEG of seeded noise, so that its entropy-coded
// data is long enough to damage.
func noiseJPEG(t *testing.T, w, h int, seed int64) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.Intn(256))
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withOrientation returns the JPEG with an EXIF APP1 segment recording
// orientation o, inserted after the start-of-image marker.
func jpegWithOrientation(jpg []byte, o byte) []byte {
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, o, 0, 0, 0, 0, 0, 0, 0}
	seg := append([]byte("Exif\x00\x00"), tiff...)
	n := len(seg) + 2
	out := []byte{0xff, 0xd8, 0xff, 0xe1, byte(n >> 8), byte(n)}
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

// container builds a file of filler holding each part, and the candidate that
// covers it.
func container(parts ...[]byte) ([]byte, []PreviewCandidate) {
	file := []byte("II*\x00 container header ")
	var cands []PreviewCandidate
	for _, p := range parts {
		file = append(file, "....filler...."...)
		cands = append(cands, PreviewCandidate{Offset: int64(len(file)), Length: int64(len(p))})
		file = append(file, p...)
	}
	return append(file, "trailer"...), cands
}

func extract(t *testing.T, p *Processor, file []byte, cands []PreviewCandidate, opts PreviewOptions) (Preview, string, Usage, error) {
	t.Helper()
	in := writeFile(t, "raw.tif", file)
	dir := t.TempDir()
	pv, usage, err := p.ExtractPreview(context.Background(), in, dir, cands, opts)
	return pv, dir, usage, err
}

func TestExtractPreviewRule(t *testing.T) {
	p, rec := sourceProcessor(t)
	small := noiseJPEG(t, 160, 120, 1)
	big := noiseJPEG(t, 1600, 1200, 2)
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewGray(image.Rect(0, 0, 2400, 2400))); err != nil {
		t.Fatal(err)
	}
	file, cands := container(small, big, big, big[:len(big)-2], pngBytes.Bytes(), big)
	cands[2].Length = int64(len(big)) + 1<<20 // runs past the end of the file
	pv, dir, usage, err := extract(t, p, file, cands, PreviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Candidate 1 is the first full-size JPEG: 2 runs past the end, 3 lacks
	// its end marker, 4 is a PNG and 5 ties with 1.
	if pv.Candidate != 1 || pv.Name != "000.jpg" || pv.Width != 1600 || pv.Height != 1200 {
		t.Fatalf("Preview = %+v", pv)
	}
	got, err := os.ReadFile(filepath.Join(dir, pv.Name))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("output is not the chosen candidate's bytes (err %v)", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("output directory holds %d files, want 1", len(entries))
	}
	if usage.CPU <= 0 || usage.MaxRSS <= 0 {
		t.Errorf("Usage = %+v, want non-zero", usage)
	}
	// Only the candidates whose range and markers are intact reach a tool:
	// candidates 0, 1 and 5. The tie is not a larger one, so it is not
	// decoded either way; the header reads are what count.
	reads := 0
	for _, c := range rec.calls {
		if c[0] == "magick" {
			reads++
		}
	}
	if reads == 0 || reads > 3*2 {
		t.Errorf("%d magick runs for three candidates with markers", reads)
	}
}

func TestExtractPreviewNoneQualifies(t *testing.T) {
	p := requireProcessor(t)
	big := noiseJPEG(t, 64, 64, 3)
	file, cands := container(big[:len(big)-2], []byte("not a jpeg at all"))
	pv, dir, _, err := extract(t, p, file, cands, PreviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pv != (Preview{Candidate: -1}) {
		t.Errorf("Preview = %+v, want Candidate -1 only", pv)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("output directory holds %d files, want none", len(entries))
	}
	if _, _, _, err := extract(t, p, file, nil, PreviewOptions{}); err != nil {
		t.Errorf("no candidates: %v", err)
	}
}

func TestExtractPreviewTieGoesToTheEarlier(t *testing.T) {
	p := requireProcessor(t)
	a, b := noiseJPEG(t, 300, 200, 4), noiseJPEG(t, 200, 300, 5)
	file, cands := container(a, b)
	pv, _, _, err := extract(t, p, file, cands, PreviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Candidate != 0 {
		t.Errorf("Candidate = %d, want 0: the sides are equal", pv.Candidate)
	}
}

func TestExtractPreviewReportsUprightSize(t *testing.T) {
	p := requireProcessor(t)
	file, cands := container(jpegWithOrientation(noiseJPEG(t, 80, 40, 6), 6))
	pv, _, _, err := extract(t, p, file, cands, PreviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Width != 40 || pv.Height != 80 {
		t.Errorf("size = %dx%d, want 40x80 upright", pv.Width, pv.Height)
	}
}

func TestExtractPreviewDecode(t *testing.T) {
	p := requireProcessor(t)
	whole := noiseJPEG(t, 200, 150, 7)
	big := noiseJPEG(t, 800, 600, 8)
	cut := append(append([]byte{}, big[:len(big)/2]...), 0xff, 0xd9)
	file, cands := container(whole, cut)

	pv, _, _, err := extract(t, p, file, cands, PreviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Candidate != 1 {
		t.Fatalf("header read chose %d, want the cut JPEG, 1", pv.Candidate)
	}
	pv, dir, _, err := extract(t, p, file, cands, PreviewOptions{Decode: true})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Candidate != 0 || pv.Width != 200 {
		t.Errorf("Decode chose %+v, want the whole JPEG, 0", pv)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "000.jpg")); !bytes.Equal(got, whole) {
		t.Error("output is not the whole JPEG")
	}
}

func TestExtractPreviewDeterministic(t *testing.T) {
	p := requireProcessor(t)
	file, cands := container(noiseJPEG(t, 100, 80, 9), noiseJPEG(t, 400, 300, 10))
	_, d1, _, err1 := extract(t, p, file, cands, PreviewOptions{Decode: true})
	_, d2, _, err2 := extract(t, p, file, cands, PreviewOptions{Decode: true})
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	a, _ := os.ReadFile(filepath.Join(d1, "000.jpg"))
	b, _ := os.ReadFile(filepath.Join(d2, "000.jpg"))
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Error("two calls wrote different bytes")
	}
}

func TestExtractPreviewLimits(t *testing.T) {
	p := requireProcessor(t)
	jpg := noiseJPEG(t, 32, 32, 11)
	file, cands := container(jpg)
	many := make([]PreviewCandidate, 17)
	for i := range many {
		many[i] = cands[0]
	}
	in := writeFile(t, "raw.tif", file)
	cases := []struct {
		name  string
		cands []PreviewCandidate
		dir   func() string
	}{
		{"17 candidates", many, t.TempDir},
		{"16 candidates", many[:16], nil},
		{"negative offset", []PreviewCandidate{{Offset: -1, Length: 10}}, t.TempDir},
		{"zero length", []PreviewCandidate{{Offset: 0, Length: 0}}, t.TempDir},
		{"dir not empty", cands, func() string {
			d := t.TempDir()
			if err := os.WriteFile(filepath.Join(d, "x"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return d
		}},
		{"dir missing", cands, func() string { return filepath.Join(t.TempDir(), "absent") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.dir != nil {
				dir = c.dir()
			}
			_, _, err := p.ExtractPreview(context.Background(), in, dir, c.cands, PreviewOptions{})
			if c.dir == nil {
				if err != nil {
					t.Errorf("err = %v, want success", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	t.Run("not a regular file", func(t *testing.T) {
		_, _, err := p.ExtractPreview(context.Background(), t.TempDir(), t.TempDir(), cands, PreviewOptions{})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("err = %v, want ErrInvalidRequest", err)
		}
	})
}

func TestExtractPreviewFileOverTheCap(t *testing.T) {
	p := requireProcessorWith(t, Config{Caps: Caps{MaxInputBytes: 1 << 10}})
	in := writeFile(t, "raw.tif", make([]byte, 2<<10))
	_, _, err := p.ExtractPreview(context.Background(), in, t.TempDir(), []PreviewCandidate{{0, 10}}, PreviewOptions{})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestExtractPreviewNeedsJPEG(t *testing.T) {
	p := requireProcessorWith(t, Config{Types: []string{"image/png"}})
	file, cands := container(noiseJPEG(t, 32, 32, 12))
	_, _, _, err := extract(t, p, file, cands, PreviewOptions{})
	if !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("err = %v, want ErrUnsupportedType", err)
	}
}

func TestExtractPreviewCandidateOverTheCapDoesNotQualify(t *testing.T) {
	p := requireProcessorWith(t, Config{Caps: Caps{MaxFramePixels: 50_000}})
	file, cands := container(noiseJPEG(t, 400, 300, 13), noiseJPEG(t, 100, 80, 14))
	pv, _, _, err := extract(t, p, file, cands, PreviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Candidate != 1 {
		t.Errorf("Candidate = %d, want 1: candidate 0 is over the frame cap", pv.Candidate)
	}
}

func TestExtractPreviewFileChangedDuringTheCall(t *testing.T) {
	p := requireProcessor(t)
	file, cands := container(noiseJPEG(t, 64, 64, 15))
	in := writeFile(t, "raw.tif", file)
	inner := p.run
	p.run = func(ctx context.Context, f *os.File, name string, args ...string) (runResult, error) {
		res, err := inner(ctx, f, name, args...)
		later := time.Now().Add(time.Hour)
		if e := os.Chtimes(in, later, later); e != nil {
			t.Error(e)
		}
		return res, err
	}
	dir := t.TempDir()
	_, _, err := p.ExtractPreview(context.Background(), in, dir, cands, PreviewOptions{})
	if !errors.Is(err, ErrInputChanged) {
		t.Errorf("err = %v, want ErrInputChanged", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("a failed call left files in dir")
	}
}

func TestExtractPreviewCancelledAndClosed(t *testing.T) {
	p := requireProcessor(t)
	file, cands := container(noiseJPEG(t, 64, 64, 16))
	in := writeFile(t, "raw.tif", file)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	if _, _, err := p.ExtractPreview(ctx, in, dir, cands, PreviewOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("a cancelled call left files in dir")
	}
	_ = p.Close()
	if _, _, err := p.ExtractPreview(context.Background(), in, t.TempDir(), cands, PreviewOptions{}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed: err = %v, want ErrClosed", err)
	}
}

func TestExtractPreviewArgv(t *testing.T) {
	cases := []struct {
		name string
		opts PreviewOptions
	}{{"preview-header", PreviewOptions{}}, {"preview-decode", PreviewOptions{Decode: true}}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, rec, g := goldenProcessor(t, c.name, Config{})
			file, cands := container(noiseJPEG(t, 48, 32, 17))
			in := writeFile(t, "raw.tif", file)
			out := t.TempDir()
			g.out = out
			if _, _, err := p.ExtractPreview(context.Background(), in, out, cands, c.opts); err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.name, goldenArgv(rec.calls, out))
		})
	}
}

// The choice is the one a caller makes by opening each candidate's bytes with
// zero OpenOptions, letting the bytes name the type, and keeping the
// strictly longest side: same index, same size.
func TestExtractPreviewMatchesOpeningEachCandidate(t *testing.T) {
	p := requireProcessor(t)
	parts := [][]byte{
		noiseJPEG(t, 120, 90, 21),
		jpegWithOrientation(noiseJPEG(t, 300, 100, 22), 6),
		noiseJPEG(t, 100, 300, 23),
		jpegWithOrientation(noiseJPEG(t, 90, 280, 24), 8),
		noiseJPEG(t, 64, 64, 25)[:200],
	}
	file, cands := container(parts...)
	pv, _, _, err := extract(t, p, file, cands, PreviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := Preview{Candidate: -1}
	for i, part := range parts {
		if len(part) < 4 || part[len(part)-2] != 0xff || part[len(part)-1] != 0xd9 {
			continue
		}
		src, _, err := p.Open(context.Background(), writeFile(t, "c.jpg", part), OpenOptions{})
		if err != nil {
			continue
		}
		info := src.Info()
		_ = src.Close()
		if max(info.Width, info.Height) > max(want.Width, want.Height) {
			want = Preview{Candidate: i, Name: "000.jpg", Width: info.Width, Height: info.Height}
		}
	}
	if pv != want {
		t.Errorf("ExtractPreview chose %+v, opening each candidate chooses %+v", pv, want)
	}
}
