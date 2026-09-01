// Lightspeed is gopls's command-line interface generalized to every
// language server: it answers definition, references, implementation,
// hover, symbols and workspace_symbol for whichever server handles the
// file, in a machine-readable envelope with an exit-code taxonomy an
// agent can branch on. See PLAN.md.
package main

import (
	"os"

	"github.com/tanevanwifferen/Lightspeed/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
