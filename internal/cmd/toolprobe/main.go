// Command toolprobe prints "flake-revision <rev>" and then "<program> <path>"
// for every program package programs lists, the path being where tool.Resolve
// finds it, or empty where it does not. It exists so the
// flake's closure check can build a binary the way a consumer does and read
// what that binary would run.
package main

import (
	"fmt"

	"github.com/davars/winnow-media/internal/programs"
	_ "github.com/davars/winnow-media/media" // links the library, as a consumer does
	"github.com/davars/winnow-media/tool"
)

func main() {
	fmt.Println("flake-revision", tool.FlakeRevision())
	for _, name := range programs.Names() {
		path, _ := tool.Resolve(name)
		fmt.Println(name, path)
	}
}
