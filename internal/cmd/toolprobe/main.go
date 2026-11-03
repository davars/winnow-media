// Command toolprobe prints where the tool package resolves magick. It exists
// so the flake's closure check can build a binary the way a consumer does.
package main

import (
	"fmt"
	"os"

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
}
