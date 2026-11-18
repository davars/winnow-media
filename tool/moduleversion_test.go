package tool

import (
	"runtime/debug"
	"testing"
)

func TestModuleVersion(t *testing.T) {
	dep := func(version string, replace *debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main: debug.Module{Path: "example.com/app", Version: "(devel)"},
			Deps: []*debug.Module{
				{Path: "example.com/other", Version: "v9.9.9"},
				{Path: modulePath, Version: version, Replace: replace},
			},
		}
	}
	for _, c := range []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"no build information", nil, ""},
		{"tag", dep("v1.1.0", nil), "v1.1.0"},
		{"pseudo-version", dep("v1.1.1-0.20261118120000-0123456789ab", nil), "v1.1.1-0.20261118120000-0123456789ab"},
		{"replaced by another version", dep("v1.1.0", &debug.Module{Path: modulePath, Version: "v1.2.0"}), "v1.2.0"},
		{"replaced by a directory, as go version -m shows it", dep("v0.0.0", &debug.Module{Path: "../winnow-media", Version: "(devel)"}), "(devel)"},
		{"the main module", &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, "(devel)"},
		{"the main module, tagged", &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v1.1.0"}}, "v1.1.0"},
		{"not a dependency", &debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}}, ""},
	} {
		if got := moduleVersion(c.info); got != c.want {
			t.Errorf("%s: moduleVersion = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestModuleVersionOfTheRunningBinary(t *testing.T) {
	// The package's own tests build the module itself.
	if got := ModuleVersion(); got != "(devel)" {
		t.Errorf("ModuleVersion() = %q, want (devel)", got)
	}
}

func TestDir(t *testing.T) {
	setVars(t, "/nix/store/x-winnow-media-tools", "")
	if got := Dir(); got != "/nix/store/x-winnow-media-tools" {
		t.Errorf("Dir() = %q", got)
	}
	setVars(t, "", "")
	if got := Dir(); got != "" {
		t.Errorf("Dir() in a PATH build = %q, want empty", got)
	}
}
