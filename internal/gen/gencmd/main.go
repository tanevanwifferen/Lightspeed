// Command gencmd regenerates lightspeed's built-in server table from
// the vendored nvim-lspconfig snapshot. It is the program behind the
// go:generate directive in internal/serverdef, takes no input but the
// checked-in corpus, and touches no network.
//
// Usage:
//
//	go generate ./internal/serverdef/
//	go run ./internal/gen/gencmd -out internal/serverdef/builtins_gen.go
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/tanevanwifferen/Lightspeed/internal/gen"
)

func main() {
	out := flag.String("out", "builtins_gen.go",
		"where to write the generated table; relative to the working directory, which under go:generate is the package directory")
	flag.Parse()

	path, err := gen.ResolveOutput(*out)
	if err != nil {
		fail(err)
	}
	if err := gen.Write(path); err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s from %s @ %s\n", path, gen.CorpusName, gen.CorpusCommit[:12])
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "gencmd: %v\n", err)
	os.Exit(1)
}
