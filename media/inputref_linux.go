//go:build linux

package media

// inputRef is how a tool names the input a runner hands it on descriptor 3.
func inputRef(dir, path string) (string, error) { return inputFDPath, nil }
