//go:build !unix

package state

// lock is a no-op on platforms without flock. clav targets macOS and Linux;
// this stub only exists so the package still compiles elsewhere.
func (s *Store) lock() (func(), error) {
	return func() {}, nil
}
