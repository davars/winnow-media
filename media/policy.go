package media

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// policyRule is one rule of an ImageMagick policy, in the form the tool lists
// it: rights are lower-case words joined by "|" in sorted order.
type policyRule struct {
	domain  string
	name    string // resources only
	value   string // resources only
	rights  string
	pattern string
}

func (r policyRule) String() string {
	return strings.Join([]string{r.domain, r.name, r.value, r.rights, r.pattern}, "|")
}

// coderRights is what the operations do with one ImageMagick coder.
type coderRights struct{ read, write bool }

func (c coderRights) rights() string {
	switch {
	case c.read && c.write:
		return "read|write"
	case c.write:
		return "write"
	}
	return "read"
}

// internalCoders are read and written by the operations whatever the types:
// the profile and intermediate formats they pass between tools and keep in a
// Source's scratch.
var internalCoders = []string{"icc", "pam"}

// policyCoders returns the coders a Processor of the given types reads and
// writes, keyed by upper-case coder name: the coders of the types, the
// internal ones, the output formats and the info: and null: sinks.
func policyCoders(types []string) map[string]coderRights {
	coders := map[string]coderRights{}
	add := func(name string, read, write bool) {
		name = strings.ToUpper(name)
		c := coders[name]
		c.read, c.write = c.read || read, c.write || write
		coders[name] = c
	}
	for _, typ := range types {
		for _, c := range typeTable[typ].coders {
			add(c, true, false)
		}
	}
	for _, n := range internalCoders {
		add(n, true, true)
	}
	for _, n := range append([]string{"info", "null"}, outputCoders...) {
		add(n, false, true)
	}
	return coders
}

// processorPolicy returns the rules of the policy a Processor with the
// resolved resources r and the resolved types installs, in the order they are
// written: resource ceilings, delegates denied, every coder denied and then
// the used ones allowed, indirect reads denied.
func processorPolicy(r Resources, types []string) []policyRule {
	rules := []policyRule{
		{domain: "resource", name: "memory", value: magickSize(r.Memory)},
		{domain: "resource", name: "map", value: magickSize(r.Map)},
		{domain: "resource", name: "disk", value: magickSize(r.Disk)},
		{domain: "resource", name: "area", value: policyArea},
		{domain: "resource", name: "width", value: policyDimension},
		{domain: "resource", name: "height", value: policyDimension},
		{domain: "resource", name: "list-length", value: policyListLength},
		{domain: "resource", name: "time", value: r.seconds()},
		{domain: "resource", name: "thread", value: strconv.Itoa(r.Threads)},
		{domain: "delegate", rights: "none", pattern: "*"},
		{domain: "coder", rights: "none", pattern: "*"},
	}
	coders := policyCoders(types)
	names := make([]string, 0, len(coders))
	for n := range coders {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		rules = append(rules, policyRule{domain: "coder", rights: coders[n].rights(), pattern: n})
	}
	return append(rules, policyRule{domain: "path", rights: "none", pattern: "@*"})
}

// policyXML renders rules as a policy.xml.
func policyXML(rules []policyRule) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<policymap>\n")
	attr := func(name, value string) {
		b.WriteString(" " + name + `="`)
		_ = xml.EscapeText(&b, []byte(value))
		b.WriteString(`"`)
	}
	for _, r := range rules {
		b.WriteString("  <policy")
		attr("domain", r.domain)
		if r.name != "" {
			attr("name", r.name)
			attr("value", r.value)
		} else {
			attr("rights", r.rights)
			attr("pattern", r.pattern)
		}
		b.WriteString("/>\n")
	}
	b.WriteString("</policymap>\n")
	return b.Bytes()
}

// policyFile is one file's contribution to the effective policy, as
// "magick -list policy" prints it.
type policyFile struct {
	path  string
	rules []policyRule
}

// parsePolicyList parses the output of "magick -list policy".
func parsePolicyList(out []byte) ([]policyFile, error) {
	var files []policyFile
	for line := range strings.SplitSeq(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(line, "Path:"):
			files = append(files, policyFile{path: strings.TrimSpace(strings.TrimPrefix(line, "Path:"))})
		case len(files) == 0:
			return nil, fmt.Errorf("policy listing line %q before any path", trimmed)
		case strings.HasPrefix(trimmed, "Policy:"):
			f := &files[len(files)-1]
			f.rules = append(f.rules, policyRule{domain: strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "Policy:")))})
		default:
			f := &files[len(files)-1]
			if len(f.rules) == 0 {
				return nil, fmt.Errorf("policy listing line %q before any policy", trimmed)
			}
			key, value, ok := strings.Cut(trimmed, ":")
			if !ok {
				return nil, fmt.Errorf("policy listing line %q is not a field", trimmed)
			}
			value = strings.TrimSpace(value)
			r := &f.rules[len(f.rules)-1]
			switch key {
			case "name":
				r.name = value
			case "value":
				r.value = value
			case "rights":
				words := strings.Fields(strings.ToLower(value))
				slices.Sort(words)
				r.rights = strings.Join(words, "|")
			case "pattern":
				r.pattern = value
			}
		}
	}
	return files, nil
}

// checkEffectivePolicy requires the first listed file to be ours and to hold
// exactly want, and every other file to contribute no rule.
func checkEffectivePolicy(files []policyFile, ourPath string, want []policyRule) error {
	if len(files) == 0 {
		return fmt.Errorf("no policy listed")
	}
	if filepath.Clean(files[0].path) != filepath.Clean(ourPath) {
		return fmt.Errorf("first policy file is %q, want %q", files[0].path, ourPath)
	}
	render := func(rules []policyRule) []string {
		out := make([]string, len(rules))
		for i, r := range rules {
			out[i] = r.String()
		}
		slices.Sort(out)
		return out
	}
	if got, want := render(files[0].rules), render(want); !slices.Equal(got, want) {
		return fmt.Errorf("effective policy %q differs from the installed one %q", got, want)
	}
	for _, f := range files[1:] {
		for _, r := range f.rules {
			if r.domain != "undefined" {
				return fmt.Errorf("policy file %q contributes a %s rule", f.path, r.domain)
			}
		}
	}
	return nil
}

// parseResourceList parses the output of "magick -list resource" into the
// raw value of each limit, keyed by lower-case name.
func parseResourceList(out []byte) map[string]string {
	limits := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, "Resource limits") {
			continue
		}
		limits[strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))] = strings.TrimSpace(value)
	}
	return limits
}

var resourceUnits = map[string]float64{
	"": 1, "B": 1, "P": 1,
	"KP": 1e3, "MP": 1e6, "GP": 1e9, "TP": 1e12,
	"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
	"PB": 1e15, "EB": 1e18,
	"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "PiB": 1 << 50, "EiB": 1 << 60,
	"second": 1, "seconds": 1, "minute": 60, "minutes": 60, "hour": 3600, "hours": 3600,
	"day": 86400, "days": 86400, "week": 7 * 86400, "weeks": 7 * 86400, "year": 365 * 86400, "years": 365 * 86400,
}

// resourceNumber converts a limit such as "1GiB", "512MP" or "5 minutes" to
// a number, so that spellings of one limit compare equal.
func resourceNumber(s string) (float64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("resource value %q: %w", s, err)
	}
	unit, ok := resourceUnits[strings.TrimSpace(s[i:])]
	if !ok {
		return 0, fmt.Errorf("resource value %q has unknown unit", s)
	}
	return n * unit, nil
}

// listedPrecision is the relative rounding of a listed limit: ImageMagick
// prints six significant digits, so 3000 MiB reads back as 2.92969GiB, which
// is above the 2.9296875GiB written.
const listedPrecision = 5e-6

// checkEffectiveResources requires each resource rule in want to read back
// at or below the quantity written, to the precision the listing has.
// ImageMagick lowers some limits to fit the host (threads to its CPUs), and a
// lower limit is still within the ceiling.
func checkEffectiveResources(limits map[string]string, want []policyRule) error {
	for _, r := range want {
		if r.domain != "resource" {
			continue
		}
		got, ok := limits[r.name]
		if !ok {
			return fmt.Errorf("resource %s is not listed", r.name)
		}
		g, err := resourceNumber(got)
		if err != nil {
			return err
		}
		w, err := resourceNumber(r.value)
		if err != nil {
			return err
		}
		if g > w*(1+listedPrecision) {
			return fmt.Errorf("resource %s is %q, above the ceiling %q", r.name, got, r.value)
		}
	}
	return nil
}

// verifyPolicy reads the policy ImageMagick will apply back from the tool and
// refuses unless the rules are exactly the installed ones and no resource
// limit is above its ceiling.
func (p *Processor) verifyPolicy(ctx context.Context, want []policyRule) error {
	res, err := p.run(ctx, nil, "magick", "-list", "policy")
	if err != nil {
		return fmt.Errorf("listing policy: %w", err)
	}
	files, err := parsePolicyList(res.Stdout)
	if err != nil {
		return err
	}
	if err := checkEffectivePolicy(files, filepath.Join(p.dir, "policy.xml"), want); err != nil {
		return err
	}
	res, err = p.run(ctx, nil, "magick", "-list", "resource")
	if err != nil {
		return fmt.Errorf("listing resources: %w", err)
	}
	return checkEffectiveResources(parseResourceList(res.Stdout), want)
}
