package media

import (
	"context"
	"fmt"
	"slices"
	"strconv"
)

// Recipe declares one registered transformation: what it accepts, what it
// takes as parameters, what it produces and which tools it runs. It is plain
// data; the implementation is registered in the library under Name.
type Recipe struct {
	Name string
	// Revision changes whenever the recipe's output for the same input and
	// parameters can change.
	Revision int
	Inputs   []Input
	Params   []Param
	Outputs  Output
	// Engines are the tool names the recipe runs ("magick", "ffmpeg").
	Engines []string
}

// Input is one media type a recipe accepts and the decoder it is read with.
type Input struct {
	MediaType string
	Decoder   Decoder
}

// Decoder pins how an input is read, so that a type never depends on the
// tool sniffing the bytes.
type Decoder struct {
	Tool string // "magick" or "ffmpeg"
	// Name is the ImageMagick coder or the ffmpeg demuxer.
	Name string
}

// ParamKind is the type of a recipe parameter.
type ParamKind string

// The parameter kinds.
const (
	// KindInt is a decimal integer within [Min, Max].
	KindInt ParamKind = "int"
	// KindEnum is one of Values.
	KindEnum ParamKind = "enum"
)

// Param declares one recipe parameter. A request supplies it as a string.
type Param struct {
	Name     string
	Kind     ParamKind
	Min, Max int      // inclusive bounds of a KindInt parameter
	Values   []string // allowed values of a KindEnum parameter
	// Default is used when a request omits the parameter; it is always valid.
	Default string
}

// Field names a per-file metadata field of OutputFile.
type Field string

// The metadata fields a recipe may fill.
const (
	FieldSourceFrame  Field = "source_frame"
	FieldSourceTimeMS Field = "source_time_ms"
	FieldWidth        Field = "width"
	FieldHeight       Field = "height"
)

// Output declares what a recipe writes.
type Output struct {
	// MediaTypes are the types its output files can have.
	MediaTypes []string
	// Fields are the OutputFile metadata fields the recipe fills; the others
	// are zero.
	Fields []Field
}

// recipeImpl runs one recipe after Execute has validated the request.
type recipeImpl func(ctx context.Context, env Env, run runInput) ([]OutputFile, error)

// runInput is a validated request.
type runInput struct {
	inputPath string
	decoder   Decoder
	outputDir string
	params    paramValues
}

type registered struct {
	recipe func() Recipe
	run    recipeImpl
}

// registry holds every recipe the library implements, keyed by name.
var registry = map[string]registered{
	stillName:  {recipe: stillRecipe, run: runStill},
	framesName: {recipe: framesRecipe, run: runFrames},
}

// Recipes returns the declarations of every recipe the library implements,
// ordered by name. The result is a fresh copy.
func Recipes() []Recipe {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]Recipe, len(names))
	for i, name := range names {
		out[i] = registry[name].recipe()
	}
	return out
}

// paramValues are validated parameter values, defaults filled in.
type paramValues map[string]string

func (p paramValues) int(name string) int {
	n, err := strconv.Atoi(p[name])
	if err != nil {
		panic("media: parameter " + name + " was not validated")
	}
	return n
}

func (p paramValues) str(name string) string { return p[name] }

// validateParams checks given against decls and returns every parameter's
// value, with defaults for the omitted ones.
func validateParams(decls []Param, given map[string]string) (paramValues, error) {
	known := make(map[string]Param, len(decls))
	for _, d := range decls {
		known[d.Name] = d
	}
	for name := range given {
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("unknown parameter %q", name)
		}
	}
	out := make(paramValues, len(decls))
	for _, d := range decls {
		value, ok := given[d.Name]
		if !ok {
			out[d.Name] = d.Default
			continue
		}
		switch d.Kind {
		case KindInt:
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("parameter %q: %q is not an integer", d.Name, value)
			}
			if n < d.Min || n > d.Max {
				return nil, fmt.Errorf("parameter %q: %d is outside %d..%d", d.Name, n, d.Min, d.Max)
			}
		case KindEnum:
			if !slices.Contains(d.Values, value) {
				return nil, fmt.Errorf("parameter %q: %q is not one of %v", d.Name, value, d.Values)
			}
		default:
			return nil, fmt.Errorf("parameter %q has unknown kind %q", d.Name, d.Kind)
		}
		out[d.Name] = value
	}
	return out, nil
}
