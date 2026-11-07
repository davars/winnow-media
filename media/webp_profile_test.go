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

func TestWebPHasICC(t *testing.T) {
	icc := []byte("not a real profile, never read")
	image := riffChunk("VP8 ", make([]byte, 21))
	many := []byte{}
	for range maxWebPChunks {
		many = append(many, riffChunk("JUNK", nil)...)
	}
	full := riffWebP(vp8xChunk(webpICCFlag), riffChunk("ICCP", icc), image)
	for _, c := range []struct {
		name    string
		file    []byte
		want    bool
		wantErr bool
	}{
		{"flag and chunk", full, true, false},
		{"flag and chunk after an odd-sized chunk", riffWebP(vp8xChunk(webpICCFlag), riffChunk("JUNK", []byte{1}), riffChunk("ICCP", icc), image), true, false},
		{"flag and chunk after the image", riffWebP(vp8xChunk(webpICCFlag), image, riffChunk("ICCP", icc)), true, false},
		{"flag without a chunk", riffWebP(vp8xChunk(webpICCFlag), image), false, false},
		{"chunk without the flag", riffWebP(vp8xChunk(0), riffChunk("ICCP", icc), image), false, false},
		{"chunk without VP8X", riffWebP(riffChunk("ICCP", icc), image), false, false},
		{"simple file", riffWebP(image), false, false},
		{"cut inside the chunk list", full[:30], false, false},
		{"cut inside a header", full[:20], false, false},
		{"empty", nil, false, false},
		{"not RIFF", append([]byte("JUNK"), full[4:]...), false, false},
		{"too many chunks", riffWebP(append(vp8xChunk(webpICCFlag), many...)), false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := webpHasICC(writeBytes(t, c.file))
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("webpHasICC = %t, %v; want %t, error %t", got, err, c.want, c.wantErr)
			}
		})
	}
}

// onlyPinged fails the test for any identify that decodes pixels.
func onlyPinged(t *testing.T, calls [][]string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("no magick runs recorded")
	}
	for _, call := range calls {
		if !slices.Contains(call, "-ping") {
			t.Errorf("a still WebP was decoded to read its headers: %q", call)
		}
	}
}

// ImageMagick honours a WebP profile only when the VP8X chunk flags it and an
// ICCP chunk follows, so the flag alone, which a still of any size can carry,
// reports no profile on both header readers.
func TestWebPStillProfileNeedsTheFlagAndTheChunk(t *testing.T) {
	real := requireMagick(t)
	icc := writeSRGBProfile(t)
	profile, err := os.ReadFile(icc)
	if err != nil {
		t.Fatal(err)
	}
	image := imageChunk(t, real)
	for _, c := range []struct {
		name string
		file []byte
		want bool
	}{
		{"flag and chunk", riffWebP(vp8xChunk(webpICCFlag), riffChunk("ICCP", profile), image), true},
		{"flag only", riffWebP(vp8xChunk(webpICCFlag), image), false},
		{"chunk only", riffWebP(vp8xChunk(0), riffChunk("ICCP", profile), image), false},
		{"simple", riffWebP(image), false},
	} {
		path := writeBytes(t, c.file)
		t.Run("ProbeFrames/"+c.name, func(t *testing.T) {
			env, calls := countingEnv(real)
			frames, err := env.ProbeFrames(context.Background(), path, "image/webp")
			if err != nil || len(frames) != 1 || frames[0].hasICC != c.want {
				t.Fatalf("frames %+v, err %v; want one frame, hasICC %t", frames, err, c.want)
			}
			onlyPinged(t, calls())
		})
		t.Run("readHeaders/"+c.name, func(t *testing.T) {
			env, calls := countingEnv(real)
			scenes, err := readHeaders(context.Background(), env, decoder{"magick", "webp"}, path, "[0]", false)
			if err != nil || len(scenes) != 1 || scenes[0].hasICC != c.want {
				t.Fatalf("scenes %+v, err %v; want one scene, hasICC %t", scenes, err, c.want)
			}
			onlyPinged(t, calls())
		})
	}
}

// ImageMagick's JXL coder reports no profile under -ping (libjxl gives every
// JXL one on a full read), so the operations' header reader must learn it from
// a decode.
func TestReadHeadersReportsTheProfileOfAJXLStill(t *testing.T) {
	env := requireMagick(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "wide.jxl")
	magick(t, env, "-size", "64x32", "gradient:#0020ff-#ffb000", "-profile", writeSRGBProfile(t), "-profile", writeAdobeProfile(t, dir), path)
	if !profileListHasICC(string(magick(t, env, "identify", "-format", "%[profiles]", path))) {
		t.Skip("this ImageMagick does not write an ICC profile into JXL")
	}
	scenes, err := readHeaders(context.Background(), env, decoder{"magick", "jxl"}, path, "[0]", false)
	if err != nil || len(scenes) != 1 || !scenes[0].hasICC || scenes[0].format != "JXL" {
		t.Fatalf("scenes %+v, err %v; want one profiled JXL scene", scenes, err)
	}
}
