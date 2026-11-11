package media

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
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
		{"grayscale jpeg", "jpeg", func(t *testing.T, env Env, dir string) string {
			path := filepath.Join(dir, "fixture.jpg")
			magick(t, env, "-size", "64x48", "gradient:white-black", "-colorspace", "Gray", "-quality", "95", path)
			return path
		}},
		{"grayscale png16", "png", func(t *testing.T, env Env, dir string) string {
			path := filepath.Join(dir, "fixture.png")
			magick(t, env, "-size", "64x48", "gradient:white-black", "-colorspace", "Gray", "-depth", "16", path)
			return path
		}},
		{"cmyk jpeg", "jpeg", func(t *testing.T, env Env, dir string) string {
			path := filepath.Join(dir, "fixture.jpg")
			magick(t, env, "-size", "64x48", "gradient:#0020ff-#ffb000", "-colorspace", "CMYK", "-quality", "95", path)
			return path
		}},
		{"cmyk jpeg profiled", "jpeg", func(t *testing.T, env Env, dir string) string {
			cmyk := findCMYKProfile(t)
			path := filepath.Join(dir, "fixture.jpg")
			magick(t, env, "-size", "64x48", "gradient:#0020ff-#ffb000", "-colorspace", "CMYK", "-profile", cmyk, "-quality", "95", path)
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
	t.Parallel()
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
			if err := completeProfile(ctx, env, ref, input{path: path}, scenes); err != nil {
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
					fusedSrc, materialisedSrc := encodeSource{}, encodeSource{orientation: head.orientation}
					fusedOut := filepath.Join(dir, "fused"+enc.ext)
					matOut := filepath.Join(dir, "materialised"+enc.ext)

					fused := encodeFrame{input: ref}
					materialised := encodeFrame{input: "pam:" + mid}
					if head.hasICC {
						fused.convertTo = destination
						materialised.convertTo, materialised.assign = destination, sidecar
					}
					if _, err := env.runEncode(ctx, encodeArgs([]encodeFrame{fused}, fusedSrc, enc, encodeOutput{kind: outputStill, path: fusedOut}), encodeCheck{}); err != nil {
						t.Fatal(err)
					}
					if _, err := env.runEncode(ctx, encodeArgs([]encodeFrame{materialised}, materialisedSrc, enc, encodeOutput{kind: outputStill, path: matOut}), encodeCheck{}); err != nil {
						t.Fatal(err)
					}
					a, b := mustReadFile(t, fusedOut), mustReadFile(t, matOut)
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

// findCMYKProfile returns a CMYK ICC profile installed on the machine, or
// skips the test.
func findCMYKProfile(t *testing.T) string {
	t.Helper()
	for _, pattern := range []string{
		"/usr/share/color/icc/*/*CMYK*", "/usr/share/color/icc/*CMYK*", "/usr/share/ghostscript/*/iccprofiles/default_cmyk.icc",
		"/nix/store/*-source/config/cmyk.icm",
	} {
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return matches[0]
		}
	}
	t.Skip("no CMYK ICC profile installed")
	return ""
}
