package media

import (
	"context"
	"errors"
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

func TestHighMemoryIsTheZeroValueAndTheDefaultFlags(t *testing.T) {
	t.Parallel()
	if HighMemory != MemoryProfile(0) {
		t.Fatal("HighMemory is not the zero value")
	}
	want := []string{
		"-limit", "memory", "1GiB",
		"-limit", "map", "2GiB",
		"-limit", "area", "512MP",
		"-limit", "list-length", "2048",
		"-limit", "time", "300",
	}
	if got := LimitArgs(); !slices.Equal(got, want) {
		t.Errorf("LimitArgs() = %q, want %q", got, want)
	}
	if got := HighMemory.LimitArgs(); !slices.Equal(got, want) {
		t.Errorf("HighMemory.LimitArgs() = %q, want %q", got, want)
	}
}

func TestLowMemoryFlagsAndPolicyAgree(t *testing.T) {
	t.Parallel()
	flags := LowMemory.LimitArgs()
	r, err := LowMemory.resources()
	if err != nil {
		t.Fatal(err)
	}
	rules := processorPolicy(r)
	for name, want := range map[string]string{"memory": "512MiB", "map": "1GiB"} {
		if got := flagValue(t, flags, name); got != want {
			t.Errorf("flag %s = %s, want %s", name, got, want)
		}
		if got := policyValue(t, rules, name); got != want {
			t.Errorf("policy %s = %s, want %s", name, got, want)
		}
	}
	// Everything else is the same under both profiles.
	high := LimitArgs()
	for _, name := range []string{"area", "list-length", "time"} {
		if flagValue(t, flags, name) != flagValue(t, high, name) {
			t.Errorf("flag %s differs between profiles", name)
		}
	}
}

// assertLimited requires the magick argv that ran to carry exactly the
// profile's limit flags, once, ahead of the rest and after a leading
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

// Every route that runs magick passes through Env.runMagick, whatever its
// own arguments.
func TestMagickRoutesCarryTheProfile(t *testing.T) {
	t.Parallel()
	frame := encodeFrame{input: "pam:/in"}
	enc := webpEncoding(100)
	routes := map[string]func(Env) error{
		"IdentifyArtifact": func(e Env) error {
			_, _, _, err := e.IdentifyArtifact(context.Background(), "/a.webp")
			return err
		},
		"identifyHeaders": func(e Env) error {
			_, err := identifyHeaders(context.Background(), e, "png:/a", true)
			return err
		},
		"identifyHeaders full": func(e Env) error {
			_, err := identifyHeaders(context.Background(), e, "png:/a", false)
			return err
		},
		"RunMagickWithCoderFallback": func(e Env) error {
			_, err := e.RunMagickWithCoderFallback(context.Background(), "image/png",
				func(string) []string { return []string{"a.png", "-resize", "1x1", "info:"} })
			return err
		},
		"RunMagickWithCoderFallback identify": func(e Env) error {
			_, err := e.RunMagickWithCoderFallback(context.Background(), "image/png",
				func(string) []string { return []string{"identify", "-ping", "a.png"} })
			return err
		},
		"RunMagickWithCoderFallback retry": func(e Env) error {
			_, err := e.RunMagickWithCoderFallback(context.Background(), "image/x-tga",
				func(coder string) []string { return []string{coder + ":a.tga", "info:"} })
			return err
		},
		"runEncode": func(e Env) error {
			_, err := e.runEncode(context.Background(),
				encodeArgs([]encodeFrame{frame}, encodeSource{}, enc, encodeOutput{kind: outputStill, path: "/o.webp"}), encodeCheck{})
			return err
		},
		"runEncode fused": func(e Env) error {
			_, err := e.runEncode(context.Background(),
				fusedArgs(nil, encodeSource{}, []fusedTarget{{frame: frame, enc: enc, path: "/o.webp"}}), encodeCheck{})
			return err
		},
	}
	for name, route := range routes {
		for _, profile := range []MemoryProfile{HighMemory, LowMemory} {
			t.Run(name+"/"+profile.String(), func(t *testing.T) {
				var ran [][]string
				retry := strings.HasSuffix(name, "retry")
				env := Env{Memory: profile, Run: func(_ context.Context, tool string, args ...string) (Result, error) {
					if tool != "magick" {
						t.Errorf("ran %s", tool)
					}
					ran = append(ran, args)
					if retry && len(ran) == 1 {
						return Result{}, errors.New("first attempt fails")
					}
					return Result{Stdout: []byte("1 8 8\n")}, nil
				}}
				_ = route(env) // the fake output need not parse
				if len(ran) == 0 {
					t.Fatal("no magick run")
				}
				if retry && len(ran) != 2 {
					t.Fatalf("%d runs, want the attempt and its retry", len(ran))
				}
				for _, argv := range ran {
					if strings.Contains(strings.ToLower(name), "identify") && argv[0] != "identify" {
						t.Errorf("argv %q does not start with the identify subcommand", argv)
					}
					assertLimited(t, argv, profile.LimitArgs())
				}
			})
		}
	}
}

func TestUnknownMemoryProfileIsRefused(t *testing.T) {
	t.Parallel()
	bad := MemoryProfile(99)
	if bad.Valid() || !HighMemory.Valid() || !LowMemory.Valid() {
		t.Error("Valid is wrong")
	}
	if _, err := bad.resources(); err == nil {
		t.Error("resources accepted an unknown profile")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("LimitArgs did not panic on an unknown profile")
			}
		}()
		bad.LimitArgs()
	}()
	env := Env{Memory: bad, Run: func(context.Context, string, ...string) (Result, error) {
		t.Error("tool ran under an unknown profile")
		return Result{}, nil
	}}
	if _, _, _, err := env.IdentifyArtifact(context.Background(), "/a.webp"); err == nil {
		t.Error("IdentifyArtifact accepted an unknown profile")
	}
	if _, err := identifyHeaders(context.Background(), env, "png:/a", true); err == nil {
		t.Error("identifyHeaders accepted an unknown profile")
	}
	if _, err := env.runEncode(context.Background(), nil, encodeCheck{}); err == nil {
		t.Error("runEncode accepted an unknown profile")
	}
}

func TestProcessorInstallsTheResources(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		profile       MemoryProfile
		memory, mapLm string
	}{{HighMemory, "1GiB", "2GiB"}, {LowMemory, "512MiB", "1GiB"}} {
		t.Run(c.profile.String(), func(t *testing.T) {
			r, err := c.profile.resources()
			if err != nil {
				t.Fatal(err)
			}
			p := requireProcessorWith(t, Config{Resources: r})
			data, err := os.ReadFile(filepath.Join(p.dir, "policy.xml"))
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"memory": c.memory, "map": c.mapLm} {
				if !strings.Contains(string(data), `name="`+name+`" value="`+want+`"`) {
					t.Errorf("policy.xml lacks %s=%s:\n%s", name, want, data)
				}
			}
			res, err := p.run(context.Background(), "magick", "-list", "resource")
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
			env := p.env(&m)
			inner := env.Run
			env.Run = func(ctx context.Context, name string, args ...string) (Result, error) {
				ran = args
				return inner(ctx, name, args...)
			}
			if _, _, _, err := env.IdentifyArtifact(context.Background(), filepath.Join(t.TempDir(), "none.webp")); err == nil {
				t.Error("identify of a missing file succeeded")
			}
			assertLimited(t, ran, c.profile.LimitArgs())
		})
	}
}

// Every magick run of a Source, whichever encode path it takes, carries the
// Env's profile.
func TestSourceRunsCarryTheProfile(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	env, calls := sourceEnv(t)
	env.Memory = LowMemory
	dir := t.TempDir()
	png := filepath.Join(dir, "x.png")
	magick(t, env, "-size", "16x16", "xc:red", png)
	gif := filepath.Join(dir, "x.gif")
	if err := os.WriteFile(gif, oneColourGIF(t, 8, 8, 3), 0o600); err != nil {
		t.Fatal(err)
	}
	enc := Encoding{Format: WebP, Quality: 80, MaxEdge: 16, MaxArea: 256}
	other := Encoding{Format: WebP, Quality: 80, MaxEdge: 32, MaxArea: 1024}
	for name, c := range map[string]struct {
		path    string
		targets []Encoding
	}{
		"still, two targets (fused)": {png, []Encoding{enc, other}},
		"animation":                  {gif, []Encoding{enc}},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(calls())
			src := openSource(t, env, c.path, Hints{})
			if _, err := src.Encode(context.Background(), 0, c.targets...); err != nil {
				t.Fatal(err)
			}
			ran := 0
			for _, call := range calls()[before:] {
				if call[0] != "magick" {
					continue
				}
				ran++
				if flagValue(t, call, "memory") != "512MiB" || flagValue(t, call, "map") != "1GiB" {
					t.Errorf("run %q lacks the LowMemory limits", call)
				}
			}
			if ran < 2 {
				t.Errorf("only %d magick runs", ran)
			}
		})
	}
}

func TestProcessorVideoRunsCarryTheProfile(t *testing.T) {
	t.Parallel()
	requireWebPBuilders(t)
	src := sdrClip(t, requireFFmpeg(t), "160x120", 10, 4)
	p := requireProcessorWith(t, Config{Resources: Resources{Memory: 512 << 20, Map: 1 << 30}})
	rec := record(p)
	if _, _, err := p.VideoFrames(context.Background(), src, Encoding{}, 4, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, call := range rec.calls {
		if call[0] != "magick" {
			continue
		}
		ran++
		if flagValue(t, call, "memory") != "512MiB" || flagValue(t, call, "map") != "1GiB" {
			t.Errorf("run %q lacks the LowMemory limits", call)
		}
	}
	if ran == 0 {
		t.Error("no magick run")
	}
}
