package media

import (
	"fmt"
	"slices"
)

// Media types the renderers read.
const (
	typePDF = "application/pdf"
)

// RenderTypes returns the media types a Processor can render when
// Config.RenderTypes names them, sorted. They are never among Types: Open
// does not read them, and they are read only by the renderers, under limits
// and confinement of their own. Later versions may add types.
func RenderTypes() []string { return []string{typePDF} }

// resolveRenderTypes returns the set requested as a sorted list without
// repeats; it is an error to name a type RenderTypes lacks. Nil and empty
// both render nothing.
func resolveRenderTypes(requested []string) ([]string, error) {
	out := slices.Clone(requested)
	slices.Sort(out)
	out = slices.Compact(out)
	known := RenderTypes()
	for _, typ := range out {
		if !slices.Contains(known, typ) {
			return nil, fmt.Errorf("render types: %q is not a type RenderTypes lists", typ)
		}
	}
	return out, nil
}

// Raster describes one image file a render wrote.
type Raster struct {
	// Name is the file's name within the output directory.
	Name string
	// Page is the index of the page the file shows; 0 for an SVG.
	Page int
	// Width and Height are the file's size in pixels.
	Width, Height int
}
