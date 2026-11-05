package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// compositorEnv marks a process as the GIF compositor. The sandbox helper
// starts the consumer's own executable with it as the tool; Init runs the
// compositor and never returns.
const compositorEnv = "WINNOW_MEDIA_COMPOSITOR"

// gifSampleName is the name of the n-th composited frame in the output
// directory while it waits to be encoded.
func gifSampleName(n int) string { return fmt.Sprintf(".sample-%03d.pam", n) }

// iccExtension is the application identifier of the extension that carries
// an ICC profile, which ImageMagick attaches to the image that follows it.
const iccExtension = "ICCRGBG1012"

// gifProfileName is the file the compositor writes the GIF's profile to, in
// the output directory.
const gifProfileName = ".gif-profile.icc"

// gifLayout is what scanGIF reads from the block structure of a GIF.
type gifLayout struct {
	width, height int
	frames        int
	// missingTrailer is set when the data ends between blocks without the
	// trailer, as ImageMagick accepts.
	missingTrailer bool
	// profile is the GIF's ICC profile and profileFrom the index of the
	// first frame it applies to, or nil and -1. A profile applies from the
	// frame that follows its extension to the end, because that is how the
	// frames come out of -coalesce.
	profile     []byte
	profileFrom int
	// own[i] is set when the profile's extension directly precedes frame i.
	own []bool
}

// scanGIF walks the blocks of a GIF without decoding any pixels and refuses
// one that is truncated inside a block, has more than maxFrames frames, has a
// frame that reaches outside the logical screen or carries more than one
// distinct ICC profile. A GIF that ends between blocks without a trailer is
// accepted. It is what bounds the memory of the decode that follows: frames
// times canvas.
func scanGIF(data []byte, maxFrames int) (gifLayout, error) {
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return gifLayout{}, errors.New("not a GIF")
	}
	l := gifLayout{
		width: int(binary.LittleEndian.Uint16(data[6:])), height: int(binary.LittleEndian.Uint16(data[8:])),
		profileFrom: -1,
	}
	pos := 13
	if flags := data[10]; flags&0x80 != 0 {
		pos += 3 << (1 + flags&7)
	}
	// readSubBlocks returns the payload of the sub-blocks at pos, and moves
	// pos past them. keep limits what is copied.
	readSubBlocks := func(keep bool) ([]byte, error) {
		var payload []byte
		for {
			if pos >= len(data) {
				return nil, errors.New("truncated GIF")
			}
			n := int(data[pos])
			if pos+1+n > len(data) {
				return nil, errors.New("truncated GIF")
			}
			if keep {
				payload = append(payload, data[pos+1:pos+1+n]...)
			}
			pos += 1 + n
			if n == 0 {
				return payload, nil
			}
		}
	}
	var extentW, extentH int
	var pending []byte
	for {
		if pos == len(data) {
			l.missingTrailer = true
			break
		}
		if pos > len(data) {
			return gifLayout{}, errors.New("truncated GIF")
		}
		done := false
		switch data[pos] {
		case 0x3B:
			done = true
		case 0x21:
			if pos+2 > len(data) {
				return gifLayout{}, errors.New("truncated GIF")
			}
			label := data[pos+1]
			pos += 2
			if label == 0xFF && pos+12 <= len(data) && data[pos] == 11 && string(data[pos+1:pos+12]) == iccExtension {
				pos += 12
				p, err := readSubBlocks(true)
				if err != nil {
					return gifLayout{}, err
				}
				pending = p
			} else if _, err := readSubBlocks(false); err != nil {
				return gifLayout{}, err
			}
		case 0x2C:
			if pos+10 > len(data) {
				return gifLayout{}, errors.New("truncated GIF")
			}
			left, top := int(binary.LittleEndian.Uint16(data[pos+1:])), int(binary.LittleEndian.Uint16(data[pos+3:]))
			w, h := int(binary.LittleEndian.Uint16(data[pos+5:])), int(binary.LittleEndian.Uint16(data[pos+7:]))
			flags := data[pos+9]
			l.frames++
			l.own = append(l.own, pending != nil)
			if l.frames > maxFrames {
				return gifLayout{}, fmt.Errorf("GIF has more than %d frames", maxFrames)
			}
			if (l.width != 0 && left+w > l.width) || (l.height != 0 && top+h > l.height) {
				return gifLayout{}, fmt.Errorf("frame %d reaches outside the %dx%d canvas", l.frames-1, l.width, l.height)
			}
			extentW, extentH = max(extentW, left+w), max(extentH, top+h)
			if pending != nil {
				switch {
				case l.profile == nil:
					l.profile, l.profileFrom = pending, l.frames-1
				case !bytes.Equal(l.profile, pending):
					return gifLayout{}, errors.New("GIF carries more than one colour profile")
				}
				pending = nil
			}
			pos += 10
			if flags&0x80 != 0 {
				pos += 3 << (1 + flags&7)
			}
			pos++ // LZW minimum code size
			if _, err := readSubBlocks(false); err != nil {
				return gifLayout{}, err
			}
		default:
			return gifLayout{}, fmt.Errorf("invalid GIF block %#x", data[pos])
		}
		if done {
			break
		}
	}
	if l.width == 0 || l.height == 0 {
		l.width, l.height = extentW, extentH
	}
	if l.frames == 0 || l.width == 0 || l.height == 0 {
		return gifLayout{}, errors.New("GIF has no frames")
	}
	return l, nil
}

// decodeGIF scans and decodes data, supplying the trailer a file that ends
// between blocks lacks.
func decodeGIF(data []byte, maxFrames int) (*gif.GIF, gifLayout, error) {
	layout, err := scanGIF(data, maxFrames)
	if err != nil {
		return nil, layout, err
	}
	if layout.missingTrailer {
		data = append(data[:len(data):len(data)], 0x3B)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, layout, err
	}
	if len(g.Image) != layout.frames {
		return nil, layout, fmt.Errorf("decoded %d frames of the %d the block scan found", len(g.Image), layout.frames)
	}
	return g, layout, nil
}

// compositeGIF calls emit with the composited canvas after each frame whose
// index is in selected, in increasing order of index. The canvas passed to
// emit is reused for the next frame. Compositing stops after the last
// selected frame.
//
// It composites the way ImageMagick's -coalesce and browsers do: background
// disposal clears the frame's rectangle to transparent, and a frame without a
// graphic control extension is a frame like any other.
//
// own[i] says that frame i has an ICC profile of its own. emit is told whether
// the canvas carries a profile: ImageMagick gives a coalesced frame the
// profile of the canvas it was built on, which a frame's own profile replaces
// and a restore to the previous canvas undoes.
func compositeGIF(g *gif.GIF, width, height int, own []bool, selected []int, emit func(index int, canvas *image.NRGBA, profiled bool) error) error {
	want := map[int]bool{}
	last := -1
	for _, i := range selected {
		want[i] = true
		last = max(last, i)
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, width, height))
	var saved *image.NRGBA
	var prevRect image.Rectangle
	var prevDisposal byte
	var profiled, savedProfiled bool
	for i, frame := range g.Image {
		if i > last {
			break
		}
		switch prevDisposal {
		case gif.DisposalBackground:
			r := prevRect.Intersect(canvas.Bounds())
			for y := r.Min.Y; y < r.Max.Y; y++ {
				clear(canvas.Pix[canvas.PixOffset(r.Min.X, y):][:r.Dx()*4])
			}
		case gif.DisposalPrevious:
			if saved != nil {
				copy(canvas.Pix, saved.Pix)
				profiled = savedProfiled
			}
		}
		disposal := byte(0)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		if disposal == gif.DisposalPrevious {
			if saved == nil {
				saved = image.NewNRGBA(canvas.Bounds())
			}
			copy(saved.Pix, canvas.Pix)
			savedProfiled = profiled
			// ImageMagick's canvas before the first frame is a clone of
			// that frame, profile included.
			if i == 0 && len(own) > 0 && own[0] {
				savedProfiled = true
			}
		}
		drawOpaque(canvas, frame)
		if i < len(own) && own[i] {
			profiled = true
		}
		prevRect, prevDisposal = frame.Bounds(), disposal
		if want[i] {
			if err := emit(i, canvas, profiled); err != nil {
				return err
			}
		}
	}
	return nil
}

// drawOpaque paints the opaque pixels of src onto dst.
func drawOpaque(dst *image.NRGBA, src *image.Paletted) {
	r := src.Bounds().Intersect(dst.Bounds())
	opaque := make([]bool, len(src.Palette))
	rgba := make([][4]uint8, len(src.Palette))
	for i, c := range src.Palette {
		cr, cg, cb, ca := c.RGBA()
		opaque[i] = ca != 0
		rgba[i] = [4]uint8{uint8(cr >> 8), uint8(cg >> 8), uint8(cb >> 8), 255}
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		in := src.Pix[src.PixOffset(r.Min.X, y):][:r.Dx()]
		out := dst.Pix[dst.PixOffset(r.Min.X, y):][:r.Dx()*4]
		for x, idx := range in {
			if int(idx) < len(opaque) && opaque[idx] {
				copy(out[x*4:], rgba[idx][:])
			}
		}
	}
}

// writePAM writes canvas as a PAM image of tuple type RGB_ALPHA, which
// ImageMagick reads as the "pam" coder without decompressing anything.
func writePAM(w io.Writer, canvas *image.NRGBA) error {
	b := canvas.Bounds()
	if _, err := fmt.Fprintf(w, "P7\nWIDTH %d\nHEIGHT %d\nDEPTH 4\nMAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\n", b.Dx(), b.Dy()); err != nil {
		return err
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if _, err := w.Write(canvas.Pix[canvas.PixOffset(b.Min.X, y):][:b.Dx()*4]); err != nil {
			return err
		}
	}
	return nil
}

// composeRequest is what the compositor is asked to do.
type composeRequest struct {
	path string
	dir  string
	// frames, width and height are what the header read found; the GIF must
	// agree.
	frames, width, height int
	// profileFrom is the first frame the header read found a profile on, or
	// -1; the GIF's profile must start there.
	profileFrom int
	selected    []int
	// inFlight is the most frames the compositor leaves unconsumed in dir.
	inFlight int
}

func (r composeRequest) args() []string {
	idx := make([]string, len(r.selected))
	for i, n := range r.selected {
		idx[i] = strconv.Itoa(n)
	}
	return []string{
		r.path, r.dir, strconv.Itoa(r.inFlight), strconv.Itoa(r.frames),
		strconv.Itoa(r.width), strconv.Itoa(r.height), strings.Join(idx, ","), strconv.Itoa(r.profileFrom),
	}
}

// compositorMain is the compositor process, started by the sandbox helper. It
// does not return.
func compositorMain() {
	if err := runCompositor(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "gif compositor:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runCompositor decodes the GIF named by args (see composeRequest.args),
// composites it and writes each selected frame to the output directory as a
// PAM file, then prints the frame's ordinal and its profile flag on out. A frame is written under
// a temporary name and renamed, so the reader never sees a partial file. Before
// writing ordinal n it waits until ordinal n-inFlight has been consumed, that
// is deleted, so the directory holds about one batch.
//
// A failure, including a panic, is returned as an error.
func runCompositor(args []string, out io.Writer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	if len(args) != 8 {
		return fmt.Errorf("%d arguments, want 8", len(args))
	}
	path, dir := args[0], args[1]
	var nums [4]int
	for i := range nums {
		if nums[i], err = strconv.Atoi(args[2+i]); err != nil || nums[i] < 0 {
			return fmt.Errorf("argument %q is not a count", args[2+i])
		}
	}
	inFlight, frames, width, height := max(1, nums[0]), nums[1], nums[2], nums[3]
	profileFrom, err := strconv.Atoi(args[7])
	if err != nil || profileFrom < -1 {
		return fmt.Errorf("argument %q is not a frame index", args[7])
	}
	var selected []int
	for _, s := range strings.Split(args[6], ",") {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n >= frames {
			return fmt.Errorf("selected frame %q is outside 0..%d", s, frames-1)
		}
		selected = append(selected, n)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	g, layout, err := decodeGIF(data, frames)
	if err != nil {
		return err
	}
	if layout.frames != frames || layout.width != width || layout.height != height || layout.profileFrom != profileFrom {
		return fmt.Errorf("GIF holds %d frames of %dx%d with a profile from frame %d, the header read found %d of %dx%d from %d",
			layout.frames, layout.width, layout.height, layout.profileFrom, frames, width, height, profileFrom)
	}
	if layout.profile != nil {
		if err := os.WriteFile(filepath.Join(dir, gifProfileName), layout.profile, 0o600); err != nil {
			return err
		}
	}

	ordinal := 0
	return compositeGIF(g, width, height, layout.own, selected, func(_ int, canvas *image.NRGBA, profiled bool) error {
		if ordinal >= inFlight {
			for {
				_, err := os.Stat(filepath.Join(dir, gifSampleName(ordinal-inFlight)))
				if errors.Is(err, os.ErrNotExist) {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
		name := filepath.Join(dir, gifSampleName(ordinal))
		f, err := os.Create(name + ".tmp")
		if err != nil {
			return err
		}
		w := bufio.NewWriterSize(f, 1<<20)
		err = writePAM(w, canvas)
		if err == nil {
			err = w.Flush()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(name+".tmp", name)
		}
		if err != nil {
			_ = os.Remove(name + ".tmp")
			return err
		}
		_, err = fmt.Fprintln(out, ordinal, btoi(profiled))
		ordinal++
		return err
	})
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// frameLines parses the compositor's output, one line per frame holding its
// ordinal and whether its canvas carries a profile, and calls onFrame with
// the path of each frame in dir.
type frameLines struct {
	dir     string
	onFrame func(path string, profiled bool) error
	buf     []byte
	next    int
	err     error
}

func (f *frameLines) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for f.err == nil {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := string(f.buf[:i])
		f.buf = f.buf[i+1:]
		if line != fmt.Sprintf("%d 0", f.next) && line != fmt.Sprintf("%d 1", f.next) {
			f.err = fmt.Errorf("compositor reported %q, expected frame %d", line, f.next)
			break
		}
		f.err = f.onFrame(filepath.Join(f.dir, gifSampleName(f.next)), line[len(line)-1] == '1')
		f.next++
	}
	if f.err != nil {
		return 0, f.err
	}
	if len(f.buf) > 64 {
		return 0, errors.New("compositor wrote an overlong line")
	}
	return len(p), nil
}
