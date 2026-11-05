package media

import (
	"errors"
	"os"
	"sync/atomic"
)

// helperEnv names the environment variable that marks a process as the
// sandbox helper. Only the helper's own environment carries it; the tool it
// runs gets exactly Processor.environ.
const helperEnv = "WINNOW_MEDIA_HELPER"

var initCalled atomic.Bool

// errNoInit is returned by NewProcessor when Init was never called.
var errNoInit = errors.New("media.Init was not called: call it first thing in main (and in TestMain), " +
	"because the Processor starts this executable as its sandbox helper")

// Init must be the first thing a program calls in main, and in TestMain, if
// it creates a Processor. The Processor runs every tool through a helper
// process that applies the sandbox, and the helper is this executable started
// again. When the process was started as that helper, Init runs it and never
// returns; otherwise it returns at once and changes nothing about the calling
// process.
//
// NewProcessor fails if Init has not been called in the process.
func Init() {
	initCalled.Store(true)
	if os.Getenv(helperEnv) != "" {
		helperMain()
		os.Exit(1)
	}
	if os.Getenv(compositorEnv) != "" {
		compositorMain()
	}
}
