package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/davars/winnow-media/tool"
)

// stderrTail is how much of a failed tool's standard error an error carries.
const stderrTail = 4 << 10

// Processor runs the media operations. Every tool runs in the sandbox
// described at NewProcessor. It owns the tools it runs, the
// ImageMagick policy those runs obey and a private directory for both; it
// refuses to be created unless ImageMagick reports that policy as the only
// one in effect. A Processor is safe for concurrent use.
type Processor struct {
	dir   string
	tools map[string]string
	run   Runner
	// compose runs the frame compositor of a GIF or animated WebP; tests
	// replace it.
	compose func(ctx context.Context, req composeRequest, onFrame func(path string, profile int) error) (*Usage, error)
}

// NewProcessor resolves ImageMagick and FFmpeg, checks that the sandbox every
// tool run goes through is in force, installs the policy in a new directory
// under the system temporary directory and checks that ImageMagick applies
// exactly that policy. Call Close when done.
//
// The sandbox is a helper process (see Init) that limits the tool's
// resources and denies it socket creation, and kills whatever the tool leaves
// running. NewProcessor requires that Init was called, and refuses to return
// unless the helper reports the sandbox applied. It returns an error on
// platforms other than Linux on amd64 or arm64.
func NewProcessor(ctx context.Context) (*Processor, error) {
	if err := sandboxAvailable(); err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	if !initCalled.Load() {
		return nil, fmt.Errorf("processor: %w", errNoInit)
	}
	if err := probeSandbox(ctx); err != nil {
		return nil, fmt.Errorf("processor: %w", err)
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
	dir, err := os.MkdirTemp("", "media-processor-")
	if err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, fmt.Errorf("processor: %w", err)
	}
	p := &Processor{dir: dir, tools: tools}
	p.run = p.runTool
	p.compose = p.composeFrames
	rules := processorPolicy()
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

// environ is the whole environment of a tool run. ImageMagick also reads
// $XDG_CONFIG_HOME/ImageMagick/policy.xml and $HOME/.config/ImageMagick/policy.xml
// after the directory MAGICK_CONFIGURE_PATH names, and the last matching rule
// wins, so inheriting either variable would let the caller's files widen the
// policy.
func (p *Processor) environ() []string {
	return []string{
		"MAGICK_CONFIGURE_PATH=" + p.dir,
		"MAGICK_TEMPORARY_PATH=" + p.tmpDir(),
		"TMPDIR=" + p.tmpDir(),
		"LC_ALL=C",
	}
}

// tailBuffer keeps the last stderrTail bytes written to it.
type tailBuffer struct{ buf []byte }

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.buf = append(t.buf, b...)
	if len(t.buf) > stderrTail {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-stderrTail:]...)
	}
	return len(b), nil
}

// meter accumulates the usage of the runs of one operation.
type meter struct{ usage Usage }

// env returns an Env that runs through the Processor and adds each run to m.
func (p *Processor) env(m *meter) Env {
	return Env{
		Run: func(ctx context.Context, name string, args ...string) (Result, error) {
			res, err := p.run(ctx, name, args...)
			m.add(res.Usage)
			return res, err
		},
		compose: func(ctx context.Context, req composeRequest, onFrame func(path string, profile int) error) error {
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

// admitted is an input that passed the gate.
type admitted struct {
	path, dir string
	decoder   decoder
}

// admit checks everything about an operation's input and output directory
// that can be checked without running a tool. The input's type comes from its
// leading bytes; neither its name nor any claim of the caller is consulted.
func (p *Processor) admit(s spec, path, dir string) (admitted, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return admitted{}, err
	}
	dir, err = filepath.Abs(dir)
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
	if info.Size() > maxInputBytes {
		return admitted{}, tooLarge("file is %d bytes, the limit is %d", info.Size(), int64(maxInputBytes))
	}
	mediaType, err := sniffFile(path)
	if err != nil {
		return admitted{}, err
	}
	dec, err := s.decoderFor(mediaType)
	if err != nil {
		return admitted{}, err
	}
	if err := requireEmptyDir(dir); err != nil {
		return admitted{}, err
	}
	return admitted{path: path, dir: dir, decoder: dec}, nil
}

// Still decodes the first image of a HEIC or HEIF file and writes it to dir
// as still.<ext> in the encoding's format. The output is upright, converted
// to sRGB when the source carries an ICC profile, flattened onto white for
// JPEG and free of metadata.
//
// The type is taken from the file's bytes, not its name. dir must exist and
// be empty. Every argument is validated, and the input checked against the
// size caps, before any tool runs: a type the operation does not accept gives
// an error wrapping ErrUnsupportedType, and an input over a cap one wrapping
// ErrTooLarge. On failure Still removes what it wrote, leaving dir empty.
// Usage covers every tool run of the call, including those of a failed one.
func (p *Processor) Still(ctx context.Context, path string, enc Encoding, dir string) (Image, Usage, error) {
	ec, err := enc.resolve()
	if err != nil {
		return Image{}, Usage{}, fmt.Errorf("still: %w", err)
	}
	in, err := p.admit(stillSpec, path, dir)
	if err != nil {
		return Image{}, Usage{}, fmt.Errorf("still: %w", err)
	}
	var m meter
	env := p.env(&m)
	images, err := inDir(in.dir, func() ([]Image, error) {
		img, err := env.still(ctx, in.decoder, in.path, ec, in.dir)
		return []Image{img}, err
	})
	if err != nil {
		return Image{}, m.usage, fmt.Errorf("still: %w", err)
	}
	return images[0], m.usage, nil
}

// AnimationFrames decodes a GIF or animated WebP file, composites its frames,
// samples up to maxFrames of them evenly and writes each to dir as
// frame-NNN.<ext>. A sampled frame identical to the one before it after
// resizing is dropped and the files are numbered consecutively from 0;
// Image.Frame is the index of the source frame a file shows. Output is as
// for Still.
//
// maxFrames 0 means 16; otherwise it must be 1 to 64. Errors, Usage and the
// state of dir are as for Still; a file with more frames or pixels than the
// caps allow gives an error wrapping ErrTooLarge.
func (p *Processor) AnimationFrames(ctx context.Context, path string, enc Encoding, maxFrames int, dir string) ([]Image, Usage, error) {
	ec, err := enc.resolve()
	if err != nil {
		return nil, Usage{}, fmt.Errorf("animation frames: %w", err)
	}
	frames, err := resolveFrames(maxFrames)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("animation frames: %w", err)
	}
	in, err := p.admit(animationSpec, path, dir)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("animation frames: %w", err)
	}
	var m meter
	env := p.env(&m)
	images, err := inDir(in.dir, func() ([]Image, error) {
		return env.animationFrames(ctx, in.decoder, in.path, ec, frames, in.dir)
	})
	if err != nil {
		return nil, m.usage, fmt.Errorf("animation frames: %w", err)
	}
	return images, m.usage, nil
}

// VideoFrames decodes up to maxFrames still images from an MP4, QuickTime,
// WebM or Matroska file and writes them to dir as frame-NNN.<ext>. The
// samples are the centres of equal intervals of the duration of the video
// stream flagged default (else the first), ignoring attached pictures, each
// taken by its own seek, so the work does not grow with the length of the
// video. A container with more than 32 streams, a stream longer than six
// hours or one wider or taller than 16384 pixels gives an error wrapping
// ErrTooLarge before any frame is decoded. HDR (HLG or PQ) video is tone-mapped to SDR, output is
// upright whatever the stream's display rotation, and a sample identical to
// the one before it is dropped, with the files numbered consecutively from 0.
// Image.TimeMS is the seek target in milliseconds; Image.Frame is 0 because a
// seeked sample has no known frame index. Output is otherwise as for Still.
//
// maxFrames 0 means 16; otherwise it must be 1 to 64. Errors, Usage and the
// state of dir are as for Still.
func (p *Processor) VideoFrames(ctx context.Context, path string, enc Encoding, maxFrames int, dir string) ([]Image, Usage, error) {
	ec, err := enc.resolve()
	if err != nil {
		return nil, Usage{}, fmt.Errorf("video frames: %w", err)
	}
	frames, err := resolveFrames(maxFrames)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("video frames: %w", err)
	}
	in, err := p.admit(videoSpec, path, dir)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("video frames: %w", err)
	}
	var m meter
	env := p.env(&m)
	images, err := inDir(in.dir, func() ([]Image, error) {
		return env.videoFrames(ctx, in.decoder, in.path, ec, frames, in.dir)
	})
	if err != nil {
		return nil, m.usage, fmt.Errorf("video frames: %w", err)
	}
	return images, m.usage, nil
}
