package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindReposFindsProjectsAndStopsAtThem(t *testing.T) {
	base := t.TempDir()
	repos := []string{"alpha", "work/beta", "work/team/gamma"}
	for _, r := range repos {
		if err := os.MkdirAll(filepath.Join(base, r, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Things that must not be reported: a submodule inside a repository, a
	// dependency directory, a hidden directory, and a plain folder.
	for _, other := range []string{
		"alpha/vendor/dep/.git",
		"alpha/node_modules/pkg/.git",
		".cache/thing/.git",
		"notes",
	} {
		if err := os.MkdirAll(filepath.Join(base, other), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	found, err := FindRepos(base, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != len(repos) {
		t.Fatalf("found %v, want %d repositories", found, len(repos))
	}
	for _, want := range repos {
		full, _ := Canonical(filepath.Join(base, want))
		var ok bool
		for _, got := range found {
			if got == full {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s was not found in %v", want, found)
		}
	}
}

func TestFindReposHonoursDepth(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "a/b/c/d/deep/.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	found, err := FindRepos(base, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("found %v, want nothing below the depth limit", found)
	}
}

func TestFindReposOnAPathThatIsNotADirectory(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if found, err := FindRepos(file, 0); err != nil || found != nil {
		t.Errorf("FindRepos(file) = %v, %v; want nil, nil", found, err)
	}
	if _, err := FindRepos(filepath.Join(base, "missing"), 0); err == nil {
		t.Error("FindRepos on a missing path should fail")
	}
}
