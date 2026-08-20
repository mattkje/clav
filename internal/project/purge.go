package project

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Junk names directories that a build or a package manager will recreate on
// demand. clav only ever deletes one of these when git already ignores it, so
// a directory that is genuinely part of a project is never matched.
var Junk = []string{
	"node_modules", "bower_components", "Pods", ".pnpm-store", ".yarn",
	".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache",
	".ruff_cache", ".tox", ".ipynb_checkpoints",
	"target", "build", "dist", "out", "obj", "bin", "vendor",
	".next", ".nuxt", ".svelte-kit", ".astro", ".docusaurus",
	".turbo", ".parcel-cache", ".cache", ".gradle", ".dart_tool",
	".terraform", ".serverless", "coverage", ".nyc_output",
	"DerivedData", ".stack-work", "_build", "deps", "elm-stuff",
}

var junkSet = func() map[string]bool {
	m := make(map[string]bool, len(Junk))
	for _, n := range Junk {
		m[n] = true
	}
	return m
}()

// IsJunk reports whether an ignored path is a regenerable build or dependency
// directory. rel is slash-separated and relative to the project root; a
// trailing slash marks a directory, as `git ls-files --directory` reports it.
func IsJunk(rel string) bool {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return false
	}
	for _, elem := range strings.Split(rel, "/") {
		if junkSet[elem] {
			return true
		}
	}
	return false
}

// Purge is what a purge removed.
type Purge struct {
	Files int
	Dirs  int
	Bytes int64
}

// Measure reports what Remove would delete, without deleting anything. It is
// what --dry-run and sweep are built on, so the number a user is shown is
// produced by the same path resolution that does the deleting.
func Measure(root string, rels []string) (Purge, error) {
	var p Purge
	for _, rel := range rels {
		target, err := under(root, rel)
		if err != nil {
			return p, err
		}
		info, err := os.Lstat(target)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return p, err
		}
		if info.IsDir() {
			size, files, merr := measure(target)
			if merr != nil {
				return p, merr
			}
			p.Bytes += size
			p.Files += files
			p.Dirs++
			continue
		}
		if info.Mode().IsRegular() {
			p.Bytes += info.Size()
		}
		p.Files++
	}
	return p, nil
}

// Remove deletes the given paths (relative to root, slash-separated) and then
// removes any directory they leave empty. Paths outside root are refused
// rather than followed: the list comes from git, but clav is deleting with it.
func Remove(root string, rels []string) (Purge, error) {
	var p Purge
	sort.Strings(rels)
	for _, rel := range rels {
		target, err := under(root, rel)
		if err != nil {
			return p, err
		}
		info, err := os.Lstat(target)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return p, err
		}
		if info.IsDir() {
			size, files, err := measure(target)
			if err != nil {
				return p, err
			}
			if err := os.RemoveAll(target); err != nil {
				return p, err
			}
			p.Bytes += size
			p.Files += files
			p.Dirs++
			continue
		}
		if info.Mode().IsRegular() {
			p.Bytes += info.Size()
		}
		if err := os.Remove(target); err != nil {
			return p, err
		}
		p.Files++
	}
	dirs, err := PruneEmptyDirs(root)
	p.Dirs += dirs
	return p, err
}

// PruneEmptyDirs removes directories left empty inside root. root itself is
// never removed.
func PruneEmptyDirs(root string) (int, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	// Deepest first, so a directory holding only empty directories also goes.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	removed := 0
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			continue
		}
		if os.Remove(dir) == nil {
			removed++
		}
	}
	return removed, nil
}

// Entries counts what is left inside a directory, recursively.
func Entries(root string) (files int, bytes int64, err error) {
	bytes, files, err = measure(root)
	return files, bytes, err
}

// IsEmpty reports whether a directory has no entries at all.
func IsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

func measure(root string) (bytes int64, files int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	})
	return bytes, files, err
}

// under joins rel onto root and refuses anything that escapes it.
func under(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSuffix(rel, "/")))
	if clean == "." || clean == string(os.PathSeparator) {
		return "", fmt.Errorf("refusing to delete the project root via %q", rel)
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing to delete %q: it points outside the project", rel)
	}
	return filepath.Join(root, clean), nil
}
