//go:build unix

package state

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lock takes an exclusive advisory lock on ~/.clav/lock so that two concurrent
// clav processes cannot interleave state mutations.
func (s *Store) lock() (func(), error) {
	path := filepath.Join(s.root, "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("cannot lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
