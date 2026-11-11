//go:build !(linux && (amd64 || arm64))

package media

import (
	"context"
	"errors"
	"io"
)

func sandboxAvailable() error {
	return errors.New("the sandbox needs Linux on amd64 or arm64")
}

func helperMain() {}

func probeSandbox(context.Context, rlimits) error { return sandboxAvailable() }

func sandboxRunner(rlimits) runner {
	return func(context.Context, runSpec, io.Writer, io.Writer) (toolResult, error) {
		return toolResult{}, sandboxAvailable()
	}
}
