package project

import "testing"

func TestPatternsMatchElementsAndPaths(t *testing.T) {
	f := NewPatterns([]string{"node_modules", "*.log", "build/cache"})
	if f == nil {
		t.Fatal("expected a filter")
	}
	yes := []string{"node_modules", "node_modules/pkg/index.js", "a/b/node_modules",
		"debug.log", "logs/debug.log", "build/cache"}
	for _, p := range yes {
		if !f.Exclude(p, false) {
			t.Errorf("Exclude(%q) = false, want true", p)
		}
	}
	no := []string{"src/main.go", ".env", "build", "build/other", "node_modules.txt"}
	for _, p := range no {
		if f.Exclude(p, false) {
			t.Errorf("Exclude(%q) = true, want false", p)
		}
	}
}

func TestNewPatternsEmptyMeansNoFilter(t *testing.T) {
	if f := NewPatterns(nil); f != nil {
		t.Error("nil patterns should produce no filter")
	}
	if f := NewPatterns([]string{"", "  ", "/"}); f != nil {
		t.Error("blank patterns should produce no filter")
	}
}
