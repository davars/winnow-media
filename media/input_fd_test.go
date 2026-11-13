package media

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// inputKinds are the three kinds of Source, each with an input and a
// different file of the same kind to swap for it.
var inputKinds = []struct {
	name string
	orig func(t *testing.T) string
	alt  func(t *testing.T) string
	sel  Select
}{
	{
		name: "still", sel: Select{},
		orig: func(t *testing.T) string { return heicFixtureFile(t, "adobe-64x32") },
		alt:  func(t *testing.T) string { return heicFixtureFile(t, "adobe-256x96") },
	},
	{
		name: "animation", sel: Select{Frames: All()},
		orig: func(t *testing.T) string { return writeGIF(t, solid, 3) },
		alt:  func(t *testing.T) string { return writeGIF(t, solid, 5) },
	},
	{
		name: "video", sel: Select{Frames: Times(msec(100), msec(700))},
		orig: func(t *testing.T) string { return sdrClip(t, requireFFmpeg(t), "160x120", 10, 2) },
		alt:  func(t *testing.T) string { return sdrClip(t, requireFFmpeg(t), "320x240", 10, 2) },
	},
}

// uploaded copies the file at path into a directory of its own, under a name
// no tool argument may contain, and returns the copy and that name.
func uploaded(t *testing.T, path string) (copyPath, marker string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const marker_ = "uploaded-by-a-user"
	copyPath = filepath.Join(t.TempDir(), marker_)
	if err := os.WriteFile(copyPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return copyPath, marker_
}

// swapHook runs swap once, before the first tool or compositor run after it is
// armed: the moment between a call's checks and its tool's read.
type swapHook struct {
	swap  func()
	armed atomic.Bool
	fired atomic.Bool
	once  sync.Once
}

func (h *swapHook) trip() {
	if h.armed.Load() {
		h.once.Do(func() { h.swap(); h.fired.Store(true) })
	}
}

func (h *swapHook) install(p *Processor) {
	run, compose := p.run, p.compose
	p.run = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		h.trip()
		return run(ctx, in, name, args...)
	}
	p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
		h.trip()
		return compose(ctx, in, req, onFrame)
	}
}

// replacements returns the swaps that make path name the file alt.
func replacements(t *testing.T, path, alt string) map[string]func() {
	t.Helper()
	b, err := os.ReadFile(alt)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]func(){
		"rename over": func() {
			next := path + ".new"
			if err := os.WriteFile(next, b, 0o600); err != nil {
				t.Error(err)
				return
			}
			if err := os.Rename(next, path); err != nil {
				t.Error(err)
			}
		},
		"symlink": func() {
			if err := os.Remove(path); err != nil {
				t.Error(err)
				return
			}
			if err := os.Symlink(alt, path); err != nil {
				t.Error(err)
			}
		},
		// The next two leave the opened file itself untouched, so its
		// status-change time stays as recorded.
		"directory swap": func() {
			dir := filepath.Dir(path)
			if err := os.Rename(dir, dir+".old"); err != nil {
				t.Error(err)
				return
			}
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Error(err)
			}
		},
		"symlink retarget": func() {
			next := path + ".lnk"
			if err := os.Symlink(alt, next); err != nil {
				t.Error(err)
				return
			}
			if err := os.Rename(next, path); err != nil {
				t.Error(err)
			}
		},
	}
}

// keepsStatusChange names the swaps that leave the opened file's own
// status-change time alone, so that nothing but the descriptor can tell the
// original from the new file.
var keepsStatusChange = map[string]bool{"directory swap": true, "symlink retarget": true}

func encodeTo(t *testing.T, src *Source, sel Select) (map[string][]byte, error) {
	t.Helper()
	dir := t.TempDir()
	if _, _, err := src.Encode(context.Background(), dir, sel, Encoding{Format: PNG, MaxEdge: 32}); err != nil {
		return nil, err
	}
	return readDir(t, dir), nil
}

func sameFiles(a, b map[string][]byte) bool {
	return maps.EqualFunc(a, b, bytes.Equal)
}

// A file put at the path after the checks, whether by rename or by a link, is
// not what any tool reads: the Source decodes the file it opened, or refuses
// because it changed.
func TestReplacedInputIsNeverRead(t *testing.T) {
	t.Parallel()
	for _, k := range inputKinds {
		for _, unsandboxed := range []bool{false, true} {
			if unsandboxed && k.name != "still" {
				continue
			}
			for swapName := range replacements(t, "", "/dev/null") {
				for _, phase := range []string{"encode", "open"} {
					name := k.name + "/" + swapName + "/" + phase
					if unsandboxed {
						name += "/unsandboxed"
					}
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						p := requireProcessorWith(t, Config{Unsandboxed: unsandboxed})
						orig, alt := k.orig(t), k.alt(t)
						path, _ := uploaded(t, orig)
						if swapName == "symlink retarget" {
							real := path + ".real"
							if err := os.Rename(path, real); err != nil {
								t.Fatal(err)
							}
							if err := os.Symlink(real, path); err != nil {
								t.Fatal(err)
							}
						}
						baseline, err := encodeTo(t, openSourceOn(t, p, orig, OpenOptions{}), k.sel)
						if err != nil {
							t.Fatal(err)
						}
						altOut, err := encodeTo(t, openSourceOn(t, p, alt, OpenOptions{}), k.sel)
						if err != nil || sameFiles(baseline, altOut) {
							t.Fatalf("the swap is not distinguishable: %v", err)
						}
						hook := &swapHook{swap: replacements(t, path, alt)[swapName]}
						hook.install(p)
						if phase == "open" {
							hook.armed.Store(true)
						}
						src, _, err := p.Open(context.Background(), path, OpenOptions{})
						if err != nil {
							// A swap that changes the opened file's status-change
							// time makes Open itself refuse; the others leave
							// nothing to refuse, so Open must have read the
							// original.
							if phase == "encode" || keepsStatusChange[swapName] || !strings.Contains(err.Error(), "changed") {
								t.Fatal(err)
							}
							return
						}
						t.Cleanup(func() { _ = src.Close() })
						if phase == "encode" {
							hook.armed.Store(true)
						}
						got, err := encodeTo(t, src, k.sel)
						if !hook.fired.Load() {
							t.Fatal("no tool ran after the swap")
						}
						switch {
						case err != nil && !keepsStatusChange[swapName] && strings.Contains(err.Error(), "changed"):
						case err != nil:
							t.Fatal(err)
						case !sameFiles(got, baseline):
							t.Error("Encode did not return the original file's output")
						}
					})
				}
			}
		}
	}
}

// A file rewritten in place is the file the Source opened, changed: the next
// Encode refuses it.
func TestInputRewrittenInPlaceIsRefused(t *testing.T) {
	t.Parallel()
	for _, k := range inputKinds {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			path, _ := uploaded(t, k.orig(t))
			src := openSourceOn(t, p, path, OpenOptions{})
			alt, err := os.ReadFile(k.alt(t))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, alt, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := encodeTo(t, src, k.sel); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Errorf("err = %v, want a refusal that the file changed", err)
			}
		})
	}
}

// No tool is told the path the input was opened by, or any part of it.
func TestNoToolArgvNamesTheInputPath(t *testing.T) {
	t.Parallel()
	for _, k := range inputKinds {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			path, marker := uploaded(t, k.orig(t))
			rec := record(p)
			var mu sync.Mutex
			var composed [][]string
			compose := p.compose
			p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
				mu.Lock()
				composed = append(composed, req.args())
				mu.Unlock()
				return compose(ctx, in, req, onFrame)
			}
			src := openSourceOn(t, p, path, OpenOptions{})
			if _, err := encodeTo(t, src, k.sel); err != nil {
				t.Fatal(err)
			}
			calls := append(rec.calls, composed...)
			var viaFD bool
			for _, call := range calls {
				for _, arg := range call {
					if strings.Contains(arg, marker) {
						t.Errorf("a run names the input path: %q", call)
					}
					viaFD = viaFD || strings.Contains(arg, inputFDPath)
				}
			}
			if runtime.GOOS == "linux" && !viaFD {
				t.Errorf("no run reads %s: %q", inputFDPath, calls)
			}
		})
	}
}

// swapAfterOpen is an input opener that, once it has opened the file at path,
// puts the file at next in its place.
func swapAfterOpen(p *Processor, next string) {
	p.openInput = func(path string) (*os.File, error) {
		f, err := openInputFile(path)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(next, path); err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	}
}

func writePadding(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "padding")
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The size cap is judged on the file that was opened, not on what the path
// names by then.
func TestInputCapsUseTheOpenFile(t *testing.T) {
	t.Parallel()
	cfg := Config{Caps: Caps{MaxInputBytes: 1 << 20}}
	t.Run("small file replaced by a large one", func(t *testing.T) {
		t.Parallel()
		p := requireProcessorWith(t, cfg)
		path, _ := uploaded(t, heicFixtureFile(t, "adobe-64x32"))
		swapAfterOpen(p, writePadding(t, 2<<20))
		openSourceOn(t, p, path, OpenOptions{})
	})
	t.Run("large file replaced by a small one", func(t *testing.T) {
		t.Parallel()
		p := requireProcessorWith(t, cfg)
		path := writePadding(t, 2<<20)
		small, _ := uploaded(t, heicFixtureFile(t, "adobe-64x32"))
		swapAfterOpen(p, small)
		if _, _, err := p.Open(context.Background(), path, OpenOptions{}); !errors.Is(err, ErrTooLarge) {
			t.Errorf("err = %v, want ErrTooLarge", err)
		}
	})
}

// The type gate reads the file that was opened.
func TestInputTypeIsSniffedFromTheOpenFile(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	path, _ := uploaded(t, writeGIF(t, solid, 3))
	other, _ := uploaded(t, heicFixtureFile(t, "adobe-64x32"))
	swapAfterOpen(p, other)
	if got := openSourceOn(t, p, path, OpenOptions{}).Info().Kind; got != Animation {
		t.Errorf("Kind = %s, want %s", got, Animation)
	}
}

// Close, a refused Open and the Processor's Close each release the descriptor.
func TestInputDescriptorIsReleased(t *testing.T) {
	t.Parallel()
	opened := func(p *Processor) *[]*os.File {
		var mu sync.Mutex
		var files []*os.File
		p.openInput = func(path string) (*os.File, error) {
			f, err := openInputFile(path)
			mu.Lock()
			files = append(files, f)
			mu.Unlock()
			return f, err
		}
		return &files
	}
	closed := func(t *testing.T, f *os.File) bool {
		t.Helper()
		_, err := f.Stat()
		return errors.Is(err, os.ErrClosed)
	}
	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		p := requireProcessor(t)
		files := opened(p)
		path, _ := uploaded(t, heicFixtureFile(t, "adobe-64x32"))
		src := openSourceOn(t, p, path, OpenOptions{})
		f := (*files)[0]
		if closed(t, f) {
			t.Fatal("the descriptor is closed while the Source is open")
		}
		if err := src.Close(); err != nil {
			t.Fatal(err)
		}
		if !closed(t, f) {
			t.Error("Close left the descriptor open")
		}
		if err := src.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
	})
	t.Run("refused Open", func(t *testing.T) {
		t.Parallel()
		p := requireProcessor(t)
		files := opened(p)
		path := filepath.Join(t.TempDir(), "text")
		if err := os.WriteFile(path, []byte("not media"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Open(context.Background(), path, OpenOptions{}); err == nil {
			t.Fatal("a text file was opened")
		}
		if !closed(t, (*files)[0]) {
			t.Error("a refused Open left the descriptor open")
		}
	})
	t.Run("late refusal", func(t *testing.T) {
		t.Parallel()
		p := requireProcessor(t)
		files := opened(p)
		path := k0video(t)
		if _, _, err := p.Open(context.Background(), path, OpenOptions{Orientation: 6}); err == nil {
			t.Fatal("an orientation was accepted for a video")
		}
		if !closed(t, (*files)[0]) {
			t.Error("an Open refused after the checks left the descriptor open")
		}
	})
	t.Run("Processor Close", func(t *testing.T) {
		t.Parallel()
		p := requireProcessor(t)
		files := opened(p)
		path, _ := uploaded(t, heicFixtureFile(t, "adobe-64x32"))
		openSourceOn(t, p, path, OpenOptions{})
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if !closed(t, (*files)[0]) {
			t.Error("closing the Processor left the descriptor open")
		}
	})
}

// A path that is not a regular file is refused before anything is opened, and
// a path that names a different file by the time it is opened is refused.
func TestInputIsStatedBeforeItIsOpened(t *testing.T) {
	t.Parallel()
	count := func(p *Processor) *atomic.Int64 {
		var n atomic.Int64
		open := p.openInput
		p.openInput = func(path string) (*os.File, error) {
			n.Add(1)
			return open(path)
		}
		return &n
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"device": "/dev/null", "fifo": fifo, "directory": t.TempDir()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			opens := count(p)
			if _, _, err := p.Open(context.Background(), path, OpenOptions{}); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("err = %v, want ErrInvalidRequest for a file that is not regular", err)
			}
			if n := opens.Load(); n != 0 {
				t.Errorf("%d opens of a non-regular file", n)
			}
		})
	}
	t.Run("different file at open", func(t *testing.T) {
		t.Parallel()
		p := requireProcessor(t)
		path, _ := uploaded(t, heicFixtureFile(t, "adobe-64x32"))
		other, _ := uploaded(t, heicFixtureFile(t, "adobe-256x96"))
		p.openInput = func(path string) (*os.File, error) {
			if err := os.Rename(other, path); err != nil {
				return nil, err
			}
			return openInputFile(path)
		}
		if _, _, err := p.Open(context.Background(), path, OpenOptions{}); err == nil || !strings.Contains(err.Error(), "changed") || !errors.Is(err, ErrInputChanged) {
			t.Errorf("err = %v, want ErrInputChanged", err)
		}
	})
}

// A tool run with no input attached is refused when its arguments name the
// input descriptor, and so is a Go read of an input that is not attached.
func TestInputDescriptorNeedsAnInput(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	rec := record(p)
	if _, err := p.run(context.Background(), nil, "magick", "identify", inputFDPath); err == nil || !strings.Contains(err.Error(), "no input") {
		t.Errorf("run: err = %v, want a refusal", err)
	}
	if _, err := p.composeFrames(context.Background(), nil, composeRequest{path: inputFDPath, dir: t.TempDir()}, nil); err == nil || !strings.Contains(err.Error(), "no input") {
		t.Errorf("compose: err = %v, want a refusal", err)
	}
	if _, _, err := (input{}).open(); err == nil {
		t.Error("Go read an input that is not attached")
	}
	if rec.count() != 1 {
		t.Errorf("%d runs recorded, want the one refused run", rec.count())
	}
}

func k0video(t *testing.T) string { return inputKinds[2].orig(t) }
