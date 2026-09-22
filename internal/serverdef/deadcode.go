package serverdef

import (
	"fmt"
	"os"
	"path/filepath"
)

// DeadCodeTable is the top-level table of a workspace's .lightspeed.toml that
// configures `lightspeed dead_code` (docs/DECISIONS.md D39). It is a setting of
// a command, not part of a server definition, so it lives beside the
// definitions without being folded into them: a file may carry it with either
// definition shape or alone.
const DeadCodeTable = "dead_code"

// DeadCodeConfig is what a workspace tells `dead_code` never to report.
type DeadCodeConfig struct {
	// IgnoreNames are symbol names: a bare name, a qualified `Type.Method`, or a
	// pattern with `*`.
	IgnoreNames []string
	// IgnorePaths are workspace-relative globs.
	IgnorePaths []string
}

// LoadDeadCodeConfig reads the [dead_code] table of root's .lightspeed.toml. A
// missing file or table is an empty configuration; a malformed one is an error
// naming the key, like the rest of the file (an ignore that silently does
// nothing is the failure the strict parser exists to prevent).
func LoadDeadCodeConfig(root string) (*DeadCodeConfig, error) {
	path := filepath.Join(root, WorkspaceFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &DeadCodeConfig{}, nil
		}
		return nil, err
	}
	cfg, err := ParseDeadCodeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseDeadCodeConfig decodes the [dead_code] table of one configuration file.
func ParseDeadCodeConfig(data []byte) (*DeadCodeConfig, error) {
	raw, err := parseTOML(data)
	if err != nil {
		return nil, fmt.Errorf("invalid TOML: %w", err)
	}
	tbl, err := tableKey("", raw, DeadCodeTable)
	if err != nil {
		return nil, err
	}
	cfg := &DeadCodeConfig{}
	if tbl == nil {
		return cfg, nil
	}
	if err := checkKeys(DeadCodeTable, tbl, "ignore_names", "ignore_paths"); err != nil {
		return nil, err
	}
	if cfg.IgnoreNames, err = stringsKey(DeadCodeTable, tbl, "ignore_names"); err != nil {
		return nil, err
	}
	if cfg.IgnorePaths, err = stringsKey(DeadCodeTable, tbl, "ignore_paths"); err != nil {
		return nil, err
	}
	return cfg, nil
}

// onlyToolSettings reports whether a decoded file sets nothing but tool
// settings (and its schema_version): no server name, table or definition.
func onlyToolSettings(raw map[string]any) bool {
	if _, ok := raw[DeadCodeTable]; !ok {
		return false
	}
	for _, k := range []string{"name", "activation", "server", "install"} {
		if _, ok := raw[k]; ok {
			return false
		}
	}
	return true
}
