package media

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

func TestEffectiveResources(t *testing.T) {
	const listed = `Resource limits:
  Width: 65536P
  Height: 65536P
  Area: 512MP
  List length: 2048B
  Memory: 1GiB
  Map: 2GiB
  Disk: 4GiB
  File: 786432
  Thread: 4
  Throttle: 0
  Time: 5 minutes
`
	want := processorPolicy(memoryProfiles[HighMemory])
	limits := parseResourceList([]byte(listed))
	if err := checkEffectiveResources(limits, want); err != nil {
		t.Errorf("matching limits refused: %v", err)
	}
	for _, lower := range []struct{ from, to string }{
		{"Thread: 4", "Thread: 3"},
		{"Memory: 1GiB", "Memory: 512MiB"},
	} {
		got := parseResourceList([]byte(strings.Replace(listed, lower.from, lower.to, 1)))
		if err := checkEffectiveResources(got, want); err != nil {
			t.Errorf("limits with %q refused: %v", lower.to, err)
		}
	}
	for _, change := range []struct{ from, to string }{
		{"Memory: 1GiB", "Memory: 3GiB"},
		{"Time: 5 minutes", "Time: 6 minutes"},
		{"Width: 65536P", "Width: 65537P"},
		{"  Thread: 4\n", ""},
	} {
		got := parseResourceList([]byte(strings.Replace(listed, change.from, change.to, 1)))
		if err := checkEffectiveResources(got, want); err == nil {
			t.Errorf("limits with %q accepted", change.to)
		}
	}
}

func TestResourceNumber(t *testing.T) {
	cases := map[string]float64{
		"1GiB": 1 << 30, "512MP": 512e6, "65536P": 65536, "2048B": 2048,
		"5 minutes": 300, "300": 300, "4": 4,
	}
	for in, want := range cases {
		if got, err := resourceNumber(in); err != nil || got != want {
			t.Errorf("resourceNumber(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := resourceNumber("12 parsecs"); err == nil {
		t.Error("unknown unit accepted")
	}
}

func TestProcessorPolicyContents(t *testing.T) {
	var coders []string
	for _, r := range processorPolicy(memoryProfiles[HighMemory]) {
		if r.domain == "coder" && r.pattern != "*" {
			coders = append(coders, r.pattern+"="+r.rights)
		}
	}
	want := []string{
		"GIF=read", "HEIC=read", "ICC=read", "INFO=write", "JPEG=write", "JPG=write",
		"PAM=read", "PNG=write", "WEBP=read|write",
	}
	if !slices.Equal(coders, want) {
		t.Errorf("coder rules %v, want %v", coders, want)
	}
	if got, again := policyXML(processorPolicy(memoryProfiles[HighMemory])), policyXML(processorPolicy(memoryProfiles[HighMemory])); string(got) != string(again) {
		t.Error("policy XML is not deterministic")
	}
}

func TestNewProcessorInstallsAndCloseRemoves(t *testing.T) {
	p := requireProcessor(t)
	data, err := os.ReadFile(filepath.Join(p.dir, "policy.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(policyXML(processorPolicy(memoryProfiles[HighMemory]))) {
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
	rules := processorPolicy(memoryProfiles[HighMemory])
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
