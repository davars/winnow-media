package media

import (
	"os"
	"testing"
)

// toolMainHook runs a fake tool when the test binary is started as one; it is
// set where the sandbox is available.
var toolMainHook func(args []string)

func TestMain(m *testing.M) {
	Init()
	if toolMainHook != nil && len(os.Args) > 2 && os.Args[1] == "--tool" {
		toolMainHook(os.Args[2:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}
