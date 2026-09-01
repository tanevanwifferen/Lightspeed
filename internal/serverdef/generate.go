package serverdef

// The built-in default layer of PLAN §6 item 3 is generated, not
// written. internal/gen reads the vendored nvim-lspconfig snapshot —
// checked in, Apache-2.0, no network — and emits builtins_gen.go.
//
// Regenerate after refreshing the snapshot:
//
//	go generate ./internal/serverdef/
//
//go:generate go run github.com/tanevanwifferen/Lightspeed/internal/gen/gencmd -out builtins_gen.go

// A Provenance says where the built-in defaults came from. It travels
// into `lightspeed servers` and `lightspeed doctor` output so that a
// default can always be traced to the corpus that produced it, rather
// than looking like an opinion lightspeed invented.
type Provenance struct {
	// Corpus is the upstream project, "nvim-lspconfig".
	Corpus string `json:"corpus"`
	// Upstream is its repository URL.
	Upstream string `json:"upstream"`
	// Commit is the exact commit vendored.
	Commit string `json:"commit"`
	// License is the upstream license identifier.
	License string `json:"license"`
	// Snapshot is where the vendored copy lives in this repository.
	Snapshot string `json:"snapshot"`
}

// BuiltinProvenance reports where the built-in table came from.
func BuiltinProvenance() Provenance { return generatedProvenance }
