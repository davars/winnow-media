package media_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The README's Go example is a copy of the package Example's body, so that
// the gate compiles what the README shows.
func TestREADMEExample(t *testing.T) {
	const fence = "```go\n"
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(readme), fence)
	if !ok {
		t.Fatal("README.md has no Go code block")
	}
	shown, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatal("README.md's Go code block is not closed")
	}

	const file = "example_test.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "Example" {
			inner := string(src[fset.Position(fn.Body.Lbrace).Offset+1 : fset.Position(fn.Body.Rbrace).Offset])
			var lines []string
			for line := range strings.SplitSeq(strings.Trim(inner, "\n"), "\n") {
				lines = append(lines, strings.TrimPrefix(line, "\t"))
			}
			body = strings.Join(lines, "\n") + "\n"
		}
	}
	if body == "" {
		t.Fatalf("%s has no package Example", file)
	}
	if shown != body {
		t.Errorf("README.md's Go code block is not the body of Example in %s.\nREADME:\n%s\nExample:\n%s", file, shown, body)
	}
}
