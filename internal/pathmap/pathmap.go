// Package pathmap rewrites ffmpeg argument strings, replacing container-side
// path prefixes with the corresponding host-side paths.
//
// Only arguments that begin with a known container prefix are rewritten.
// All other arguments are returned unchanged, so flags, codec names, and
// network URLs (rtsp://, pipe:, etc.) are never touched.
package pathmap

import "strings"

// Mapping is a single container→host path prefix pair.
type Mapping struct {
	Container string `yaml:"container"`
	Host      string `yaml:"host"`
}

// Mapper rewrites paths according to a list of prefix mappings.
type Mapper struct {
	mappings []Mapping
}

// New creates a Mapper from the provided list of mappings.
// Mappings are applied in order; the first matching prefix wins.
func New(mappings []Mapping) *Mapper {
	return &Mapper{mappings: mappings}
}

// Rewrite returns the argument with any matching container prefix replaced by
// the corresponding host prefix. If no prefix matches, the argument is
// returned unchanged.
func (m *Mapper) Rewrite(arg string) string {
	for _, mapping := range m.mappings {
		if mapping.Container == "" {
			continue
		}
		// Require the argument to start with the container prefix followed by
		// '/' or to be exactly equal to the prefix (a directory itself).
		prefix := mapping.Container
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if arg == mapping.Container {
			return mapping.Host
		}
		if strings.HasPrefix(arg, prefix) {
			return mapping.Host + arg[len(mapping.Container):]
		}
	}
	return arg
}

// RewriteArgs applies Rewrite to every argument in args and returns a new slice.
func (m *Mapper) RewriteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = m.Rewrite(a)
	}
	return out
}
