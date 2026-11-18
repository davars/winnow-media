package media

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

// toolMainHook runs a fake tool when the test binary is started as one; it is
// set where the sandbox is available.
var toolMainHook func(args []string)

func TestMain(m *testing.M) {
	if toolMainHook != nil && len(os.Args) > 2 && os.Args[1] == "--tool" {
		toolMainHook(os.Args[2:])
		os.Exit(0)
	}
	flag.Parse()
	// A test that starts a tool without skipShort fails under -short instead of silently running it.
	if testing.Short() {
		if err := os.Setenv("PATH", ""); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
