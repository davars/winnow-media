package media

import (
	"bufio"
	"bytes"
	"context"
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
	// headerEnd is the offset after the logical screen descriptor and the
	// global colour table. span[i] is the byte range from the end of frame
	// i-1 (headerEnd for frame 0) to the end of frame i's image data, so it
	// holds the extensions that precede the frame; tail is what follows the
	// last frame up to, not including, the trailer.
	headerEnd int
	span      [][2]int
	tail      [2]int
	// gce[i] is set when a graphic control extension precedes frame i.
	gce []bool
}

// scanGIF walks the blocks of a GIF without decoding any pixels and refuses
// one that is truncated inside a block, has more than maxFrames frames, or has
// a frame that reaches outside the logical screen. A canvas (the logical
// screen, or the extent of the frames when that is 0x0) wider or taller than
// maxDimension or larger than maxFramePixels is refused with ErrTooLarge. A
// GIF that ends between blocks without a trailer is accepted. It is what
// bounds the decode that follows: the number of frames and the extent of the
// canvas.
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
	if pos > len(data) {
		return gifLayout{}, errors.New("truncated GIF")
	}
	l.headerEnd = pos
	spanStart := pos
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
	gce := false
	for {
		if pos == len(data) {
			l.missingTrailer = true
			l.tail = [2]int{spanStart, pos}
			break
		}
		if pos > len(data) {
			return gifLayout{}, errors.New("truncated GIF")
		}
		done := false
		switch data[pos] {
		case 0x3B:
			l.tail = [2]int{spanStart, pos}
			done = true
		case 0x21:
			if pos+2 > len(data) {
				return gifLayout{}, errors.New("truncated GIF")
			}
			label := data[pos+1]
			pos += 2
			if label == 0xF9 {
				gce = true
			}
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
			l.gce = append(l.gce, gce)
			gce = false
			l.span = append(l.span, [2]int{spanStart, pos})
			spanStart = pos
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
	if l.width > maxDimension || l.height > maxDimension {
		return gifLayout{}, tooLarge("GIF canvas is %dx%d, the limit is %d on each side", l.width, l.height, maxDimension)
	}
	if px := int64(l.width) * int64(l.height); px > maxFramePixels {
		return gifLayout{}, tooLarge("GIF canvas has %d pixels, the limit is %d", px, int64(maxFramePixels))
	}
	if l.frames == 0 || l.width == 0 || l.height == 0 {
		return gifLayout{}, errors.New("GIF has no frames")
	}
	return l, nil
}

// gifDecoder decodes the frames of a scanned GIF one at a time.
type gifDecoder struct {
	data   []byte
	layout gifLayout
	buf    []byte
	// disposal is the disposal method of the previous frame, which a frame
	// without a graphic control extension inherits, as in ImageMagick and in
	// gif.DecodeAll.
	disposal byte
}

// newGIFDecoder scans data and returns a decoder of its frames.
func newGIFDecoder(data []byte, maxFrames int) (*gifDecoder, error) {
	layout, err := scanGIF(data, maxFrames)
	if err != nil {
		return nil, err
	}
	return &gifDecoder{data: data, layout: layout}, nil
}

// frame decodes frame i the way gif.DecodeAll does and returns it with its
// disposal method. The standard library is given a GIF of the header, the
// extensions before the frame, the frame and a trailer, so memory is that of
// one frame whatever their number. The last frame carries the blocks after it,
// so that the whole file is validated, and the trailer a file lacks is
// supplied. Frames must be requested in order, from 0.
func (d *gifDecoder) frame(i int) (*image.Paletted, byte, error) {
	l := d.layout
	d.buf = append(d.buf[:0], d.data[:l.headerEnd]...)
	d.buf = append(d.buf, d.data[l.span[i][0]:l.span[i][1]]...)
	if i == l.frames-1 {
		d.buf = append(d.buf, d.data[l.tail[0]:l.tail[1]]...)
	}
	d.buf = append(d.buf, 0x3B)
	g, err := gif.DecodeAll(bytes.NewReader(d.buf))
	if err != nil {
		return nil, 0, fmt.Errorf("frame %d: %w", i, err)
	}
	if len(g.Image) != 1 || len(g.Disposal) != 1 {
		return nil, 0, fmt.Errorf("frame %d: decoded %d frames", i, len(g.Image))
	}
	if l.gce[i] {
		d.disposal = g.Disposal[0]
	}
	return g.Image[0], d.disposal, nil
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
func compositeGIF(frame func(i int) (*image.Paletted, byte, error), width, height int, own []int, selected []int, emit func(index int, canvas *image.NRGBA, profile int) error) error {
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
	for i := 0; i <= last; i++ {
		img, disposal, err := frame(i)
		if err != nil {
			return err
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
		drawOpaque(canvas, img)
		if i < len(own) && own[i] > 0 {
			profile = own[i]
		}
		prevRect, prevDisposal = img.Bounds(), disposal
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

// job is the in-process GIF compositing the request describes.
func (r composeRequest) job() gifJob {
	return gifJob{
		path: r.path, dir: r.dir, frames: r.frames, width: r.width, height: r.height,
		mask: ownMask(r.own), selected: r.selected,
	}
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
// PAM file, then prints the frame's ordinal, the id of its profile and 1 on
// out, the last being the frame's declared colour format (see frameLines). A
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
	job := gifJob{path: path, dir: dir, frames: frames, width: width, height: height, mask: mask, selected: selected}
	return job.run(context.Background(), func(ordinal int) error {
		if ordinal < inFlight {
			return nil
		}
		deadline := time.Now().Add(timeout)
		for {
			_, err := os.Stat(filepath.Join(dir, gifSampleName(ordinal-inFlight)))
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("frame %d not consumed in %v", ordinal-inFlight, timeout)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}, func(ordinal, profile int) error {
		// Every GIF frame is palette-based.
		_, err := fmt.Fprintln(out, ordinal, profile, 1)
		return err
	})
}

// gifJob is a request to composite the selected frames of the GIF at path
// into PAM files in dir. frames, width, height and mask (see ownMask) are what
// the header read found; the GIF must agree.
type gifJob struct {
	path, dir             string
	frames, width, height int
	mask                  string
	selected              []int
}

// run decodes and composites the GIF and writes each selected frame to dir
// as gifSampleName(ordinal), through a temporary name, with the profile it
// carries written to composedProfileName before the first frame that uses it.
// wait is called before each frame is written; written is called after, with
// the ordinal and the id of the profile of the frame. Decoding stops with
// ctx's error between frames.
func (j gifJob) run(ctx context.Context, wait func(ordinal int) error, written func(ordinal, profile int) error) error {
	data, err := os.ReadFile(j.path)
	if err != nil {
		return err
	}
	dec, err := newGIFDecoder(data, j.frames)
	if err != nil {
		return err
	}
	layout := dec.layout
	if layout.frames != j.frames || layout.width != j.width || layout.height != j.height || ownMask(layout.ownFlags()) != j.mask {
		return fmt.Errorf("GIF holds %d frames of %dx%d, the header read found %d of %dx%d, or they differ in their profiles",
			layout.frames, layout.width, layout.height, j.frames, j.width, j.height)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	frame := func(i int) (*image.Paletted, byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		return dec.frame(i)
	}
	profilesWritten := map[int]bool{}
	ordinal := 0
	err = compositeGIF(frame, j.width, j.height, layout.own, j.selected, func(_ int, canvas *image.NRGBA, profile int) error {
		if wait != nil {
			if err := wait(ordinal); err != nil {
				return err
			}
		}
		if profile > 0 && !profilesWritten[profile] {
			if err := os.WriteFile(filepath.Join(j.dir, composedProfileName(profile)), layout.profiles[profile-1], 0o600); err != nil {
				return err
			}
			profilesWritten[profile] = true
		}
		name := filepath.Join(j.dir, gifSampleName(ordinal))
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
		err = written(ordinal, profile)
		ordinal++
		return err
	})
	return err
}

// ownFlags says which frames have a profile of their own.
func (l gifLayout) ownFlags() []bool {
	flags := make([]bool, len(l.own))
	for i, id := range l.own {
		flags[i] = id > 0
	}
	return flags
}

// composedFrame is a frame a compositor wrote.
type composedFrame struct {
	path string
	// profile is the id of the ICC profile the frame carries, 0 for none.
	profile int
	// soft is set when the frame's source declares a palette or alpha, which
	// picks the resize filter (see filterFor).
	soft bool
}

// frameLines parses the compositor's output, one line per frame holding its
// ordinal, the id of the profile its canvas carries and 1 or 0 for whether
// its source declares a palette or alpha, and calls onFrame with each frame
// in dir.
type frameLines struct {
	dir     string
	onFrame func(composedFrame) error
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
		var n, id, soft int
		var err1, err2, err3 error
		if len(fields) == 3 {
			n, err1 = strconv.Atoi(fields[0])
			id, err2 = strconv.Atoi(fields[1])
			soft, err3 = strconv.Atoi(fields[2])
		}
		if len(fields) != 3 || err1 != nil || err2 != nil || err3 != nil || n != f.next || id < 0 || id > maxAnimationFrames || soft < 0 || soft > 1 {
			f.err = fmt.Errorf("compositor reported %q, expected frame %d", line, f.next)
			break
		}
		f.err = f.onFrame(composedFrame{path: filepath.Join(f.dir, gifSampleName(f.next)), profile: id, soft: soft == 1})
		f.next++
	}
	if f.err != nil {
		return 0, f.err
	}
	if len(f.buf) > 64 {
		f.err = errors.New("compositor wrote an overlong line")
		return 0, f.err
	}
	return len(p), nil
}
