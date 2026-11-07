package media

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func riffChunk(kind string, data []byte) []byte {
	out := append([]byte(kind), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
	out = append(out, data...)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func vp8xChunk(flags byte) []byte {
	payload := []byte{flags, 0, 0, 0, 7, 0, 0, 7, 0, 0} // an 8x8 canvas
	return riffChunk("VP8X", payload)
}

func riffWebP(chunks ...[]byte) []byte {
	body := []byte("WEBP")
	for _, c := range chunks {
		body = append(body, c...)
	}
	out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	return append(out, body...)
}

// imageChunk returns the VP8/VP8L chunk of an 8x8 simple WebP that magick made.
func imageChunk(t *testing.T, env Env) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "simple.webp")
	magick(t, env, "-size", "8x8", "xc:red", path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b[12:]
}

func writeBytes(t *testing.T, b []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.webp")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// onlyPinged reports whether every identify in calls read headers only.
func onlyPinged(calls [][]string) bool {
	for _, call := range calls {
		if !slices.Contains(call, "-ping") {
			return false
		}
	}
	return true
}

// padChunks returns n empty JUNK chunks.
func padChunks(n int) []byte {
	var out []byte
	for range n {
		out = append(out, riffChunk("JUNK", nil)...)
	}
	return out
}

// The profile of a WebP still comes from its chunk list where the layout
// settles it, and from a decode otherwise. Both header readers and the scan
// must agree with a full ImageMagick read of every layout, and decode only
// when the scan cannot answer.
func TestWebPProfileAgreesWithImageMagick(t *testing.T) {
	real := requireMagick(t)
	profile, err := os.ReadFile(writeSRGBProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	image := imageChunk(t, real)
	iccp := riffChunk("ICCP", profile)
	const icc, anim = webpICCFlag, webpAnimFlag
	frame := append([]byte{0, 0, 0, 0, 0, 0, 7, 0, 0, 7, 0, 0, 100, 0, 0, 0}, image...)
	oneFrameAnimation := riffWebP(vp8xChunk(icc|anim), iccp, riffChunk("ANIM", make([]byte, 6)), riffChunk("ANMF", frame))
	const limit = 1024 // the chunk headers the scan reads
	atLimit := riffWebP(append(append(vp8xChunk(icc), image...), padChunks(limit-2)...))
	pastLimit := riffWebP(append(append(vp8xChunk(icc), image...), padChunks(limit-1)...))
	shortRIFF := riffWebP(vp8xChunk(icc), image, iccp)
	binary.LittleEndian.PutUint32(shortRIFF[4:], uint32(len(shortRIFF)-8-len(iccp)))
	trailing := append(riffWebP(vp8xChunk(icc), iccp, image), "trailing"...)

	for _, c := range []struct {
		name  string
		file  []byte
		known bool // the chunk list settles the answer
		// The profile of an animation applies to its frames, though
		// ImageMagick's own decode of one ignores it.
		animated bool
	}{
		{"flag and chunk", riffWebP(vp8xChunk(icc), iccp, image), true, false},
		{"flag only", riffWebP(vp8xChunk(icc), image), true, false},
		{"chunk only", riffWebP(vp8xChunk(0), iccp, image), true, false},
		{"simple", riffWebP(image), true, false},
		{"chunk after the image", riffWebP(vp8xChunk(icc), image, iccp), true, false},
		{"chunk after another chunk", riffWebP(vp8xChunk(icc|0x08), riffChunk("EXIF", []byte("Exif\x00\x00")), iccp, image), true, false},
		{"chunk after an odd-sized chunk", riffWebP(vp8xChunk(icc), riffChunk("JUNK", []byte{1}), iccp, image), true, false},
		{"bytes after the RIFF", trailing, true, false},
		{"chunk beyond the RIFF size", shortRIFF, true, false},
		{"one-frame animation with a chunk", oneFrameAnimation, true, true},
		{"two image chunks", riffWebP(vp8xChunk(icc), iccp, image, image), false, false},
		{"two image chunks after a simple image", riffWebP(image, iccp, image), false, false},
		{"empty chunk", riffWebP(vp8xChunk(icc), riffChunk("ICCP", nil), image), false, false},
		{"animation flag over an image chunk", riffWebP(vp8xChunk(icc|anim), iccp, image), false, false},
		{"frames without the animation flag", riffWebP(vp8xChunk(icc), iccp, riffChunk("ANIM", make([]byte, 6)), riffChunk("ANMF", frame)), false, false},
		{"frames and an image without the animation flag", riffWebP(vp8xChunk(icc), iccp, image, riffChunk("ANIM", make([]byte, 6)), riffChunk("ANMF", frame)), false, false},
		{"frames and an image with the animation flag", riffWebP(vp8xChunk(icc|anim), iccp, image, riffChunk("ANIM", make([]byte, 6)), riffChunk("ANMF", frame)), false, false},
		{"animation flag without frames", riffWebP(vp8xChunk(icc|anim), iccp, riffChunk("ANIM", make([]byte, 6))), false, false},
		{"VP8X after the image", riffWebP(image, vp8xChunk(icc), iccp), false, false},
		{"second VP8X", riffWebP(vp8xChunk(icc), vp8xChunk(icc), iccp, image), false, false},
		{"two chunks", riffWebP(vp8xChunk(icc), iccp, iccp, image), false, false},
		{"as many chunks as the scan reads", atLimit, true, false},
		{"one chunk more", pastLimit, false, false},
	} {
		path := writeBytes(t, c.file)
		read, err := real.Run(context.Background(), "magick", "identify", "-format", "%[profiles]", path)
		if has, known := webpICC(path); err != nil {
			// ImageMagick refuses the file, so there is no profile to agree
			// with; the scan must still leave it to the decode.
			t.Logf("%s: refused by ImageMagick", c.name)
			if known != c.known {
				t.Errorf("%s: webpICC = %t, known %t; want known %t", c.name, has, known, c.known)
			}
			continue
		} else if want := profileListHasICC(string(read.Stdout)) || c.animated; known != c.known || known && has != want {
			t.Errorf("%s: webpICC = %t, known %t; want %t, known %t", c.name, has, known, want, c.known)
		}
		want := profileListHasICC(string(read.Stdout)) || c.animated
		t.Run("ProbeFrames/"+c.name, func(t *testing.T) {
			env, calls := countingEnv(real)
			frames, err := env.ProbeFrames(context.Background(), path, "image/webp")
			if err != nil || len(frames) != 1 || frames[0].hasICC != want {
				t.Fatalf("frames %+v, err %v; want one frame, hasICC %t", frames, err, want)
			}
			if onlyPinged(calls()) != c.known {
				t.Errorf("runs %q: headers only = %t, want %t", calls(), !c.known, c.known)
			}
		})
		t.Run("readHeaders/"+c.name, func(t *testing.T) {
			env, calls := countingEnv(real)
			scenes, err := readHeaders(context.Background(), env, decoder{"magick", "webp"}, path, true)
			if err != nil || len(scenes) != 1 || scenes[0].hasICC != want {
				t.Fatalf("scenes %+v, err %v; want one scene, hasICC %t", scenes, err, want)
			}
			if onlyPinged(calls()) != c.known {
				t.Errorf("runs %q: headers only = %t, want %t", calls(), !c.known, c.known)
			}
		})
	}
}

// A file the scan cannot read to its end gives no answer, so its profile is
// left to the decode.
func TestWebPICCDeclinesAnUnreadableLayout(t *testing.T) {
	image := riffChunk("VP8 ", make([]byte, 21))
	full := riffWebP(vp8xChunk(webpICCFlag), riffChunk("ICCP", []byte("profile")), image)
	oversized := slices.Clone(full)
	binary.LittleEndian.PutUint32(oversized[4:], 1<<20)
	zeroVP8X := riffWebP(append([]byte("VP8X\x00\x00\x00\x00"), riffChunk("ICCP", nil)...), image)
	for name, file := range map[string][]byte{
		"cut inside the chunk list": full[:30],
		"cut inside a header":       full[:20],
		"RIFF size past the file":   oversized,
		"empty":                     nil,
		"not RIFF":                  append([]byte("JUNK"), full[4:]...),
		"unknown first chunk":       riffWebP(riffChunk("JUNK", []byte("xx")), vp8xChunk(webpICCFlag), image),
		"empty VP8X":                zeroVP8X,
	} {
		if has, known := webpICC(writeBytes(t, file)); known {
			t.Errorf("%s: webpICC = %t, known; want no answer", name, has)
		}
	}
	if has, known := webpICC(filepath.Join(t.TempDir(), "missing")); known {
		t.Errorf("missing file: webpICC = %t, known; want no answer", has)
	}
}
