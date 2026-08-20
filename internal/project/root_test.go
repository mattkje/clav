package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindRootWalksUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, "Projects", "sorta")
	deep := filepath.Join(root, "internal", "api")
	must(t, os.MkdirAll(deep, 0o755))
	must(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))

	got, marker, ok := FindRoot(deep)
	if !ok {
		t.Fatal("FindRoot found nothing")
	}
	if want := canon(t, root); got != want {
		t.Errorf("root = %q, want %q", got, want)
	}
	if marker != ".git" {
		t.Errorf("marker = %q, want .git", marker)
	}
}

func TestFindRootPrefersNearest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	outer := filepath.Join(home, "monorepo")
	inner := filepath.Join(outer, "packages", "widget")
	must(t, os.MkdirAll(inner, 0o755))
	must(t, os.MkdirAll(filepath.Join(outer, ".git"), 0o755))
	must(t, os.WriteFile(filepath.Join(inner, "package.json"), []byte("{}"), 0o644))

	got, marker, ok := FindRoot(inner)
	if !ok {
		t.Fatal("FindRoot found nothing")
	}
	if want := canon(t, inner); got != want {
		t.Errorf("root = %q, want the nearest marker at %q", got, want)
	}
	if marker != "package.json" {
		t.Errorf("marker = %q, want package.json", marker)
	}
}

func TestFindRootStopsAtHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A dotfiles repository in the home directory must not turn the whole home
	// directory into a project root.
	must(t, os.MkdirAll(filepath.Join(home, ".git"), 0o755))
	plain := filepath.Join(home, "scratch", "notes")
	must(t, os.MkdirAll(plain, 0o755))

	if root, marker, ok := FindRoot(plain); ok {
		t.Errorf("FindRoot escaped the home directory: %q via %q", root, marker)
	}
}

func TestFindRootHonoursClavRootOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	root := filepath.Join(home, "weird-project")
	sub := filepath.Join(root, "a", "b")
	must(t, os.MkdirAll(sub, 0o755))
	must(t, os.WriteFile(filepath.Join(root, ".clav-root"), nil, 0o644))

	got, marker, ok := FindRoot(sub)
	if !ok || got != canon(t, root) {
		t.Fatalf("FindRoot = %q, %q, %v", got, marker, ok)
	}
	if marker != ".clav-root" {
		t.Errorf("marker = %q, want .clav-root", marker)
	}
}

func TestFindRootReportsNothingForAPlainDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plain := filepath.Join(home, "just", "files")
	must(t, os.MkdirAll(plain, 0o755))
	if root, _, ok := FindRoot(plain); ok {
		t.Errorf("FindRoot invented a root at %q", root)
	}
}

func canon(t *testing.T, p string) string {
	t.Helper()
	c, err := Canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
