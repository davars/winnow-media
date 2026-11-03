package tool

import (
	"os"
	"path/filepath"
	"testing"
)

func setVars(t *testing.T, dir, rev string) {
	t.Helper()
	oldDir, oldRev := toolDir, flakeRev
	toolDir, flakeRev = dir, rev
	t.Cleanup(func() { toolDir, flakeRev = oldDir, oldRev })
}

func writeFile(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveUsesInjectedDirectory(t *testing.T) {
	dir := t.TempDir()
	want := writeFile(t, dir, "magick", 0o755)
	setVars(t, dir, "")
	got, err := Resolve("magick")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, t.TempDir(), "real", 0o755)
	if err := os.Symlink(target, filepath.Join(dir, "magick")); err != nil {
		t.Fatal(err)
	}
	setVars(t, dir, "")
	if _, err := Resolve("magick"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveInjectedDirectoryNeverFallsBackToPath(t *testing.T) {
	pathDir := t.TempDir()
	writeFile(t, pathDir, "magick", 0o755)
	t.Setenv("PATH", pathDir)
	setVars(t, t.TempDir(), "")
	if got, err := Resolve("magick"); err == nil {
		t.Fatalf("Resolve = %q, want error for tool missing from injected dir", got)
	}
}

func TestResolveRejectsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "magick", 0o644)
	setVars(t, dir, "")
	if got, err := Resolve("magick"); err == nil {
		t.Fatalf("Resolve = %q, want error for non-executable file", got)
	}
}

func TestResolveRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "magick"), 0o755); err != nil {
		t.Fatal(err)
	}
	setVars(t, dir, "")
	if got, err := Resolve("magick"); err == nil {
		t.Fatalf("Resolve = %q, want error for directory", got)
	}
}

func TestResolveFallsBackToPathWhenNotInjected(t *testing.T) {
	pathDir := t.TempDir()
	want := writeFile(t, pathDir, "magick", 0o755)
	t.Setenv("PATH", pathDir)
	setVars(t, "", "")
	got, err := Resolve("magick")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveRejectsPathLikeNames(t *testing.T) {
	setVars(t, t.TempDir(), "")
	for _, name := range []string{"", "../magick", "a/b", "."} {
		if got, err := Resolve(name); err == nil {
			t.Errorf("Resolve(%q) = %q, want error", name, got)
		}
	}
}

func TestFlakeRevision(t *testing.T) {
	setVars(t, "", "abc123")
	if got := FlakeRevision(); got != "abc123" {
		t.Fatalf("FlakeRevision = %q", got)
	}
}
