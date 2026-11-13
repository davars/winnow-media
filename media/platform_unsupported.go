//go:build !linux && !darwin

package media

// This package builds only on Linux and macOS. The undefined identifier below
// is the compile error that says so.
var _ = this_package_builds_only_on_linux_and_macos
