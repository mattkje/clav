//go:build unix

package archive

import (
	"archive/tar"
	"fmt"
	"os"
	"syscall"
)

// osNoFollow is added to open flags so the final path element is never
// traversed through a symlink.
const osNoFollow = syscall.O_NOFOLLOW

// hardLinkKey returns a device+inode identity for files with more than one
// link, so that hard links can be represented as links in the archive rather
// than duplicated content.
func hardLinkKey(fi os.FileInfo) (string, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink < 2 {
		return "", false
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)), true
}

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// lchtimes would set a symlink's own timestamps. Doing so portably needs
// utimensat with AT_SYMLINK_NOFOLLOW, which the standard library does not
// expose; clav deliberately does not add a dependency for it. Symlink target
// and permissions are preserved, only the link's own mtime is not.
func lchtimes(string, *tar.Header) error { return nil }

// makeSpecial recreates FIFOs. Device nodes need root and effectively never
// appear inside a project directory, so they are reported instead of recreated.
func makeSpecial(path string, h *tar.Header) error {
	switch h.Typeflag {
	case tar.TypeFifo:
		return syscall.Mkfifo(path, uint32(h.FileInfo().Mode().Perm()))
	default:
		return fmt.Errorf("recreating device nodes is not supported")
	}
}
