//go:build !(linux && (amd64 || arm64))

package media

import (
	"context"
	"errors"
)

func sandboxAvailable() error {
	return errors.New("the sandbox needs Linux on amd64 or arm64")
}

func helperMain() {}

func probeSandbox(context.Context) error { return sandboxAvailable() }

func (p *Processor) runTool(context.Context, string, ...string) (Result, error) {
	return Result{}, sandboxAvailable()
}

func (p *Processor) composeGIF(context.Context, composeRequest, func(string, bool) error) (*Usage, error) {
	return nil, sandboxAvailable()
}
