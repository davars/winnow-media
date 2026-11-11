package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/davars/winnow-media/tool"
)

// stderrHead and stderrTail are how much of the start and of the end of a
// failed tool's standard error an error carries. A fatal runtime error names
// its cause first and buries it under a goroutine dump.
const (
	stderrHead = 512
	stderrTail = 4 << 10
)

// Processor opens input files as Sources and runs the tools that read them.
// Every tool runs in the sandbox described at NewProcessor, unless
// Config.Unsandboxed is set. It owns the tools it runs, the ImageMagick policy
// those runs obey and a private directory for both; it refuses to be created unless ImageMagick reports that
// policy as the only one in effect. A Processor is safe for concurrent use.
//
// The paths a call takes, path and dir, may be relative. A call moves no bytes
// beyond reading the source and writing into its dir, where hidden
// intermediates (dot-files) exist while a call runs and are removed before it
// returns. The library sets no deadline on a whole call: a caller that wants
// one passes a context. The guarantees that span the calls (the gate, the
// caps, the sandbox, the policy) are in the package documentation.
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
	// openInput opens an input's file; tests replace it.
	openInput func(path string) (*os.File, error)
	// compose runs the frame compositor of a GIF or animated WebP; tests
	// replace it.
	compose func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) (*Usage, error)

	// mu guards closed and sources, the Sources Open made that are not yet
	// closed.
	mu      sync.Mutex
	closed  bool
	sources map[*Source]struct{}
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
	p := &Processor{openInput: openInputFile, dir: dir, res: res, caps: limits, types: types, accepts: accepts, tools: tools, start: start}
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

// Close closes every Source the Processor opened that is still open, waiting
// for the calls running on them, then removes the Processor's directory. The Processor must not be used
// afterwards; Open on a closed Processor fails.
func (p *Processor) Close() error {
	p.mu.Lock()
	p.closed = true
	open := make([]*Source, 0, len(p.sources))
	for s := range p.sources {
		open = append(open, s)
	}
	p.mu.Unlock()
	for _, s := range open {
		_ = s.Close()
	}
	return os.RemoveAll(p.dir)
}

// register records src as open, and reports false if the Processor is closed.
func (p *Processor) register(src *Source) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	if p.sources == nil {
		p.sources = map[*Source]struct{}{}
	}
	p.sources[src] = struct{}{}
	return true
}

func (p *Processor) forget(src *Source) {
	p.mu.Lock()
	delete(p.sources, src)
	p.mu.Unlock()
}

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
// Each run it starts inherits in, when not nil, as descriptor 3.
func (p *Processor) env(m *meter, in *os.File) Env {
	res, limits := p.res, p.caps
	return Env{
		resources: &res,
		caps:      &limits,
		Run: func(ctx context.Context, name string, args ...string) (Result, error) {
			res, err := p.run(withInput(ctx, in), name, args...)
			m.add(res.Usage)
			return res, err
		},
		compose: func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) error {
			usage, err := p.compose(withInput(ctx, in), req, onFrame)
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

// admitted is an input that passed the gate. file is the descriptor every
// later read of the input goes through, and ref what the tools call it. dir is
// a directory of the Processor's that release removes; the caller closes file.
type admitted struct {
	file     *os.File
	ref, dir string
	// abs is the input's own absolute path and id its identity when it was
	// admitted.
	abs     string
	id      fileID
	typ     string
	decoder decoder
	// scenes are the headers of every image, read by the gate for a file it
	// named by ImageMagick's detection; nil otherwise.
	scenes  []sceneHeader
	release func()
}

// accepted says what an operation takes: the kinds of type, and, when typ is
// not empty, the type the caller names, which replaces the type the bytes and
// ImageMagick's detection name.
type accepted struct {
	kind mediaKind
	typ  string
}

// admitInput opens the input once and checks everything about it that can be
// checked before pixels are read, through that descriptor alone, so that
// replacing the file at path afterwards changes nothing that was checked or
// that is read. The input must be a regular file within Caps.MaxInputBytes;
// its type must be one of Config.Types that the operation reads. A GIF, WebP,
// ISO base media or EBML file is typed by its leading bytes before any tool
// runs, and every animation and video type is of that kind. Other content is
// named by one header read under the generated policy, which allows only the
// coders of Config.Types, so the content can select no other. A type the
// caller names is used as it is, with no sniff or header read to name one.
func (p *Processor) admitInput(ctx context.Context, m *meter, a accepted, path string) (in admitted, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return admitted{}, err
	}
	// The mode is judged before the open, which can have effects on a device;
	// the descriptor must then be the file that was judged.
	pre, err := os.Stat(path)
	if err != nil {
		return admitted{}, fmt.Errorf("source: %w", err)
	}
	if !pre.Mode().IsRegular() {
		return admitted{}, fmt.Errorf("source %s is not a regular file", path)
	}
	f, err := p.openInput(path)
	if err != nil {
		return admitted{}, fmt.Errorf("source: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return admitted{}, fmt.Errorf("source: %w", err)
	}
	if a, b := idOf(pre), idOf(info); a.dev != b.dev || a.ino != b.ino || !info.Mode().IsRegular() {
		return admitted{}, fmt.Errorf("source %s changed while it was opened", path)
	}
	if info.Size() > p.caps.inputBytes {
		return admitted{}, tooLarge("file is %d bytes, the limit is %d", info.Size(), p.caps.inputBytes)
	}
	typ := a.typ
	if typ == "" {
		if typ, err = sniffFile(input{f: f}); err != nil && !errors.Is(err, errUnnamed) {
			return admitted{}, err
		}
	}
	if typ == "" && a.kind&kindStill == 0 {
		return admitted{}, errUnnamed
	}
	if typ != "" {
		if err := p.checkType(typ, a.kind); err != nil {
			return admitted{}, err
		}
		if typ == typeWebP && a.kind&kindAnimation == 0 && webpAnimated(input{f: f}) {
			return admitted{}, unrecognized("an animated WebP is read by the animation operations")
		}
	}
	dir, release, err := p.inputDir()
	if err != nil {
		return admitted{}, err
	}
	ref, err := inputRef(dir, path)
	if err != nil {
		release()
		return admitted{}, err
	}
	in = admitted{file: f, ref: ref, dir: dir, abs: path, id: idOf(info), release: release}
	if typ == "" {
		var coder string
		if typ, coder, in.scenes, err = p.detectType(ctx, m, f, ref, a.kind); err != nil {
			release()
			return admitted{}, err
		}
		in.typ, in.decoder = typ, typeTable[typ].decoderFor(coder)
		return in, nil
	}
	in.typ, in.decoder = typ, typeTable[typ].decoderFor("")
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

// inputDir makes the directory of one input under the Processor's temporary
// directory. release removes it.
func (p *Processor) inputDir() (dir string, release func(), err error) {
	dir, err = os.MkdirTemp(p.tmpDir(), "in-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// detectType reads the headers of every image of the file f holds, which ref
// names to the tool, under the Processor's policy. It returns the type the
// coder that read them maps to, which must be one the operation accepts, the
// coder, and the headers, which the operation then does not read again. A TIFF
// whose first image is a reduced-resolution subfile, as the preview of a camera
// raw file is, is refused: the file's own type is not TIFF.
func (p *Processor) detectType(ctx context.Context, m *meter, f *os.File, ref string, kind mediaKind) (typ, coder string, scenes []sceneHeader, err error) {
	scenes, err = identifyHeadersAs(ctx, p.env(m, f), ref+stillScenes, true, gateHeaderFormat)
	if err != nil {
		if msg := err.Error(); !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrTimeLimit) &&
			(strings.Contains(msg, "security policy") || strings.Contains(msg, "no decode delegate")) {
			return "", "", nil, unrecognized("content no supported coder reads: " + firstLine(msg))
		}
		return "", "", nil, err
	}
	if err := checkStillImages(scenes); err != nil {
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
