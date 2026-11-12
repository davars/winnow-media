package media

import (
	"errors"
	"slices"
	"testing"
)

func TestSniff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
		want string // "" for unsupported
	}{
		{"gif87a", []byte("GIF87a\x01\x00"), typeGIF},
		{"gif89a", []byte("GIF89a\x01\x00"), typeGIF},
		{"webp", append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), 0), typeWebP},
		{"riff wave", []byte("RIFF\x10\x00\x00\x00WAVEfmt "), ""},
		{"heic", ftypBox("heic", "mif1", "heic"), typeHEIC},
		{"heix", ftypBox("heix"), typeHEIC},
		{"hevc", ftypBox("hevc"), typeHEIC},
		{"heif with heic brand", ftypBox("mif1", "mif1", "heic"), typeHEIF},
		{"msf1 with heic brand", ftypBox("msf1", "heic"), typeHEIF},
		{"heif with heic and avif brands", ftypBox("mif1", "heic", "avif"), ""},
		{"avif", ftypBox("avif", "mif1", "miaf"), typeAVIF},
		{"heif with avif brand", ftypBox("mif1", "avif", "miaf"), typeAVIF},
		{"avif sequence", ftypBox("avis", "avis", "msf1"), ""},
		{"heif without heic brand", ftypBox("mif1", "miaf"), ""},
		{"mp4 isom", ftypBox("isom", "isom", "mp42"), typeMP4},
		{"mp4 mp42", ftypBox("mp42", "mp42"), typeMP4},
		{"3gp", ftypBox("3gp4"), typeMP4},
		{"quicktime", ftypBox("qt  ", "qt  "), typeMOV},
		{"m4a", ftypBox("M4A ", "M4A "), ""},
		{"ftyp too short", ftypBox("isom")[:12], ""},
		{"ftyp size below minimum", append([]byte{0, 0, 0, 8}, "ftypisom\x00\x00\x00\x00"...), ""},
		{"ftyp not first", append([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}, ftypBox("isom")...), ""},
		{"webm", ebmlHeader("webm"), typeWebM},
		{"matroska", ebmlHeader("matroska"), typeMKV},
		{"unknown doctype", ebmlHeader("other"), ""},
		{"ebml truncated", ebmlHeader("webm")[:9], ""},
		{"ebml bare magic", []byte{0x1A, 0x45, 0xDF, 0xA3}, ""},
		{"ebml unknown size", []byte{0x1A, 0x45, 0xDF, 0xA3, 0xFF, 0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}, ""},
		{"ebml zero length byte", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x00}, ""},
		{"png", []byte("\x89PNG\r\n\x1a\n"), ""},
		{"empty", nil, ""},
		{"text", []byte("#EXTM3U\n"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := sniff(c.in)
			if c.want == "" {
				if !errors.Is(err, ErrUnsupportedType) {
					t.Errorf("sniff = %q, %v; want ErrUnsupportedType", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("sniff = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestSniffFileIgnoresName(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "image.gif", []byte("\x89PNG\r\n\x1a\n"))
	if got, err := sniffPath(path); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("sniffFile = %q, %v; want ErrUnsupportedType", got, err)
	}
}

func FuzzSniff(f *testing.F) {
	seeds := [][]byte{
		[]byte("GIF89a\x01\x00\x01\x00"),
		[]byte("RIFF\x10\x00\x00\x00WEBPVP8 "),
		ftypBox("heic", "mif1", "heic"),
		ftypBox("mif1", "mif1", "heic"),
		ftypBox("isom", "isom", "mp42"),
		ftypBox("qt  ", "qt  "),
		ebmlHeader("webm"),
		ebmlHeader("matroska"),
		ebmlHeader("webm")[:9],
		ftypBox("isom")[:10],
		{0x1A, 0x45, 0xDF, 0xA3, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{0x1A, 0x45, 0xDF, 0xA3, 0x84, 0x42, 0x82, 0x01, 0x00},
		{},
		[]byte("garbage"),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	known := []string{typeGIF, typeWebP, typeHEIC, typeHEIF, typeMP4, typeMOV, typeWebM, typeMKV}
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := sniff(b)
		if err != nil {
			if !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("error %v does not wrap ErrUnsupportedType", err)
			}
			return
		}
		if !slices.Contains(known, got) {
			t.Fatalf("sniff returned %q", got)
		}
	})
}
