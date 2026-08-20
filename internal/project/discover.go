package project

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultDepth is how far below a swept directory clav looks for repositories.
// Deep enough for ~/Developer/work/team/project, shallow enough that a stray
// vendored checkout twelve levels down does not turn up.
const DefaultDepth = 4

// FindRepos lists the git repositories under root, nearest first. A repository
// is never descended into, so a submodule or a vendored checkout inside one is
// part of that project rather than a project of its own.
func FindRepos(root string, maxDepth int) ([]string, error) {
	if maxDepth <= 0 {
		maxDepth = DefaultDepth
	}
	start, err := Canonical(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(start)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	var repos []string
	err = filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable corner of the tree is not a reason to abandon the
			// sweep; it simply has nothing to offer.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path == start {
			return nil
		}
		rel, rerr := filepath.Rel(start, path)
		if rerr != nil {
			return fs.SkipDir
		}
		depth := len(strings.Split(filepath.ToSlash(rel), "/"))
		name := d.Name()
		switch {
		case junkSet[name]:
			return fs.SkipDir
		case name == ".git":
			// The repository is the directory holding it.
			repos = append(repos, filepath.Dir(path))
			return fs.SkipDir
		case strings.HasPrefix(name, ".") && name != ".":
			return fs.SkipDir
		case depth >= maxDepth:
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Drop anything inside a repository already found.
	sort.Strings(repos)
	out := repos[:0]
	for _, r := range repos {
		if len(out) > 0 && isAncestor(out[len(out)-1], r) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
