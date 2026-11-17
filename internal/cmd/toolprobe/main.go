// Command toolprobe prints where the tool package resolves magick, the flake
// revision, and webp-frames, pdftoppm and pdfinfo, one a line; a program the
// tool directory does not hold prints as an empty line. It exists so the
// flake's closure check can build a binary the way a consumer does.
package main

import (
	"fmt"
	"os"

	_ "github.com/davars/winnow-media/media" // links the library, as a consumer does
	"github.com/davars/winnow-media/tool"
)

func main() {
	p, err := tool.Resolve("magick")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(p)
	fmt.Println(tool.FlakeRevision())
	w, err := tool.Resolve("webp-frames")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(w)
	for _, name := range []string{"pdftoppm", "pdfinfo"} {
		if p, err := tool.Resolve(name); err != nil {
			fmt.Println()
		} else {
			fmt.Println(p)
		}
	}
}
