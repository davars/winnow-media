package media

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// canary makes a FIFO at path that nothing may open, and returns what reports
// whether something did. A reader that opens a FIFO blocks until a writer
// does, so a poller that tries to open its write end, without blocking, finds
// the reader and the test fails. It needs neither strace nor a platform
// feature.
func canary(t *testing.T, path string) (opened func() bool) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		hit  bool
		stop = make(chan struct{})
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err == nil {
				_ = syscall.Close(fd)
				mu.Lock()
				hit = true
				mu.Unlock()
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return hit
	}
}

// canaryDir makes a directory holding FIFO canaries named names, and returns
// its path and a check that fails the test if any was opened.
func canaryDir(t *testing.T, names ...string) (dir string, check func()) {
	t.Helper()
	dir = t.TempDir()
	hits := map[string]func() bool{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		hits[path] = canary(t, path)
	}
	return dir, func() {
		t.Helper()
		for path, opened := range hits {
			if opened() {
				t.Errorf("%s was opened", path)
			}
		}
	}
}

// The detector sees a reader, which is what makes its silence mean something.
func TestCanarySeesAReader(t *testing.T) {
	dir, check := canaryDir(t, "bait")
	hit := canary(t, filepath.Join(dir, "second"))
	go func() {
		if f, err := os.Open(filepath.Join(dir, "second")); err == nil {
			_ = f.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !hit() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !hit() {
		t.Error("a reader that opened the FIFO was not seen")
	}
	check()
}
