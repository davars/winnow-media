package media

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const svgHead = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:xi="http://www.w3.org/2001/XInclude" width="40" height="40">`

// svgScene is a directory laid out for the reference tests: the input's
// directory, with canaries beside it, under it and above it, and one at an
// absolute path of its own.
type svgScene struct {
	dir   string // the input's directory
	abs   string // an absolute path outside it
	check func()
}

func newSVGScene(t *testing.T) svgScene {
	t.Helper()
	base, check := canaryDir(t, "abs/canary", "in/canary", "in/sub/canary", "up-canary")
	return svgScene{dir: filepath.Join(base, "in"), abs: filepath.Join(base, "abs", "canary"), check: check}
}

// hostileSVGs returns SVGs that name the scene's canaries in each way an SVG
// can, each of which draws something besides.
func (s svgScene) hostileSVGs() map[string]string {
	png := "iVBORw0KGgoAAAANSUhEUgAAABQAAAAUAQMAAAC3R49OAAAAIGNIUk0AAHomAACAhAAA+gAAAIDoAAB1MAAA6mAAADqYAAAXcJy6UTwAAAADUExURf8AABniCTcAAAAMSURBVAjXY2CgLgAAAFAAASIT6HUAAAAASUVORK5CYII="
	file := "file://" + s.abs
	nested := base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="20" height="20"><image href="` + file + `" width="20" height="20"/></svg>`))
	body := func(inner string) string {
		return svgHead + `<rect width="40" height="40" fill="#eee"/>` + inner + `</svg>`
	}
	img := func(href string) string { return body(`<image href="` + href + `" width="20" height="20"/>`) }
	return map[string]string{
		"absolute file URL":          img(file),
		"absolute path":              img(s.abs),
		"xlink:href":                 body(`<image xlink:href="` + file + `" width="20" height="20"/>`),
		"relative sibling":           img("canary"),
		"relative in a subdirectory": img("sub/canary"),
		"relative upward":            img("../up-canary"),
		"descriptor 4":               img("4"),
		"descriptor via /dev/fd":     img("/dev/fd/4"),
		"descriptor via ..":          img("../fd/4"),
		"use of another file":        body(`<use href="` + file + `#r"/>`),
		"use of a sibling":           body(`<use href="canary#r"/>`),
		"feImage":                    body(`<defs><filter id="f"><feImage href="` + file + `"/></filter></defs><rect width="30" height="30" filter="url(#f)"/>`),
		"css import":                 body(`<style>@import url("` + file + `");</style><rect width="10" height="10" class="a"/>`),
		"css import of a sibling":    body(`<style>@import url("canary");</style><rect width="10" height="10" class="a"/>`),
		"font face":                  body(`<style>@font-face { font-family: x; src: url("` + file + `"); } text { font-family: x; }</style><text x="2" y="20">A</text>`),
		"xinclude as text":           body(`<text x="0" y="20"><xi:include href="` + file + `" parse="text"/></text>`),
		"xinclude as xml":            body(`<xi:include href="` + file + `" parse="xml"/>`),
		"data SVG naming a file":     img("data:image/svg+xml;base64," + nested),
		"data PNG":                   img("data:image/png;base64," + png),
		"data URL in CSS":            body(`<style>.a { fill: url("data:image/png;base64,` + png + `"); }</style><rect width="10" height="10" class="a"/>`),
		"external entity": `<?xml version="1.0"?>` + "\n" + `<!DOCTYPE svg [<!ENTITY x SYSTEM "` + file + `">]>` + "\n" +
			`<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><text x="0" y="20">&x;</text></svg>`,
		"external parameter entity": `<?xml version="1.0"?>` + "\n" + `<!DOCTYPE svg [<!ENTITY % p SYSTEM "` + file + `"> %p;]>` + "\n" +
			`<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><rect width="9" height="9"/></svg>`,
		"external DTD": `<?xml version="1.0"?>` + "\n" + `<!DOCTYPE svg SYSTEM "` + file + `">` + "\n" +
			`<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><rect width="9" height="9"/></svg>`,
		"ImageMagick MSL document": `<?xml version="1.0"?><image><read filename="` + s.abs + `"/><write filename="` + s.abs + `.out"/></image>`,
		"ImageMagick coder prefix": img("msl:" + s.abs),
	}
}

// Whatever an SVG names, nothing but the SVG itself is opened, under the
// confinement, and, to show that the renderer itself does not follow them,
// without it.
func TestHostileSVGReferencesAreNotFollowed(t *testing.T) {
	for _, mode := range []struct {
		name string
		cfg  Config
	}{
		{"confined", svgConfig},
		{"unconfined", Config{RenderTypes: svgConfig.RenderTypes, Unsandboxed: true}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			p := requireProcessorWith(t, mode.cfg)
			scene := newSVGScene(t)
			if err := os.MkdirAll(scene.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, svg := range scene.hostileSVGs() {
				path := filepath.Join(scene.dir, "input.svg")
				if err := os.WriteFile(path, []byte(svg), 0o600); err != nil {
					t.Fatal(err)
				}
				out := t.TempDir()
				_, _, err := p.RenderSVG(context.Background(), path, out, SVGRender{})
				var te *ToolError
				if err != nil && !errors.As(err, &te) {
					t.Errorf("%s: err = %v, want success or a *ToolError", name, err)
				}
				// The entities are the one reference an SVG that is drawn
				// today fails on.
				if strings.Contains(name, "external entity") && err == nil {
					t.Errorf("%s: the call succeeded", name)
				}
				if err != nil {
					if entries, _ := os.ReadDir(out); len(entries) != 0 {
						t.Errorf("%s: a failed call left files", name)
					}
				}
			}
			scene.check()
		})
	}
}

// The SVG's own inline image is drawn, which shows that the scene's other
// fixtures are not silently drawing nothing.
func TestHostileSVGFixtureDrawsItsInlineImage(t *testing.T) {
	p, _ := requireSVGProcessor(t)
	env := requireMagick(t)
	scene := newSVGScene(t)
	if err := os.MkdirAll(scene.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scene.dir, "input.svg")
	if err := os.WriteFile(path, []byte(scene.hostileSVGs()["data PNG"]), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, _, err := p.RenderSVG(context.Background(), path, dir, SVGRender{}); err != nil {
		t.Fatal(err)
	}
	if got := string(magick(t, env, filepath.Join(dir, "000.png"), "-format", "%[pixel:p{5,5}]", "info:")); got != "srgb(255,0,0)" {
		t.Errorf("pixel inside the inline image = %q, want red", got)
	}
	scene.check()
}

// An image at an http URL is not fetched: the tool has no socket.
func TestHostileSVGNetworkReferenceIsNotFetched(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			_ = c.Close()
		}
	}()
	p, _ := requireSVGProcessor(t)
	url := "http://" + ln.Addr().String() + "/x.png"
	svg := svgHead + `<image href="` + url + `" width="20" height="20"/><image href="//` + ln.Addr().String() + `/y.png" width="5" height="5"/></svg>`
	_, _, err = p.RenderSVG(context.Background(), writeFile(t, "net.svg", []byte(svg)), t.TempDir(), SVGRender{})
	var te *ToolError
	if err != nil && !errors.As(err, &te) {
		t.Errorf("err = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Errorf("%d connections were made", n)
	}
}

// A document that is not SVG reaches no other coder.
func TestRenderSVGPinsTheCoder(t *testing.T) {
	scene := newSVGScene(t)
	p, rec := requireSVGProcessor(t)
	for name, c := range map[string]struct {
		content string
		noRun   bool
	}{
		"MVG":        {"push graphic-context\nimage over 0,0 0,0 '" + scene.abs + "'\npop graphic-context\n", true},
		"PostScript": {"%!PS-Adobe-3.0\n%%BoundingBox: 0 0 10 10\n0 0 10 10 rectfill\nshowpage\n", true},
		"MSL":        {`<?xml version="1.0"?><image><read filename="` + scene.abs + `"/></image>`, false},
		"text coder": {"<svg" + strings.Repeat(" ", 10) + "\n", false},
	} {
		before := rec.count()
		path := writeFile(t, "x.svg", []byte(c.content))
		_, _, err := p.RenderSVG(context.Background(), path, t.TempDir(), SVGRender{})
		if err == nil {
			t.Errorf("%s: the call succeeded", name)
		}
		if c.noRun && (!errors.Is(err, ErrUnsupportedType) || rec.count() != before) {
			t.Errorf("%s: err = %v after %d runs, want ErrUnsupportedType with no run", name, err, rec.count()-before)
		}
	}
	scene.check()
}

// An SVG built to exhaust the renderer ends within the limits.
func TestHostileSVGContentIsBounded(t *testing.T) {
	p := requireProcessorWith(t, Config{RenderTypes: svgConfig.RenderTypes, Resources: Resources{TimeLimit: 10 * time.Second, CPUTime: 20 * time.Second}})
	var laughs strings.Builder
	laughs.WriteString(`<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY a0 "ha">`)
	for i := 1; i < 10; i++ {
		fmt.Fprintf(&laughs, `<!ENTITY a%d "&a%d;&a%d;&a%d;&a%d;&a%d;&a%d;&a%d;&a%d;&a%d;&a%d;">`, i, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1)
	}
	laughs.WriteString(`]><svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><text x="0" y="20">&a9;</text></svg>`)
	nest := svgHead + strings.Repeat("<g>", 200_000) + `<rect width="9" height="9"/>` + strings.Repeat("</g>", 200_000) + "</svg>"
	many := svgHead + strings.Repeat(`<rect width="1" height="1"/>`, 1_000_000) + "</svg>"
	for name, svg := range map[string]string{"billion laughs": laughs.String(), "200,000 nested groups": nest, "a million elements": many} {
		start := time.Now()
		_, _, err := p.RenderSVG(context.Background(), writeFile(t, "x.svg", []byte(svg)), t.TempDir(), SVGRender{})
		var te *ToolError
		if err != nil && !errors.As(err, &te) && !errors.Is(err, ErrTimeLimit) && !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: err = %v", name, err)
		}
		if took := time.Since(start); took > 60*time.Second {
			t.Errorf("%s took %v", name, took)
		}
		t.Logf("%s: %v after %v", name, err, time.Since(start).Round(time.Millisecond))
	}
}
