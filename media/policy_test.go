package media

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// listPolicyOutput is "magick -list policy" as ImageMagick 7.1.2 prints it
// for a policy directory holding ours, plus the package's empty policy.
const listPolicyOutput = `Path: /run/proc/policy.xml
  Policy: Resource
    name: memory
    value: 1GiB
  Policy: Delegate
    rights: None 
    pattern: *
  Policy: Coder
    rights: None 
    pattern: *
  Policy: Coder
    rights: Read Write 
    pattern: PNG
  Policy: Path
    rights: None 
    pattern: @*

Path: /store/etc/ImageMagick-7/policy.xml
  Policy: Undefined
    rights: None 

Path: [built-in]
  Policy: Undefined
    rights: None 
`

var testRules = []policyRule{
	{domain: "resource", name: "memory", value: "1GiB"},
	{domain: "delegate", rights: "none", pattern: "*"},
	{domain: "coder", rights: "none", pattern: "*"},
	{domain: "coder", rights: "read|write", pattern: "PNG"},
	{domain: "path", rights: "none", pattern: "@*"},
}

func TestEffectivePolicyAcceptsOnlyOurs(t *testing.T) {
	t.Parallel()
	files, err := parsePolicyList([]byte(listPolicyOutput))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("parsed %d files", len(files))
	}
	ours := "/run/proc/policy.xml"
	if err := checkEffectivePolicy(files, ours, testRules); err != nil {
		t.Errorf("matching policy refused: %v", err)
	}

	mutate := map[string]string{
		"rule widened":      strings.Replace(listPolicyOutput, "pattern: PNG", "pattern: {MVG,PNG}", 1),
		"rule removed":      strings.Replace(listPolicyOutput, "  Policy: Path\n    rights: None \n    pattern: @*\n", "", 1),
		"limit changed":     strings.Replace(listPolicyOutput, "1GiB", "2GiB", 1),
		"rights changed":    strings.Replace(listPolicyOutput, "Read Write", "Read", 1),
		"other file first":  "Path: /home/u/.config/ImageMagick/policy.xml\n  Policy: Undefined\n    rights: None \n\n" + listPolicyOutput,
		"third file a rule": listPolicyOutput + "\nPath: /home/u/.config/ImageMagick/policy.xml\n  Policy: Coder\n    rights: Read Write \n    pattern: MVG\n",
		"package file rule": strings.Replace(listPolicyOutput, "Path: /store/etc/ImageMagick-7/policy.xml\n  Policy: Undefined", "Path: /store/etc/ImageMagick-7/policy.xml\n  Policy: Delegate", 1),
	}
	for name, out := range mutate {
		t.Run(name, func(t *testing.T) {
			files, err := parsePolicyList([]byte(out))
			if err != nil {
				return
			}
			if err := checkEffectivePolicy(files, ours, testRules); err == nil {
				t.Error("differing policy accepted")
			}
		})
	}
	if _, err := parsePolicyList([]byte("    value: 1\n")); err == nil {
		t.Error("field before any path accepted")
	}
}

// The effective resource limits of the pinned ImageMagick, listed under the
// policy of ordinary runs and of SVG runs, are the ceilings the policy writes.
// ImageMagick lists sizes to six significant digits, so a value that is not a
// whole number of the unit it picks, such as 3000 MiB, lists rounded. It lowers
// the thread limit to one less than the host's CPUs, and caps memory, map and
// area at one, two and four times the host's RAM, so the cases assume a host
// with more than 3 GiB of it.
func TestEffectiveResourcesMatchThePolicy(t *testing.T) {
	t.Parallel()
	units := map[string]float64{
		"": 1, "B": 1, "P": 1, "KP": 1e3, "MP": 1e6, "GP": 1e9,
		"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "PiB": 1 << 50,
		"second": 1, "seconds": 1, "minute": 60, "minutes": 60, "hour": 3600, "hours": 3600,
		"day": 86400, "days": 86400, "week": 7 * 86400, "weeks": 7 * 86400,
		"month": 2628000, "months": 2628000, "year": 365 * 86400, "years": 365 * 86400,
	}
	number := func(t testing.TB, s string) float64 {
		s = strings.TrimSpace(s)
		if s == "unlimited" {
			return math.Inf(1)
		}
		i := strings.IndexFunc(s, func(r rune) bool { return r != '.' && (r < '0' || r > '9') })
		if i < 0 {
			i = len(s)
		}
		n, err := strconv.ParseFloat(s[:i], 64)
		unit, ok := units[strings.TrimSpace(s[i:])]
		if err != nil || !ok {
			t.Fatalf("cannot read the listed limit %q", s)
		}
		return n * unit
	}
	for name, r := range map[string]Resources{
		"default":      {},
		"low memory":   lowMemory(),
		"MiB memory":   {Memory: 3000 << 20},
		"byte map":     {Map: 3_000_000_000},
		"byte disk":    {Disk: 3_000_000_000},
		"under 2 GiB":  {Map: 2<<30 - 1},
		"aligned more": {Memory: 3 << 30},
		"a week":       {TimeLimit: 7 * 24 * time.Hour},
		"a PiB disk":   {Disk: 1 << 50},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := requireProcessorWith(t, Config{Resources: r, RenderTypes: svgConfig.RenderTypes, Unsandboxed: true})
			run, err := p.runTempDir()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(run) })
			for policy, c := range map[string]struct {
				ctx   context.Context
				rules []policyRule
			}{
				"processor": {context.Background(), processorPolicy(p.res, p.types)},
				"svg":       {withRenderRun(context.Background(), renderRun{dir: run, policyDir: p.svgDir()}), svgPolicy(p.res)},
			} {
				res, err := p.run(c.ctx, nil, "magick", "-list", "resource")
				if err != nil {
					t.Fatal(err)
				}
				listed := map[string]string{}
				for line := range strings.SplitSeq(string(res.Stdout), "\n") {
					if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, "Resource limits") {
						listed[strings.ToLower(strings.ReplaceAll(strings.TrimSpace(k), " ", "-"))] = strings.TrimSpace(v)
					}
				}
				for _, rule := range c.rules {
					if rule.domain != "resource" {
						continue
					}
					got, ok := listed[rule.name]
					if !ok {
						t.Errorf("%s: resource %s is not listed", policy, rule.name)
						continue
					}
					g, w := number(t, got), number(t, rule.value)
					if g > w*(1+5e-6) || (rule.name != "thread" && g < w*(1-5e-6)) {
						t.Errorf("%s: resource %s lists as %q, policy writes %q", policy, rule.name, got, rule.value)
					}
				}
			}
		})
	}
}

func TestProcessorPolicyContents(t *testing.T) {
	t.Parallel()
	var coders []string
	types := []string{"image/gif", "image/webp", "image/heic", "image/heif", "video/mp4"}
	for _, r := range processorPolicy(defaultResources(), types) {
		if r.domain == "coder" && r.pattern != "*" {
			coders = append(coders, r.pattern+"="+r.rights)
		}
	}
	want := []string{
		"GIF=read", "HEIC=read", "ICC=read|write", "INFO=write", "JPEG=write", "JPG=write",
		"NULL=write", "PAM=read|write", "PNG=write", "WEBP=read|write",
	}
	if !slices.Equal(coders, want) {
		t.Errorf("coder rules %v, want %v", coders, want)
	}
	if got, again := policyXML(processorPolicy(defaultResources(), Types())), policyXML(processorPolicy(defaultResources(), Types())); string(got) != string(again) {
		t.Error("policy XML is not deterministic")
	}
}

func TestNewProcessorInstallsAndCloseRemoves(t *testing.T) {
	t.Parallel()
	p := requireProcessor(t)
	data, err := os.ReadFile(filepath.Join(p.dir, "policy.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(policyXML(processorPolicy(defaultResources(), Types()))) {
		t.Error("installed policy differs from the generated one")
	}
	if info, err := os.Stat(p.tmpDir()); err != nil || !info.IsDir() {
		t.Errorf("tmp directory: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.dir); !os.IsNotExist(err) {
		t.Errorf("directory after Close: %v", err)
	}
}

func TestProcessorPolicyDeniesEverythingFirst(t *testing.T) {
	t.Parallel()
	rules := processorPolicy(defaultResources(), Types())
	index := func(domain, pattern string) int {
		for i, r := range rules {
			if r.domain == domain && r.pattern == pattern && r.rights == "none" {
				return i
			}
		}
		return -1
	}
	if index("delegate", "*") < 0 {
		t.Error("no deny-all delegate rule")
	}
	if index("path", "@*") < 0 {
		t.Error("no deny of indirect reads")
	}
	deny := index("coder", "*")
	if deny < 0 {
		t.Fatal("no deny-all coder rule")
	}
	for i, r := range rules {
		if r.domain == "coder" && r.rights != "none" && i < deny {
			t.Errorf("coder allow %s precedes the deny-all rule", r.pattern)
		}
	}
}
