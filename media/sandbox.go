package media

import "os"

// helperEnv names the environment variable that marks a process as the
// sandbox helper. Only the helper's own environment carries it; the tool it
// runs gets exactly Processor.environ.
const helperEnv = "WINNOW_MEDIA_HELPER"

// The Processor starts this executable again as its sandbox helper and as the
// GIF compositor, each marked in its environment.
func init() {
	if os.Getenv(helperEnv) != "" {
		helperMain()
		os.Exit(1)
	}
	if os.Getenv(compositorEnv) != "" {
		compositorMain()
	}
}
