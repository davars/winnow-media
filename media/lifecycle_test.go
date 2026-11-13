package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cancelOnRun makes the next tool run or compositor run of p cancel the
// context it was given before it starts, so the call is cancelled with its
// tools running.
type cancelOnRun struct {
	armed  atomic.Bool
	cancel context.CancelFunc
}

func (c *cancelOnRun) install(p *Processor) {
	run, compose := p.run, p.compose
	p.run = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		if c.armed.CompareAndSwap(true, false) {
			c.cancel()
		}
		return run(ctx, in, name, args...)
	}
	p.compose = func(ctx context.Context, in *os.File, req composeRequest, onFrame func(composedFrame) error) (*Usage, error) {
		if c.armed.CompareAndSwap(true, false) {
			c.cancel()
		}
		return compose(ctx, in, req, onFrame)
	}
}

// A call that is cancelled while it runs leaves the Source usable and the
// output directory empty, for each kind.
func TestEncodeAfterACancelledEncodeSucceeds(t *testing.T) {
	t.Parallel()
	env := requireFFmpeg(t)
	for name, c := range map[string]struct {
		path string
		opts OpenOptions
		sel  Select
	}{
		"still":             {heicFixtureFile(t, "adobe-64x32"), OpenOptions{}, Select{}},
		"still keeping":     {heicFixtureFile(t, "adobe-64x32"), OpenOptions{KeepDecode: true}, Select{}},
		"animation":         {writeGIF(t, solid, 6), OpenOptions{}, Select{Frames: Even(3)}},
		"animation indices": {writeGIF(t, solid, 6), OpenOptions{}, Select{Frames: Indices(1, 4)}},
		"video":             {sdrClip(t, env, "160x120", 10, 2), OpenOptions{}, Select{Frames: Even(2)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessor(t)
			var cr cancelOnRun
			cr.install(p)
			src := openSourceOn(t, p, c.path, c.opts)
			enc := Encoding{Format: PNG, MaxEdge: 32}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cr.cancel = cancel
			cr.armed.Store(true)
			dir := t.TempDir()
			if _, _, err := src.Encode(ctx, dir, c.sel, enc); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Encode: err = %v, want context.Canceled", err)
			}
			if got := dirNames(t, dir); len(got) != 0 {
				t.Errorf("the cancelled call left %v", got)
			}
			images, _, err := src.Encode(context.Background(), t.TempDir(), c.sel, enc)
			if err != nil || len(images) == 0 {
				t.Errorf("Encode after the cancelled one: %d images, err = %v", len(images), err)
			}
		})
	}
}

// blockOnce makes the first tool run of p wait until release is closed,
// after saying it started.
func blockOnce(p *Processor) (started <-chan struct{}, release func()) {
	st, rel := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var closeRel sync.Once
	run := p.run
	p.run = func(ctx context.Context, in *os.File, name string, args ...string) (runResult, error) {
		once.Do(func() {
			close(st)
			<-rel
		})
		return run(ctx, in, name, args...)
	}
	return st, func() { closeRel.Do(func() { close(rel) }) }
}

func TestQueuedEncodeReturnsWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	rec := record(p)
	started, release := blockOnce(p)
	defer release()
	enc := Encoding{Format: PNG, MaxEdge: 32}
	first := make(chan error, 1)
	go func() {
		_, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, enc)
		first <- err
	}()
	<-started
	runs := rec.count()

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	dir := t.TempDir()
	go func() {
		_, usage, err := src.Encode(ctx, dir, Select{}, enc)
		if usage != (Usage{}) {
			err = errors.Join(err, errors.New("the queued call ran tools"))
		}
		second <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued Encode: err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued Encode did not return when its context was cancelled")
	}
	if got := dirNames(t, dir); len(got) != 0 {
		t.Errorf("the queued call left %v", got)
	}
	if rec.count() != runs {
		t.Error("the queued call ran a tool")
	}
	// Info and KeptBytes do not wait for the running call.
	done := make(chan struct{})
	go func() { _ = src.Info(); _ = src.KeptBytes(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Info or KeptBytes waited for a running Encode")
	}
	release()
	if err := <-first; err != nil {
		t.Errorf("the running Encode: %v", err)
	}
}

func TestOpenAfterCloseIsErrClosed(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	src, _, err := p.Open(context.Background(), heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	if src != nil || !errors.Is(err, ErrClosed) {
		t.Errorf("Open after Close: src = %v, err = %v, want ErrClosed", src, err)
	}
}

// A closed Source reports ErrClosed whatever the output directory holds.
func TestEncodeAfterCloseIsErrClosedForAnyOutputDirectory(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src, _, err := p.Open(context.Background(), heicFixtureFile(t, "adobe-64x32"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "existing"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"empty":     t.TempDir(),
		"non-empty": nonEmpty,
		"missing":   filepath.Join(t.TempDir(), "absent"),
	} {
		if _, _, err := src.Encode(context.Background(), dir, Select{}, Encoding{Format: PNG}); !errors.Is(err, ErrClosed) {
			t.Errorf("Encode after Close into a %s directory: err = %v, want ErrClosed", name, err)
		}
	}
}

// Close waits for an Open already running, which then fails with ErrClosed
// instead of leaving a Source over a removed directory.
func TestCloseWaitsForAnOpenInProgress(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	started, release := blockOnce(p)
	defer release()
	opened := make(chan error, 1)
	go func() {
		_, _, err := p.Open(context.Background(), writeGIF(t, solid, 3), OpenOptions{Type: typeGIF})
		opened <- err
	}()
	select {
	case <-started:
	case err := <-opened:
		t.Fatalf("Open ran no tool (err = %v)", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Open did not start a tool")
	}
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while an Open was running")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-opened; err != nil && !errors.Is(err, ErrClosed) {
		t.Errorf("Open: err = %v, want nil or ErrClosed", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the Open finished")
	}
}

// A call refused for its region changes nothing the Source keeps.
func TestRefusedEncodeKeepsWhatTheSourceKept(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	src := openSourceOn(t, p, writeFile(t, "pages.tif", tiffPages(0, 0, 0)), OpenOptions{KeepDecode: true})
	enc := Encoding{Format: PNG, MaxEdge: 16}
	encodeAll(t, src, Select{Frames: Indices(0, 1)}, enc)
	kept := func() []int {
		var ks []int
		for k := range src.kept {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		return ks
	}
	before, bytes := kept(), src.KeptBytes()
	_, _, err := src.Encode(context.Background(), t.TempDir(), Select{Frames: Indices(2), Region: Region{Width: 100, Height: 100}}, enc)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := kept(); !slices.Equal(got, before) || src.KeptBytes() != bytes {
		t.Errorf("kept %v (%d bytes) after a refused call, was %v (%d bytes)", got, src.KeptBytes(), before, bytes)
	}
}
