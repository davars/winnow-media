package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/davars/winnow-media/tool"
)

// stderrHead and stderrTail are how much of the start and of the end of a
// failed tool's standard error an error carries. A fatal runtime error names
// its cause first and buries it under a goroutine dump.
const (
	stderrHead = 512
	stderrTail = 4 << 10
)

// Processor runs the media operations. Every tool runs in the sandbox
// described at NewProcessor, unless Config.Unsandboxed is set. It owns the
// tools it runs, the ImageMagick policy those runs obey and a private
// directory for both; it refuses to be created unless ImageMagick reports that
// policy as the only one in effect. A Processor is safe for concurrent use.
//
// The paths an operation takes, path and dir, may be relative. An operation
// moves no bytes beyond reading the source and writing into its dir, where
// hidden intermediates (dot-files) exist while a call runs and are removed
// before it returns. The library sets no deadline on a whole operation: a
// caller that wants one passes a context. The
// guarantees that span the operations (the gate, the caps, the sandbox, the
// policy) are in the package documentation.
type Processor struct {
	dir  string
	res  Resources
	caps caps
	// types is the resolved Config.Types, sorted; accepts is it as a set.
	types   []string
	accepts map[string]bool
	tools   map[string]string
	// start runs one process: the sandbox helper, or the direct runner.
	start runner
	run   Runner
	// compose runs the frame compositor of a GIF or animated WebP; tests
	// replace it.
	compose func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) (*Usage, error)
}

// NewProcessor resolves magick, ffmpeg, ffprobe and webp-frames with the tool
// package, checks that the sandbox every tool run goes through is in force,
// creates a private directory with os.MkdirTemp under cfg.Dir, installs a
// generated ImageMagick policy there and reads it back with
// "magick -list policy" and "-list resource". It returns an error, after
// removing the directory, unless the first policy file listed is the
// installed one with exactly the rules written, no other file contributes a
// rule and no resource limit reads back above the ceiling written
// (ImageMagick may lower one, such as threads to the host's CPUs). Call Close
// when done.
//
// The sandbox is a helper process that limits the tool's resources and denies
// it socket creation, and kills whatever the tool leaves running. NewProcessor
// refuses to return unless the helper reports the sandbox applied, with the
// limits derived from cfg.Resources. It returns an error on platforms other
// than Linux on amd64 or arm64, unless cfg.Unsandboxed is set.
//
// It is an error if cfg.Resources has a negative field or a TimeLimit that is
// not a whole number of seconds of at least one, if cfg.Caps has a negative
// field or one above its ceiling, if cfg.Types names a type Types does not
// list, or if cfg.Dir is not an existing directory.
func NewProcessor(ctx context.Context, cfg Config) (*Processor, error) {
	return newProcessor(ctx, cfg, nil)
}

// newProcessor is NewProcessor with wrap, when not nil, applied to the runner
// the Processor would use; tests inject runners through it.
func newProcessor(ctx context.Context, cfg Config, wrap func(runner) runner) (*Processor, error) {
	res, err := cfg.Resources.resolve()
	if err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	limits, err := cfg.Caps.resolve()
	if err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	types, err := resolveTypes(cfg.Types)
	if err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	start := runner(directRunner)
	if !cfg.Unsandboxed {
		if err := sandboxAvailable(); err != nil {
			return nil, fmt.Errorf("processor: %w", err)
		}
		rl := rlimitsFor(res)
		if err := probeSandbox(ctx, rl); err != nil {
			return nil, fmt.Errorf("processor: %w", err)
		}
		start = sandboxRunner(rl)
	}
	if wrap != nil {
		start = wrap(start)
	}
	tools := map[string]string{}
	for _, name := range []string{"magick", "ffmpeg", "ffprobe", webpFramesTool} {
		path, err := tool.Resolve(name)
		if err != nil {
			return nil, fmt.Errorf("processor: %w", err)
		}
		if tools[name], err = filepath.Abs(path); err != nil {
			return nil, fmt.Errorf("processor: %w", err)
		}
	}
	parent := cfg.Dir
	if parent == "" {
		parent = os.TempDir()
	}
	if parent, err = filepath.Abs(parent); err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	if info, err := os.Stat(parent); err != nil {
		return nil, fmt.Errorf("processor: Dir: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("processor: Dir %s is not a directory", parent)
	}
	dir, err := os.MkdirTemp(parent, "media-processor-")
	if err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	accepts := make(map[string]bool, len(types))
	for _, typ := range types {
		accepts[typ] = true
	}
	p := &Processor{dir: dir, res: res, caps: limits, types: types, accepts: accepts, tools: tools, start: start}
	p.run = p.runTool
	p.compose = p.composeFrames
	rules := processorPolicy(res, types)
	if err := p.install(rules); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("processor: %w", err)
	}
	if err := p.verifyPolicy(ctx, rules); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("processor: %w", err)
	}
	return p, nil
}

func (p *Processor) install(rules []policyRule) error {
	if err := os.Mkdir(p.tmpDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.dir, "policy.xml"), policyXML(rules), 0o600)
}

func (p *Processor) tmpDir() string { return filepath.Join(p.dir, "tmp") }

// Close removes the Processor's directory. The Processor must not be used
// afterwards.
func (p *Processor) Close() error { return os.RemoveAll(p.dir) }

// runTempDir creates the directory of one tool run, under the Processor's
// temporary directory. The caller removes it when the run ends: a killed tool
// cannot remove what it wrote there, such as ImageMagick's pixel cache.
func (p *Processor) runTempDir() (string, error) {
	return os.MkdirTemp(p.tmpDir(), "run-")
}

// environ is the whole environment of a tool run, whose temporary files go
// in tmp. ImageMagick also reads $XDG_CONFIG_HOME/ImageMagick/policy.xml and
// $HOME/.config/ImageMagick/policy.xml after the directory
// MAGICK_CONFIGURE_PATH names, and the last matching rule wins, so inheriting
// either variable would let the caller's files widen the policy.
func (p *Processor) environ(tmp string) []string {
	return []string{
		"MAGICK_CONFIGURE_PATH=" + p.dir,
		"MAGICK_TEMPORARY_PATH=" + tmp,
		"TMPDIR=" + tmp,
		"LC_ALL=C",
	}
}

// headTailBuffer keeps the first stderrHead and the last stderrTail bytes
// written to it. It is for a single writer, which is all it needs: exec's copy
// goroutine writes it, and bytes is read after Wait.
type headTailBuffer struct {
	head, tail []byte
	omitted    int
}

func (t *headTailBuffer) Write(b []byte) (int, error) {
	n := len(b)
	if room := stderrHead - len(t.head); room > 0 {
		k := min(room, len(b))
		t.head = append(t.head, b[:k]...)
		b = b[k:]
	}
	t.tail = append(t.tail, b...)
	if drop := len(t.tail) - stderrTail; drop > 0 {
		t.tail = append(t.tail[:0], t.tail[drop:]...)
		t.omitted += drop
	}
	return n, nil
}

// bytes returns what was written, with a note in place of what was dropped
// between the head and the tail.
func (t *headTailBuffer) bytes() []byte {
	out := append([]byte{}, t.head...)
	switch {
	case t.omitted == 1:
		out = append(out, "\n[1 byte omitted]\n"...)
	case t.omitted > 1:
		out = fmt.Appendf(out, "\n[%d bytes omitted]\n", t.omitted)
	}
	return append(out, t.tail...)
}

// meter accumulates the usage of the runs of one operation.
type meter struct{ usage Usage }

// env returns an Env that runs through the Processor and adds each run to m.
func (p *Processor) env(m *meter) Env {
	res, limits := p.res, p.caps
	return Env{
		resources: &res,
		caps:      &limits,
		Run: func(ctx context.Context, name string, args ...string) (Result, error) {
			res, err := p.run(ctx, name, args...)
			m.add(res.Usage)
			return res, err
		},
		compose: func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) error {
			usage, err := p.compose(ctx, req, onFrame)
			m.add(usage)
			return err
		},
	}
}

func (m *meter) add(u *Usage) {
	if u != nil {
		m.usage.CPU += u.CPU
		m.usage.MaxRSS = max(m.usage.MaxRSS, u.MaxRSS)
	}
}

// admitted is an input that passed the gate. path names it by a neutral link
// in the Processor's directory, which release removes.
type admitted struct {
	path, dir string
	decoder   decoder
	// scenes are the headers of the first image, read by the gate for a file
	// it named by ImageMagick's detection; nil otherwise.
	scenes  []sceneHeader
	release func()
}

// request is the open request of an operation on the admitted input, which
// decodes into its output directory.
func (in admitted) request(h Hints, pick func(int) ([]int, error), stream bool) openRequest {
	return openRequest{
		path: in.path, hints: h, dec: in.decoder, pinned: true, caps: true,
		scenes: in.scenes, pick: pick, scratch: in.dir, stream: stream,
	}
}

// encodeAnimationFrames opens the admitted animation with the frames pick
// chooses and writes them to its output directory.
func (e Env) encodeAnimationFrames(ctx context.Context, r region, in admitted, enc encoding, pick func(int) ([]int, error)) ([]Image, error) {
	src, err := e.open(ctx, in.request(Hints{}, pick, true))
	if err != nil {
		return nil, err
	}
	defer src.Close()
	return src.encodeFiles(ctx, r, enc, in.dir)
}

// accepted says what an operation takes: the kind of type, and, when typ is
// not empty, the type the caller names for bytes that name none themselves.
type accepted struct {
	kind mediaKind
	typ  string
}

// admit checks everything about an operation's input and output directory
// that can be checked before pixels are read. The input's type comes from its
// bytes; neither its name nor any claim of the caller is consulted.
func (p *Processor) admit(ctx context.Context, m *meter, a accepted, path, dir string) (admitted, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return admitted{}, err
	}
	if err := requireEmptyDir(dir); err != nil {
		return admitted{}, err
	}
	in, err := p.admitInput(ctx, m, a, path)
	if err != nil {
		return admitted{}, err
	}
	in.dir = dir
	return in, nil
}

// admitInput is admit for an operation that writes no files: it checks the
// input alone, and the result has no dir. The input must be a regular file
// within Caps.MaxInputBytes; its type must be one of Config.Types that the
// operation reads. A GIF, WebP, ISO base media or EBML file is typed by its
// leading bytes before any tool runs, and every animation and video type is
// of that kind. Other content is named by one header read under the generated
// policy, which allows only the coders of Config.Types, so the content can
// select no other.
func (p *Processor) admitInput(ctx context.Context, m *meter, a accepted, path string) (admitted, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return admitted{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return admitted{}, fmt.Errorf("source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return admitted{}, fmt.Errorf("source %s is not a regular file", path)
	}
	if info.Size() > p.caps.inputBytes {
		return admitted{}, tooLarge("file is %d bytes, the limit is %d", info.Size(), p.caps.inputBytes)
	}
	typ, err := sniffFile(path)
	named := err == nil
	if err != nil && !errors.Is(err, errUnnamed) {
		return admitted{}, err
	}
	if named && a.typ != "" && a.typ != typ {
		return admitted{}, unrecognized(fmt.Sprintf("the bytes are %s, not %s", typ, a.typ))
	}
	if !named {
		typ = a.typ
	}
	if typ == "" && a.kind&kindStill == 0 {
		return admitted{}, errUnnamed
	}
	if typ != "" {
		if err := p.checkType(typ, a.kind); err != nil {
			return admitted{}, err
		}
		if typ == typeWebP && a.kind&kindAnimation == 0 && webpAnimated(path) {
			return admitted{}, unrecognized("an animated WebP is read by the animation operations")
		}
	}
	link, release, err := p.neutralLink(path)
	if err != nil {
		return admitted{}, err
	}
	in := admitted{path: link, release: release}
	if typ == "" {
		var coder string
		if typ, coder, in.scenes, err = p.detectType(ctx, m, link, a.kind); err != nil {
			release()
			return admitted{}, err
		}
		in.decoder = typeTable[typ].decoderFor(coder)
		return in, nil
	}
	in.decoder = typeTable[typ].decoderFor("")
	return in, nil
}

// checkType refuses a type the Processor was not configured with, or one the
// operation does not read, with ErrUnsupportedType.
func (p *Processor) checkType(typ string, kind mediaKind) error {
	if !p.accepts[typ] {
		return unrecognized(typ)
	}
	if typeTable[typ].kind&kind == 0 {
		return unrecognized(typ + " is not read by this operation")
	}
	return nil
}

// neutralLink makes an extensionless link to path in a new directory of the
// Processor's, so that no tool is given the file's name. release removes it.
func (p *Processor) neutralLink(path string) (link string, release func(), err error) {
	dir, err := os.MkdirTemp(p.tmpDir(), "in-")
	if err != nil {
		return "", nil, err
	}
	release = func() { _ = os.RemoveAll(dir) }
	link = filepath.Join(dir, "input")
	if err := os.Symlink(path, link); err != nil {
		release()
		return "", nil, err
	}
	return link, release, nil
}

// detectType reads the headers of the first image of the file link names
// under the Processor's policy. It returns the type the coder that read them
// maps to, which must be one the operation accepts, the coder, and the
// headers, which the operation then does not read again. A TIFF whose first
// image is a reduced-resolution subfile, as the preview of a camera raw
// file is, is refused: the file's own type is not TIFF.
func (p *Processor) detectType(ctx context.Context, m *meter, link string, kind mediaKind) (typ, coder string, scenes []sceneHeader, err error) {
	scenes, err = identifyHeadersAs(ctx, p.env(m), link+"[0]", true, gateHeaderFormat)
	if err != nil {
		if msg := err.Error(); !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrTimeLimit) &&
			(strings.Contains(msg, "security policy") || strings.Contains(msg, "no decode delegate")) {
			return "", "", nil, unrecognized("content no supported coder reads: " + firstLine(msg))
		}
		return "", "", nil, err
	}
	coder = scenes[0].format
	typ, ok := coderTypes[coder]
	if !ok {
		return "", "", nil, unrecognized(fmt.Sprintf("ImageMagick coder %q", coder))
	}
	if err := p.checkType(typ, kind); err != nil {
		return "", "", nil, err
	}
	if scenes[0].subfile == "REDUCEDIMAGE" {
		return "", "", nil, unrecognized("the file's first image is a reduced-resolution preview, as in a camera raw file; name its type")
	}
	return typ, coder, scenes, nil
}

// firstLine returns s up to its first newline.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Still decodes the first image of a file of any type in Config.Types other
// than GIF and video, and of a WebP that is not animated, and writes it to dir
// as still.<ext> in the encoding's format. The output is upright, converted
// to sRGB when the source carries an ICC profile, flattened onto white for
// JPEG and free of metadata.
//
// The type is taken from the file's bytes, not its name. dir must exist and
// be empty. Every argument is validated, and the input checked against the
// size caps, before pixels are read: a type the operation does not accept
// gives an error wrapping ErrUnsupportedType, and an input over a cap one
// wrapping ErrTooLarge. A tool run that reaches ImageMagick's time limit
// gives an error wrapping ErrTimeLimit. On failure Still removes what it
// wrote, leaving dir empty. Usage covers every tool run of the call,
// including those of a failed one.
func (p *Processor) Still(ctx context.Context, path string, enc Encoding, dir string) (Image, Usage, error) {
	return p.still(ctx, path, "", enc, dir)
}

// still is Still for bytes that name no type themselves, which the caller
// may name as typ.
func (p *Processor) still(ctx context.Context, path, typ string, enc Encoding, dir string) (Image, Usage, error) {
	var ec encoding
	images, usage, err := p.operate(ctx, "still", accepted{kindStill, typ}, path, dir,
		func() (err error) { ec, err = enc.resolve(); return err },
		func(env Env, in admitted) ([]Image, error) {
			src, err := env.open(ctx, in.request(Hints{Still: true}, nil, false))
			if err != nil {
				return nil, err
			}
			defer src.Close()
			img, err := src.stillImage(ctx, region{}, ec, in.dir)
			return []Image{img}, err
		})
	if err != nil {
		return Image{}, usage, err
	}
	return images[0], usage, nil
}

// AnimationFrames decodes a GIF or WebP file whose type is in Config.Types,
// composites its frames, samples up to maxFrames of them evenly and writes
// each to dir as frame-NNN.<ext>. A sampled frame identical to the one before it after
// resizing is dropped and the files are numbered consecutively from 0;
// Image.Frame is the index of the source frame a file shows. Output is as
// for Still.
//
// maxFrames 0 means 16; otherwise it must be 1 to 64. Errors, Usage and the
// state of dir are as for Still; a file with more frames or pixels than the
// caps allow gives an error wrapping ErrTooLarge. A GIF that ends between
// blocks without its trailer is accepted, as ImageMagick does; one truncated
// inside a block, or with a frame outside the logical screen, is refused.
func (p *Processor) AnimationFrames(ctx context.Context, path string, enc Encoding, maxFrames int, dir string) ([]Image, Usage, error) {
	var ec encoding
	var frames int
	return p.operate(ctx, "animation frames", accepted{kind: kindAnimation}, path, dir,
		func() (err error) {
			if ec, err = enc.resolve(); err != nil {
				return err
			}
			frames, err = resolveFrames(maxFrames)
			return err
		},
		func(env Env, in admitted) ([]Image, error) {
			return env.encodeAnimationFrames(ctx, region{}, in, ec, func(sourceFrames int) ([]int, error) {
				return selectFrames(frameCount(frames, sourceFrames, true), sourceFrames), nil
			})
		})
}

// VideoFrames decodes up to maxFrames still images from an MP4, QuickTime,
// WebM or Matroska file whose type is in Config.Types and writes them to dir
// as frame-NNN.<ext>. The samples are the centres of equal intervals of the
// duration of the video stream flagged default (else the first), ignoring
// attached pictures; fewer
// when the stream has fewer frames, targets that coincide are merged, and a
// clip of unknown duration gives one sample at 0. A video that yields no frame
// at any of those times, as when its only frame precedes a duration the
// container reports, is sampled at 0 instead, and fails only if that gives no
// frame either. A stream whose frame count is known and whose decoded pixels
// (frames times width times height as stored) are at most 1e9 is read by one
// decode, in which a select filter keeps the first frame at or after each
// sample time; any other by one decode per sample, started a margin before
// the time. The frame count is the container's or, where it gives none (as in
// Matroska and MPEG-TS), the duration times the declared rate, so a
// variable-rate stream is estimated high and may take the seeks; a stream with
// neither is always read by seeks. An input with no video stream is an
// error.
//
// A container with more than 32 streams, a stream longer than six hours or
// one wider or taller than 16384 pixels gives an error wrapping ErrTooLarge
// before any frame is decoded. HDR (HLG or PQ) video is tone-mapped to SDR,
// output is upright whatever the stream's display rotation, and a sample
// identical to the one before it is dropped, with the files numbered
// consecutively from 0.
// Image.TimeMS is the sample time in milliseconds; Image.Frame is 0 because a
// sample has no known frame index. Output is otherwise as for Still.
//
// maxFrames 0 means 16; otherwise it must be 1 to 64. Errors, Usage and the
// state of dir are as for Still.
func (p *Processor) VideoFrames(ctx context.Context, path string, enc Encoding, maxFrames int, dir string) ([]Image, Usage, error) {
	var ec encoding
	var frames int
	return p.operate(ctx, "video frames", accepted{kind: kindVideo}, path, dir,
		func() (err error) {
			if ec, err = enc.resolve(); err != nil {
				return err
			}
			frames, err = resolveFrames(maxFrames)
			return err
		},
		func(env Env, in admitted) ([]Image, error) {
			return env.videoFrames(ctx, region{}, in.decoder, in.path, ec, frames, in.dir)
		})
}

// operate runs a Processor operation that writes into dir. check validates
// the caller's arguments and runs first, then path and dir are admitted
// against a, all before pixels are read. body runs in dir with an Env that
// meters its tool runs; dir is emptied again if it fails. Every error is
// prefixed with name, and the Usage returned covers body's runs and the type
// detection's, even when the call fails.
func (p *Processor) operate(ctx context.Context, name string, a accepted, path, dir string, check func() error, body func(Env, admitted) ([]Image, error)) ([]Image, Usage, error) {
	if err := check(); err != nil {
		return nil, Usage{}, fmt.Errorf("%s: %w", name, err)
	}
	var m meter
	in, err := p.admit(ctx, &m, a, path, dir)
	if err != nil {
		return nil, m.usage, fmt.Errorf("%s: %w", name, err)
	}
	defer in.release()
	images, err := inDir(in.dir, func() ([]Image, error) { return body(p.env(&m), in) })
	if err != nil {
		return nil, m.usage, fmt.Errorf("%s: %w", name, err)
	}
	return images, m.usage, nil
}
