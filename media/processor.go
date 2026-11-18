package media

import (
	"cmp"
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
// failed tool's standard error a ToolError carries. A fatal runtime error names
// its cause first and buries it under a goroutine dump.
const (
	stderrHead = 512
	stderrTail = 4 << 10
)

// Processor opens input files as Sources and runs the tools that read them,
// in the sandbox unless Config.Unsandboxed is set. It is safe for concurrent
// use, and does not limit how many calls run at once. The package
// documentation states what it guarantees.
type Processor struct {
	dir  string
	res  Resources
	caps caps
	// types is the resolved Config.Types, sorted; accepts is it as a set.
	types   []string
	accepts map[string]bool
	// renders is the resolved Config.RenderTypes as a set.
	renders map[string]bool
	// confinement is how render runs are confined, as probed at start.
	confinement Confinement
	// detect is the -format_whitelist of the probe that names a video the
	// leading bytes do not: the demuxers of the detected types of types.
	// Empty when there are none, which turns the probe off.
	detect string
	tools  map[string]string
	// start runs one process: the sandbox helper, or the direct runner.
	start runner
	// run runs one tool, with in, when not nil, as its input descriptor; tests
	// wrap it.
	run toolRun
	// openInput opens an input's file; tests replace it.
	openInput func(path string) (*os.File, error)
	// compose runs the frame compositor of a GIF or animated WebP, with in as
	// its input descriptor; tests wrap it.
	compose func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error)

	// mu guards closed and sources, the Sources Open made that are not yet
	// closed. opening counts the Opens that began before Close; Add is only
	// called under mu while closed is false, which orders it before Close's
	// Wait.
	mu      sync.Mutex
	closed  bool
	opening sync.WaitGroup
	sources map[*Source]struct{}
	// docs are the Documents OpenDocument made that are not yet closed.
	docs map[*Document]struct{}
}

// NewProcessor returns a Processor for cfg. It needs the magick, ffmpeg,
// ffprobe and webp-frames programs, whatever cfg.Types is, found as package
// tool finds them, and, unless cfg.Unsandboxed is set, a host the sandbox
// supports (see the package documentation). With cfg.RenderTypes naming
// application/pdf it also needs pdfinfo and pdftoppm. It checks, within ctx,
// that the sandbox and the generated ImageMagick policy are in force, and
// returns an error rather than a Processor that would run without either.
//
// Under the sandbox, render runs are confined to their files with Landlock
// where the kernel has it. Where it has none, the Processor is returned and
// render runs go unconfined unless cfg.RequireConfinement is set, which makes
// it an error; where the kernel reports Landlock but the confinement is
// found not to hold, it is an error regardless. Processor.Confinement reports
// the outcome. Call Close when done.
//
// It is also an error if cfg.Resources has a negative field or a TimeLimit or
// CPUTime that is not a whole number of seconds of at least one, if cfg.Caps
// has a negative field or one above its ceiling, if cfg.Types is empty but
// not nil, names a type Types does not list or names only some of the HEIC,
// HEIF and AVIF types, or if cfg.Dir is not an existing directory.
// NewProcessor's errors wrap no sentinel.
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
	renderTypes, err := resolveRenderTypes(cfg.RenderTypes)
	if err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	start := runner(directRunner)
	rl := rlimitsFor(res)
	if !cfg.Unsandboxed {
		if err := sandboxAvailable(); err != nil {
			return nil, fmt.Errorf("processor: %w", err)
		}
		if err := probeSandbox(ctx, rl); err != nil {
			return nil, fmt.Errorf("processor: %w", err)
		}
		start = sandboxRunner(rl)
	}
	if wrap != nil {
		start = wrap(start)
	}
	tools := map[string]string{}
	names := []string{"magick", "ffmpeg", "ffprobe", webpFramesTool}
	renders := make(map[string]bool, len(renderTypes))
	for _, typ := range renderTypes {
		renders[typ] = true
	}
	if renders[typePDF] {
		names = append(names, "pdfinfo", "pdftoppm")
	}
	for _, name := range names {
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
	p := &Processor{openInput: openInputFile, dir: dir, res: res, caps: limits, types: types, accepts: accepts, renders: renders, detect: detectedDemuxers(types), tools: tools, start: start}
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
	switch {
	case len(renders) == 0:
		p.confinement.Reason = reasonNoRender
	case cfg.Unsandboxed:
		if cfg.RequireConfinement {
			_ = p.Close()
			return nil, errors.New("processor: RequireConfinement cannot be met with Unsandboxed")
		}
		p.confinement.Reason = "the sandbox is off"
	default:
		abi, err := probeConfinement(ctx, p.dir, rl)
		switch {
		case err == nil:
			p.confinement = Confinement{Confined: true, ABI: abi}
		case errors.Is(err, errConfinementUnavailable) && !cfg.RequireConfinement:
			p.confinement.Reason = err.Error()
		default:
			_ = p.Close()
			return nil, fmt.Errorf("processor: %w", err)
		}
	}
	if renders[typeSVG] {
		rules := svgPolicy(res)
		if err := p.installSVGPolicy(rules); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("processor: %w", err)
		}
		if err := p.verifySVGPolicy(ctx, rules); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("processor: %w", err)
		}
	}
	return p, nil
}

// Confinement reports how a Processor confines the files of render runs.
type Confinement struct {
	// Confined is true when every render run is confined to its files by
	// Landlock, as the startup probe found the kernel enforces it.
	Confined bool
	// ABI is the kernel's Landlock ABI version, at least 1, when Confined,
	// and 0 otherwise.
	ABI int
	// Reason, when Confined is false, says why: no render types, no sandbox,
	// or Landlock unavailable on the host. It is for logs, and its wording may
	// change.
	Reason string
}

// reasonNoRender is the Reason of a Processor with no render types, which has
// no render runs to confine.
const reasonNoRender = "no render types"

// String describes c for a log line.
func (c Confinement) String() string {
	if c.Reason == reasonNoRender {
		return "no render runs: Config.RenderTypes is empty"
	}
	if c.Confined {
		return fmt.Sprintf("render runs confined by Landlock ABI %d", c.ABI)
	}
	return "render runs not confined: " + c.Reason
}

// Confinement reports whether the Processor's render runs are confined to
// their files, and with which Landlock ABI. It is fixed when NewProcessor
// returns. Where Confined is false the other tools' runs are as they always
// are, and render runs have the rest of the sandbox.
func (p *Processor) Confinement() Confinement { return p.confinement }

func (p *Processor) install(rules []policyRule) error {
	if err := os.Mkdir(p.tmpDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.dir, "policy.xml"), policyXML(rules), 0o600)
}

func (p *Processor) tmpDir() string { return filepath.Join(p.dir, "tmp") }

// Close closes every Source and Document the Processor opened that is still
// open and removes the Processor's scratch. It waits for the Open calls in progress,
// each of which returns ErrClosed or a Source that Close then closes, and for
// the calls running on the Sources. It interrupts none of them: cancel their
// contexts first to end them sooner. Open after Close has begun returns
// ErrClosed. Close is safe to call more than once.
func (p *Processor) Close() error {
	p.mu.Lock()
	p.closed = true
	open := make([]*Source, 0, len(p.sources))
	for s := range p.sources {
		open = append(open, s)
	}
	docs := make([]*Document, 0, len(p.docs))
	for d := range p.docs {
		docs = append(docs, d)
	}
	p.mu.Unlock()
	p.opening.Wait()
	for _, s := range open {
		_ = s.Close()
	}
	for _, d := range docs {
		_ = d.Close()
	}
	return os.RemoveAll(p.dir)
}

// beginOpen records an Open in progress, which Close waits for; it reports
// false if the Processor is closed. The caller calls p.opening.Done when the
// Open returns.
func (p *Processor) beginOpen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.opening.Add(1)
	return true
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

// registerDocument records d as open, and reports false if the Processor is
// closed.
func (p *Processor) registerDocument(d *Document) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	if p.docs == nil {
		p.docs = map[*Document]struct{}{}
	}
	p.docs[d] = struct{}{}
	return true
}

func (p *Processor) forgetDocument(d *Document) {
	p.mu.Lock()
	delete(p.docs, d)
	p.mu.Unlock()
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
func (p *Processor) environ(tmp string) []string { return p.environFor(tmp, p.dir) }

// environFor is environ with ImageMagick reading its configuration from
// policyDir.
func (p *Processor) environFor(tmp, policyDir string) []string {
	return []string{
		"MAGICK_CONFIGURE_PATH=" + policyDir,
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

// meter accumulates the usage of the runs of one call.
type meter struct{ usage Usage }

// env returns a toolEnv that runs through the Processor and adds each run to
// m. Each run it starts inherits in, when not nil, as descriptor 3.
func (p *Processor) env(m *meter, in *os.File) toolEnv {
	return toolEnv{
		resources: p.res,
		caps:      p.caps,
		run: func(ctx context.Context, name string, args ...string) (runResult, error) {
			res, err := p.run(ctx, in, name, args...)
			m.add(res.Usage)
			return res, err
		},
		compose: func(ctx context.Context, req composeRequest, onFrame func(composedFrame) error) error {
			usage, err := p.compose(ctx, in, req, onFrame)
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
	scenes []sceneHeader
	// probe is the probe that named a video the bytes did not; nil otherwise.
	probe   *videoInfo
	release func()
}

// admitInput opens the input once and checks everything about it that can be
// checked before pixels are read, through that descriptor alone, so that
// replacing the file at path afterwards changes nothing that was checked or
// that is read. The input must be a regular file within Caps.MaxInputBytes;
// its type must be one of Config.Types. A GIF, WebP,
// ISO base media or EBML file is typed by its leading bytes before any tool
// runs. Other content is named by one header read under the generated policy,
// which allows only the coders of Config.Types, so the content can select no
// other. A file whose bytes open a known video container, or an ISO base media
// file of no still image's brand, is first probed by ffprobe (detectVideo),
// restricted to the demuxers of the detected types of Config.Types, and the
// header read is the fallback if the probe refuses it. A type the caller names
// is used as it is, with no sniff, probe or header read to name one.
func (p *Processor) admitInput(ctx context.Context, m *meter, named, path string) (in admitted, err error) {
	f, path, info, err := p.openRegular(path)
	if err != nil {
		return admitted{}, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	typ := named
	var lead leadBytes
	if typ == "" {
		head, err := readHead(input{f: f})
		if err != nil {
			return admitted{}, err
		}
		if typ, err = sniff(head); err != nil {
			lead.err = err
			lead.looksVideo = videoContainerOf(head) && !leadsStill(head)
			if !errors.Is(err, errUnnamed) && !errors.Is(err, errOtherBrand) {
				return admitted{}, err
			}
		}
	}
	if typ != "" {
		if err := p.checkType(typ); err != nil {
			return admitted{}, err
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
		if err := p.admitUnnamed(ctx, m, f, ref, lead, &in); err != nil {
			release()
			return admitted{}, err
		}
		return in, nil
	}
	in.typ, in.decoder = typ, typeTable[typ].decoderFor("")
	return in, nil
}

// openRegular opens the file at path, which may be relative, for reading. The
// file must be a regular file within Config.Caps.MaxInputBytes. It returns the
// descriptor, which the caller closes, the absolute path and the descriptor's
// file info, from which idOf gives the identity to check later. The mode is
// judged before the open, which can have effects on a device; the descriptor
// must then be the file that was judged.
func (p *Processor) openRegular(path string) (f *os.File, abs string, info os.FileInfo, err error) {
	abs, err = filepath.Abs(path)
	if err != nil {
		return nil, "", nil, err
	}
	pre, err := os.Stat(abs)
	if err != nil {
		return nil, "", nil, fmt.Errorf("source: %w", withoutPath(err))
	}
	if !pre.Mode().IsRegular() {
		return nil, "", nil, invalid("source is not a regular file")
	}
	f, err = p.openInput(abs)
	if err != nil {
		return nil, "", nil, fmt.Errorf("source: %w", withoutPath(err))
	}
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	info, err = f.Stat()
	if err != nil {
		return nil, "", nil, fmt.Errorf("source: %w", withoutPath(err))
	}
	if a, b := idOf(pre), idOf(info); a.dev != b.dev || a.ino != b.ino || !info.Mode().IsRegular() || unlinked(info) {
		return nil, "", nil, fmt.Errorf("source: %w while it was opened", ErrInputChanged)
	}
	if info.Size() > p.caps.inputBytes {
		return nil, "", nil, tooLarge("file is %d bytes, the limit is %d", info.Size(), p.caps.inputBytes)
	}
	return f, abs, info, nil
}

// leadBytes is what sniff made of the leading bytes of an input it named no
// type for.
type leadBytes struct {
	// err is sniff's error: errUnnamed, or errOtherBrand for an ISO base media
	// file of a brand that is not a still image's.
	err error
	// looksVideo is set when the bytes open a container videoContainerOf knows.
	looksVideo bool
}

// admitUnnamed types an input whose leading bytes name no type, filling in.
// An ISO base media file of an unknown brand, and a file whose bytes open a
// known video container, are probed as a video; any other file is never shown
// to ffprobe, so content that is not video, such as an image ImageMagick
// reads or refuses, costs no probe. A file the probe refuses is read as an
// image next, except an unknown ISO base media brand, which no coder reads.
func (p *Processor) admitUnnamed(ctx context.Context, m *meter, f *os.File, ref string, lead leadBytes, in *admitted) error {
	env := p.env(m, f).pinnedTo(f, in.id, "while it was opened")
	otherBrand := errors.Is(lead.err, errOtherBrand)
	var probeErr error
	if p.detect != "" && (lead.looksVideo || otherBrand) {
		probeErr = p.detectVideo(ctx, env, ref, !otherBrand, in)
		if probeErr == nil || !errors.Is(probeErr, ErrUnsupportedType) {
			return probeErr
		}
	}
	if otherBrand {
		return cmp.Or(probeErr, lead.err)
	}
	typ, coder, scenes, err := p.detectType(ctx, env, ref)
	if err != nil {
		if probeErr != nil {
			return fmt.Errorf("%w; and as a video: %v", err, probeErr)
		}
		return err
	}
	in.typ, in.decoder, in.scenes = typ, typeTable[typ].decoderFor(coder), scenes
	return nil
}

// probeRefused reports whether err says that ffprobe ran to completion and
// found no video of the whitelisted containers: it exited with the demuxer's
// refusal, or its output had no usable video stream. A probe that was killed,
// could not start or ran out of resources says nothing about the file, so its
// error is not a refusal; nor is a file over a cap (ErrTooLarge).
func probeRefused(err error) bool {
	if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrTimeLimit) {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "no video stream") || strings.Contains(msg, "video stream reports size") {
		return true
	}
	var te *ToolError
	if !errors.As(err, &te) || te.ExitCode <= 0 {
		return false
	}
	stderr := string(te.Stderr)
	return strings.Contains(stderr, "Invalid data found") || strings.Contains(stderr, "not on whitelist")
}

// detectVideo asks ffprobe, in the sandbox and through the input's
// descriptor, whether the file is a video of a detected type of
// Config.Types, and on success fills in with its type, decoder and probe. The
// probe may pick no demuxer outside p.detect, so what it names is one of the
// configured types; no frame is decoded. preferMOV is set for a file with no
// ftyp box, which the mov demuxer reads as QuickTime.
func (p *Processor) detectVideo(ctx context.Context, env toolEnv, ref string, preferMOV bool, in *admitted) error {
	info, err := probeDetected(ctx, env, p.detect, ref)
	if err != nil {
		if !probeRefused(err) {
			return err
		}
		return unrecognized("not a video of a configured container")
	}
	typ, demuxer, ok := p.detectedType(info.format, preferMOV)
	if !ok {
		return unrecognized(fmt.Sprintf("video container %q", info.format))
	}
	in.typ, in.decoder, in.probe = typ, decoder{"ffmpeg", demuxer}, &info
	return nil
}

// checkType refuses a type the Processor was not configured with, with
// ErrUnsupportedType.
func (p *Processor) checkType(typ string) error {
	if !p.accepts[typ] {
		return unrecognized(typ)
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
// coder that read them maps to, which must be one of Config.Types, the coder,
// and the headers, which Source then does not read again. A TIFF
// whose first image is a reduced-resolution subfile, as the preview of a camera
// raw file is, is refused: the file's own type is not TIFF.
func (p *Processor) detectType(ctx context.Context, env toolEnv, ref string) (typ, coder string, scenes []sceneHeader, err error) {
	scenes, err = identifyHeadersAs(ctx, env, ref+stillScenes, true, gateHeaderFormat)
	if err != nil {
		if stderr := string(Stderr(err)); !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrTimeLimit) &&
			(strings.Contains(stderr, "security policy") || strings.Contains(stderr, "no decode delegate")) {
			return "", "", nil, unrecognized("content no supported coder reads")
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
	if err := p.checkType(typ); err != nil {
		return "", "", nil, err
	}
	if scenes[0].subfile == "REDUCEDIMAGE" {
		return "", "", nil, unrecognized("the file's first image is a reduced-resolution preview, as in a camera raw file; name its type")
	}
	return typ, coder, scenes, nil
}
