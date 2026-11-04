package media

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var regenerateHEIC = flag.Bool("regenerate-heic", false, "rewrite the HEIC fixtures in testdata/heic")

// HEIC fixtures are committed rather than written per test: nixpkgs' x265 on
// aarch64-linux cannot encode them, and the decoder those platforms run is
// exactly what the tests must exercise.
var heicFixtures = map[string]func(t *testing.T, env Env, path string){
	// A 640x480 gradient tagged RightTop, which reads back as 480x640.
	"gradient-righttop": func(t *testing.T, env Env, path string) {
		magick(t, env, "-size", "640x480", "gradient:red-blue", "-orient", "RightTop", path)
	},
	"adobe-256x96": func(t *testing.T, env Env, path string) { writeAdobeHEIC(t, env, "256x96", path) },
	"adobe-64x64":  func(t *testing.T, env Env, path string) { writeAdobeHEIC(t, env, "64x64", path) },
	"adobe-64x32":  func(t *testing.T, env Env, path string) { writeAdobeHEIC(t, env, "64x32", path) },
}

// writeAdobeHEIC writes a gradient of the given size converted to Adobe RGB
// and carrying that profile.
func writeAdobeHEIC(t *testing.T, env Env, size, path string) {
	t.Helper()
	dir := t.TempDir()
	srgb, adobe := writeSRGBProfile(t), writeAdobeProfile(t, dir)
	png := filepath.Join(dir, "w.png")
	magick(t, env, "-size", size, "gradient:#0020ff-#ffb000", "-profile", srgb, "-profile", adobe, png)
	magick(t, env, png, "-quality", "100", path)
}

// TestRegenerateHEICFixtures rewrites testdata/heic when run with
// -regenerate-heic, on a platform whose HEIC encoder works (x86_64-linux).
func TestRegenerateHEICFixtures(t *testing.T) {
	if !*regenerateHEIC {
		t.Skip("run with -regenerate-heic to rewrite the fixtures")
	}
	env := requireMagick(t)
	if err := os.MkdirAll(filepath.Join("testdata", "heic"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, write := range heicFixtures {
		write(t, env, filepath.Join("testdata", "heic", name+".heic"))
	}
}

// heicFixture copies the named committed fixture and stages the copy.
func heicFixture(t *testing.T, name string) string {
	t.Helper()
	if _, ok := heicFixtures[name]; !ok {
		t.Fatalf("no HEIC fixture %q", name)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "heic", name+".heic"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name+".heic")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return stage(t, path)
}

// heicFixtureFile is heicFixture without staging, for tests that also read
// the file with magick under its extension.
func heicFixtureFile(t *testing.T, name string) string {
	t.Helper()
	staged := heicFixture(t, name)
	path := filepath.Join(t.TempDir(), name+".heic")
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
	return path
}
