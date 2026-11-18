// Package programs lists the programs package media can run. The flake's
// closure check reads it through internal/cmd/toolprobe.
package programs

import "slices"

var names = []string{"magick", "ffmpeg", "ffprobe", "webp-frames", "pdfinfo", "pdftoppm"}

// Names returns the programs package media can run, whichever its Config: a
// Processor runs pdfinfo and pdftoppm only when Config.RenderTypes names
// application/pdf, and a tool directory built without poppler lacks them. The
// result is a copy.
func Names() []string { return slices.Clone(names) }
