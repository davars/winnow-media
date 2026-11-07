package main

import (
	"fmt"
	"math"
	"strings"
)

// size is one source geometry.
type size struct {
	key  string
	w, h int
}

func (s size) mp() float64 { return float64(s.w) * float64(s.h) / 1e6 }

// fourThree returns the 4:3 size of about mp megapixels, never above it.
func fourThree(mp int) size {
	w := int(math.Round(math.Sqrt(float64(mp)*1e6*4/3)/2)) * 2
	h := mp * 1_000_000 / w
	for w*h > mp*1_000_000 {
		h--
	}
	return size{key: fmt.Sprint(mp), w: w, h: h}
}

var sizes = func() []size {
	var out []size
	for _, mp := range []int{24, 36, 52, 64, 80, 100} {
		out = append(out, fourThree(mp))
	}
	// 16380 stays under the 16383 side limit of WebP.
	out = append(out, size{"pano60", 16380, 3640})
	// The three catalog stills over 52 MP.
	out = append(out,
		size{"c73", 7308, 10000},
		size{"c59a", 16380, 3628},
		size{"c59b", 16382, 3628})
	return out
}()

func sizeByKey(key string) (size, bool) {
	for _, s := range sizes {
		if s.key == key {
			return s, true
		}
	}
	return size{}, false
}

// format is one still format and how a master is written as it.
type format struct {
	key   string
	ext   string
	coder string   // ImageMagick coder, for reading and writing
	args  []string // output options before the file
	// ladder is the qualities tried, in order, until the file fits the
	// budget; empty for a format with no quality knob.
	ladder []int
}

var formats = []format{
	{"jpeg", ".jpg", "jpeg", []string{"-depth", "8", "-sampling-factor", "4:2:0"}, []int{90, 80, 70, 60, 50, 40}},
	{"png8", ".png", "png", []string{"-depth", "8", "-define", "png:compression-level=3"}, nil},
	{"png16", ".png", "png", []string{"-depth", "16", "-define", "png:compression-level=3"}, nil},
	{"webp-lossy", ".webp", "webp", []string{"-depth", "8"}, []int{85, 75, 65, 55, 45, 35}},
	{"webp-lossless", ".webp", "webp", []string{"-depth", "8", "-define", "webp:lossless=true", "-define", "webp:method=1"}, nil},
	{"heic8", ".heic", "heic", []string{"-depth", "8"}, []int{85, 70, 55, 40, 25}},
	{"heic10", ".heic", "heic", []string{"-depth", "10"}, []int{85, 70, 55, 40, 25}},
	{"avif", ".avif", "avif", []string{"-depth", "10", "-define", "heic:speed=8"}, []int{80, 65, 50, 35, 20}},
	{"jxl", ".jxl", "jxl", []string{"-depth", "8"}, []int{90, 80, 70, 60, 50, 40}},
	{"tiff8", ".tif", "tiff", []string{"-depth", "8", "-compress", "zip"}, nil},
	{"tiff16", ".tif", "tiff", []string{"-depth", "16", "-compress", "zip"}, nil},
}

func formatByKey(key string) (format, bool) {
	for _, f := range formats {
		if f.key == key {
			return f, true
		}
	}
	return format{}, false
}

// caseName is the file stem of one input.
func caseName(f format, s size) string { return f.key + "-" + s.key }

func parseCase(name string) (format, size, error) {
	i := strings.LastIndex(name, "-")
	if i < 0 {
		return format{}, size{}, fmt.Errorf("bad case %q", name)
	}
	f, ok := formatByKey(name[:i])
	s, ok2 := sizeByKey(name[i+1:])
	if !ok || !ok2 {
		return format{}, size{}, fmt.Errorf("bad case %q", name)
	}
	return f, s, nil
}

// matrix is every case measured, in order.
func matrix() []string {
	var out []string
	for _, f := range formats {
		for _, s := range sizes {
			switch {
			case strings.HasPrefix(s.key, "c") && f.key != "jpeg":
				continue
			case f.key == "tiff16" && s.key != "52" && s.key != "100":
				continue
			}
			out = append(out, caseName(f, s))
		}
	}
	return out
}
