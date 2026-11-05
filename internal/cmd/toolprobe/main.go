// Command toolprobe prints where the tool package resolves magick and
// webp-frames. It exists so the flake's closure check can build a binary the
// way a consumer does.
package main

import (
	"fmt"
	"os"

	"github.com/davars/winnow-media/media"
	"github.com/davars/winnow-media/tool"
)

func main() {
	media.Init()
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
}
