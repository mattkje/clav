package project

import (
	"io"
	"os"
	"path/filepath"
)

// MergeResult reports what MergeInto did.
type MergeResult struct {
	Moved     int
	Conflicts []string // paths that already existed in dst
}

// Overlay moves the contents of src into dst, which already exists and is kept
// in place — the same directory, with the same inode, so a shell standing in it
// stays where it is. That is the whole point: restoring a project must not pull
// the ground out from under the terminal it was run from.
//
// Where both sides have a directory, the merge continues inside it. Where both
// have a file, the incoming one wins and the one already there is moved aside
// with a ".clav-kept" suffix and reported, so neither copy is ever lost.
func Overlay(src, dst string) (MergeResult, error) {
	var res MergeResult
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return res, err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return res, err
	}
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())

		existing, serr := os.Lstat(to)
		switch {
		case serr != nil && !os.IsNotExist(serr):
			return res, serr

		case serr != nil:
			// Nothing in the way: one rename moves the whole subtree.
			if err := move(from, to); err != nil {
				return res, err
			}
			res.Moved++

		case existing.IsDir() && e.IsDir():
			sub, err := Overlay(from, to)
			res.Moved += sub.Moved
			for _, c := range sub.Conflicts {
				res.Conflicts = append(res.Conflicts, filepath.ToSlash(filepath.Join(e.Name(), c)))
			}
			if err != nil {
				return res, err
			}
			// The subdirectory has been emptied into its counterpart.
			_ = os.Remove(from)

		default:
			if err := move(to, to+".clav-kept"); err != nil {
				return res, err
			}
			if err := move(from, to); err != nil {
				return res, err
			}
			res.Conflicts = append(res.Conflicts, e.Name())
			res.Moved++
		}
	}
	return res, nil
}

// Wipe empties a directory without removing the directory itself.
func Wipe(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// move renames a file or directory, falling back to a copy when src and dst are
// on different filesystems.
func move(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, rerr := os.ReadDir(src)
		if rerr != nil {
			return rerr
		}
		for _, e := range entries {
			if err := move(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return os.Remove(src)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, rerr := os.Readlink(src)
		if rerr != nil {
			return rerr
		}
		if err := os.Symlink(link, dst); err != nil {
			return err
		}
		return os.Remove(src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}
