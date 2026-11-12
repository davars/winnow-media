package media

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// flagValue returns the value of "-limit name" in args.
func flagValue(t *testing.T, args []string, name string) string {
	t.Helper()
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "-limit" && args[i+1] == name {
			return args[i+2]
		}
	}
	t.Fatalf("no -limit %s in %q", name, args)
	return ""
}

func policyValue(t *testing.T, rules []policyRule, name string) string {
	t.Helper()
	for _, r := range rules {
		if r.domain == "resource" && r.name == name {
			return r.value
		}
	}
	t.Fatalf("no resource %s in the policy", name)
	return ""
}

func TestDefaultResourcesGiveTheDefaultFlags(t *testing.T) {
	t.Parallel()
	want := []string{
		"-limit", "memory", "1GiB",
		"-limit", "map", "2GiB",
		"-limit", "area", "512MP",
		"-limit", "list-length", "2048",
		"-limit", "time", "300",
	}
	if got := defaultResources().limitArgs(); !slices.Equal(got, want) {
		t.Errorf("limitArgs() = %q, want %q", got, want)
	}
}

// lowMemory is Resources for a memory-limited container.
func lowMemory() Resources { return Resources{Memory: 512 << 20, Map: 1 << 30}.withDefaults() }

func TestResourceFlagsAndPolicyAgree(t *testing.T) {
	t.Parallel()
	r := lowMemory()
	flags := r.limitArgs()
	rules := processorPolicy(r, Types())
	for name, want := range map[string]string{"memory": "512MiB", "map": "1GiB"} {
		if got := flagValue(t, flags, name); got != want {
			t.Errorf("flag %s = %s, want %s", name, got, want)
		}
		if got := policyValue(t, rules, name); got != want {
			t.Errorf("policy %s = %s, want %s", name, got, want)
		}
	}
	// Everything else is the same under both.
	high := defaultResources().limitArgs()
	for _, name := range []string{"area", "list-length", "time"} {
		if flagValue(t, flags, name) != flagValue(t, high, name) {
			t.Errorf("flag %s differs between the resources", name)
		}
	}
}

// assertLimited requires the magick argv that ran to carry exactly the
// Resources' limit flags, once, ahead of the rest and after a leading
// "identify".
func assertLimited(t *testing.T, argv []string, want []string) {
	t.Helper()
	rest := argv
	if len(rest) > 0 && rest[0] == "identify" {
		rest = rest[1:]
	}
	if len(rest) < len(want) || !slices.Equal(rest[:len(want)], want) {
		t.Errorf("argv %q does not start with %q", argv, want)
	}
	n := 0
	for _, a := range argv {
		if a == "-limit" {
			n++
		}
	}
	if n != len(want)/3 {
		t.Errorf("argv %q has %d -limit flags, want %d", argv, n, len(want)/3)
	}
}

// Every route that runs magick passes through toolEnv.runMagick, whatever its
// own arguments.
func TestMagickRoutesCarryTheResources(t *testing.T) {
	t.Parallel()
	frame := encodeFrame{input: "pam:/in"}
	enc := webpEncoding(100)
	routes := map[string]func(toolEnv) error{
		"identifyScenes": func(e toolEnv) error {
			_, _, _, err := e.identifyScenes(context.Background(), "/a.webp")
			return err
		},
		"identifyHeaders": func(e toolEnv) error {
			_, err := identifyHeaders(context.Background(), e, "png:/a", true)
			return err
		},
		"identifyHeaders full": func(e toolEnv) error {
			_, err := identifyHeaders(context.Background(), e, "png:/a", false)
			return err
		},
		"runEncode": func(e toolEnv) error {
			_, err := e.runEncode(context.Background(),
				encodeArgs([]encodeFrame{frame}, encodeSource{}, enc, encodeOutput{kind: outputStill, path: "/o.webp"}), encodeCheck{})
			return err
		},
		"runEncode fused": func(e toolEnv) error {
			_, err := e.runEncode(context.Background(),
				fusedArgs(nil, encodeSource{}, []fusedTarget{{frame: frame, enc: enc, path: "/o.webp"}}), encodeCheck{})
			return err
		},
	}
	for name, route := range routes {
		for pname, res := range map[string]Resources{"default": defaultResources(), "low memory": lowMemory()} {
			t.Run(name+"/"+pname, func(t *testing.T) {
				var ran [][]string
				env := toolEnv{resources: res, caps: measuredCaps, run: func(_ context.Context, tool string, args ...string) (runResult, error) {
					if tool != "magick" {
						t.Errorf("ran %s", tool)
					}
					ran = append(ran, args)
					return runResult{Stdout: []byte("1 8 8\n")}, nil
				}}
				_ = route(env) // the fake output need not parse
				if len(ran) == 0 {
					t.Fatal("no magick run")
				}
				for _, argv := range ran {
					if strings.Contains(strings.ToLower(name), "identify") && argv[0] != "identify" {
						t.Errorf("argv %q does not start with the identify subcommand", argv)
					}
					assertLimited(t, argv, res.limitArgs())
				}
			})
		}
	}
}

func TestProcessorInstallsTheResources(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name          string
		resources     Resources
		memory, mapLm string
	}{{"default", defaultResources(), "1GiB", "2GiB"}, {"low memory", lowMemory(), "512MiB", "1GiB"}} {
		t.Run(c.name, func(t *testing.T) {
			p := requireProcessorWith(t, Config{Resources: c.resources})
			data, err := os.ReadFile(filepath.Join(p.dir, "policy.xml"))
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"memory": c.memory, "map": c.mapLm} {
				if !strings.Contains(string(data), `name="`+name+`" value="`+want+`"`) {
					t.Errorf("policy.xml lacks %s=%s:\n%s", name, want, data)
				}
			}
			res, err := p.run(context.Background(), nil, "magick", "-list", "resource")
			if err != nil {
				t.Fatal(err)
			}
			got := parseResourceList(res.Stdout)
			for name, want := range map[string]string{"memory": c.memory, "map": c.mapLm} {
				g, _ := resourceNumber(got[name])
				w, _ := resourceNumber(want)
				if g != w {
					t.Errorf("effective %s = %s, want %s", name, got[name], want)
				}
			}
			var m meter
			var ran []string
			env := p.env(&m, nil)
			inner := env.run
			env.run = func(ctx context.Context, name string, args ...string) (runResult, error) {
				ran = args
				return inner(ctx, name, args...)
			}
			if _, _, _, err := env.identifyScenes(context.Background(), filepath.Join(t.TempDir(), "none.webp")); err == nil {
				t.Error("identify of a missing file succeeded")
			}
			assertLimited(t, ran, c.resources.limitArgs())
		})
	}
}

// Every magick run of a Source, whichever encode path it takes, carries the
// Processor's Resources.
func TestSourceRunsCarryTheResources(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	p := requireProcessorWith(t, Config{Resources: lowMemory()})
	rec := record(p)
	dir := t.TempDir()
	png := filepath.Join(dir, "x.png")
	magick(t, requireMagick(t), "-size", "16x16", "xc:red", png)
	gif := filepath.Join(dir, "x.gif")
	if err := os.WriteFile(gif, oneColourGIF(t, 8, 8, 3), 0o600); err != nil {
		t.Fatal(err)
	}
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 16, MaxArea: 256}
	other := Encoding{Format: WebP, Quality: 80, MaxEdge: 32, MaxArea: 1024}
	animated := enc
	animated.Animate = true
	for name, c := range map[string]struct {
		path    string
		targets []Encoding
	}{
		"still, two targets (fused)": {png, []Encoding{enc, other}},
		"animation":                  {gif, []Encoding{animated}},
	} {
		t.Run(name, func(t *testing.T) {
			before := rec.count()
			src := openSourceOn(t, p, c.path, OpenOptions{})
			if _, _, err := src.Encode(context.Background(), t.TempDir(), Select{}, c.targets...); err != nil {
				t.Fatal(err)
			}
			rec.mu.Lock()
			calls := slices.Clone(rec.calls[before:])
			rec.mu.Unlock()
			ran := 0
			for _, call := range calls {
				if call[0] != "magick" {
					continue
				}
				ran++
				if flagValue(t, call, "memory") != "512MiB" || flagValue(t, call, "map") != "1GiB" {
					t.Errorf("run %q lacks the low-memory limits", call)
				}
			}
			if ran < 1 {
				t.Errorf("only %d magick runs", ran)
			}
		})
	}
}

func TestProcessorVideoRunsCarryTheResources(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	src := sdrClip(t, requireFFmpeg(t), "160x120", 10, 4)
	p := requireProcessorWith(t, Config{Resources: Resources{Memory: 512 << 20, Map: 1 << 30}})
	rec := record(p)
	if _, _, err := p.encodeEven(context.Background(), src, Encoding{}, 4, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, call := range rec.calls {
		if call[0] != "magick" {
			continue
		}
		ran++
		if flagValue(t, call, "memory") != "512MiB" || flagValue(t, call, "map") != "1GiB" {
			t.Errorf("run %q lacks the low-memory limits", call)
		}
	}
	if ran == 0 {
		t.Error("no magick run")
	}
}
