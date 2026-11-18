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

// resourceRules are the resource ceilings of every policy the library
// installs.
func resourceRules(r Resources) []policyRule {
	return []policyRule{
		{domain: "resource", name: "memory", value: magickSize(r.Memory)},
		{domain: "resource", name: "map", value: magickSize(r.Map)},
		{domain: "resource", name: "disk", value: magickSize(r.Disk)},
		{domain: "resource", name: "area", value: policyArea},
		{domain: "resource", name: "width", value: policyDimension},
		{domain: "resource", name: "height", value: policyDimension},
		{domain: "resource", name: "list-length", value: policyListLength},
		{domain: "resource", name: "time", value: r.seconds()},
		{domain: "resource", name: "thread", value: strconv.Itoa(r.Threads)},
	}
}

// svgPolicy returns the rules of the policy SVG renders run under: the
// resource ceilings, no delegate, and one coder to read, librsvg's, with the
// PNG, info and null sinks to write. ImageMagick's own SVG and MSVG coders can
// run other programs or read other files, so none of them is allowed.
func svgPolicy(r Resources) []policyRule {
	rules := append(
		resourceRules(r),
		policyRule{domain: "delegate", rights: "none", pattern: "*"},
		policyRule{domain: "coder", rights: "none", pattern: "*"},
		policyRule{domain: "coder", rights: "read", pattern: "RSVG"},
	)
	for _, sink := range []string{"INFO", "NULL", "PNG"} {
		rules = append(rules, policyRule{domain: "coder", rights: "write", pattern: sink})
	}
	return append(rules, policyRule{domain: "path", rights: "none", pattern: "@*"})
}

// processorPolicy returns the rules of the policy a Processor with the
// resolved resources r and the resolved types installs, in the order they are
// written: resource ceilings, delegates denied, every coder denied and then
// the used ones allowed, indirect reads denied.
func processorPolicy(r Resources, types []string) []policyRule {
	rules := append(
		resourceRules(r),
		policyRule{domain: "delegate", rights: "none", pattern: "*"},
		policyRule{domain: "coder", rights: "none", pattern: "*"},
	)
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

// verifyPolicy reads the policy ImageMagick will apply back from the tool and
// refuses unless the rules are exactly the installed ones.
func (p *Processor) verifyPolicy(ctx context.Context, want []policyRule) error {
	res, err := p.run(ctx, nil, "magick", "-list", "policy")
	if err != nil {
		return fmt.Errorf("listing policy: %w", err)
	}
	files, err := parsePolicyList(res.Stdout)
	if err != nil {
		return err
	}
	return checkEffectivePolicy(files, filepath.Join(p.dir, "policy.xml"), want)
}
