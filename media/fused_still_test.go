package media

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fusedFixture writes a still of one format that carries an Adobe RGB profile
// where the format can, and returns the ImageMagick coder that reads it.
type fusedFixture struct {
	name  string
	coder string
	write func(t *testing.T, env Env, dir string) string
}

// gradientArgs is a 64x48 gradient in Adobe RGB, with an alpha ramp when
// alpha is set, as the arguments that produce it at depth bits.
func gradientArgs(t *testing.T, dir string, depth int, alpha bool) []string {
	t.Helper()
	srgb, adobe := writeSRGBProfile(t), writeAdobeProfile(t, dir)
	args := []string{"-size", "64x48", "gradient:#0020ff-#ffb000"}
	if alpha {
		args = append(args, "(", "-size", "64x48", "gradient:white-black", ")",
			"-alpha", "off", "-compose", "copy_opacity", "-composite")
	}
	return append(args, "-depth", "16", "-profile", srgb, "-profile", adobe, "-depth", strconv.Itoa(depth))
}

func fusedFixtures() []fusedFixture {
	plain := func(ext string, depth int, alpha bool, extra ...string) func(t *testing.T, env Env, dir string) string {
		return func(t *testing.T, env Env, dir string) string {
			path := filepath.Join(dir, "fixture"+ext)
			magick(t, env, append(append(gradientArgs(t, dir, depth, alpha), extra...), path)...)
			return path
		}
	}
	return []fusedFixture{
		{"jpeg profiled oriented", "jpeg", func(t *testing.T, env Env, dir string) string {
			requireExiftool(t)
			path := plain(".jpg", 8, false, "-quality", "95")(t, env, dir)
			exiftool(t, env, "-overwrite_original", "-Orientation#=6", path)
			return path
		}},
		{"jpeg profiled", "jpeg", plain(".jpg", 8, false, "-quality", "95")},
		{"png8 alpha", "png", plain(".png", 8, true)},
		{"png16 alpha profiled", "png", plain(".png", 16, true)},
		{"tiff16 oriented page 0 of 2", "tiff", func(t *testing.T, env Env, dir string) string {
			path := filepath.Join(dir, "fixture.tif")
			args := gradientArgs(t, dir, 16, false)
			args = append(args, "(", "+clone", "-negate", ")", "-orient", "LeftBottom", path)
			magick(t, env, args...)
			return path
		}},
		{"webp lossless", "webp", plain(".webp", 8, true, "-define", "webp:lossless=true")},
		{"jxl", "jxl", plain(".jxl", 16, true)},
		{"gif frame 0 of 2", "gif", func(t *testing.T, env Env, dir string) string {
			path := filepath.Join(dir, "fixture.gif")
			args := gradientArgs(t, dir, 8, false)
			magick(t, env, append(args, "(", "+clone", "-negate", ")", "-loop", "0", path)...)
			return path
		}},
		{"avif 10-bit", "avif", plain(".avif", 10, false, "-quality", "95")},
		{"heic adobe", "heic", func(t *testing.T, env Env, dir string) string {
			return heicFixtureFile(t, "adobe-256x96")
		}},
		{"heic righttop", "heic", func(t *testing.T, env Env, dir string) string {
			return heicFixtureFile(t, "gradient-righttop")
		}},
	}
}

// The fused still, one magick process that decodes and encodes, writes what
// the materialised path writes: the decode to a 16-bit PAM beside its
// profile, then the same encode over that. Both are built by encodeArgs, so
// the fusion is the only difference.
func TestFusedStillEqualsMaterialisedStill(t *testing.T) {
	env := requireMagick(t)
	ctx := context.Background()
	destination := writeSRGBProfile(t)
	outputs := []encoding{
		{format: JPEG, ext: ".jpg", quality: 90},
		{format: WebP, ext: ".webp", quality: 80},
		{format: PNG, ext: ".png", quality: 90},
	}
	for _, fx := range fusedFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			dir := t.TempDir()
			path := fx.write(t, env, dir)
			ref := fx.coder + ":" + path + "[0]"
			scenes, err := identifyHeaders(ctx, env, ref, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := completeProfile(ctx, env, ref, path, scenes); err != nil {
				t.Fatal(err)
			}
			head := scenes[0]
			if fx.name == "jpeg profiled oriented" && head.orientation != "RightTop" {
				t.Fatalf("the fixture's orientation reads as %q", head.orientation)
			}
			w, h := uprightSize(head.width, head.height, head.orientation)
			t.Logf("%dx%d upright %dx%d orientation %s profiled %v", head.width, head.height, w, h, head.orientation, head.hasICC)

			// The materialised intermediate: raw 16-bit pixels, not oriented,
			// and the profile beside them.
			mid := filepath.Join(dir, "intermediate.pam")
			sidecar := filepath.Join(dir, "intermediate.icc")
			decode := append(LimitArgs(), ref, "-depth", "16", "-write", "pam:"+mid)
			if head.hasICC {
				decode = append(decode, "+delete", ref, "-write", "icc:"+sidecar)
			}
			magick(t, env, append(decode, "null:")...)

			for _, enc := range outputs {
				for _, edge := range []int{40, 400} {
					enc := enc
					enc.maxEdge, enc.maxArea = edge, edge*edge
					src := encodeSource{orientation: head.orientation, width: w, height: h}
					fusedOut := filepath.Join(dir, "fused"+enc.ext)
					matOut := filepath.Join(dir, "materialised"+enc.ext)

					fused := encodeFrame{input: ref}
					materialised := encodeFrame{input: "pam:" + mid}
					if head.hasICC {
						fused.convertTo = destination
						materialised.convertTo, materialised.assign = destination, sidecar
					}
					if _, err := env.runEncode(ctx, encodeArgs([]encodeFrame{fused}, src, enc, encodeOutput{kind: outputStill, path: fusedOut})); err != nil {
						t.Fatal(err)
					}
					if _, err := env.runEncode(ctx, encodeArgs([]encodeFrame{materialised}, src, enc, encodeOutput{kind: outputStill, path: matOut})); err != nil {
						t.Fatal(err)
					}
					a, b := mustReadFile(t, fusedOut), mustReadFile(t, matOut)
					if enc.format == PNG && !bytes.Equal(a, b) {
						// A PNG is written at the bit depth of the image it is
						// given. The 16-bit intermediate of an 8-bit source
						// keeps the resampled value the fused path rounds to 8
						// bits, so the two differ by at most that rounding.
						if d := maxAbsoluteDifference(t, env, fusedOut, matOut); d > 1.0/255+1e-6 {
							t.Errorf("png at %d: pixels differ by %g, more than one 8-bit level", edge, d)
						}
						continue
					}
					if !bytes.Equal(a, b) {
						t.Errorf("%s at %d: fused %d bytes, materialised %d bytes, they differ", enc.format, edge, len(a), len(b))
					}
				}
			}
		})
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func maxAbsoluteDifference(t *testing.T, env Env, first, second string) float64 {
	t.Helper()
	out := magick(t, env, first, second, "-compose", "difference", "-composite", "-format", "%[fx:maxima]", "info:")
	v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parsing image difference %q: %v", out, err)
	}
	return v
}
