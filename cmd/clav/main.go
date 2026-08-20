// Command clav parks local projects to reclaim disk space and restores them
// later, byte for byte.
package main

import (
	"os"

	"clav/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
