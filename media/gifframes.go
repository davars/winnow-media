package media

import (
	"bufio"
	"bytes"
	"crypto/sha256"
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

// composedProfileName is the file a compositor writes the ICC profile with the
// given id to, in the output directory, before the first frame that carries
// it.
func composedProfileName(id int) string { return fmt.Sprintf(".profile-%d.icc", id) }

// gifLayout is what scanGIF reads from the block structure of a GIF.
type gifLayout struct {
	width, height int
	frames        int
	// missingTrailer is set when the data ends between blocks without the
	// trailer, as ImageMagick accepts.
	missingTrailer bool
	// profiles holds the distinct ICC profiles in order of first appearance;
	// the profile with id n is profiles[n-1].
	profiles [][]byte
	// own[i] is the id of the profile whose extension directly precedes frame
	// i, or 0.
	own []int
}

// scanGIF walks the blocks of a GIF without decoding any pixels and refuses
// one that is truncated inside a block, has more than maxFrames frames, has a
// frame that reaches outside the logical screen. A GIF that ends between blocks without a trailer is
// accepted. It is what bounds the memory of the decode that follows: frames
// times canvas.
func scanGIF(data []byte, maxFrames int) (gifLayout, error) {
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return gifLayout{}, errors.New("not a GIF")
	}
	l := gifLayout{
		width: int(binary.LittleEndian.Uint16(data[6:])), height: int(binary.LittleEndian.Uint16(data[8:])),
	}
	ids := map[[sha256.Size]byte]int{}
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
			id := 0
			if pending != nil {
				sum := sha256.Sum256(pending)
				if id = ids[sum]; id == 0 {
					l.profiles = append(l.profiles, pending)
					id = len(l.profiles)
					ids[sum] = id
				}
				pending = nil
			}
			l.own = append(l.own, id)
			if l.frames > maxFrames {
				return gifLayout{}, fmt.Errorf("GIF has more than %d frames", maxFrames)
			}
			if (l.width != 0 && left+w > l.width) || (l.height != 0 && top+h > l.height) {
				return gifLayout{}, fmt.Errorf("frame %d reaches outside the %dx%d canvas", l.frames-1, l.width, l.height)
			}
			extentW, extentH = max(extentW, left+w), max(extentH, top+h)
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
// own[i] is the id of the ICC profile frame i has of its own, or 0. emit is
// told the id of the profile the canvas carries, 0 for none: ImageMagick gives
// a coalesced frame the profile of the canvas it was built on, which a frame's
// own profile replaces and a restore to the previous canvas undoes.
func compositeGIF(g *gif.GIF, width, height int, own []int, selected []int, emit func(index int, canvas *image.NRGBA, profile int) error) error {
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
	var profile, savedProfile int
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
				profile = savedProfile
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
			savedProfile = profile
			// ImageMagick's canvas before the first frame is a clone of
			// that frame, profile included.
			if i == 0 && len(own) > 0 && own[0] > 0 {
				savedProfile = own[0]
			}
		}
		drawOpaque(canvas, frame)
		if i < len(own) && own[i] > 0 {
			profile = own[i]
		}
		prevRect, prevDisposal = frame.Bounds(), disposal
		if want[i] {
			if err := emit(i, canvas, profile); err != nil {
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
	// own[i] is set when the header read found a profile on frame i; the
	// GIF must agree.
	own      []bool
	selected []int
	// inFlight is the most frames the compositor leaves unconsumed in dir.
	inFlight int
	// consumeTimeout is how long the compositor waits for a frame to be
	// consumed; zero means the package's consumeTimeout.
	consumeTimeout time.Duration
	// external names the tool that composites the frames, or is empty for
	// the GIF compositor of this package.
	external string
}

// ownMask renders own as a string of 0 and 1, one per frame.
func ownMask(own []bool) string {
	b := make([]byte, len(own))
	for i, o := range own {
		b[i] = '0'
		if o {
			b[i] = '1'
		}
	}
	return string(b)
}

func (r composeRequest) timeoutMS() string {
	d := r.consumeTimeout
	if d <= 0 {
		d = consumeTimeout
	}
	return strconv.FormatInt(d.Milliseconds(), 10)
}

func (r composeRequest) args() []string {
	if r.external != "" {
		return r.externalArgs()
	}
	idx := make([]string, len(r.selected))
	for i, n := range r.selected {
		idx[i] = strconv.Itoa(n)
	}
	return []string{
		r.path, r.dir, strconv.Itoa(r.inFlight), strconv.Itoa(r.frames),
		strconv.Itoa(r.width), strconv.Itoa(r.height), strings.Join(idx, ","), ownMask(r.own),
		r.timeoutMS(),
	}
}

// externalArgs are the arguments of the webp-frames tool, which is told the
// header read's canvas and frame count and refuses a file that differs.
func (r composeRequest) externalArgs() []string {
	idx := make([]string, len(r.selected))
	for i, n := range r.selected {
		idx[i] = strconv.Itoa(n)
	}
	return []string{
		r.path, r.dir, strconv.Itoa(r.inFlight),
		strconv.Itoa(r.width), strconv.Itoa(r.height), strconv.Itoa(r.frames), strings.Join(idx, ","),
		r.timeoutMS(),
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
// PAM file, then prints the frame's ordinal and the id of its profile on out. A
// frame is written under a temporary name and renamed, so the reader never sees
// a partial file. A profile is written to its own file before the first frame
// that uses it. Before writing ordinal n it waits until ordinal n-inFlight has
// been consumed, that is deleted, so the directory holds about one batch; it
// fails if that takes longer than the timeout in the last argument.
//
// A failure, including a panic, is returned as an error.
func runCompositor(args []string, out io.Writer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	if len(args) != 9 {
		return fmt.Errorf("%d arguments, want 9", len(args))
	}
	timeoutMS, err := strconv.Atoi(args[8])
	if err != nil || timeoutMS < 1 {
		return fmt.Errorf("argument %q is not a timeout in milliseconds", args[8])
	}
	timeout := time.Duration(timeoutMS) * time.Millisecond
	path, dir := args[0], args[1]
	var nums [4]int
	for i := range nums {
		if nums[i], err = strconv.Atoi(args[2+i]); err != nil || nums[i] < 0 {
			return fmt.Errorf("argument %q is not a count", args[2+i])
		}
	}
	inFlight, frames, width, height := max(1, nums[0]), nums[1], nums[2], nums[3]
	mask := args[7]
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
	if layout.frames != frames || layout.width != width || layout.height != height || ownMask(layout.ownFlags()) != mask {
		return fmt.Errorf("GIF holds %d frames of %dx%d, the header read found %d of %dx%d, or they differ in their profiles",
			layout.frames, layout.width, layout.height, frames, width, height)
	}
	written := map[int]bool{}

	ordinal := 0
	return compositeGIF(g, width, height, layout.own, selected, func(_ int, canvas *image.NRGBA, profile int) error {
		if ordinal >= inFlight {
			deadline := time.Now().Add(timeout)
			for {
				_, err := os.Stat(filepath.Join(dir, gifSampleName(ordinal-inFlight)))
				if errors.Is(err, os.ErrNotExist) {
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("frame %d not consumed in %v", ordinal-inFlight, timeout)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
		if profile > 0 && !written[profile] {
			if err := os.WriteFile(filepath.Join(dir, composedProfileName(profile)), layout.profiles[profile-1], 0o600); err != nil {
				return err
			}
			written[profile] = true
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
		_, err = fmt.Fprintln(out, ordinal, profile)
		ordinal++
		return err
	})
}

// ownFlags says which frames have a profile of their own.
func (l gifLayout) ownFlags() []bool {
	flags := make([]bool, len(l.own))
	for i, id := range l.own {
		flags[i] = id > 0
	}
	return flags
}

// frameLines parses the compositor's output, one line per frame holding its
// ordinal and the id of the profile its canvas carries, and calls onFrame with
// the path of each frame in dir.
type frameLines struct {
	dir     string
	onFrame func(path string, profile int) error
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
		fields := strings.Fields(line)
		var n, id int
		var err1, err2 error
		if len(fields) == 2 {
			n, err1 = strconv.Atoi(fields[0])
			id, err2 = strconv.Atoi(fields[1])
		}
		if len(fields) != 2 || err1 != nil || err2 != nil || n != f.next || id < 0 || id > maxAnimationFrames {
			f.err = fmt.Errorf("compositor reported %q, expected frame %d", line, f.next)
			break
		}
		f.err = f.onFrame(filepath.Join(f.dir, gifSampleName(f.next)), id)
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
