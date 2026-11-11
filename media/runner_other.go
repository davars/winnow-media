//go:build !unix

package media

import (
	"context"
	"errors"
	"io"
)

func directRunner(context.Context, runSpec, io.Writer, io.Writer) (toolResult, error) {
	return toolResult{}, errors.New("running tools directly needs a Unix host")
}
