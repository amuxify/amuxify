// Command amuxify is the ingest gate for self-hosted media: scan, remux,
// clean, doctor. See internal/cli for the dispatcher.
package main

import (
	"os"

	"github.com/amuxify/amuxify/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
