package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"syscall"
)

// inputFDPath names, inside a tool, descriptor 3: the first after the standard
// three, where os/exec puts the first of Cmd.ExtraFiles, and where every
// runner puts the input. On Linux, opening it reopens the file the descriptor
// holds, however the file is named or linked now, with an offset of its own.
const inputFDPath = "/dev/fd/3"

// input is where the Go side of the package reads an input from: the
// descriptor a Source holds, or, for the deprecated Env, a path.
type input struct {
	f    *os.File
	path string
}

// open returns a reader of the input from its first byte, and what releases
// it. A descriptor is read with ReadAt, so its offset is never used.
func (in input) open() (io.ReadSeeker, func(), error) {
	if in.f != nil {
		return io.NewSectionReader(in.f, 0, math.MaxInt64), func() {}, nil
	}
	if in.path == inputFDPath {
		return nil, nil, errors.New("the input descriptor is not attached")
	}
	f, err := os.Open(in.path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// openInputFile opens the file at path for reading without blocking, and
// without making a terminal the process's controlling one.
func openInputFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
}

// requireInput refuses arguments that name the input descriptor when the run
// has no input to hand the tool, which would otherwise read whatever
// descriptor 3 happens to be.
func requireInput(f *os.File, args []string) error {
	if f != nil {
		return nil
	}
	for _, a := range args {
		if strings.Contains(a, inputFDPath) {
			return fmt.Errorf("argument %q names the input descriptor but the run has no input", a)
		}
	}
	return nil
}

type inputKey struct{}

// withInput returns ctx marking the tool runs under it as reading f; runTool
// and composeFrames pass it to the process. It rides on the context because
// the Runner signature, which the deprecated Env also uses, has no room for a
// file.
func withInput(ctx context.Context, f *os.File) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, inputKey{}, f)
}

func inputOf(ctx context.Context) *os.File {
	f, _ := ctx.Value(inputKey{}).(*os.File)
	return f
}
