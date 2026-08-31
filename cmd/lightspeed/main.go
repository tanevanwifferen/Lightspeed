// Lightspeed is gopls's command-line interface generalized to every
// language server. See PLAN.md; this is the M0 spike.
package main

import (
	"os"

	"github.com/tanevanwifferen/Lightspeed/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
