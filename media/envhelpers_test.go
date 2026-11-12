package media

import (
	"context"
)

// Helpers for the tests of the toolEnv layer.

// fullSoft reports whether the first scene of the file ref names is
// palette-based or declares alpha, by decoding it. It is for the coders in
// fullReadFormats, whose -ping read cannot say.
func fullSoft(ctx context.Context, env toolEnv, ref string) (bool, error) {
	return fullSoftAt(ctx, env, ref, 0)
}
