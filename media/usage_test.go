package media

import (
	"context"
	"testing"
)

func TestUsageOfNilState(t *testing.T) {
	t.Parallel()
	if got := UsageOf(nil); got != nil {
		t.Fatalf("UsageOf(nil) = %+v, want nil", got)
	}
}

func TestRunnerReportsUsage(t *testing.T) {
	t.Parallel()
	env := requireMagick(t)
	res, err := env.Run(context.Background(), "magick", "-version")
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage == nil {
		t.Fatal("Usage is nil for a measured run")
	}
	if res.Usage.CPU < 0 {
		t.Errorf("CPU = %v, want >= 0", res.Usage.CPU)
	}
	// Any process image is over a mebibyte; a count left in kilobytes is not.
	if res.Usage.MaxRSS < 1<<20 {
		t.Errorf("MaxRSS = %d bytes, want at least 1 MiB", res.Usage.MaxRSS)
	}
}
