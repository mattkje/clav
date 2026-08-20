// Package project deals with locating, identifying and measuring projects on
// disk. A "project" is simply a directory; clav deliberately knows nothing
// about Git or about any particular toolchain.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Ref identifies a project by its canonical location on disk.
//
// Identity is derived from the path, never from the directory name, so that
// ~/Projects/foo and ~/Other/foo are distinct projects that can coexist.
type Ref struct {
	// Path is the cleaned, absolute, symlink-resolved path of the project.
	Path string
	// Name is the final path element, used for display only.
	Name string
	// Key is a stable identifier derived from Path. It is the same for every
	// park/restore cycle of the same location.
	Key string
}

// Display renders the path with the user's home directory abbreviated to "~".
func (r Ref) Display() string { return Shorten(r.Path) }

// Shorten abbreviates a leading home directory as "~" for human output.
func Shorten(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	home = filepath.Clean(home)
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + string(os.PathSeparator) + path[len(home)+1:]
	}
	return path
}

// Expand resolves a leading "~" and makes the path absolute and clean. It does
// not require the path to exist.
func Expand(input string) (string, error) {
	if input == "" {
		return "", errors.New("empty path")
	}
	if input == "~" || strings.HasPrefix(input, "~"+string(os.PathSeparator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve %q: %w", input, err)
		}
		input = filepath.Join(home, strings.TrimPrefix(input[1:], string(os.PathSeparator)))
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", input, err)
	}
	return filepath.Clean(abs), nil
}

// Canonical resolves input to an absolute path with symlinks resolved as far as
// the path exists. The final element is never resolved through a symlink, so a
// symlinked project directory keeps its own identity (parking one is refused
// separately by Resolve).
//
// Canonical works for paths that do not exist yet, which is what makes
// `clav restore <original path>` possible after the directory has been deleted.
func Canonical(input string) (string, error) {
	abs, err := Expand(input)
	if err != nil {
		return "", err
	}
	// Walk up to the deepest existing ancestor, resolve that, then re-append
	// the remaining (non-existent) elements.
	rest := []string{}
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("cannot resolve %q: %w", input, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// KeyFor derives the stable project key for a canonical path.
func KeyFor(canonicalPath string) string {
	sum := sha256.Sum256([]byte(canonicalPath))
	return hex.EncodeToString(sum[:])[:16]
}

// Locate builds a Ref for any path, existing or not. Used by commands that
// address a parked project by its original location.
func Locate(input string) (Ref, error) {
	path, err := Canonical(input)
	if err != nil {
		return Ref{}, err
	}
	if path == string(os.PathSeparator) {
		return Ref{}, errors.New("refusing to operate on the filesystem root")
	}
	return Ref{Path: path, Name: filepath.Base(path), Key: KeyFor(path)}, nil
}

// Resolve builds a Ref for a directory that must exist and be safe to park.
func Resolve(input string, reservedDirs ...string) (Ref, error) {
	abs, err := Expand(input)
	if err != nil {
		return Ref{}, err
	}

	fi, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Ref{}, fmt.Errorf("no such directory: %s", Shorten(abs))
		}
		return Ref{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, terr := filepath.EvalSymlinks(abs)
		if terr != nil {
			return Ref{}, fmt.Errorf("%s is a symlink that cannot be resolved: %w", Shorten(abs), terr)
		}
		return Ref{}, fmt.Errorf("%s is a symlink; park its target instead: %s", Shorten(abs), Shorten(target))
	}
	if !fi.IsDir() {
		return Ref{}, fmt.Errorf("not a directory: %s", Shorten(abs))
	}

	ref, err := Locate(abs)
	if err != nil {
		return Ref{}, err
	}

	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		if canonHome, cerr := Canonical(home); cerr == nil && ref.Path == canonHome {
			return Ref{}, errors.New("refusing to park your home directory")
		}
	}
	for _, reserved := range reservedDirs {
		canon, cerr := Canonical(reserved)
		if cerr != nil {
			continue
		}
		if ref.Path == canon || isAncestor(ref.Path, canon) || isAncestor(canon, ref.Path) {
			return Ref{}, fmt.Errorf("refusing to park %s: it overlaps clav's own storage at %s",
				Shorten(ref.Path), Shorten(canon))
		}
	}
	if _, err := os.Open(ref.Path); err != nil {
		return Ref{}, fmt.Errorf("cannot read %s: %w", Shorten(ref.Path), err)
	}
	return ref, nil
}

// isAncestor reports whether ancestor contains descendant.
func isAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return false
	}
	return strings.HasPrefix(descendant, strings.TrimRight(ancestor, string(os.PathSeparator))+string(os.PathSeparator))
}

// RootMarkers are the files and directories that mark a project root, checked
// in this order at each level.
//
// This is only used to answer "which project am I in?" when no path is given.
// It has nothing to do with what gets archived: once a root is chosen the whole
// directory is archived, regardless of Git or any other tool.
//
// ".clav-root" comes first so an empty file of that name can override the
// detection in a project this list does not cover.
var RootMarkers = []string{
	".clav-root",
	".git", ".hg", ".svn", ".jj",
	"go.mod", "Cargo.toml", "package.json", "deno.json", "pyproject.toml",
	"composer.json", "Gemfile", "mix.exs", "pubspec.yaml", "build.sbt",
	"pom.xml", "build.gradle", "build.gradle.kts", "flake.nix",
}

// FindRoot walks up from start looking for a project root, so that `clav park`
// run from deep inside a project still means the project.
//
// The nearest marker wins. The search stops at the home directory and at the
// filesystem root, so it can never wander into a parent of your whole
// workspace.
func FindRoot(start string) (root, marker string, found bool) {
	dir, err := Canonical(start)
	if err != nil {
		return "", "", false
	}
	home := ""
	if h, herr := os.UserHomeDir(); herr == nil && h != "" {
		if c, cerr := Canonical(h); cerr == nil {
			home = c
		}
	}
	for {
		if dir == string(os.PathSeparator) || (home != "" && dir == home) {
			return "", "", false
		}
		for _, m := range RootMarkers {
			if _, err := os.Lstat(filepath.Join(dir, m)); err == nil {
				return dir, m, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// Stat is the result of scanning a project directory.
type Stat struct {
	Files    int
	Dirs     int
	Symlinks int
	Special  int
	Skipped  []string // entries that cannot be archived (e.g. sockets)
	Size     int64    // sum of apparent file sizes
	Entries  int      // total entries that will be written to the archive
}

// Scan measures a project directory without following symlinks. Errors reading
// individual entries are returned rather than ignored: clav must not park a
// project it cannot fully read.
func Scan(root string, filter Filter) (Stat, error) {
	var st Stat
	st.Dirs++ // the root itself is archived as an entry
	st.Entries++

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", Shorten(path), err)
		}
		if path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if filter != nil && filter.Exclude(rel, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return fmt.Errorf("%s: %w", Shorten(path), ierr)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			st.Symlinks++
		case info.IsDir():
			st.Dirs++
		case info.Mode().IsRegular():
			st.Files++
			st.Size += info.Size()
		case info.Mode()&os.ModeSocket != 0:
			st.Skipped = append(st.Skipped, rel)
			return nil
		default:
			st.Special++
		}
		st.Entries++
		return nil
	})
	if err != nil {
		return Stat{}, err
	}
	return st, nil
}
