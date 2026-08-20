package project

import (
	"path"
	"strings"
)

// Filter decides whether a path inside a project is left out of the archive.
//
// clav v1 archives everything: the CLI never installs a non-nil Filter. The
// interface exists so that opt-in exclusions (`--exclude node_modules`) can be
// added later without touching the archive or state layers.
type Filter interface {
	// Exclude reports whether rel (a slash-separated path relative to the
	// project root) should be skipped. Excluded directories are not descended
	// into.
	Exclude(rel string, isDir bool) bool
}

// Patterns is a simple Filter matching either a full relative path or any
// single path element, with glob support via path.Match.
type Patterns struct {
	patterns []string
}

// NewPatterns builds a Filter from user-supplied patterns. It returns nil when
// no usable pattern is given, so callers can pass the result straight through
// as a Filter meaning "exclude nothing".
func NewPatterns(patterns []string) Filter {
	cleaned := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(strings.Trim(p, "/"))
		if p == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	if len(cleaned) == 0 {
		return nil
	}
	return &Patterns{patterns: cleaned}
}

// Exclude implements Filter.
func (p *Patterns) Exclude(rel string, isDir bool) bool {
	if rel == "" || rel == "." {
		return false
	}
	elems := strings.Split(rel, "/")
	for _, pattern := range p.patterns {
		if ok, _ := path.Match(pattern, rel); ok {
			return true
		}
		if !strings.Contains(pattern, "/") {
			for _, e := range elems {
				if ok, _ := path.Match(pattern, e); ok {
					return true
				}
			}
		}
	}
	return false
}
