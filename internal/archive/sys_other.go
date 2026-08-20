//go:build !unix

package archive

import (
	"archive/tar"
	"errors"
	"os"
)

// clav targets macOS and Linux. These stubs exist only so the package compiles
// on other platforms; hard links are stored as ordinary files there.

const osNoFollow = 0

func hardLinkKey(os.FileInfo) (string, bool) { return "", false }

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }

func lchtimes(string, *tar.Header) error { return nil }

func makeSpecial(string, *tar.Header) error {
	return errors.New("special files are not supported on this platform")
}
