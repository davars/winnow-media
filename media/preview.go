package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maxPreviewCandidates is how many candidates one ExtractPreview call may
// judge, which bounds the header reads and scratch copies of a call
// (docs/measurements.md#rendering).
const maxPreviewCandidates = 16

// PreviewCandidate is a byte range of a file that may hold an embedded JPEG,
// such as a preview whose offset and length an EXIF reader reports. Offset
// counts bytes from the start of the file.
type PreviewCandidate struct {
	Offset, Length int64
}

// PreviewOptions say how ExtractPreview judges a candidate. The zero value
// requires that its header be read.
type PreviewOptions struct {
	// Decode also requires that its whole image decode without an error or a
	// warning: a full decode of each candidate that could be chosen, which
	// turns away one whose image data is damaged or cut short.
	Decode bool
}

// Preview is the candidate ExtractPreview chose.
type Preview struct {
	// Candidate is the index of the chosen candidate, or -1 when none
	// qualified.
	Candidate int
	// Name is the JPEG's file name within the output directory, 000.jpg, or
	// empty when none qualified.
	Name string
	// Width and Height are the JPEG's size as Open reports it with zero
	// OpenOptions: upright when the JPEG records an orientation, else as
	// stored.
	Width, Height int
}

// ExtractPreview chooses the largest embedded JPEG among candidates of the
// file at path, which may be relative, and writes a copy of its bytes into
// dir, which must exist and be empty, as 000.jpg. Open it to encode it;
// OpenOptions.Orientation gives it the orientation of the file it came from
// when it records none.
//
// A candidate qualifies when its range lies wholly inside the file, starts
// with a JPEG start-of-image marker (FF D8) and ends with an end-of-image
// marker (FF D9), Open admits its bytes as image/jpeg within the caps, its
// size is within Caps.MaxFramePixels, and, with PreviewOptions.Decode, it
// decodes cleanly. The chosen one has the longest side by Width and Height as
// above; of equal ones, the earliest in candidates. When none qualifies,
// Candidate is -1, dir stays empty, and the error is nil.
//
// The file must be a regular file of at most Caps.MaxInputBytes; its own
// type is not judged, since only the candidates' bytes are read as images,
// and image/jpeg must be among Config.Types (else ErrUnsupportedType). At most
// 16 candidates may be given, each with Offset of at least 0 and Length of at
// least 1; otherwise the error wraps ErrInvalidRequest. The file is opened
// once and read only through that descriptor; it is refused with
// ErrInputChanged if it changes during the call.
//
// The Usage covers the tool runs of the call, also when it fails: one header
// read per candidate whose bytes have the markers, and with Decode one decode
// of each that is larger than the best so far. Errors wrap ErrInvalidRequest,
// ErrUnsupportedType, ErrTooLarge (the file), ErrInputChanged, ErrClosed or
// ctx's error. A candidate that a tool fails on, is over the caps or reaches a
// time limit does not qualify; it is not an error. A call that fails leaves
// dir as it found it.
func (p *Processor) ExtractPreview(ctx context.Context, path, dir string, candidates []PreviewCandidate, opts PreviewOptions) (Preview, Usage, error) {
	if !p.beginOpen() {
		return Preview{}, Usage{}, fmt.Errorf("extract preview: processor: %w", ErrClosed)
	}
	defer p.opening.Done()
	var m meter
	pv, err := p.extractPreview(ctx, &m, path, dir, candidates, opts)
	if err != nil {
		return Preview{}, m.usage, fmt.Errorf("extract preview: %w", err)
	}
	return pv, m.usage, nil
}

func (p *Processor) extractPreview(ctx context.Context, m *meter, path, dir string, candidates []PreviewCandidate, opts PreviewOptions) (Preview, error) {
	if err := p.checkType("image/jpeg"); err != nil {
		return Preview{}, err
	}
	if len(candidates) > maxPreviewCandidates {
		return Preview{}, invalid("%d candidates, the limit is %d", len(candidates), maxPreviewCandidates)
	}
	for i, c := range candidates {
		if c.Offset < 0 || c.Length < 1 {
			return Preview{}, invalid("candidate %d has offset %d and length %d", i, c.Offset, c.Length)
		}
	}
	if err := requireEmptyDir(dir); err != nil {
		return Preview{}, err
	}
	f, _, info, err := p.openRegular(path)
	if err != nil {
		return Preview{}, err
	}
	defer func() { _ = f.Close() }()
	id, size := idOf(info), info.Size()
	scratch, err := p.runTempDir()
	if err != nil {
		return Preview{}, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	best := Preview{Candidate: -1}
	var bestPath string
	for i, c := range candidates {
		if err := ctx.Err(); err != nil {
			return Preview{}, err
		}
		if !hasJPEGMarkers(f, size, c) {
			continue
		}
		file := filepath.Join(scratch, fmt.Sprintf("cand-%02d.jpg", i))
		if err := copyRange(f, c, file); err != nil {
			return Preview{}, err
		}
		w, h, err := p.judgePreview(ctx, m, file, best, opts)
		if err != nil {
			_ = os.Remove(file)
			if ctx.Err() != nil || errors.Is(err, ErrClosed) {
				return Preview{}, err
			}
			continue
		}
		if w == 0 {
			_ = os.Remove(file)
			continue
		}
		if bestPath != "" {
			_ = os.Remove(bestPath)
		}
		best, bestPath = Preview{Candidate: i, Name: numbered(0, ".jpg"), Width: w, Height: h}, file
	}
	if cur, err := f.Stat(); err != nil {
		return Preview{}, fmt.Errorf("source: %w", withoutPath(err))
	} else if idOf(cur) != id || unlinked(cur) {
		return Preview{}, fmt.Errorf("source: %w during the call", ErrInputChanged)
	}
	if best.Candidate < 0 {
		return best, nil
	}
	if err := copyCandidate(bestPath, filepath.Join(dir, best.Name)); err != nil {
		clearDir(dir)
		return Preview{}, err
	}
	return best, nil
}

// judgePreview opens the candidate file as an image/jpeg and returns its size
// when it is longer-sided than best and, with opts.Decode, decodes. It returns
// width 0 for a candidate that does not beat best.
func (p *Processor) judgePreview(ctx context.Context, m *meter, file string, best Preview, opts PreviewOptions) (w, h int, err error) {
	src, usage, err := p.open(ctx, file, OpenOptions{Type: "image/jpeg"})
	m.add(&usage)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = src.Close() }()
	info := src.Info()
	if max(info.Width, info.Height) <= max(best.Width, best.Height) {
		return 0, 0, nil
	}
	if int64(info.Width)*int64(info.Height) > p.caps.framePixels {
		return 0, 0, tooLarge("candidate is %dx%d pixels, the limit is %d", info.Width, info.Height, p.caps.framePixels)
	}
	if opts.Decode {
		if err := p.decodesWhole(ctx, m, file); err != nil {
			return 0, 0, err
		}
	}
	return info.Width, info.Height, nil
}

// decodesWhole decodes every pixel of the JPEG at file and discards them. A
// JPEG that decodes with a warning, as a cut-short one does, is refused: the
// decoder exits 0 for it.
func (p *Processor) decodesWhole(ctx context.Context, m *meter, file string) error {
	in, err := p.admitInput(ctx, m, "image/jpeg", file)
	if err != nil {
		return err
	}
	defer func() { _ = in.file.Close() }()
	defer in.release()
	env := p.env(m, in.file).pinnedTo(in.file, in.id, "while it was decoded")
	res, err := env.runMagick(ctx, "jpeg:"+in.ref+"[0]", "null:")
	if err != nil {
		return err
	}
	if len(res.Stderr) > 0 {
		return fmt.Errorf("decode: %s", res.Stderr)
	}
	return nil
}

// hasJPEGMarkers reports whether c lies inside the size bytes of f and begins
// with FF D8 and ends with FF D9.
func hasJPEGMarkers(f *os.File, size int64, c PreviewCandidate) bool {
	if c.Length < 4 || c.Offset > size || c.Length > size-c.Offset {
		return false
	}
	var head, tail [2]byte
	if _, err := f.ReadAt(head[:], c.Offset); err != nil {
		return false
	}
	if _, err := f.ReadAt(tail[:], c.Offset+c.Length-2); err != nil {
		return false
	}
	return head == [2]byte{0xff, 0xd8} && tail == [2]byte{0xff, 0xd9}
}

// copyRange writes the bytes of c from f to a new file at dst.
func copyRange(f *os.File, c PreviewCandidate, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.NewSectionReader(f, c.Offset, c.Length)); err != nil {
		_ = out.Close()
		return fmt.Errorf("source: %w", withoutPath(err))
	}
	return out.Close()
}

// copyCandidate writes the bytes of src to a new file at dst.
func copyCandidate(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
